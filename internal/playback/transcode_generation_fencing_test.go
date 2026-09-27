package playback

import (
	"context"
	"testing"
	"time"
)

// A dead generation's stderr writer must not contaminate the replacement
// generation's verdict. The writer captured generation N; a restart reset the
// session to N+1; the late line must not advance N+1's counters or stage.
func TestStaleGenerationStderrDoesNotContaminateReplacement(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	attachDecodeClock(s, time.Unix(3000, 0))

	// The writer a live process would hold.
	writer := &ffmpegStderrWriter{session: s, ctx: context.Background(), generation: s.decodeGeneration}
	staleGeneration := writer.generation

	// Restart resets the verdict and opens the replacement generation.
	s.mu.Lock()
	s.resetDecodeVerdictLocked()
	s.mu.Unlock()
	if staleGeneration == s.decodeGeneration {
		t.Fatal("test did not actually advance the generation")
	}

	// The dead process's late stderr arrives, a full threshold's worth.
	for range decodeErrorThreshold {
		if _, err := writer.Write([]byte(prodSPSMissingLine() + "\n")); err != nil {
			t.Fatalf("write stale stderr: %v", err)
		}
	}

	if got := decodeStageOf(s); got != decodeStageObserving {
		t.Fatalf("replacement generation reached stage %d from a stale writer's stderr, want observing", got)
	}
	if s.IsSourceRejected() {
		t.Fatal("a stale generation's stderr confirmed the replacement source")
	}
	s.mu.Lock()
	count := s.decodeErrorCount
	s.mu.Unlock()
	if count != 0 {
		t.Fatalf("replacement generation counted %d errors from a stale writer, want 0", count)
	}

	// The live generation must still reach its own verdict.
	for range decodeErrorThreshold {
		if s.observeDecodeError(hevcFatalErrorLine()) {
			break
		}
	}
	if got := decodeStageOf(s); got != decodeStageSuspected {
		t.Fatalf("live generation did not reach suspicion, stage %d", got)
	}
}

// The demux counter is fenced the same way: a dead process's late demux line
// must not push the live generation toward a demux stamp.
func TestStaleGenerationDemuxStderrDoesNotStampReplacement(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	deadGeneration := s.decodeGeneration

	s.mu.Lock()
	s.resetDecodeVerdictLocked()
	s.mu.Unlock()

	ctx := context.Background()
	// A full threshold of demux failures, all carrying the dead generation.
	for range demuxErrorThreshold {
		if s.logFFmpegLineForGeneration(ctx, deadGeneration, demuxVideoIOErrorLine()) {
			t.Fatal("a stale generation's demux line was accepted by the replacement session")
		}
	}
	if s.IsDemuxFailed() {
		t.Fatal("a stale generation's demux stderr stamped the replacement session")
	}

	// The live generation still reaches the demux verdict on its own lines.
	for range demuxErrorThreshold {
		s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
		if s.IsDemuxFailed() {
			break
		}
	}
	if !s.IsDemuxFailed() {
		t.Fatal("live generation could not reach the demux verdict after a reset")
	}
}

// The generation handle is carried into the mutation, not only checked
// beforehand: a reset landing between the check and the observation discards
// the line. Calling the generation-scoped entry point directly reproduces the
// interleaving the stderr writer relies on.
func TestGenerationScopedObservationDiscardsLineAfterReset(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	attachDecodeClock(s, time.Unix(4000, 0))

	// Simulate the writer that holds the dead generation: its line arrives
	// after the restart, so the fenced entry point drops it.
	deadGeneration := s.decodeGeneration
	// The restart lands here, between the writer's capture and the mutation.
	s.mu.Lock()
	s.resetDecodeVerdictLocked()
	s.mu.Unlock()

	// The mutation the writer is now attempting.
	if s.logFFmpegLineForGeneration(context.Background(), deadGeneration, hevcFatalErrorLine()) {
		t.Fatal("a stale generation's line was accepted by the replacement")
	}
	s.mu.Lock()
	count := s.decodeErrorCount
	s.mu.Unlock()
	if count != 0 {
		t.Fatalf("stale observation recorded %d errors on the replacement generation, want 0", count)
	}
	if s.observeDecodeError(hevcFatalErrorLine()) {
		t.Fatal("the live generation was already at the threshold, so the stale line was not isolated")
	}
}

// The session-level hardware latch survives a reset, but it must not stop the
// replacement generation from reaching its own verdict.
func TestHardwareLatchPreservedButFreshGenerationStillReachesVerdict(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	clock := attachDecodeClock(s, time.Unix(5000, 0))

	// The hardware generation fails and confirms.
	stormDecode(s)
	clock.Advance(decodeObservationWindow)
	s.evaluateDecodeVerdict()
	if !s.IsDecodeFailed() {
		t.Fatal("hardware generation did not latch")
	}

	// A seek restart preserves decodeStamped but opens a new generation.
	s.mu.Lock()
	s.resetDecodeVerdictLocked()
	s.mu.Unlock()
	if !s.IsDecodeFailed() {
		t.Fatal("restart cleared the session-level hardware latch")
	}
	if s.IsSourceRejected() {
		t.Fatal("restart left the replacement generation confirmed")
	}

	// One fresh error must not reject.
	if s.observeDecodeError(hevcFatalErrorLine()) {
		t.Fatal("a single fresh error entered suspicion")
	}
	if s.IsSourceRejected() {
		t.Fatal("one fresh error confirmed the replacement generation")
	}

	// A complete fresh window must confirm despite the preserved latch.
	for range decodeErrorThreshold {
		if s.observeDecodeError(hevcFatalErrorLine()) {
			break
		}
	}
	clock.Advance(decodeObservationWindow)
	s.evaluateDecodeVerdict()
	if !s.IsSourceRejected() {
		t.Fatal("the preserved hardware latch blocked the fresh generation from confirming")
	}
}
