package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestSubtitleSearchHoldsSlotsUntilNonCooperativeCallbackExits pins blocker 5's
// reservation rule: a callback that ignores cancellation keeps both its
// aggregate and its dedicated subtitle slot (and its dedupe key) until it
// actually returns. Shutdown cancels its context but must not free the slot,
// because releasing there would admit a replacement behind a still-running
// callback.
func TestSubtitleSearchHoldsSlotsUntilNonCooperativeCallbackExits(t *testing.T) {
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	exit := make(chan struct{})
	h := &PlaybackHandler{
		ServiceContext:         serviceCtx,
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(context.Context, string, string, string, int, int, int, int, []string) {
			close(started)
			// Deliberately ignores ctx: a non-cooperative provider.
			<-exit
		},
	}
	file := &models.MediaFile{ID: 71, ContentID: "movie-noncoop"}
	cand := VirtualPlaybackStream{URI: "virtual://movie/noncoop?result=x"}

	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("subtitle search did not start")
	}

	gate := h.detachedGate()
	slots := h.subtitleSearchGate()
	serviceCancel()
	time.Sleep(50 * time.Millisecond)

	if !containsKey(h.SubtitleSearchInFlight, virtualSubtitleSearchKey(file, cand)) {
		t.Fatal("shutdown dropped the in-flight key while the callback was still running")
	}
	if len(gate.slots) != 1 {
		t.Fatalf("aggregate slots held = %d, want 1 while the callback runs", len(gate.slots))
	}
	if len(slots.slots) != 1 {
		t.Fatalf("subtitle slots held = %d, want 1 while the callback runs", len(slots.slots))
	}

	// Explicit cleanup: the test unblocks the callback and only then may the
	// reservation be released.
	close(exit)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !containsKey(h.SubtitleSearchInFlight, virtualSubtitleSearchKey(file, cand)) && len(gate.slots) == 0 && len(slots.slots) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cleanup after callback exit: key=%v aggregate=%d subtitle=%d",
		containsKey(h.SubtitleSearchInFlight, virtualSubtitleSearchKey(file, cand)), len(gate.slots), len(slots.slots))
}

func containsKey(m *sync.Map, key any) bool {
	_, ok := m.Load(key)
	return ok
}

// TestSubtitleSearchCapacityIsDedicatedAndBounded pins that the dedicated gate
// caps concurrent subtitle searches independently of the aggregate gate: a
// distinct-key flood starts at most virtualSubtitleSearchCap callbacks and
// sheds the rest loudly, rather than spawning one goroutine per request.
func TestSubtitleSearchCapacityIsDedicatedAndBounded(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	release := make(chan struct{})
	var started int64
	h := &PlaybackHandler{
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(context.Context, string, string, string, int, int, int, int, []string) {
			atomic.AddInt64(&started, 1)
			<-release
		},
	}

	total := virtualSubtitleSearchCap + 6
	for i := 0; i < total; i++ {
		file := &models.MediaFile{ID: 100 + i, ContentID: "movie-flood"}
		h.maybeTriggerSubtitleSearch(context.Background(), file, VirtualPlaybackStream{URI: "virtual://movie/flood?result=x"})
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&started) < int64(virtualSubtitleSearchCap) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&started); got != int64(virtualSubtitleSearchCap) {
		t.Fatalf("concurrent subtitle searches = %d, want the dedicated cap %d", got, virtualSubtitleSearchCap)
	}
	if !strings.Contains(logs.String(), "subtitle search budget exhausted") {
		t.Fatalf("overload shed was not logged: %s", logs.String())
	}
	close(release)
}

// TestSubtitleSearchLazyInitializesInFlight pins that a handler built as a
// literal (as the tests and some embedders do) does not panic on a nil dedupe
// map.
func TestSubtitleSearchLazyInitializesInFlight(t *testing.T) {
	done := make(chan struct{})
	h := &PlaybackHandler{
		VirtualSubtitleSearcher: func(context.Context, string, string, string, int, int, int, int, []string) {
			close(done)
		},
	}
	file := &models.MediaFile{ID: 72, ContentID: "movie-lazy"}
	h.maybeTriggerSubtitleSearch(context.Background(), file, VirtualPlaybackStream{URI: "virtual://movie/lazy?result=x"})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subtitle search did not run with a lazily initialized in-flight map")
	}
}
