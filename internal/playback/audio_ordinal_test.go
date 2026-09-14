package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestAudioStreamOrdinalInterleavedAbsoluteIndexes verifies the shared
// conversion maps a selected track's array position to its rank among the
// audio tracks ordered by absolute container stream Index, with subtitles
// interleaved between the audio streams — the shape that breaks a naive
// array-position or absolute-index mapping.
func TestAudioStreamOrdinalInterleavedAbsoluteIndexes(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 1, Language: "eng"},
		{Index: 3, Language: "fra"},
	}
	tests := []struct {
		name          string
		selectedIndex int
		want          int
	}{
		{name: "first audio at stream 1 maps to ordinal 0", selectedIndex: 0, want: 0},
		{name: "second audio at stream 3 maps to ordinal 1", selectedIndex: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
				t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
			}
		})
	}
}

// TestAudioStreamOrdinalSingleAudioAtStreamOne verifies a lone audio track at
// absolute stream 1 (video at 0) maps to ordinal 0.
func TestAudioStreamOrdinalSingleAudioAtStreamOne(t *testing.T) {
	tracks := []models.AudioTrack{{Index: 1, Language: "eng"}}
	if got := AudioStreamOrdinal(tracks, 0); got != 0 {
		t.Fatalf("AudioStreamOrdinal(tracks, 0) = %d, want 0", got)
	}
}

// TestAudioStreamOrdinalAudioFirstAtStreamZero verifies a real probed audio
// track at absolute stream 0 (audio-only file, or audio muxed before video)
// maps to ordinal 0: stream index 0 is a real container position, not a
// missing index.
func TestAudioStreamOrdinalAudioFirstAtStreamZero(t *testing.T) {
	tracks := []models.AudioTrack{{Index: 0, Language: "eng"}}
	if got := AudioStreamOrdinal(tracks, 0); got != 0 {
		t.Fatalf("AudioStreamOrdinal(tracks, 0) = %d, want 0", got)
	}
}

// TestAudioStreamOrdinalMultiTrackAudioOnlyZeroBasedIndexes verifies a probed
// audio-only inventory with indexes [0, 1] ranks by absolute stream index:
// selecting the second track (Index 1) yields ordinal 1, not the array
// position fallback.
func TestAudioStreamOrdinalMultiTrackAudioOnlyZeroBasedIndexes(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 0, Language: "eng"},
		{Index: 1, Language: "fra"},
	}
	tests := []struct {
		name          string
		selectedIndex int
		want          int
	}{
		{name: "first audio at stream 0 maps to ordinal 0", selectedIndex: 0, want: 0},
		{name: "second audio at stream 1 maps to ordinal 1", selectedIndex: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
				t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
			}
		})
	}
}

// TestAudioStreamOrdinalSynthesizedFallback verifies the legacy synthesized
// case: an all-zero-index inventory (virtual sources) falls back to the array
// position as the ordinal for every track, even when a track carries Index 0.
func TestAudioStreamOrdinalSynthesizedFallback(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng"},
		{Language: "fra"},
	}
	for _, tt := range []struct {
		selectedIndex int
		want          int
	}{
		{selectedIndex: 0, want: 0},
		{selectedIndex: 1, want: 1},
	} {
		if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
			t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
		}
	}
}

// TestAudioStreamOrdinalSynthesizedFallbackWithZeroIndex verifies the
// synthesized fallback also applies when the inventory is all-zero-indexed
// (the shape mergeVirtualCandidateTracks produces when it synthesizes a lone
// audio track): Index 0 alone must not be mistaken for a real stream position.
func TestAudioStreamOrdinalSynthesizedFallbackWithZeroIndex(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 0, Language: "eng"},
		{Index: 0, Language: "fra"},
	}
	for _, tt := range []struct {
		selectedIndex int
		want          int
	}{
		{selectedIndex: 0, want: 0},
		{selectedIndex: 1, want: 1},
	} {
		if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
			t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
		}
	}
}

// TestAudioStreamOrdinalOutOfRangePassthrough verifies out-of-range selections
// pass through unchanged, matching the legacy behavior of the v3 conversion.
func TestAudioStreamOrdinalOutOfRangePassthrough(t *testing.T) {
	tracks := []models.AudioTrack{{Index: 1, Language: "eng"}}
	for _, selectedIndex := range []int{-1, 1, 5} {
		if got := AudioStreamOrdinal(tracks, selectedIndex); got != selectedIndex {
			t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want passthrough %d", selectedIndex, got, selectedIndex)
		}
	}
}
