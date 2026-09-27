package playback

import (
	"sync/atomic"
	"testing"
	"time"
)

// confirmedWithoutDriving reads the confirmed verdict without running the
// evaluator. Asserting through IsSourceRejected would drive the evaluator itself
// and manufacture the state under test.
func confirmedWithoutDriving(s *TranscodeSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sourceRejected
}

// D: every verdict read path must agree with the one shared evaluator. The
// hardware-latch read (IsDecodeFailed) drives the software-fallback decision
// during failure recovery, so it must run the evaluator too. Without that, a
// caller reaching only for the latch sees suspicion while the generation has in
// fact gone unopposed past the observation window: the source stays
// unconfirmed, so the bounded teardown and the rejection notification never
// fire and ffmpeg keeps decoding bytes no client will ever receive.
func TestHardwareLatchReadDrivesSharedEvaluator(t *testing.T) {
	base := time.Unix(31_000, 0)
	dir := t.TempDir()
	s := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo:   "hevc",
		CanonicalInputPath: "virtual://movie/tt-latch?result=x",
	}}
	clock := attachDecodeClock(s, base)
	torn := make(chan struct{})
	finished := make(chan struct{})
	var tornOnce atomic.Bool
	var killed int32
	s.mu.Lock()
	s.outputDir = dir
	s.generationStartedAt = base
	s.cancel = func() {
		if tornOnce.CompareAndSwap(false, true) {
			close(torn)
		}
		atomic.StoreInt32(&killed, 1)
	}
	s.done = finished
	s.decodeReapTimeout = 500 * time.Millisecond
	s.mu.Unlock()

	stormDecode(s)
	// The latch is set at suspicion, deliberately before confirmation.
	if !s.IsDecodeFailed() {
		t.Fatal("hardware latch not set at suspicion")
	}
	if confirmedWithoutDriving(s) {
		t.Fatal("session confirmed before the observation window elapsed")
	}

	// Silence past the window. Reading the latch must run the shared evaluator
	// and surface the confirmation.
	clock.Advance(decodeObservationWindow)
	if !s.IsDecodeFailed() {
		t.Fatal("latch read lost the session-level latch")
	}
	if !confirmedWithoutDriving(s) {
		t.Fatal("latch read did not drive the shared evaluator: the generation went unopposed " +
			"past the observation window, so no teardown or notification would ever run")
	}
	// The confirmation's bounded teardown must reach the process. Closing done
	// releases the reap, which waits for it.
	select {
	case <-torn:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmed generation was not torn down after a latch-only read")
	}
	close(finished)
}

// A generation the evaluator recovered must not be left looking condemned to a
// reader that consults the latch: the latch is session-level and survives, so
// the serving verdict is the authority on the live generation.
func TestHardwareLatchReadAgreesWithRecoveredGeneration(t *testing.T) {
	base := time.Unix(32_000, 0)
	dir := t.TempDir()
	s := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo:   "hevc",
		CanonicalInputPath: "virtual://movie/tt-recover?result=x",
	}}
	attachDecodeClock(s, base)
	var killed int32
	s.mu.Lock()
	s.outputDir = dir
	s.generationStartedAt = base
	s.cancel = func() { atomic.StoreInt32(&killed, 1) }
	s.done = make(chan struct{})
	s.mu.Unlock()

	stormDecode(s)
	if !s.IsDecodeFailed() {
		t.Fatal("hardware latch not set at suspicion")
	}

	// Qualifying progress retires the episode.
	writeDecodeSegment(t, dir, "seg_00000.ts", base.Add(5*time.Millisecond))
	if !s.IsDecodeFailed() {
		t.Fatal("latch read lost the session-level latch")
	}
	if confirmedWithoutDriving(s) {
		t.Fatal("a recovered generation was still condemned by the latch read")
	}
	if atomic.LoadInt32(&killed) != 0 {
		t.Fatal("a recovered generation was torn down")
	}
}
