package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// External subtitles must carry collision-free full stream indexes in catalog
// responses. Before this test they all serialized the zero value, so files with
// multiple external subtitles published duplicate identities to clients.
func TestBuildVersionSubtitleTracksAssignsUniqueExternalIndexes(t *testing.T) {
	file := &models.MediaFile{
		VideoTracks: []models.VideoTrack{{Codec: "hevc"}},
		AudioTracks: []models.AudioTrack{{Codec: "eac3"}},
		SubtitleTracks: []models.SubtitleTrack{
			// Attachments can leave gaps in ffprobe's full stream indexes. The
			// zero-valued second track exercises the positional fallback contract.
			{Index: 4, Codec: "subrip", Language: "en"},
			{Index: 0, Codec: "hdmv_pgs_subtitle", Language: "de"},
		},
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/movie.en.srt", Format: "srt", Language: "en"},
			{Path: "/media/movie.nl.srt", Format: "srt", Language: "nl"},
		},
	}

	tracks := buildVersionSubtitleTracks(file)
	if len(tracks) != 4 {
		t.Fatalf("tracks len = %d, want 4", len(tracks))
	}

	// Embedded entries keep their stored indexes. Downstream consumers treat
	// zero as a fallback to video+audio+ordinal, which is 3 for tracks[1].
	if tracks[0].Index != 4 || tracks[1].Index != 0 {
		t.Fatalf("embedded indexes = %d,%d, want 4,0", tracks[0].Index, tracks[1].Index)
	}

	// Externals start after every occupied full stream index. They must not use
	// local ordinals, which collide with video/audio indexes and serialize zero
	// as a missing field because VersionSubtitleTrack.Index uses omitempty.
	if tracks[2].Index != 5 || !tracks[2].External {
		t.Fatalf("first external = index %d external %v, want 5/true", tracks[2].Index, tracks[2].External)
	}
	if tracks[3].Index != 6 || !tracks[3].External {
		t.Fatalf("second external = index %d external %v, want 6/true", tracks[3].Index, tracks[3].External)
	}

	encoded, err := json.Marshal(tracks[2:])
	if err != nil {
		t.Fatalf("marshal external tracks: %v", err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal external tracks: %v", err)
	}
	if wire[0]["index"] != float64(5) || wire[1]["index"] != float64(6) {
		t.Fatalf("serialized external indexes = %#v,%#v, want 5,6", wire[0]["index"], wire[1]["index"])
	}
}

// TestBuildVersionSubtitleTracksPublishesOpaquePathKey proves the wire carries
// an opaque path_key for external sidecars (and only those), so a client can
// tell two same-basename sidecars in different directories apart without
// receiving the server filesystem path.
func TestBuildVersionSubtitleTracksPublishesOpaquePathKey(t *testing.T) {
	file := &models.MediaFile{
		SubtitleTracks: []models.SubtitleTrack{{Index: 2, Codec: "subrip", Language: "en"}},
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: "/media/dir-a/movie.en.srt", Format: "srt", Language: "en"},
			{Path: "/media/dir-b/movie.en.srt", Format: "srt", Language: "en"},
		},
	}

	tracks := buildVersionSubtitleTracks(file)
	if len(tracks) != 3 {
		t.Fatalf("tracks len = %d, want 3", len(tracks))
	}
	if tracks[0].PathKey != "" {
		t.Fatalf("embedded track path_key = %q, want empty", tracks[0].PathKey)
	}
	if tracks[1].PathKey == "" || tracks[2].PathKey == "" {
		t.Fatal("external sidecars must carry a non-empty path_key")
	}
	if tracks[1].PathKey == tracks[2].PathKey {
		t.Fatal("two same-basename sidecars in different directories must get distinct path_keys")
	}
	// The key is opaque: it must not contain the path or its basename.
	for _, track := range tracks[1:] {
		if track.PathKey == track.FileName || strings.Contains(track.PathKey, "/") {
			t.Fatalf("path_key %q leaks the sidecar path or basename", track.PathKey)
		}
	}
}
