package handlers

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

var (
	errSubtitleIdentityInvalid     = errors.New("invalid subtitle identity")
	errSubtitleIdentityUnavailable = errors.New("the selected subtitle identity is unavailable")
)

// resolveSubtitleEditionSwitch re-maps a subtitle request that names the
// session's requested (old) edition after an edition switch moved the effective
// file. The requested edition's inventory is not what is playing: interpreting
// its ordinal against the effective file can serve a different language.
//
// The named file is treated as foreign unless the request's pinned identity
// proves it exists in the current plan's inventory. A pin that resolves against
// the effective file does prove it, and the effective file with that resolved
// ordinal is returned. A pin that does not resolve against the effective file —
// or a bare ordinal carry — is translated from the named file's segment
// identity (external path/language or embedded language/codec/flags) onto the
// effective file's inventory. A miss returns errSubtitleIdentityUnavailable so
// the client degrades to subtitles-off instead of being served a wrong track.
func (h *StreamHandler) resolveSubtitleEditionSwitch(
	ctx context.Context,
	namedFile, effectiveFile *models.MediaFile,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	if namedFile == nil || effectiveFile == nil || namedFile.ID == effectiveFile.ID {
		return namedFile, index, nil
	}
	// A pinned identity that resolves against the effective file is the
	// current plan's own track: honor it directly.
	if pinned := hasSubtitleIdentityPin(query); pinned {
		if resolved, err := subtitleRouteIndex(effectiveFile, index, query); err == nil {
			return effectiveFile, resolved, nil
		}
	}
	// A named row with no subtitle inventory has nothing to remap FROM: it is
	// a catalog placeholder, and any selection the client made was against a
	// resolved candidate's track list. The plan-time ordinal already names the
	// effective inventory.
	if len(namedFile.ExternalSubtitles) == 0 && len(namedFile.SubtitleTracks) == 0 {
		if resolved, err := subtitleRouteIndex(effectiveFile, index, query); err == nil {
			return effectiveFile, resolved, nil
		}
		return nil, 0, errSubtitleIdentityUnavailable
	}
	location, ok := classifySubtitleIndexV3(namedFile, index)
	if !ok {
		return nil, 0, errSubtitleIdentityUnavailable
	}
	switch location.source {
	case playback.SubtitleSourceExternalV3:
		wanted := namedFile.ExternalSubtitles[location.offset]
		for candidateIndex, candidate := range effectiveFile.ExternalSubtitles {
			equivalent := strings.EqualFold(candidate.Language, wanted.Language) &&
				strings.EqualFold(candidate.Format, wanted.Format) &&
				candidate.Forced == wanted.Forced &&
				candidate.HearingImpaired == wanted.HearingImpaired
			if candidate.Path != wanted.Path && !equivalent {
				continue
			}
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(effectiveFile, candidateIndex); mapped {
				return effectiveFile, published, nil
			}
		}
	case playback.SubtitleSourceEmbeddedV3:
		wanted := namedFile.SubtitleTracks[location.offset]
		if ordinal, _, matched := playback.MatchEmbeddedSubtitleTrack(wanted, effectiveFile.SubtitleTracks); matched {
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(effectiveFile, len(effectiveFile.ExternalSubtitles)+ordinal); mapped {
				return effectiveFile, published, nil
			}
		}
	default:
		// Downloaded rows are file-bound. Only a stable row-id match against the
		// effective file's own downloaded list may survive the switch.
		if h.SubtitleRepo != nil {
			wantedList, wantedErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, namedFile.ID)
			targetList, targetErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, effectiveFile.ID)
			if wantedErr == nil && targetErr == nil && location.offset >= 0 && location.offset < len(wantedList) {
				wanted := wantedList[location.offset]
				base := len(playback.BuildSubtitleInventoryV3(effectiveFile, nil))
				for candidateIndex, candidate := range targetList {
					if wanted.ID > 0 && candidate.ID == wanted.ID {
						return effectiveFile, base + candidateIndex, nil
					}
				}
			}
		}
	}
	slog.InfoContext(ctx, "subtitle edition switch has no equivalent track on the effective file",
		"component", "api", "named_file_id", namedFile.ID, "effective_file_id", effectiveFile.ID, "track_index", index)
	return nil, 0, errSubtitleIdentityUnavailable
}

// resolveSubtitleSourceRequest resolves the media file and combined ordinal a
// subtitle request serves from. It first handles an edition switch: when the
// request names the session's requested (old) edition while a different
// effective file is playing, the ordinal is re-mapped onto the effective
// inventory (see resolveSubtitleEditionSwitch). Otherwise it binds the
// session's planned virtual candidate and resolves any pinned identity against
// the bound file.
func (h *StreamHandler) resolveSubtitleSourceRequest(
	ctx context.Context,
	namedFile *models.MediaFile,
	session *playback.Session,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	if namedFile == nil || session == nil {
		return namedFile, index, nil
	}
	// After an edition switch the session's effective file differs from the
	// requested edition. A request naming the requested (old) edition must not
	// interpret its ordinal against that stale inventory.
	if session.MediaFileID > 0 && session.MediaFileID != namedFile.ID {
		effective, err := h.fileResolver.GetByID(ctx, session.MediaFileID)
		if err != nil || effective == nil {
			return nil, 0, errors.New("media file not found")
		}
		effective = bindSessionVirtualSourceWithTracks(ctx, effective, session, h.fileResolver)
		resolvedFile, resolvedIndex, switchErr := h.resolveSubtitleEditionSwitch(ctx, namedFile, effective, index, query)
		if switchErr != nil {
			return nil, 0, switchErr
		}
		return resolvedFile, resolvedIndex, nil
	}
	file := bindSessionVirtualSourceWithTracks(ctx, namedFile, session, h.fileResolver)
	resolvedIndex, err := subtitleRouteIndex(file, index, query)
	if err != nil {
		return nil, 0, err
	}
	return file, resolvedIndex, nil
}

// hasSubtitleIdentityPin reports whether the request carries one of the stable
// subtitle identity pins (embedded stream index, external path key, downloaded
// row id).
func hasSubtitleIdentityPin(query url.Values) bool {
	for _, key := range []string{
		playback.EmbeddedSubtitleStreamIndexParamV3,
		playback.ExternalSubtitleKeyParamV3,
		playback.DownloadedSubtitleIDParamV3,
	} {
		if query.Get(key) != "" {
			return true
		}
	}
	return false
}

// subtitleRouteIndex resolves a pinned identity before applying the combined
// ordinal dispatch. Old URLs without a pin retain their original behavior.
func subtitleRouteIndex(file *models.MediaFile, index int, query url.Values) (int, error) {
	pins := 0
	for _, key := range []string{
		playback.EmbeddedSubtitleStreamIndexParamV3,
		playback.ExternalSubtitleKeyParamV3,
		playback.DownloadedSubtitleIDParamV3,
	} {
		if values, ok := query[key]; ok {
			if len(values) != 1 || values[0] == "" {
				return 0, errSubtitleIdentityInvalid
			}
			pins++
		}
	}
	if pins > 1 {
		return 0, errSubtitleIdentityInvalid
	}
	if value := query.Get(playback.EmbeddedSubtitleStreamIndexParamV3); value != "" {
		streamIndex, err := strconv.Atoi(value)
		if err != nil || streamIndex < 0 {
			return 0, errSubtitleIdentityInvalid
		}
		match := -1
		for ordinal, track := range file.SubtitleTracks {
			if track.Index == streamIndex {
				if match >= 0 {
					return 0, errSubtitleIdentityUnavailable
				}
				match = len(file.ExternalSubtitles) + ordinal
			}
		}
		if match < 0 {
			// Virtual sources plan against provider-declared placeholder
			// tracks (Index 0, no codec) and then inherit the real probed
			// tracks at stream time, whose container indices differ. The
			// combined ordinal is the stable identity across that transition,
			// so fall back to it rather than 404ing the pinned URL. Local
			// files keep strict matching: a pin that no longer resolves means
			// the inventory changed and serving a different track would be
			// wrong.
			if isVirtualPlaybackFile(file) {
				return index, nil
			}
			return 0, errSubtitleIdentityUnavailable
		}
		return match, nil
	}
	if key := query.Get(playback.ExternalSubtitleKeyParamV3); key != "" {
		if _, err := hex.DecodeString(key); len(key) != 64 || err != nil {
			return 0, errSubtitleIdentityInvalid
		}
		match := -1
		for ordinal, subtitle := range file.ExternalSubtitles {
			if playback.ExternalSubtitlePathKeyV3(subtitle.Path) == key {
				if match >= 0 {
					return 0, errSubtitleIdentityUnavailable
				}
				match = ordinal
			}
		}
		if match < 0 {
			return 0, errSubtitleIdentityUnavailable
		}
		return match, nil
	}
	return index, nil
}
