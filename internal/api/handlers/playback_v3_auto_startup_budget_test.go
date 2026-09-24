package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePlaybackTestFFmpegStall writes a fake FFmpeg that starts, writes nothing,
// and stays alive until it is killed. A startup attempt against it can only end
// at a deadline.
func writePlaybackTestFFmpegStall(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-stall.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 120\n"), 0o755); err != nil {
		t.Fatalf("write stalling ffmpeg: %v", err)
	}
	return path
}

// TestPrepareLocalTransportV3AutoStartupHonorsOverallBudget pins the request
// budget for a local hw_accel=auto start. The path stalls without producing a
// manifest: pre-fix the handler sat in the bare ManifestStartupTimeout wait
// (30s) before answering, and a fallback whose attempts each fail near their
// deadline could total one timeout per path. The explicit overall budget ends
// the walk within the request deadline instead.
func TestPrepareLocalTransportV3AutoStartupHonorsOverallBudget(t *testing.T) {
	stallPath := writePlaybackTestFFmpegStall(t)
	previousBudget := localAutoStartupBudgetV3
	localAutoStartupBudgetV3 = 200 * time.Millisecond
	t.Cleanup(func() { localAutoStartupBudgetV3 = previousBudget })

	startedAt := time.Now()
	handler, transport, transportErr := prepareAutoLocalTransportV3(t, stallPath, "session-auto-budget")
	_ = handler
	if transportErr == nil {
		transport.rollback()
		t.Fatal("a stalling startup returned a playable transport")
	}
	if !strings.Contains(transportErr.message, "not become ready") && !strings.Contains(transportErr.message, "before media became ready") {
		t.Fatalf("transport error = %#v, want a readiness classification", transportErr)
	}
	// Three unbounded per-attempt waits would be 3 x 30s; the budget plus
	// process teardown must land far below that.
	if elapsed := time.Since(startedAt); elapsed > 30*time.Second {
		t.Fatalf("startup ran %v, the overall budget was not enforced", elapsed)
	}
}

// TestPrepareLocalTransportV3AutoObservesManifestFailure is the handler-boundary
// regression pin for 42d4607b. The first hw_accel=auto execution path starts a
// real FFmpeg that exits before writing its first manifest. The manifest
// failure must reach runTranscodeStartup as a readiness failure so the pipeline
// advances to the CPU-decode path; if startLocalPlaybackTransport waited for
// the manifest itself it would return the failure as a spawn error, which the
// pipeline treats as non-hardware and terminal, and the start would fail on the
// first GPU attempt.
func TestPrepareLocalTransportV3AutoObservesManifestFailure(t *testing.T) {
	ffmpegPath, logPath := writePlaybackTestFFmpegFailingOn(t, "-hwaccel cuda")
	handler, transport, transportErr := prepareAutoLocalTransportV3(t, ffmpegPath, "session-auto-manifest-failure")
	if transportErr != nil {
		t.Fatalf("prepare auto local transport: %v (cause: %v)", transportErr, transportErr.cause)
	}
	if commitErr := transport.commit(); commitErr != nil {
		t.Fatalf("commit: %v", commitErr)
	}
	defer handler.tm.CloseTranscodeSession("session-auto-manifest-failure", "")

	invocations := readPlaybackTestFFmpegInvocations(t, logPath)
	if len(invocations) < 2 {
		t.Fatalf("invocations = %q, want the manifest failure on the first path to advance the pipeline", invocations)
	}
	if !strings.Contains(invocations[0], "-hwaccel cuda") {
		t.Fatalf("first invocation = %q, want the full-hardware path", invocations[0])
	}
	live := handler.tm.GetTranscodeSession("session-auto-manifest-failure")
	if live == nil {
		t.Fatal("committed transport did not register a transcode session")
	}
	if opts := live.Opts(); opts.HWAccel != "nvenc" || !opts.SoftwareVideoDecode {
		t.Fatalf("live session = %s software decode %v, want the CPU-decode fallback path", opts.HWAccel, opts.SoftwareVideoDecode)
	}
}

// TestPrepareLocalTransportV3AutoExhaustedManifestFailureIsReadiness pins the
// classification once every path fails before its first manifest: the start
// reports a readiness failure ("failed before media became ready"), not the
// spawn-failure message, so a caller can tell a dead encoder from an invalid
// start request.
func TestPrepareLocalTransportV3AutoExhaustedManifestFailureIsReadiness(t *testing.T) {
	ffmpegPath, logPath := writePlaybackTestFFmpegFailingOn(t, "-f hls")
	_, transport, transportErr := prepareAutoLocalTransportV3(t, ffmpegPath, "session-auto-manifest-exhausted")
	if transportErr == nil {
		transport.rollback()
		t.Fatal("all manifest failures returned a playable transport")
	}
	if strings.Contains(transportErr.message, "Failed to start") {
		t.Fatalf("transport error = %#v, want the readiness classification, not a spawn failure", transportErr)
	}
	if !strings.Contains(transportErr.message, "before media became ready") {
		t.Fatalf("transport error message = %q, want the readiness failure message", transportErr.message)
	}
	invocations := readPlaybackTestFFmpegInvocations(t, logPath)
	if len(invocations) != 3 {
		t.Fatalf("invocations = %q, want all three execution paths attempted", invocations)
	}
}
