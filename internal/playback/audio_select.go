package playback

import (
	"strings"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// OriginalLanguageSentinel is the value stored in audio language preference
// columns to mean "use the media item's original language." It is resolved
// to a concrete language code in the playback handler before reaching
// SelectAudioTrack.
const OriginalLanguageSentinel = "original"

// AudioTrackPreference holds a per-series audio track preference.
type AudioTrackPreference struct {
	AudioTrackIndex int
	AudioLanguage   string
	TrackSignature  *userstore.AudioTrackSignature
}

// langMatch accepts compatible languages for previously saved track selections.
func langMatch(a, b string) bool { return langMatchRank(a, b) >= 0 }

// langMatchRank prefers an exact BCP-47 tag, then a bare language tag, and
// finally another regional/script variant of the same language.
func langMatchRank(candidate, preferred string) int {
	candidate = lang.CompatibleTag(candidate)
	preferred = lang.CompatibleTag(preferred)
	if candidate == "" || preferred == "" {
		return -1
	}
	if candidate == preferred {
		return 0
	}
	candidateBase := lang.PrimaryLanguage(candidate)
	preferredBase := lang.PrimaryLanguage(preferred)
	if candidateBase == "" || candidateBase != preferredBase {
		return -1
	}
	if !strings.Contains(candidate, "-") {
		return 1
	}
	return 2
}

// trackHasLanguage reports whether the track carries the preferred language,
// either as its primary code or anywhere in its MULTi language list.
func trackHasLanguage(track models.AudioTrack, preferred string) bool {
	if langMatch(track.Language, preferred) {
		return true
	}
	if preferred == "" || len(track.Languages) == 0 {
		return false
	}
	canonical := lang.Canonical(preferred)
	for _, code := range track.Languages {
		if lang.Canonical(code) == canonical {
			return true
		}
	}
	return false
}

// SelectAudioTrack determines which audio track to use based on preferences.
//
// Priority:
// 1. Series preference exact track signature
// 2. Series preference index (if track exists at that index with matching language)
// 3. Series preference language (best language match)
// 4. Profile preferred language (best language match)
// 5. File's default track (first track with Default: true)
// 6. First track (index 0)
//
// Language matches rank exact tag > bare language > another variant of the same
// language. Track order breaks ties within a language rank. Saved signatures
// and compatible saved indices take precedence over language-only preferences.
//
// SelectAudioTrack is SelectAudioTrackPreferringPlayable without a client
// capability predicate: callers with no client context (catalog metadata,
// cross-version remap) keep the historical selection.
func SelectAudioTrack(tracks []models.AudioTrack, preferredLang string, seriesPref *AudioTrackPreference) int {
	return SelectAudioTrackPreferringPlayable(tracks, preferredLang, seriesPref, nil)
}

// SelectAudioTrackPreferringPlayable determines which audio track to use,
// preferring a track the client can render directly when one exists.
//
// playable, when non-nil, reports whether the client can render a track without
// a server transform (see AudioTrackPlayableFuncV3). If the ordered selection
// is not playable, the same priority order is run over the playable tracks and
// its result is used only when the replacement preserves both the selected
// track's language and its role. The intent is to keep a file that contains a
// playable track on a direct route without trading language or track role away
// for codec; when no such replacement exists, the original selection stands and
// the planner transforms the audio as before.
func SelectAudioTrackPreferringPlayable(tracks []models.AudioTrack, preferredLang string, seriesPref *AudioTrackPreference, playable func(models.AudioTrack) bool) int {
	if len(tracks) == 0 {
		return 0
	}
	selected := selectAudioTrackOrdered(tracks, preferredLang, seriesPref)
	if playable == nil || playable(tracks[selected]) {
		return selected
	}
	playableTracks := make([]models.AudioTrack, 0, len(tracks))
	playableIndex := make([]int, 0, len(tracks))
	for i, track := range tracks {
		if playable(track) {
			playableTracks = append(playableTracks, track)
			playableIndex = append(playableIndex, i)
		}
	}
	if len(playableTracks) == 0 {
		return selected
	}
	alternative := playableIndex[selectAudioTrackOrdered(playableTracks, preferredLang, seriesPref)]
	if audioTrackReplacementPreserved(tracks[selected], tracks[alternative]) {
		return alternative
	}
	return selected
}

// audioTrackReplacementPreserved reports whether substituting alternative for
// selected keeps the selected track's identity: the same language and the same
// role. Language alone is not enough because a same-language replacement could
// otherwise swap a commentary or descriptive track for the main track.
func audioTrackReplacementPreserved(selected, alternative models.AudioTrack) bool {
	return audioTrackLanguagePreserved(selected, alternative) && audioTrackRolePreserved(selected, alternative)
}

// audioTrackRolePreserved reports whether the replacement carries the same role
// as the selected track. The role is the descriptive identity (title/embedded
// title), not the codec, so a codec-only compatibility track keeps the role
// while a commentary track does not.
func audioTrackRolePreserved(selected, alternative models.AudioTrack) bool {
	return strings.EqualFold(strings.TrimSpace(selected.Title), strings.TrimSpace(alternative.Title)) &&
		strings.EqualFold(strings.TrimSpace(selected.EmbeddedTitle), strings.TrimSpace(alternative.EmbeddedTitle))
}

// audioTrackLanguagePreserved reports whether substituting alternative for
// selected keeps the selected track's language intent. A selected track that
// carries no concrete language (empty, "und"/"mul" with no language list) has
// no intent to preserve, so any playable alternative is allowed.
func audioTrackLanguagePreserved(selected, alternative models.AudioTrack) bool {
	selectedLanguages := crossVersionAudioLanguages(selected)
	if len(selectedLanguages) == 0 {
		return true
	}
	for _, code := range selectedLanguages {
		if trackHasLanguage(alternative, code) {
			return true
		}
	}
	return false
}

func selectAudioTrackOrdered(tracks []models.AudioTrack, preferredLang string, seriesPref *AudioTrackPreference) int {
	// 1. Series preference: try exact signature match first.
	if seriesPref != nil {
		if idx := findExactAudioTrack(tracks, seriesPref.TrackSignature); idx >= 0 {
			return idx
		}

		// 2. Series preference: honor the saved index when its track is still
		// the same language. Saved preferences may carry a bare tag while the
		// scanner now preserves regional subtags, so any compatible match keeps
		// the index rather than falling through to a different track.
		if seriesPref.AudioTrackIndex >= 0 && seriesPref.AudioTrackIndex < len(tracks) {
			if trackHasLanguage(tracks[seriesPref.AudioTrackIndex], seriesPref.AudioLanguage) {
				return seriesPref.AudioTrackIndex
			}
		}

		// 3. Series preference: fall back to language match.
		if seriesPref.AudioLanguage != "" {
			if idx := bestLanguageTrack(tracks, seriesPref.AudioLanguage); idx >= 0 {
				return idx
			}
		}
	}

	// 4. Profile language preference.
	if preferredLang != "" {
		if idx := bestLanguageTrack(tracks, preferredLang); idx >= 0 {
			return idx
		}
	}

	// 5. File's default track.
	for i, t := range tracks {
		if t.Default {
			return i
		}
	}

	// 6. First track.
	return 0
}

func bestLanguageTrack(tracks []models.AudioTrack, preferred string) int {
	best, bestRank := -1, 3
	for i, track := range tracks {
		if rank := langMatchRank(track.Language, preferred); rank >= 0 && rank < bestRank {
			best, bestRank = i, rank
		}
		for _, code := range track.Languages {
			if rank := langMatchRank(code, preferred); rank >= 0 && rank < bestRank {
				best, bestRank = i, rank
			}
		}
		if bestRank == 0 {
			break
		}
	}
	return best
}

// MatchAudioTrackAcrossVersions maps a selection made against one file's
// audio inventory onto another version of the same content. Track ordering is
// not stable across encodes, so carrying the raw ordinal can select a different
// language. Prefer the stable signature, then the selected language, and
// finally the effective file's default track.
func MatchAudioTrackAcrossVersions(
	requestedTracks []models.AudioTrack,
	effectiveTracks []models.AudioTrack,
	requestedIndex int,
) int {
	if len(effectiveTracks) == 0 {
		return 0
	}
	if len(requestedTracks) == 0 {
		return SelectAudioTrack(effectiveTracks, "", nil)
	}
	if requestedIndex < 0 || requestedIndex >= len(requestedTracks) {
		requestedIndex = SelectAudioTrack(requestedTracks, "", nil)
	}

	selected := requestedTracks[requestedIndex]
	signature := AudioTrackSignatureFromTrack(selected)
	// A signature match is language-independent and authoritative: the same
	// track on another encode keeps its identity even if the language list is
	// ordered differently.
	if idx := findExactAudioTrack(effectiveTracks, signature); idx >= 0 {
		return idx
	}
	// A MULTi/undetermined track's primary Language is "mul"/"und" and matches
	// nothing; it carries several concrete languages instead. Try every
	// language the requested track carries, in order, before falling back to
	// the effective file's default. Reducing to a single code (the old
	// Languages[0] behavior) degraded to default whenever that one language was
	// absent even though a later one was present.
	for _, code := range crossVersionAudioLanguages(selected) {
		candidate := SelectAudioTrack(effectiveTracks, "", &AudioTrackPreference{
			AudioTrackIndex: requestedIndex,
			AudioLanguage:   code,
			TrackSignature:  signature,
		})
		if trackHasLanguage(effectiveTracks[candidate], code) {
			return candidate
		}
	}
	// No carried language resolved on the target; keep the previous
	// default/first-track fallback.
	return SelectAudioTrack(effectiveTracks, "", nil)
}

// crossVersionAudioLanguages returns the concrete languages a track carries,
// primary code first, then its MULTi language list, deduplicated by canonical
// form. The "und"/"mul" sentinels are placeholders and are skipped.
func crossVersionAudioLanguages(track models.AudioTrack) []string {
	codes := make([]string, 0, len(track.Languages)+1)
	primary := track.Language
	if primary != "" && primary != "und" && primary != "mul" {
		codes = append(codes, primary)
	}
	for _, code := range track.Languages {
		if code == "" {
			continue
		}
		duplicate := false
		for _, existing := range codes {
			if langMatch(existing, code) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			codes = append(codes, code)
		}
	}
	return codes
}

// BrowserSupportsAudioCodec returns true if the given audio codec can be
// played natively by web browsers without transcoding.
func BrowserSupportsAudioCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "aac", "mp3", "opus", "vorbis", "flac":
		return true
	default:
		return false
	}
}
