package playback

import (
	"fmt"
	"testing"
	"time"
)

// A restart that retains an identical recipe's segments leaves a manifest
// listing media the replacement process did not produce. The throttler must
// measure its forward buffer against current-generation output only, so a
// retained window ahead cannot pause a stable play before the replacement
// writes anything.
func TestSegmentProgressFencesPriorGenerationOutput(t *testing.T) {
	dir := t.TempDir()
	staleTime := time.Now().Add(-time.Minute)
	writeManifestRange(t, dir, 225, 293, ".ts")
	for i := 225; i <= 293; i++ {
		writeSegmentFile(t, dir, fmt.Sprintf("seg_%05d.ts", i), []byte("retained"), staleTime)
	}

	session := &TranscodeSession{
		outputDir:            dir,
		running:              true,
		lastRequestedSegment: 225,
		generationStartedAt:  time.Now(),
		opts: TranscodeOpts{
			TargetCodecVideo:   "h264",
			SegmentDuration:    2,
			StartSegmentNumber: 225,
		},
	}
	progress := session.SegmentProgress(time.Now())
	if progress.ProducedHead != 293 {
		t.Fatalf("ProducedHead = %d, want 293 (the retained manifest lists the prior generation)", progress.ProducedHead)
	}
	if progress.ProducedHeadCurrentGeneration >= progress.StartSegmentNumber {
		t.Fatalf("ProducedHeadCurrentGeneration = %d, want < %d before the replacement produced anything",
			progress.ProducedHeadCurrentGeneration, progress.StartSegmentNumber)
	}
	if !progressPredatesGeneration(progress) {
		t.Fatal("prior-generation output was not recognized as predating the current generation")
	}

	writer := &recordingWriteCloser{}
	throttler := NewTranscodeThrottler(session, writer, 60, 2)
	throttler.CheckOnce()
	if throttler.paused {
		t.Fatal("throttler paused on a prior generation's retained window")
	}

	// This generation produces its own segment; a large forward buffer now
	// pauses normally.
	writeSegmentFile(t, dir, "seg_00293.ts", []byte("current"), time.Now())
	progress = session.SegmentProgress(time.Now())
	if progress.ProducedHeadCurrentGeneration != 293 {
		t.Fatalf("ProducedHeadCurrentGeneration = %d, want 293 after this generation produced it",
			progress.ProducedHeadCurrentGeneration)
	}
	throttler.CheckOnce()
	if !throttler.paused {
		t.Fatal("throttler did not pause on this generation's own produced head")
	}
}
