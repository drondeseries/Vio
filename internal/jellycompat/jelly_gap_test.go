package jellycompat

import (
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"

	"github.com/Silo-Server/silo-server/internal/models"
)

// Mirror of the handlers probe-failure regression: a leading release marker
// must not leak into the audio inventory, and the merged tracks must be one
// per real language in declaration order — matching the native surface.
func TestMergeCompatCandidateTracksFiltersReleaseMarkersAndOrdersDeclaration(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "eac3",
		AudioLanguages: []string{"MULTI", "eng", "fre"},
	}

	mergeCompatCandidateTracks(probed, candidate)

	got := make([]string, 0, len(probed.AudioTracks))
	for _, track := range probed.AudioTracks {
		got = append(got, track.Language)
	}
	if len(got) != 2 {
		t.Fatalf("audio languages = %v, want exactly [eng fre] (no MULTI leak, no duplicates)", got)
	}
	if got[0] != "eng" || got[1] != "fre" {
		t.Fatalf("audio languages = %v, want declaration order [eng fre]", got)
	}
	for _, track := range probed.AudioTracks {
		if track.Language == "" {
			t.Fatalf("anonymous audio track leaked: %#v", probed.AudioTracks)
		}
	}
}

// Mirror of the native surface's hint-becomes-metadata fixture: once a real
// probed inventory exists, provider-only language hints must not become
// fabricated selectable audio tracks on the Jellyfin surface either.
func TestMergeCompatCandidateTracksAuthoritativeInventory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tracks []models.AudioTrack
		want   int
	}{
		{"index zero", []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true}}, 0},
		{"default beats hint", []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2}, {Index: 2, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 1},
		{"audio first", []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}, {Index: 1, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 1},
		{"genuine multi", []models.AudioTrack{{Index: 0, Language: "en", Languages: []string{"en", "fr"}, Codec: "aac", Channels: 2}, {Index: 1, Language: "de", Codec: "aac", Channels: 2, Default: true}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.tracks)
			file := &models.MediaFile{AudioTracks: tc.tracks}
			mergeCompatCandidateTracks(file, VirtualPlaybackStream{AudioLanguages: []string{"ENG", "FRA"}})
			after, _ := json.Marshal(file.AudioTracks)
			if string(before) != string(after) {
				t.Fatalf("inventory changed: %s -> %s", before, after)
			}
			if got := playback.SelectAudioTrack(file.AudioTracks, "fr", nil); got != tc.want {
				t.Fatalf("selected %d, want %d", got, tc.want)
			}
			if got := playback.AudioStreamOrdinal(file.AudioTracks, tc.want); got != tc.want {
				t.Fatalf("ordinal %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMergeCompatCandidateTracksHintsStayOnCandidate(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
	}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"ENG", "FRA"},
	}

	mergeCompatCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want exactly the probed English track (FRA is metadata, not a stream)", probed.AudioTracks)
	}
	if len(probed.AudioTracks[0].Languages) != 0 {
		t.Fatalf("provider hints changed real track languages: %#v", probed.AudioTracks[0].Languages)
	}
}
