package playback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A backward seek inside the retention window that reuses an identical copy
// (remux) recipe must keep the already-produced segments on disk so the read is
// a file serve instead of a fresh FFmpeg spawn.
func TestRemuxWindowHitSeekKeepsRetainedSegments(t *testing.T) {
	dir := t.TempDir()
	opts := TranscodeOpts{
		SessionID:               "remux-window",
		TargetCodecVideo:        "copy",
		TargetCodecAudio:        "aac",
		AudioTrackIndex:         1,
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	}
	writeManifestRange(t, dir, 5, 9, hlsSegmentExtension(opts))
	for i := 5; i <= 9; i++ {
		writeSegmentFile(t, dir, segmentFilename(i, opts), []byte("window bytes"), time.Now())
	}

	session := &TranscodeSession{outputDir: dir, opts: opts}

	// A seek back to segment 7 re-emits the identical recipe.
	if session.cleanStaleOutputForRestart(opts, opts, 9) {
		t.Fatal("an identical-recipe remux restart discarded retained segments")
	}
	if _, err := os.Stat(filepath.Join(dir, "stream.m3u8")); err != nil {
		t.Fatalf("retained manifest was removed: %v", err)
	}
	for i := 5; i <= 9; i++ {
		name := segmentFilename(i, opts)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("retained segment %s was removed: %v", name, err)
		}
	}

	// The in-window segment is served from the retained file.
	lease, err := session.OpenSegment(segmentFilename(7, opts))
	if err != nil {
		t.Fatalf("window-hit segment was not served from disk: %v", err)
	}
	if got := lease.Info.Size(); got != int64(len("window bytes")) {
		t.Fatalf("served segment size = %d, want %d", got, len("window bytes"))
	}
	_ = lease.Close()
}

// A recipe change invalidates retained remux segments: they were produced by a
// different byte recipe, so serving them would mix generations.
func TestRemuxRecipeChangeDiscardsRetainedSegments(t *testing.T) {
	dir := t.TempDir()
	previous := TranscodeOpts{
		SessionID:               "remux-recipe",
		TargetCodecVideo:        "copy",
		TargetCodecAudio:        "aac",
		AudioTrackIndex:         1,
		SegmentDuration:         2,
		SegmentRetentionSeconds: 600,
	}
	writeManifestRange(t, dir, 5, 9, hlsSegmentExtension(previous))
	for i := 5; i <= 9; i++ {
		writeSegmentFile(t, dir, segmentFilename(i, previous), []byte("old recipe"), time.Now())
	}

	session := &TranscodeSession{outputDir: dir, opts: previous}

	next := previous
	next.AudioTrackIndex = 2 // a different track re-renders every segment

	if !session.cleanStaleOutputForRestart(previous, next, 9) {
		t.Fatal("a changed remux recipe kept stale segments")
	}
	if _, err := os.Stat(filepath.Join(dir, "stream.m3u8")); err == nil {
		t.Fatal("the previous generation's manifest survived a recipe change")
	}
	for i := 9; i <= 9; i++ {
		if _, err := os.Stat(filepath.Join(dir, segmentFilename(i, previous))); err == nil {
			t.Fatalf("segment %d survived a recipe change", i)
		}
	}
	// Retained segments below the restart point still survive the clean.
	if _, err := os.Stat(filepath.Join(dir, segmentFilename(5, previous))); err != nil {
		t.Fatalf("pre-restart segment was removed by the clean: %v", err)
	}
}

// The remux recipe comparison covers packaging and audio choices that change
// the emitted bytes even when the video codec string is unchanged.
func TestRemuxRecipeComparisonCoversPackagingAndAudio(t *testing.T) {
	base := TranscodeOpts{TargetCodecVideo: "copy", TargetCodecAudio: "aac", AudioTrackIndex: 1, TargetAudioChannels: 2}

	same := base
	same.SeekSeconds = 42
	same.StartSegmentNumber = 7
	same.FastStart = true
	if emittedRecipeOf(base) != emittedRecipeOf(same) {
		t.Fatal("a restart that only moves the seek position changed the emitted recipe")
	}

	for name, mutate := range map[string]func(*TranscodeOpts){
		"audio track":       func(o *TranscodeOpts) { o.AudioTrackIndex = 2 },
		"audio codec":       func(o *TranscodeOpts) { o.TargetCodecAudio = "copy" },
		"audio channels":    func(o *TranscodeOpts) { o.TargetAudioChannels = 6 },
		"mpegts packaging":  func(o *TranscodeOpts) { o.CopyVideoMPEGTS = true },
		"sample entry":      func(o *TranscodeOpts) { o.VideoSampleEntry = VideoSampleEntryHVC1 },
		"copy recipe":       func(o *TranscodeOpts) { o.CopyFMP4RecipeVersion = CopyFMP4RecipeVersion },
		"subtitle burn-in":  func(o *TranscodeOpts) { o.SubtitleBurnIn = true },
		"segment duration":  func(o *TranscodeOpts) { o.SegmentDuration = 6 },
		"resolution":        func(o *TranscodeOpts) { o.TargetResolution = "720p" },
		"video bitrate cap": func(o *TranscodeOpts) { o.TargetBitrateKbps = 4000 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if emittedRecipeOf(base) == emittedRecipeOf(changed) {
				t.Fatalf("changing %s did not change the emitted recipe comparison", name)
			}
		})
	}
}
