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

// A generation fence must not outlive the entry it protects. The fence refuses
// a write only while a newer generation still owns a live entry; the expiry
// sweep runs on the next write, so once the newer entry lapses the key is empty
// and the fence no longer blocks. This is the guarantee that keeps a
// stamped-out generation from permanently blocking a key that has no evidence.
//
// The sweep runs on the write that observes the lapse, and that write must
// itself carry the newer generation (the resolve that owns the token). The
// write after it may then carry the older generation, which the emptied key no
// longer fences.
func TestVirtualBestResultCacheGenerationFenceAfterExpiry(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 0)
	const key = "expiry-fence"
	now := time.Now()
	streams := func(id string) []VirtualPlaybackStream {
		return []VirtualPlaybackStream{{ID: id, URI: "virtual://movie/tt?result=" + id}}
	}

	// A newer generation owns the key while it is live: the older write is
	// fenced out.
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, streams("newer"), now, 20)
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, streams("older"), now, 10)
	if got := cache.get(key, now); len(got) != 1 || got[0].ID != "newer" {
		t.Fatalf("cached streams = %+v, want the live newer entry to win", got)
	}

	later := now.Add(2 * time.Minute)
	// The write that observes the lapse sweeps the expired entry; its own
	// generation owns the key, so a same-generation write now replaces it. A
	// later resolve legitimately carries a newer generation and replaces it
	// again. What the sweep buys is that the *expired* entry no longer fences:
	// an older generation arriving after the sweep competes with a live entry
	// rather than with a dead one.
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, streams("sweeper"), later, 20)
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, streams("sweeper-late"), later, 20)
	got := cache.get(key, later)
	if len(got) != 1 || got[0].ID != "sweeper-late" {
		t.Fatalf("cached streams = %+v, want the post-sweep write to take the emptied key", got)
	}

	// While a live entry still carries the highest generation the fence holds,
	// so the assertion above is about the sweep, not an absent fence.
	cache.setWithDetailsAt(key, "content", "virtual://movie/tt", 5, streams("older-fenced"), later, 10)
	if got := cache.get(key, later); len(got) != 1 || got[0].ID != "sweeper-late" {
		t.Fatalf("live cache = %+v, want the live newer entry to keep fencing the older generation", got)
	}
}

// A sticky pin is dropped once its TTL lapses, and the dropped pin must not
// keep fencing later writes. A pin generation only orders live pins.
func TestVirtualStickyPinGenerationFenceAfterExpiry(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const key = "sticky-expiry-fence"

	h.pinVirtualStickyAt(key, "virtual://movie/tt?result=newer", 20)
	if got := h.peekVirtualSticky(key); got != "virtual://movie/tt?result=newer" {
		t.Fatalf("pin = %q, want the newer generation's candidate", got)
	}

	// Age the live pin past its TTL; peek drops it, so a later older-generation
	// write seeds a fresh pin instead of being refused by a dead one.
	h.virtualStickyMu.Lock()
	pin := h.virtualStickyPins[key]
	pin.pinnedAt = time.Now().Add(-virtualStickyTTL - time.Minute)
	h.virtualStickyPins[key] = pin
	h.virtualStickyMu.Unlock()

	if got := h.peekVirtualSticky(key); got != "" {
		t.Fatalf("expired pin = %q, want it dropped", got)
	}
	h.pinVirtualStickyAt(key, "virtual://movie/tt?result=older", 10)
	if got := h.peekVirtualSticky(key); got != "virtual://movie/tt?result=older" {
		t.Fatalf("pin = %q, want the older write to seed the expired key", got)
	}
}

// The best-result cache and the sticky pin are written from the same resolve
// with the same generation, so after a newer resolve both surfaces name the
// newer candidate and neither is left steered by the older one. This is the
// coherence contract the two fences exist to keep.
func TestVirtualCacheAndStickyPinStayCoherentUnderGenerationFence(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	cache := NewVirtualBestResultCache(0, 0)
	const (
		key      = "coherence"
		neutral  = "virtual://movie/tt"
		newerURI = neutral + "?result=newer"
		olderURI = neutral + "?result=older"
		cacheKey = "coherence-cache"
		olderGen = uint64(10)
		newerGen = uint64(20)
	)
	streamsFor := func(uri string) []VirtualPlaybackStream {
		return []VirtualPlaybackStream{{ID: virtualResultCandidateID(uri), URI: uri}}
	}

	// A newer resolve finishes first on both surfaces.
	h.pinVirtualStickyAt(key, newerURI, newerGen)
	cache.setWithDetailsAt(cacheKey, "content", neutral, 5, streamsFor(newerURI), time.Now(), newerGen)

	// An older resolve then lands on both surfaces; neither may replace the
	// newer evidence.
	h.pinVirtualStickyAt(key, olderURI, olderGen)
	cache.setWithDetailsAt(cacheKey, "content", neutral, 5, streamsFor(olderURI), time.Now(), olderGen)

	if got := h.peekVirtualSticky(key); got != newerURI {
		t.Fatalf("pin = %q, want the coherent newer candidate %q", got, newerURI)
	}
	if got := cache.get(cacheKey, time.Now()); len(got) != 1 || got[0].URI != newerURI {
		t.Fatalf("cached streams = %+v, want the coherent newer candidate %q", got, newerURI)
	}
}
