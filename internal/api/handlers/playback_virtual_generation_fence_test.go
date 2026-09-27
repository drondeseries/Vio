package handlers

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A best-result cache entry written by a newer generation must survive a write
// carrying an older generation, so a slow resolve or detached prefetch that
// finishes after a newer one cannot overwrite it.
func TestVirtualBestResultCacheGenerationFence(t *testing.T) {
	cache := NewVirtualBestResultCache(0, 0)
	const key = "generation-fence"
	now := time.Now()

	newer := []VirtualPlaybackStream{{ID: "newer", URI: "virtual://movie/tt?result=newer"}}
	older := []VirtualPlaybackStream{{ID: "older", URI: "virtual://movie/tt?result=older"}}

	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, newer, now, 20)
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, older, now, 10)

	got := cache.get(key, now)
	if len(got) != 1 || got[0].ID != "newer" {
		t.Fatalf("cached streams = %+v, want the newer generation's candidate", got)
	}

	// A newer write still replaces the entry.
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, older, now, 30)
	got = cache.get(key, now)
	if len(got) != 1 || got[0].ID != "older" {
		t.Fatalf("cached streams = %+v, want the newest generation's candidate", got)
	}
}

// A sticky pin written by a newer generation must survive a write carrying an
// older generation, so a slow resolve cannot steer later starts onto stale
// evidence after a newer resolve already pinned the live candidate.
func TestVirtualStickyPinGenerationFence(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const key = "sticky-generation-fence"

	h.pinVirtualStickyAt(key, "virtual://movie/tt?result=newer", 20)
	h.pinVirtualStickyAt(key, "virtual://movie/tt?result=older", 10)
	if got := h.peekVirtualSticky(key); got != "virtual://movie/tt?result=newer" {
		t.Fatalf("pin = %q, want the newer generation's candidate", got)
	}

	// A newer write still replaces the pin.
	h.pinVirtualStickyAt(key, "virtual://movie/tt?result=older", 30)
	if got := h.peekVirtualSticky(key); got != "virtual://movie/tt?result=older" {
		t.Fatalf("pin = %q, want the newest generation's candidate", got)
	}
}
