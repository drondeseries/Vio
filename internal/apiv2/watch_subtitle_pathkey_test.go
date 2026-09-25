package apiv2

import (
	"testing"

	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
)

// TestWatchVersionCarriesSidecarPathKey proves the v2 watch projection carries
// the opaque sidecar path_key through to the wire so a client can keep two
// same-basename sidecars distinct.
func TestWatchVersionCarriesSidecarPathKey(t *testing.T) {
	watch := watchVersionOf(catalogpkg.FileVersion{
		FileID: 7,
		SubtitleTracks: []catalogpkg.VersionSubtitleTrack{
			{Index: 2, Codec: "subrip", Language: "en"},
			{Index: 4, External: true, Codec: "srt", Language: "en", FileName: "movie.en.srt", PathKey: "hash-dir-a"},
			{Index: 5, External: true, Codec: "srt", Language: "en", FileName: "movie.en.srt", PathKey: "hash-dir-b"},
		},
	})
	if len(watch.SubtitleTracks) != 3 {
		t.Fatalf("subtitle tracks = %d, want 3", len(watch.SubtitleTracks))
	}
	if watch.SubtitleTracks[0].PathKey != "" {
		t.Fatalf("embedded path_key = %q, want empty", watch.SubtitleTracks[0].PathKey)
	}
	if watch.SubtitleTracks[1].PathKey != "hash-dir-a" || watch.SubtitleTracks[2].PathKey != "hash-dir-b" {
		t.Fatalf("external path_keys = %q,%q, want hash-dir-a,hash-dir-b",
			watch.SubtitleTracks[1].PathKey, watch.SubtitleTracks[2].PathKey)
	}
}
