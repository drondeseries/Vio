package playback

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// cleanupCounter counts how many times an input cleanup func ran, so restart
// ownership can be asserted without reaching into session internals.
type cleanupCounter struct {
	calls atomic.Int64
}

func (c *cleanupCounter) fn() func() {
	return func() { c.calls.Add(1) }
}

func (c *cleanupCounter) count() int64 { return c.calls.Load() }

// TestRestartSamePathRetainsLiveRelayCleanup covers the same-path case: a
// restart that refreshes input to the SAME path still owns that relay, so the
// original cleanup must survive the restart and must not have run mid-restart.
// Only closing the session releases it, exactly once.
func TestRestartSamePathRetainsLiveRelayCleanup(t *testing.T) {
	sess := newFakeFFmpegSession(t)
	counter := &cleanupCounter{}
	sess.opts.InputPath = "/relay/live.mkv"
	sess.opts.InputCleanup = counter.fn()
	sess.opts.RefreshInput = func(context.Context) (string, func(), error) {
		// Same path back with no new cleanup: nothing to swap, and the
		// existing relay registration stays valid.
		return "/relay/live.mkv", nil, nil
	}
	t.Cleanup(func() { _ = sess.Close() })

	if err := sess.Restart(t.Context(), 0, 0); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("cleanup ran %d times during a same-path restart, want 0 while the relay is still live", got)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("cleanup ran %d times after close, want exactly 1", got)
	}
}

// TestRestartPathChangeReleasesOldCleanupOnce covers the rotation case: when a
// restart swaps in a different path, the old relay's cleanup runs exactly once
// at the swap, and the replacement's cleanup is what the session later
// releases. The old cleanup must not be deferred to close, and must not run
// twice.
func TestRestartPathChangeReleasesOldCleanupOnce(t *testing.T) {
	sess := newFakeFFmpegSession(t)
	oldCleanup := &cleanupCounter{}
	newCleanup := &cleanupCounter{}
	sess.opts.InputPath = "/relay/old.mkv"
	sess.opts.InputCleanup = oldCleanup.fn()
	sess.opts.RefreshInput = func(context.Context) (string, func(), error) {
		return "/relay/new.mkv", newCleanup.fn(), nil
	}
	t.Cleanup(func() { _ = sess.Close() })

	if err := sess.Restart(t.Context(), 0, 0); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := oldCleanup.count(); got != 1 {
		t.Fatalf("old cleanup ran %d times on a path change, want exactly 1 at the swap", got)
	}
	if got := newCleanup.count(); got != 0 {
		t.Fatalf("replacement cleanup ran %d times before close, want 0", got)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := oldCleanup.count(); got != 1 {
		t.Fatalf("old cleanup ran %d times after close, want still exactly 1", got)
	}
	if got := newCleanup.count(); got != 1 {
		t.Fatalf("replacement cleanup ran %d times after close, want exactly 1", got)
	}
}

// TestRestartRotationDoesNotLeakCleanup covers repeated rotations: each
// superseded relay is released exactly once and the last one is released at
// close, so nothing dangles across generations.
func TestRestartRotationDoesNotLeakCleanup(t *testing.T) {
	sess := newFakeFFmpegSession(t)
	gen0 := &cleanupCounter{}
	var mu sync.Mutex
	var cleanups []*cleanupCounter
	sess.opts.InputPath = "/relay/gen0.mkv"
	sess.opts.InputCleanup = gen0.fn()
	sess.opts.RefreshInput = func(context.Context) (string, func(), error) {
		mu.Lock()
		defer mu.Unlock()
		next := &cleanupCounter{}
		cleanups = append(cleanups, next)
		return fmt.Sprintf("/relay/gen%d.mkv", len(cleanups)), next.fn(), nil
	}
	t.Cleanup(func() { _ = sess.Close() })

	for i := range 2 {
		if err := sess.Restart(t.Context(), 0, 0); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// gen0 was superseded by the first restart, cleanups[0] by the second, and
	// cleanups[1] is the live generation released at close.
	if len(cleanups) != 2 {
		t.Fatalf("registered %d replacement cleanups, want 2", len(cleanups))
	}
	// The two superseded generations were each released exactly once at their
	// swap; Close must not run them again.
	if got := gen0.count(); got != 1 {
		t.Fatalf("gen0 cleanup ran %d times, want exactly 1", got)
	}
	if got := cleanups[0].count(); got != 1 {
		t.Fatalf("gen1 cleanup ran %d times, want exactly 1", got)
	}
	if got := cleanups[1].count(); got != 1 {
		t.Fatalf("live gen2 cleanup ran %d times after close, want exactly 1", got)
	}
}
