package playback

import "github.com/Silo-Server/silo-server/internal/models"

// AudioStreamOrdinal translates a selected audio track's array position in
// tracks to the audio-only stream ordinal ffmpeg's `0:a:N` expects.
//
// The two index domains are distinct and must not be mixed:
//   - tracks[i].Index is the ABSOLUTE container stream index recorded by the
//     probe (video at stream 0, the only audio at stream 1, subtitles
//     interleaved between audio streams). Index 0 is a real container
//     position: an audio-only file's first audio stream sits at stream 0.
//   - ffmpeg's `0:a:N` counts only audio streams: 0 is the first audio stream
//     regardless of its absolute container index.
//
// Mapping by array position alone would select the wrong language when the
// track list order differs from the container order (MULTi releases, virtual
// sources), and mapping by the absolute Index would emit a specifier that can
// silently omit audio entirely (the optional `?` map) or pick a different
// language. The ordinal is therefore the rank of the selected track among the
// file's audio tracks ordered by their absolute stream Index (audio tracks
// with Index 1 and 3 → ordinals 0 and 1).
//
// A missing index is indistinguishable from a real stream index 0 on a single
// track, so the inventory is classified as a whole: it is probed iff any track
// carries Index > 0. ffprobe records s.Index verbatim, so a real probe with
// audio beyond stream 0 always yields Index >= 1 for later tracks, while
// synthesized virtual-source inventories carry all-zero indexes. In a probed
// inventory every track ranks by Index >= 0 — stream index 0 is a valid
// rankable position (audio indexes [0, 1] → ordinals 0 and 1). In a
// synthesized inventory the array position is used as the ordinal for every
// track, preserving the historical behavior for tracks without a recorded
// stream index. Out-of-range selections pass through unchanged.
func AudioStreamOrdinal(tracks []models.AudioTrack, selectedIndex int) int {
	if selectedIndex < 0 || selectedIndex >= len(tracks) {
		return selectedIndex
	}
	selected := tracks[selectedIndex]
	probed := false
	for _, track := range tracks {
		if track.Index > 0 {
			probed = true
			break
		}
	}
	if !probed {
		// Synthesized inventory (virtual sources): no track carries a real
		// container stream index, so the array position is the ordinal.
		return selectedIndex
	}
	ordinal := 0
	for _, track := range tracks {
		if track.Index >= 0 && track.Index < selected.Index {
			ordinal++
		}
	}
	return ordinal
}
