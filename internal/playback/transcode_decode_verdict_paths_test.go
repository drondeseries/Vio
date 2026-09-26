package playback

import (
	"context"
	"errors"
	"testing"
	"time"
)

// suspectedPastDeadlineSession builds a session whose single generation is
// suspected with its observation deadline already past, and with no watcher
// running. Each verdict read path must therefore confirm the generation on its
// own by driving the shared evaluator; none may depend on the watcher ticker or
// on another path having run first.
func suspectedPastDeadlineSession(t *testing.T, opts TranscodeOpts) (*TranscodeSession, *decodeTestClock) {
	t.Helper()
	base := time.Unix(41_000, 0)
	s := &TranscodeSession{opts: opts}
	clock := attachDecodeClock(s, base)
	s.mu.Lock()
	s.outputDir = t.TempDir()
	s.running = true
	s.generationStartedAt = base
	s.decodeStage = decodeStageSuspected
	s.decodeSuspectAt = base.Add(-decodeObservationWindow)
	s.mu.Unlock()
	return s, clock
}

// TestVerdictReadPathsDriveSharedEvaluator pins D's single-evaluator rule: a
// suspected generation whose deadline has passed is confirmed by serving,
// waiters, restart, and notification alike, with no watcher running. Before the
// fix, notification read the verdict without driving the evaluator and could
// disagree with the other paths on the same generation.
func TestVerdictReadPathsDriveSharedEvaluator(t *testing.T) {
	virtualOpts := func() TranscodeOpts {
		return TranscodeOpts{
			TargetCodecVideo:   "hevc",
			CanonicalInputPath: "virtual://movie/tt-paths?result=x",
		}
	}

	t.Run("serving", func(t *testing.T) {
		s, _ := suspectedPastDeadlineSession(t, virtualOpts())
		if !s.IsSourceRejected() {
			t.Fatal("serving read did not drive the evaluator to confirmation")
		}
	})

	t.Run("waiter_segment", func(t *testing.T) {
		s, _ := suspectedPastDeadlineSession(t, virtualOpts())
		if _, err := s.WaitForSegment("seg_00009.ts", 5*time.Second); !errors.Is(err, ErrSourceDecodeRejected) {
			t.Fatalf("WaitForSegment err = %v, want ErrSourceDecodeRejected", err)
		}
	})

	t.Run("waiter_open_segment", func(t *testing.T) {
		s, _ := suspectedPastDeadlineSession(t, virtualOpts())
		if _, err := s.WaitForOpenSegment("seg_00009.ts", 5*time.Second); !errors.Is(err, ErrSourceDecodeRejected) {
			t.Fatalf("WaitForOpenSegment err = %v, want ErrSourceDecodeRejected", err)
		}
	})

	t.Run("waiter_manifest", func(t *testing.T) {
		s, _ := suspectedPastDeadlineSession(t, virtualOpts())
		if _, err := s.waitForManifest(context.Background(), 5*time.Second, true); !errors.Is(err, ErrSourceDecodeRejected) {
			t.Fatalf("waitForManifest err = %v, want ErrSourceDecodeRejected", err)
		}
	})

	t.Run("restart", func(t *testing.T) {
		s, _ := suspectedPastDeadlineSession(t, virtualOpts())
		s.mu.Lock()
		s.cancel = func() {}
		s.done = make(chan struct{})
		s.decodeReapTimeout = 100 * time.Millisecond
		s.mu.Unlock()
		if err := s.Restart(context.Background(), 0, 0); !errors.Is(err, ErrSourceDecodeRejected) {
			t.Fatalf("Restart err = %v, want ErrSourceDecodeRejected", err)
		}
	})

	t.Run("notification", func(t *testing.T) {
		marked := make(chan struct{}, 2)
		opts := virtualOpts()
		opts.OnSourceRejected = func(context.Context, int, string) error {
			marked <- struct{}{}
			return nil
		}
		s, _ := suspectedPastDeadlineSession(t, opts)
		s.mu.Lock()
		s.cancel = func() {}
		s.done = make(chan struct{})
		s.decodeReapTimeout = 100 * time.Millisecond
		s.mu.Unlock()

		s.notifySourceRejected(context.Background())
		waitDecodeMarker(t, marked)
	})
}

// TestFailedMarkerDoesNotBlockConfirmationOrRefire pins D's failed-marker
// behavior: a marker callback that returns an error is still a claim (the
// verdict is preserved and confirmation is terminal), and it is not retried, so
// a failed persistence write cannot turn one confirmation into a storm of
// callbacks.
func TestFailedMarkerDoesNotBlockConfirmationOrRefire(t *testing.T) {
	base := time.Unix(42_000, 0)
	attempts := make(chan struct{}, 4)
	s := &TranscodeSession{opts: TranscodeOpts{
		MediaFileID:        7,
		CanonicalInputPath: "virtual://movie/tt-failed-marker?result=x",
		TargetCodecVideo:   "hevc",
		OnSourceRejected: func(context.Context, int, string) error {
			attempts <- struct{}{}
			return errors.New("persist failed")
		},
	}}
	attachDecodeClock(s, base)
	s.mu.Lock()
	s.outputDir = t.TempDir()
	s.running = true
	s.generationStartedAt = base
	s.decodeStage = decodeStageSuspected
	s.decodeSuspectAt = base.Add(-decodeObservationWindow)
	s.mu.Unlock()

	s.notifySourceRejected(context.Background())
	waitDecodeMarker(t, attempts)
	if !s.IsSourceRejected() {
		t.Fatal("failed marker lost the confirmed verdict")
	}
	// A later read must not re-claim the marker.
	s.evaluateDecodeVerdict()
	s.notifySourceRejected(context.Background())
	select {
	case <-attempts:
		t.Fatal("failed marker was retried; the claim is exactly-once")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestThresholdBurstNoRecoveryConfirmsExactlyOnce pins D's core acceptance: a
// threshold burst followed by expiry confirms once, and re-reading the verdict
// through any path neither re-confirms nor re-fires the marker.
func TestThresholdBurstNoRecoveryConfirmsExactlyOnce(t *testing.T) {
	base := time.Unix(43_000, 0)
	calls := make(chan struct{}, 4)
	s := &TranscodeSession{opts: TranscodeOpts{
		MediaFileID:        11,
		CanonicalInputPath: "virtual://movie/tt-once?result=x",
		TargetCodecVideo:   "hevc",
		OnSourceRejected: func(context.Context, int, string) error {
			calls <- struct{}{}
			return nil
		},
	}}
	clock := attachDecodeClock(s, base)
	s.mu.Lock()
	s.outputDir = t.TempDir()
	s.running = true
	s.generationStartedAt = base
	s.mu.Unlock()

	// Burst crosses the threshold into suspicion; no output follows.
	for i := 0; i < decodeErrorThreshold; i++ {
		s.observeDecodeError(hevcFatalErrorLine())
	}
	if got := decodeStageOf(s); got != decodeStageSuspected {
		t.Fatalf("burst stage = %d, want suspected", got)
	}
	if s.IsSourceRejected() {
		t.Fatal("burst confirmed before the observation window")
	}

	clock.Advance(decodeObservationWindow)
	// Serving read confirms and claims the marker.
	if !s.IsSourceRejected() {
		t.Fatal("expiry did not confirm through the serving read")
	}
	waitDecodeMarker(t, calls)
	if got := decodeStageOf(s); got != decodeStageConfirmed {
		t.Fatalf("confirmed stage = %d, want confirmed", got)
	}

	// Every other path re-reads the same terminal verdict without re-firing.
	for i := 0; i < 20; i++ {
		if !s.IsSourceRejected() {
			t.Fatal("confirmed verdict flipped back")
		}
	}
	if _, err := s.WaitForSegment("seg_00009.ts", 200*time.Millisecond); !errors.Is(err, ErrSourceDecodeRejected) {
		t.Fatalf("waiter err = %v, want ErrSourceDecodeRejected", err)
	}
	select {
	case <-calls:
		t.Fatal("confirmation invoked the marker more than once")
	case <-time.After(100 * time.Millisecond):
	}
}
