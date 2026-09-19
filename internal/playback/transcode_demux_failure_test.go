package playback

import (
	"context"
	"errors"
	"testing"
	"time"
)

// demuxIOErrorLine mirrors the prod stderr shape the loop originally keyed on:
// an input container read failure. The container tag names no stream, so it is
// ambiguous evidence about the video candidate: the same input holds the audio
// and subtitle streams, and any of them can fail the demuxer.
func demuxIOErrorLine() string {
	return `[in#0/matroska,webm @ 0x55d0] Error during demuxing: Input/output error`
}

// demuxVideoIOErrorLine mirrors a demux failure FFmpeg attributes to the video
// input stream through its vist# stream-class tag. That is the positive video
// identity an indictment now requires.
func demuxVideoIOErrorLine() string {
	return `[vist#0:0/h264 @ 0x55d0] Error during demuxing: Input/output error`
}

// demuxSubtitleIOErrorLine mirrors a corrupt subtitle stream failing during
// demux. The error is real but names a subtitle stream, so it must never stamp
// the video candidate.
func demuxSubtitleIOErrorLine() string {
	return `[sist#0:3/subrip @ 0x55d0] Error during demuxing: Invalid data found when processing input`
}

// demuxAudioIOErrorLine mirrors a corrupt audio stream failing during demux.
// The error names an audio codec, so it must never stamp the video candidate.
func demuxAudioIOErrorLine() string {
	return `[aac @ 0x55d0] Error during demuxing: Invalid data found when processing input`
}

// TestDemuxFailureStampsCandidateAfterThreeErrors covers the three-strike
// threshold: the callback fires exactly once, with the effective media file id
// and the canonical source path, and only after the third failure. The line
// names the video stream, so the indictment is justified.
func TestDemuxFailureStampsCandidateAfterThreeErrors(t *testing.T) {
	type call struct {
		fileID    int
		canonical string
	}
	calls := make(chan call, 4)
	s := &TranscodeSession{
		opts: TranscodeOpts{
			MediaFileID:        77,
			CanonicalInputPath: "virtual://movie/tt1?result=bad",
			OnDemuxFailure: func(_ context.Context, fileID int, canonical string) error {
				calls <- call{fileID: fileID, canonical: canonical}
				return nil
			},
		},
	}
	ctx := context.Background()

	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
	if s.IsDemuxFailed() {
		t.Fatal("session marked demux-failed after only two errors")
	}

	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
	if !s.IsDemuxFailed() {
		t.Fatal("session not marked demux-failed after three errors")
	}
	select {
	case got := <-calls:
		if got.fileID != 77 || got.canonical != "virtual://movie/tt1?result=bad" {
			t.Fatalf("marker call = %+v, want file 77 and canonical virtual path", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("candidate failure marker was not invoked after three demux errors")
	}

	// The stamp is set under the session mutex before the callback is
	// dispatched, so a fourth error must not spawn a second marker call.
	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
	select {
	case extra := <-calls:
		t.Fatalf("candidate failure marker invoked more than once: %+v", extra)
	default:
	}
}

// TestDemuxFailureRequiresVideoStreamIdentity proves the candidate indictment is
// scoped to the video stream. A container-level demux error, a corrupt subtitle
// stream, and a corrupt audio stream all fail to demux but name no video
// identity, so none may stamp; a video-tagged line still does.
func TestDemuxFailureRequiresVideoStreamIdentity(t *testing.T) {
	stamped := func(line string) bool {
		s := &TranscodeSession{opts: TranscodeOpts{MediaFileID: 9}}
		ctx := context.Background()
		for i := 0; i < demuxErrorThreshold; i++ {
			s.logFFmpegLine(ctx, line)
		}
		return s.IsDemuxFailed()
	}

	if stamped(demuxIOErrorLine()) {
		t.Fatal("an ambiguous container-level demux error stamped the video candidate")
	}
	if stamped(demuxSubtitleIOErrorLine()) {
		t.Fatal("a subtitle-stream demux error stamped the video candidate")
	}
	if stamped(demuxAudioIOErrorLine()) {
		t.Fatal("an audio-stream demux error stamped the video candidate")
	}
	if stamped(`[aist#0:1/ac3 @ 0x55d0] Error during demuxing: Invalid data found when processing input`) {
		t.Fatal("a tagged audio-stream demux error stamped the video candidate")
	}
	if !stamped(demuxVideoIOErrorLine()) {
		t.Fatal("a video-stream demux error no longer stamped the candidate")
	}
}

// TestDemuxFailureDoesNotStampBeforeThreshold proves two demux errors never
// invoke the marker: transient blips must still take the normal recovery path.
func TestDemuxFailureDoesNotStampBeforeThreshold(t *testing.T) {
	calls := make(chan struct{}, 1)
	s := &TranscodeSession{
		opts: TranscodeOpts{
			MediaFileID:        12,
			CanonicalInputPath: "virtual://movie/tt2?result=flaky",
			OnDemuxFailure: func(context.Context, int, string) error {
				calls <- struct{}{}
				return nil
			},
		},
	}
	ctx := context.Background()
	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())
	s.logFFmpegLine(ctx, demuxVideoIOErrorLine())

	if s.IsDemuxFailed() {
		t.Fatal("session marked demux-failed after two errors")
	}
	select {
	case <-calls:
		t.Fatal("candidate failure marker invoked before the threshold")
	default:
	}
}

// TestNonDemuxErrorsDoNotCount proves output errors and transient network
// chatter never accumulate toward the stamp; only input demux failures count.
func TestNonDemuxErrorsDoNotCount(t *testing.T) {
	calls := make(chan struct{}, 1)
	s := &TranscodeSession{
		opts: TranscodeOpts{
			MediaFileID: 3,
			OnDemuxFailure: func(context.Context, int, string) error {
				calls <- struct{}{}
				return nil
			},
		},
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.logFFmpegLine(ctx, `[hls @ 0x1] Error writing trailer: Broken pipe`)
		s.logFFmpegLine(ctx, `[https @ 0x1] HTTP error 503 Service Unavailable, reconnecting`)
	}
	if s.IsDemuxFailed() {
		t.Fatal("non-demux errors stamped the session")
	}
	select {
	case <-calls:
		t.Fatal("candidate failure marker invoked for non-demux errors")
	default:
	}
}

// TestDemuxFailureDecaysAfterQuietWindow proves a healthy stretch resets the
// counter, so failures spread past the decay window are not accumulated into a
// false stamp.
func TestDemuxFailureDecaysAfterQuietWindow(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{MediaFileID: 1}}
	base := time.Unix(1000, 0)

	if s.observeDemuxError(base) {
		t.Fatal("stamped on first error")
	}
	if s.observeDemuxError(base.Add(time.Second)) {
		t.Fatal("stamped on second error")
	}
	// More than demuxErrorDecay after the previous failure: the counter resets,
	// so this is a fresh first strike and must not stamp.
	decayed := base.Add(time.Second + demuxErrorDecay + time.Second)
	if s.observeDemuxError(decayed) {
		t.Fatal("decayed error counted toward the threshold")
	}
	if s.IsDemuxFailed() {
		t.Fatal("stamped after a decayed error")
	}
	// Two more inside the new window still reach the threshold.
	if s.observeDemuxError(decayed.Add(time.Second)) {
		t.Fatal("stamped before threshold")
	}
	if !s.observeDemuxError(decayed.Add(2 * time.Second)) {
		t.Fatal("not stamped when the new window reaches the threshold")
	}
}

// TestDemuxFailedSessionRefusesRestart covers fail-fast: once stamped, both the
// segment recovery and the plain restart path surface
// ErrVirtualSourceDemuxFailed so the caller rotates instead of rebuilding the
// same bad transport.
func TestDemuxFailedSessionRefusesRestart(t *testing.T) {
	s := &TranscodeSession{
		outputDir: t.TempDir(),
		opts: TranscodeOpts{
			SessionID:          "sess-demux",
			MediaFileID:        5,
			CanonicalInputPath: "virtual://movie/tt3?result=bad",
		},
	}
	s.demuxStamped = true
	ctx := context.Background()

	if _, ok, err := s.RestartSegment(ctx, 1); !errors.Is(err, ErrVirtualSourceDemuxFailed) || ok {
		t.Fatalf("RestartSegment = (ok=%v, err=%v), want false/ErrVirtualSourceDemuxFailed", ok, err)
	}
	if err := s.Restart(ctx, 1, 1); !errors.Is(err, ErrVirtualSourceDemuxFailed) {
		t.Fatalf("Restart err = %v, want ErrVirtualSourceDemuxFailed", err)
	}
}

// TestRestartSegmentLockedSurfacesDemuxFailed exercises the manager recovery
// seam the segment handler calls, so the marked-failed behavior is what the
// handler sees rather than a silent rebuild.
func TestRestartSegmentLockedSurfacesDemuxFailed(t *testing.T) {
	s := &TranscodeSession{
		outputDir: t.TempDir(),
		opts:      TranscodeOpts{SessionID: "sess-demux-locked", MediaFileID: 9},
	}
	s.demuxStamped = true
	m := NewTranscodeManager()
	if !m.RegisterTranscodeSession("sess-demux-locked", s) {
		t.Fatal("register session")
	}
	_, ok, err := m.RestartSegmentLocked(context.Background(), "sess-demux-locked", s, 3)
	if !errors.Is(err, ErrVirtualSourceDemuxFailed) {
		t.Fatalf("RestartSegmentLocked err = %v, want ErrVirtualSourceDemuxFailed", err)
	}
	if ok {
		t.Fatal("RestartSegmentLocked reported ok for a demux-failed session")
	}
}
