package playback

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// Subtitle source classes in the combined-ordinal space.
const (
	SubtitleSourceExternalV3   = "external"
	SubtitleSourceEmbeddedV3   = "embedded"
	SubtitleSourceDownloadedV3 = "downloaded"
)

// Sidecar file extensions the stream handler serves a subtitle track as. The
// extension is part of the published URL, so it belongs to the contract rather
// than to whichever call site happens to build a path.
const (
	SubtitleExtASSV3 = ".ass"
	SubtitleExtPGSV3 = ".sup"
	SubtitleExtVTTV3 = ".vtt"
	// DownloadedSubtitleIDParamV3 pins a generated/downloaded sidecar URL to
	// its stable database row instead of resolving a mutable inventory ordinal.
	DownloadedSubtitleIDParamV3 = "downloaded_subtitle_id"
)

// WebVTT is the format every convert-mode subtitle lands in, so its format
// token and media type travel together with the extension above: a plan that
// advertises one of the three and serves another hands the client an artifact
// its parser rejects.
const (
	SubtitleFormatVTTV3 = "vtt"
	SubtitleMIMEVTTV3   = "text/vtt"
)

// Subtitle delivery classes describing how a track reaches the screen.
const (
	// SubtitleDeliverySidecarV3 marks a track the client can fetch directly
	// from its published URL.
	SubtitleDeliverySidecarV3 = "sidecar"
	// SubtitleDeliveryBurnInOnlyV3 marks a track with no client-fetchable
	// representation: DVD/DVB bitmap streams have no sidecar shape the stream
	// handler can serve, so server-side burn-in is the only route. The track
	// still occupies its combined ordinal and stays selectable; it carries no
	// URL.
	SubtitleDeliveryBurnInOnlyV3 = "burn_in_only"
)

// SubtitleInventoryItemV3 is one selectable subtitle track at its frozen
// combined ordinal.
type SubtitleInventoryItemV3 struct {
	TrackID       string `json:"track_id"`
	CombinedIndex int    `json:"combined_index"`
	Source        string `json:"source"`
	Codec         string `json:"codec,omitempty"`
	Language      string `json:"language,omitempty"`
	Label         string `json:"label,omitempty"`
	Forced        bool   `json:"forced"`
	// Default marks the track the source container flags as its default
	// selection. Only embedded and external tracks can carry it.
	Default         bool   `json:"default"`
	HearingImpaired bool   `json:"hearing_impaired"`
	Delivery        string `json:"delivery"`
	// URL is the session-scoped sidecar URL, present only on
	// SubtitleDeliverySidecarV3 tracks and only once a session exists.
	URL string `json:"url,omitempty"`
	// FontBundleURL is the attachment bundle needed to render an embedded
	// ASS/SSA track with its authored typesetting.
	FontBundleURL string `json:"font_bundle_url,omitempty"`
	// downloadedSubtitleID is deliberately not serialized: clients address the
	// opaque session URL, while the server uses the row ID to make that URL
	// stable across inventory reordering and seek reanchors.
	downloadedSubtitleID int
}

// BuildSubtitleInventoryV3 is the single normative implementation of the
// combined-ordinal ordering rule, and the only place in the server that
// assigns subtitle ordinals.
//
// Ordinals are assigned across three consecutive ranges, in this order:
//
//	[0, len(ExternalSubtitles))                       external sidecar files
//	[len(External), len(External)+len(SubtitleTracks)) embedded container tracks
//	[that, +len(additional))                           downloaded/generated tracks
//
// Every track occupies the ordinal of its position in its source array, so the
// stream handler and subtitleEntryAtCombinedIndexV3 resolve a published
// ordinal against the same array slot. With no suppressed duplicates the
// ordinals are dense and gap-free; a duplicate suppressed by the de-duplication
// below simply leaves its ordinal unpublished, and the tracks after it keep
// their original ordinals (and remain correctly addressable).
//
// A track that cannot be delivered as a sidecar is still published — with
// SubtitleDeliveryBurnInOnlyV3 and no URL — rather than omitted, so the
// ordinal space never shifts under a client.
//
// Within each range the order is the source order: catalog order for
// externals, container stream order for embedded tracks, and
// ListDownloadedSubtitles' created_at order for downloaded ones. Ordinals are
// therefore stable for as long as the file's track set is, which is what makes
// the `file:{id}:subtitle:{ordinal}` identity meaningful.
//
// Entries are collapsed only when they positively identify the same track: the
// same source range, the same non-empty base language (or, for downloaded
// rows, the same positive stable row ID), the same normalized codec, and the
// same forced and hearing-impaired flags. An entry with an unknown language and
// no other positive identity is never suppressed, so several unknown-language
// tracks that merely share a codec or flags are all published. The seen-set
// spans all three ranges, so a duplicate later in the combined list
// de-duplicates against an earlier one. Downloaded entries key on their stable
// row ID, so two distinct downloads in one language are both kept, and a
// downloaded or AI subtitle is never dropped merely because an embedded track
// shares its language (the source range differs).
//
// The returned items carry no URLs; use SubtitleInventoryV3 once a session
// exists.
func BuildSubtitleInventoryV3(file *models.MediaFile, additional []SubtitleInventoryEntryV3) []SubtitleInventoryItemV3 {
	if file == nil {
		return nil
	}
	externalCount := len(file.ExternalSubtitles)
	base := externalCount + len(file.SubtitleTracks)
	items := make([]SubtitleInventoryItemV3, 0, base+len(additional))
	seen := make(map[string]struct{}, base+len(additional))

	for index, sub := range file.ExternalSubtitles {
		if subtitleInventoryDuplicateV3(seen, SubtitleSourceExternalV3, "", sub.Format, sub.Language, sub.Forced, sub.HearingImpaired) {
			continue
		}
		items = append(items, subtitleInventoryItemV3(file.ID, index, SubtitleSourceExternalV3, sub.Format,
			sub.Language, firstNonEmptySubtitleLabelV3(sub.Title, sub.EmbeddedTitle, filepath.Base(sub.Path), sub.Language),
			sub.Forced, sub.Default, sub.HearingImpaired))
	}
	for index, track := range file.SubtitleTracks {
		if subtitleInventoryDuplicateV3(seen, SubtitleSourceEmbeddedV3, "", track.Codec, track.Language, track.Forced, track.HearingImpaired) {
			continue
		}
		items = append(items, subtitleInventoryItemV3(file.ID, externalCount+index, SubtitleSourceEmbeddedV3, track.Codec,
			track.Language, firstNonEmptySubtitleLabelV3(track.Title, track.EmbeddedTitle, track.Language),
			track.Forced, track.Default, track.HearingImpaired))
	}
	for index, entry := range additional {
		source := entry.Source
		if source == "" {
			source = SubtitleSourceDownloadedV3
		}
		identity := ""
		if source == SubtitleSourceDownloadedV3 && entry.DownloadedSubtitleID > 0 {
			identity = strconv.Itoa(entry.DownloadedSubtitleID)
		}
		if subtitleInventoryDuplicateV3(seen, source, identity, entry.Codec, entry.Language, entry.Forced, entry.HearingImpaired) {
			continue
		}
		ordinal := entry.CombinedIndex
		if ordinal <= 0 {
			ordinal = base + index
		}
		item := subtitleInventoryItemV3(file.ID, ordinal, source, entry.Codec,
			entry.Language, firstNonEmptySubtitleLabelV3(entry.Label, entry.Language),
			entry.Forced, false, entry.HearingImpaired)
		item.downloadedSubtitleID = entry.DownloadedSubtitleID
		items = append(items, item)
	}
	return items
}

// subtitleInventoryDuplicateV3 records an inventory entry's de-duplication key
// and reports whether an equivalent track was already seen.
//
// Suppression requires a positive identity for the track: a recognized,
// non-empty canonical language, or (for downloaded rows) a positive stable row
// ID, passed as identity. An entry with neither is never a de-duplication
// candidate and is never recorded, so two tracks whose language is unknown are
// never collapsed merely because their codec, forced flag or hearing-impaired
// flag coincide. The key pairs that identity with the source range, normalized
// codec and flags, so entries differing in any of those stay distinct.
func subtitleInventoryDuplicateV3(seen map[string]struct{}, source, identity, codec, language string, forced, hearingImpaired bool) bool {
	base := stream.CanonicalLanguageBase(language)
	if base == "" && identity == "" {
		return false
	}
	key := strings.Join([]string{
		source,
		identity,
		normalizeCodecV3(codec),
		base,
		strconv.FormatBool(forced),
		strconv.FormatBool(hearingImpaired),
	}, "\x00")
	if _, ok := seen[key]; ok {
		return true
	}
	seen[key] = struct{}{}
	return false
}

// SubtitleInventoryV3 returns the combined-ordinal inventory with
// session-scoped stream URLs attached to every sidecar-deliverable track.
func SubtitleInventoryV3(sessionID string, file *models.MediaFile, additional []SubtitleInventoryEntryV3) []SubtitleInventoryItemV3 {
	return ScopeSubtitleInventoryV3(sessionID, file, BuildSubtitleInventoryV3(file, additional))
}

// ScopeSubtitleInventoryV3 attaches session URLs to an already planned
// inventory without resolving it again against mutable subtitle repositories.
// This keeps the advertised menu and the planner's selected ordinal on the
// same snapshot.
func ScopeSubtitleInventoryV3(sessionID string, file *models.MediaFile, inventory []SubtitleInventoryItemV3) []SubtitleInventoryItemV3 {
	// Inventory is required by the v3 wire contract and must always encode as
	// an array. Copy into a non-nil slice so an empty inventory remains []
	// instead of becoming JSON null while session URLs are attached.
	items := append([]SubtitleInventoryItemV3{}, inventory...)
	if sessionID == "" || file == nil {
		return items
	}
	for i := range items {
		if items[i].Delivery != SubtitleDeliverySidecarV3 {
			continue
		}
		previousURL := items[i].URL
		items[i].URL = SubtitleStreamURLV3(sessionID, items[i].CombinedIndex, items[i].Codec, file.ID)
		if items[i].Source == SubtitleSourceDownloadedV3 && items[i].downloadedSubtitleID > 0 {
			items[i].URL = DownloadedSubtitleStreamURLV3(
				sessionID,
				items[i].CombinedIndex,
				items[i].Codec,
				file.ID,
				items[i].downloadedSubtitleID,
			)
		}
		switch items[i].Source {
		case SubtitleSourceExternalV3:
			key := subtitleURLIdentityV3(previousURL, ExternalSubtitleKeyParamV3, file.ID)
			if index := items[i].CombinedIndex; key == "" && index >= 0 && index < len(file.ExternalSubtitles) {
				key = ExternalSubtitlePathKeyV3(file.ExternalSubtitles[index].Path)
			}
			if key != "" {
				items[i].URL += "&" + ExternalSubtitleKeyParamV3 + "=" + key
			}
		case SubtitleSourceEmbeddedV3:
			items[i].FontBundleURL = SubtitleFontBundleURLV3(sessionID, items[i].CombinedIndex, items[i].Codec, file.ID)
			index := subtitleURLIdentityV3(previousURL, EmbeddedSubtitleStreamIndexParamV3, file.ID)
			if ordinal := items[i].CombinedIndex - len(file.ExternalSubtitles); index == "" && ordinal >= 0 && ordinal < len(file.SubtitleTracks) {
				index = strconv.Itoa(file.SubtitleTracks[ordinal].Index)
			}
			if index != "" {
				identity := "&" + EmbeddedSubtitleStreamIndexParamV3 + "=" + index
				items[i].URL += identity
				if items[i].FontBundleURL != "" {
					items[i].FontBundleURL += identity
				}
			}
		}
	}
	return items
}

// SubtitleInventoryNeedsDownloadedIdentityV3 reports whether an inventory was
// decoded from its wire form and therefore lost the server-only row IDs needed
// to mint stable downloaded-subtitle URLs. Freshly planned inventories retain
// the IDs in memory; durable JSON plans are rebuilt from the repository.
func SubtitleInventoryNeedsDownloadedIdentityV3(items []SubtitleInventoryItemV3) bool {
	for _, item := range items {
		if item.Source == SubtitleSourceDownloadedV3 && item.downloadedSubtitleID <= 0 {
			return true
		}
	}
	return false
}

// SubtitleInventoryItemAtV3 finds the inventory entry at a combined ordinal.
func SubtitleInventoryItemAtV3(items []SubtitleInventoryItemV3, combinedIndex int) (SubtitleInventoryItemV3, bool) {
	for _, item := range items {
		if item.CombinedIndex == combinedIndex {
			return item, true
		}
	}
	return SubtitleInventoryItemV3{}, false
}

// SubtitleURLExtV3 returns the sidecar file extension the stream handler serves
// a subtitle codec as.
func SubtitleURLExtV3(codec string) string {
	switch {
	case IsASS(codec):
		return SubtitleExtASSV3
	case IsPGS(codec):
		return SubtitleExtPGSV3
	}
	return SubtitleExtVTTV3
}

// SubtitleStreamURLV3 builds the session-scoped sidecar URL for a combined
// ordinal. The ordinal in the path is the combined one, not the container's
// stream index: the stream handler resolves it back through the same ranges
// BuildSubtitleInventoryV3 assigns.
func SubtitleStreamURLV3(sessionID string, combinedIndex int, codec string, fileID int) string {
	return fmt.Sprintf("/stream/%s/subtitles/%d%s?file_id=%d", sessionID, combinedIndex, SubtitleURLExtV3(codec), fileID)
}

// DownloadedSubtitleStreamURLV3 binds the public combined ordinal to the
// stable downloaded-subtitle row selected when the plan was accepted.
func DownloadedSubtitleStreamURLV3(sessionID string, combinedIndex int, codec string, fileID, downloadedSubtitleID int) string {
	return SubtitleStreamURLV3(sessionID, combinedIndex, codec, fileID) +
		"&" + DownloadedSubtitleIDParamV3 + "=" + strconv.Itoa(downloadedSubtitleID)
}

// SubtitleFontBundleURLV3 builds the attachment-bundle URL for an embedded
// ASS/SSA track, or "" for any other codec.
func SubtitleFontBundleURLV3(sessionID string, combinedIndex int, codec string, fileID int) string {
	if !IsASS(codec) {
		return ""
	}
	return fmt.Sprintf("/stream/%s/subtitles/%d/fonts?file_id=%d", sessionID, combinedIndex, fileID)
}

func subtitleInventoryItemV3(fileID, combinedIndex int, source, codec, language, label string, forced, isDefault, hearingImpaired bool) SubtitleInventoryItemV3 {
	return SubtitleInventoryItemV3{
		TrackID:         TrackIDV3(fileID, "subtitle", combinedIndex),
		CombinedIndex:   combinedIndex,
		Source:          source,
		Codec:           codec,
		Language:        language,
		Label:           label,
		Forced:          forced,
		Default:         isDefault,
		HearingImpaired: hearingImpaired,
		Delivery:        subtitleDeliveryClassV3(source, codec),
	}
}

// subtitleDeliveryClassV3 mirrors what the stream handler can actually serve:
// text tracks convert to WebVTT/ASS, and an embedded PGS track extracts
// losslessly to a .sup elementary stream. Every other bitmap shape — DVD/DVB
// embedded, and any bitmap arriving as an external or downloaded file — has no
// sidecar representation, so it is burn-in only.
func subtitleDeliveryClassV3(source, codec string) string {
	switch {
	case isTextSubtitleV3(codec):
		return SubtitleDeliverySidecarV3
	case source == SubtitleSourceEmbeddedV3 && IsPGS(codec):
		return SubtitleDeliverySidecarV3
	}
	return SubtitleDeliveryBurnInOnlyV3
}

func firstNonEmptySubtitleLabelV3(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
