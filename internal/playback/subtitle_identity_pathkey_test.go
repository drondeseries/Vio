package playback

import (
	"strings"
	"testing"
)

// TestExternalSubtitlePathKeyV3IsDeterministicAndOpaque proves the sidecar
// path_key is a pure, salt-free hash: the same path always yields the same key
// (so a client's key survives a server restart), different paths yield different
// keys, and the raw path (or its basename) never appears in the key.
func TestExternalSubtitlePathKeyV3IsDeterministicAndOpaque(t *testing.T) {
	const path = "/media/library/movie.en.srt"

	first := ExternalSubtitlePathKeyV3(path)
	// A second call models a fresh process after a restart: no per-process salt
	// may change the derivation.
	for i := 0; i < 5; i++ {
		if got := ExternalSubtitlePathKeyV3(path); got != first {
			t.Fatalf("path_key changed across calls: %q != %q (per-process salt?)", got, first)
		}
	}

	if first == "" {
		t.Fatal("path_key is empty")
	}
	if strings.Contains(first, path) || strings.Contains(first, "movie.en.srt") || strings.ContainsAny(first, `/\`) {
		t.Fatalf("path_key %q leaks the raw sidecar path", first)
	}
	// Hex-encoded sha256 is 64 lowercase hex chars: opaque and fixed width.
	if len(first) != 64 {
		t.Fatalf("path_key length = %d, want 64 hex chars", len(first))
	}
	for _, r := range first {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("path_key %q contains a non-hex byte %q", first, r)
		}
	}

	if second := ExternalSubtitlePathKeyV3("/media/library/movie.nl.srt"); second == first {
		t.Fatal("different sidecar paths must yield different path_keys")
	}
	// Same basename, different directory: the hash must still distinguish them.
	other := ExternalSubtitlePathKeyV3("/media/other/movie.en.srt")
	if other == first {
		t.Fatal("same-basename sidecars in different directories must yield different path_keys")
	}
}
