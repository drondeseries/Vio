package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hevcFatalErrorLine mirrors the prod stderr shape from an HEVC decoder
// refusing an invalid packet: the bitstream itself is undecodable.
func hevcFatalErrorLine() string {
	return `[vist#0:0/hevc @ 0x55d0] [dec:hevc @ 0x55d1] Error submitting packet to decoder: Invalid data found when processing input`
}

// hevcPOCErrorLine mirrors the prod stderr shape from an HEVC decoder
// priming its reference lists on a Dolby Vision profile 8 stream. It is a
// warning, not a source verdict: live sessions emitted a bounded burst and then
// produced valid, decodable segments.
func hevcPOCErrorLine() string {
	return `[hevc @ 0x55d0] Could not find ref with POC 16`
}

// TestDecodeErrorLineMatchesOnlyDecoderFailures proves which stderr lines count
// as a decoder rejecting the source and which are encoder/output/HLS/network
// chatter or tolerable reference-list warnings. The negative set mirrors
// TestNonDemuxErrorsDoNotCount's discipline.
func TestDecodeErrorLineMatchesOnlyDecoderFailures(t *testing.T) {
	positive := []string{
		`[vist#0:0/hevc @ 0x55d0] [dec:hevc @ 0x55d1] Error submitting packet to decoder: Invalid data found when processing input`,
		`[hevc @ 0x55d0] Invalid NAL unit size (1215484279 > 206).`,
		`[hevc @ 0x55d0] Error splitting the input into NAL units.`,
		`[h264 @ 0x55d0] Failed to decode picture`,
		`[hevc @ 0x55d0] decode_slice_header error`,
	}
	for _, line := range positive {
		if !decodeErrorLine(line) {
			t.Errorf("decodeErrorLine(%q) = false, want true", line)
		}
	}
	negative := []string{
		// Reference-list warnings: a bounded opening burst on a playable Dolby
		// Vision profile 8 source, verified to keep producing decodable segments.
		`[hevc @ 0x55d0] Could not find ref with POC 16`,
		`[hevc @ 0x55d0] Error constructing the frame RPS`,
		`[h264_qsv @ 0x55d0] Error writing trailer: Broken pipe`,
		`[hls @ 0x55d0] Error writing trailer`,
		`[https @ 0x55d0] HTTP error 503 Service Unavailable, reconnecting`,
		`[hevc @ 0x55d0] Reinit context to 1920x1080, pix_fmt yuv420p`,
		`frame=  120 fps=0.0 q=-0.0 size=   512kB time=00:00:05.00 bitrate= 838.9kbits/s`,
	}
	for _, line := range negative {
		if decodeErrorLine(line) {
			t.Errorf("decodeErrorLine(%q) = true, want false", line)
		}
	}
}

// TestReferenceListWarningsDoNotRejectSource proves the decoder stderr shape
// that wrongly fatalised playable Dolby Vision profile 8 content no longer
// records a source rejection, even well past the failure threshold and even on
// a hardware plan. A live session emitted 170 such lines and still produced
// decodable segments.
func TestReferenceListWarningsDoNotRejectSource(t *testing.T) {
	marked := make(chan struct{}, 4)
	opts := TranscodeOpts{
		MediaFileID:        91,
		CanonicalInputPath: "virtual://movie/tt1?result=poc",
		TargetCodecVideo:   "av1",
		OnSourceRejected: func(context.Context, int, string) error {
			marked <- struct{}{}
			return nil
		},
	}
	s := &TranscodeSession{opts: opts}
	ctx := context.Background()
	for i := 0; i < decodeErrorThreshold*20; i++ {
		s.logFFmpegLine(ctx, hevcPOCErrorLine())
		s.logFFmpegLine(ctx, `[hevc @ 0x55d0] Error constructing the frame RPS`)
	}
	if s.IsSourceRejected() {
		t.Fatal("reference-list warnings recorded a source rejection")
	}
	if s.IsDecodeFailed() {
		t.Fatal("reference-list warnings claimed the hardware decoder failed")
	}
	select {
	case <-marked:
		t.Fatal("reference-list warnings invoked the source-rejected marker")
	default:
	}
}

// TestDecodeFailureStampsAfterThreshold covers the threshold and idempotence:
// the stamp is not set before decodeErrorThreshold matched lines and never
// reports twice.
func TestDecodeFailureStampsAfterThreshold(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	base := time.Unix(2000, 0)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		if s.observeDecodeError(base.Add(time.Duration(i)*time.Millisecond), hevcFatalErrorLine()) {
			t.Fatalf("stamped after only %d errors", i+1)
		}
	}
	if s.IsDecodeFailed() {
		t.Fatal("session marked decode-failed before the threshold")
	}
	if !s.observeDecodeError(base.Add(time.Second), hevcFatalErrorLine()) {
		t.Fatal("not stamped when the threshold is reached")
	}
	if !s.IsDecodeFailed() {
		t.Fatal("session not marked decode-failed after the threshold")
	}
	if s.observeDecodeError(base.Add(2*time.Second), hevcFatalErrorLine()) {
		t.Fatal("stamp reported more than once")
	}
}

// TestDecodeFailureDecaysAfterQuietWindow proves a healthy stretch resets the
// counter so failures spread past the window never accumulate into a stamp.
func TestDecodeFailureDecaysAfterQuietWindow(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	base := time.Unix(3000, 0)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		s.observeDecodeError(base, hevcFatalErrorLine())
	}
	// More than decodeErrorDecay after the previous failure: the counter resets,
	// so this is a fresh first strike and must not stamp.
	decayed := base.Add(decodeErrorDecay + time.Second)
	if s.observeDecodeError(decayed, hevcFatalErrorLine()) {
		t.Fatal("decayed error counted toward the threshold")
	}
	if s.IsDecodeFailed() {
		t.Fatal("stamped after a decayed error")
	}
}

// TestDecodeFailureDoesNotStampSoftwareOrCopy proves a decoder error on a
// software-decode plan or a copy target is a bad bitstream, not evidence that
// hardware decode should be replaced, so it never stamps.
func TestDecodeFailureDoesNotStampSoftwareOrCopy(t *testing.T) {
	for _, test := range []struct {
		name string
		opts TranscodeOpts
	}{
		{name: "software decode", opts: TranscodeOpts{TargetCodecVideo: "h264", SoftwareVideoDecode: true}},
		{name: "copy video", opts: TranscodeOpts{TargetCodecVideo: "copy"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &TranscodeSession{opts: test.opts}
			for i := 0; i < decodeErrorThreshold*3; i++ {
				if s.observeDecodeError(time.Unix(4000, 0), hevcFatalErrorLine()) {
					t.Fatal("software/copy session stamped on decoder failures")
				}
			}
			if s.IsDecodeFailed() {
				t.Fatal("software/copy session marked decode-failed")
			}
		})
	}
}

// TestDecodeFailureRecordsSourceRejectionForEveryDecodeMode proves the source
// verdict is independent of the decode-mode retry: a software plan records the
// rejection at the threshold (so the serve path can report it as permanent)
// without claiming the hardware decoder failed, and a copy target records
// neither.
func TestDecodeFailureRecordsSourceRejectionForEveryDecodeMode(t *testing.T) {
	software := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264", SoftwareVideoDecode: true}}
	for i := 0; i < decodeErrorThreshold; i++ {
		if software.observeDecodeError(time.Unix(5000, 0), hevcFatalErrorLine()) {
			t.Fatal("software plan reported a hardware-decode transition")
		}
	}
	if !software.IsSourceRejected() {
		t.Fatal("software plan did not record the source rejection")
	}
	if software.IsDecodeFailed() {
		t.Fatal("software plan claimed the hardware decoder failed")
	}

	hardware := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	for i := 0; i < decodeErrorThreshold; i++ {
		hardware.observeDecodeError(time.Unix(5100, 0), hevcFatalErrorLine())
	}
	if !hardware.IsSourceRejected() || !hardware.IsDecodeFailed() {
		t.Fatalf("hardware plan rejected=%v decodeFailed=%v, want both true", hardware.IsSourceRejected(), hardware.IsDecodeFailed())
	}

	copyTarget := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "copy"}}
	for i := 0; i < decodeErrorThreshold*3; i++ {
		copyTarget.observeDecodeError(time.Unix(6000, 0), hevcFatalErrorLine())
	}
	if copyTarget.IsSourceRejected() {
		t.Fatal("copy target recorded a source decode rejection")
	}
}

// TestDecodeFailureInvokesSourceRejectedMarkerOnce proves a decoder rejection
// is handed to the source-candidate failure callback exactly once, past the
// same threshold and with the same identity the demux marker uses, so the
// candidate is stamped through the one existing failed_at mechanism. The
// callback is the mark path: it fires for a hardware plan and for a software
// plan (the source is bad, not the decoder mode), never for a copy target, and
// never before the threshold.
func TestDecodeFailureInvokesSourceRejectedMarkerOnce(t *testing.T) {
	type call struct {
		fileID    int
		canonical string
	}
	newSession := func(opts TranscodeOpts, calls chan call) *TranscodeSession {
		opts.OnSourceRejected = func(_ context.Context, fileID int, canonical string) error {
			calls <- call{fileID: fileID, canonical: canonical}
			return nil
		}
		return &TranscodeSession{opts: opts}
	}
	ctx := context.Background()

	hardwareCalls := make(chan call, 4)
	hardware := newSession(TranscodeOpts{
		MediaFileID:        77,
		CanonicalInputPath: "virtual://movie/tt1?result=bad",
		TargetCodecVideo:   "h264",
	}, hardwareCalls)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		hardware.logFFmpegLine(ctx, hevcFatalErrorLine())
	}
	select {
	case got := <-hardwareCalls:
		t.Fatalf("marker invoked before the threshold: %+v", got)
	default:
	}
	hardware.logFFmpegLine(ctx, hevcFatalErrorLine())
	select {
	case got := <-hardwareCalls:
		if got.fileID != 77 || got.canonical != "virtual://movie/tt1?result=bad" {
			t.Fatalf("marker call = %+v, want file 77 and the canonical virtual path", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source-rejected marker was not invoked at the threshold")
	}
	// The notified flag is set under the mutex before dispatch, so a later
	// error must not spawn a second marker call.
	hardware.logFFmpegLine(ctx, hevcFatalErrorLine())
	select {
	case extra := <-hardwareCalls:
		t.Fatalf("source-rejected marker invoked more than once: %+v", extra)
	default:
	}

	// A software plan records the source rejection too: the bitstream is bad,
	// not merely the hardware decoder.
	softwareCalls := make(chan call, 1)
	software := newSession(TranscodeOpts{
		MediaFileID:         78,
		CanonicalInputPath:  "virtual://movie/tt2?result=sw",
		TargetCodecVideo:    "h264",
		SoftwareVideoDecode: true,
	}, softwareCalls)
	for i := 0; i < decodeErrorThreshold; i++ {
		software.logFFmpegLine(ctx, hevcFatalErrorLine())
	}
	select {
	case got := <-softwareCalls:
		if got.fileID != 78 || got.canonical != "virtual://movie/tt2?result=sw" {
			t.Fatalf("software marker call = %+v, want file 78 and the canonical path", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("software source rejection did not invoke the marker")
	}
	if software.IsDecodeFailed() {
		t.Fatal("software plan claimed the hardware decoder failed")
	}

	// A copy target never reports a decode verdict, so it never marks.
	copyCalls := make(chan struct{}, 1)
	copyTarget := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "copy",
		OnSourceRejected: func(context.Context, int, string) error {
			copyCalls <- struct{}{}
			return nil
		},
	}}
	for i := 0; i < decodeErrorThreshold*3; i++ {
		copyTarget.logFFmpegLine(ctx, hevcFatalErrorLine())
	}
	select {
	case <-copyCalls:
		t.Fatal("copy target invoked the source-rejected marker")
	default:
	}
}

// TestLogFFmpegLineObservesDecodeFailure exercises the stderr plumbing the
// transcode process uses, including the diagnostic sample kept for the replan
// decision log.
func TestLogFFmpegLineObservesDecodeFailure(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	ctx := context.Background()
	for i := 0; i < decodeErrorThreshold; i++ {
		s.logFFmpegLine(ctx, hevcFatalErrorLine())
	}
	if !s.IsDecodeFailed() {
		t.Fatal("logFFmpegLine did not observe the decoder failure")
	}
	sample, count := s.DecodeFailureEvidence()
	if sample != hevcFatalErrorLine() || count != decodeErrorThreshold {
		t.Fatalf("evidence = (%q, %d), want (%q, %d)", sample, count, hevcFatalErrorLine(), decodeErrorThreshold)
	}
}

// TestDecodeFailureRequiresVideoStreamIdentity proves the source-rejection
// verdict is scoped to the video stream: a subtitle decoder refusing a corrupt
// subtitle packet, and a decoder line with no stream identity at all, must not
// reject the video source. A video codec/NAL failure still does.
func TestDecodeFailureRequiresVideoStreamIdentity(t *testing.T) {
	stamped := func(line string) (rejected, marked bool) {
		markedCh := make(chan struct{}, 1)
		s := &TranscodeSession{opts: TranscodeOpts{
			MediaFileID:        11,
			CanonicalInputPath: "virtual://movie/tt-id?result=x",
			TargetCodecVideo:   "h264",
			OnSourceRejected: func(context.Context, int, string) error {
				markedCh <- struct{}{}
				return nil
			},
		}}
		ctx := context.Background()
		for i := 0; i < decodeErrorThreshold; i++ {
			s.logFFmpegLine(ctx, line)
		}
		rejected = s.IsSourceRejected()
		select {
		case <-markedCh:
			marked = true
		case <-time.After(time.Second):
		}
		return rejected, marked
	}

	ambiguous := `Error submitting packet to decoder: Invalid data found when processing input`
	if rejected, marked := stamped(ambiguous); rejected || marked {
		t.Fatal("a decoder failure with no stream identity rejected the video source")
	}
	subtitle := `[sist#0:2/subrip @ 0x55d0] [dec:subrip @ 0x55d1] Error submitting packet to decoder: Invalid data found when processing input`
	if rejected, marked := stamped(subtitle); rejected || marked {
		t.Fatal("a subtitle decoder failure rejected the video source")
	}
	videoNAL := `[hevc @ 0x55d0] Invalid NAL unit size (1215484279 > 206).`
	if rejected, marked := stamped(videoNAL); !rejected || !marked {
		t.Fatal("a video NAL failure no longer rejected the video source")
	}
}

// TestVideoStreamEvidenceRejectsAudioStreams pins the lexical rule directly: an
// audio decoder tag or explicit audio-stream wording never counts as video
// evidence, even when the line also carries a video-shaped word, while a
// video-tagged or video-codec line does.
func TestVideoStreamEvidenceRejectsAudioStreams(t *testing.T) {
	for _, line := range []string{
		`[aac @ 0x55d0] Error submitting packet to decoder`,
		`[ac3 @ 0x55d0] error decoding audio bitstream`,
		`[eac3 @ 0x55d0] error decoding audio bitstream`,
		`[truehd @ 0x55d0] error decoding audio bitstream`,
		`[dts @ 0x55d0] error decoding audio bitstream`,
		`[flac @ 0x55d0] error decoding audio bitstream`,
		`[opus @ 0x55d0] error decoding audio bitstream`,
		`[aist#0:1/aac @ 0x55d0] error decoding audio bitstream`,
		`Audio stream 0:1 failed to decode bitstream`,
	} {
		if videoStreamEvidenceV3(line) {
			t.Fatalf("audio stream line %q was treated as video evidence", line)
		}
	}
	if !videoStreamEvidenceV3(`[vist#0:0/h264 @ 0x55d0] Error during demuxing`) {
		t.Fatal("a vist# line must remain video evidence")
	}
	if !videoStreamEvidenceV3(`[hevc @ 0x55d0] Invalid NAL unit size (1215484279 > 206)`) {
		t.Fatal("a video codec line must remain video evidence")
	}
}

// TestDecodeFailureAudioStreamIdentityNeverStamps proves an audio decoder error
// can never stamp the video candidate or reject the source, even when the line
// carries a word ("bitstream") that also appears in video failures. A genuine
// video failure on the same line shape still indicts.
func TestDecodeFailureAudioStreamIdentityNeverStamps(t *testing.T) {
	stamped := func(line string) (rejected, marked bool) {
		markedCh := make(chan struct{}, 1)
		s := &TranscodeSession{opts: TranscodeOpts{
			MediaFileID:      11,
			TargetCodecVideo: "h264",
			OnSourceRejected: func(context.Context, int, string) error {
				markedCh <- struct{}{}
				return nil
			},
		}}
		ctx := context.Background()
		for i := 0; i < decodeErrorThreshold; i++ {
			s.logFFmpegLine(ctx, line)
		}
		rejected = s.IsSourceRejected()
		select {
		case <-markedCh:
			marked = true
		case <-time.After(time.Second):
		}
		return rejected, marked
	}

	for _, line := range []string{
		`[aac @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`[ac3 @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`[eac3 @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`[truehd @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`[dca @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`[flac @ 0x55d0] Failed to decode audio bitstream`,
		`[opus @ 0x55d0] Error splitting the input into NAL units`, // audio tag wins over the video-shaped phrase
		`[aist#0:1/aac @ 0x55d0] Error submitting packet to decoder: Invalid data found when processing input`,
		`Audio stream 0:1 Error submitting packet to decoder: Invalid data found when processing input`,
	} {
		if rejected, marked := stamped(line); rejected || marked {
			t.Fatalf("an audio decoder failure %q rejected the video source", line)
		}
	}
	// A video decoder failure is unchanged even though it mentions the same
	// "bitstream" word an audio failure can carry.
	videoBitstream := `[hevc @ 0x55d0] Invalid NAL unit size (1215484279 > 206) while parsing bitstream.`
	if rejected, marked := stamped(videoBitstream); !rejected || !marked {
		t.Fatal("a video bitstream failure no longer rejected the video source")
	}
}

// prodSPSMissingLine and prodPPSRangeLine are the exact stderr shapes from the
// production incident where a 4K HEVC provider stream carried no parameter
// sets: ffmpeg emitted them for every frame with zero segments produced, while
// the pre-split matcher counted nothing and the start decayed into a generic
// startup timeout instead of a candidate rotation.
func prodSPSMissingLine() string {
	return `[hevc @ 0x560d3bfc3280] SPS 0 does not exist.`
}

func prodPPSRangeLine() string {
	return `[hevc @ 0x560d3bfc82c0] PPS id out of range: 0`
}

// TestDecodeErrorLineMatchesParameterSetLoss proves the parameter-set-missing
// family counts as a decoder rejecting the source, while reference-list
// warnings and encoder/output chatter still do not.
func TestDecodeErrorLineMatchesParameterSetLoss(t *testing.T) {
	positive := []string{
		prodSPSMissingLine(),
		prodPPSRangeLine(),
		`[hevc @ 0x55d0] non-existing PPS 0 referenced`,
		`[h265 @ 0x55d0] SPS 1 does not exist.`,
	}
	for _, line := range positive {
		if !decodeErrorLine(line) {
			t.Errorf("decodeErrorLine(%q) = false, want true", line)
		}
		if !videoStreamEvidenceV3(line) {
			t.Errorf("videoStreamEvidenceV3(%q) = false, want true: the indictment path requires video identity", line)
		}
	}
	negative := []string{
		`[hevc @ 0x55d0] Could not find ref with POC 16`,
		`[hevc @ 0x55d0] Error constructing the frame RPS`,
		`[hevc_qsv @ 0x55d0] Error writing trailer: Broken pipe`,
		`[hevc @ 0x55d0] Reinit context to 1920x1080, pix_fmt yuv420p`,
	}
	for _, line := range negative {
		if decodeErrorLine(line) {
			t.Errorf("decodeErrorLine(%q) = true, want false", line)
		}
	}
}

// TestDecodeFailureStampsParameterSetLossStorm replays the production
// incident shape: a storm of SPS/PPS-missing lines with no output stamps the
// source rejection and invokes the candidate marker exactly once, so the serve
// path rotates instead of stalling into a startup timeout.
func TestDecodeFailureStampsParameterSetLossStorm(t *testing.T) {
	markedCh := make(chan struct{}, 1)
	s := &TranscodeSession{opts: TranscodeOpts{
		MediaFileID:        1433,
		CanonicalInputPath: "virtual://series/tt3006802/8/5?result=59959f2d428d742fb6d18d0c",
		TargetCodecVideo:   "hevc",
		OnSourceRejected: func(context.Context, int, string) error {
			markedCh <- struct{}{}
			return nil
		},
	}}
	ctx := context.Background()
	lines := []string{prodSPSMissingLine(), prodPPSRangeLine()}
	for i := 0; i < decodeErrorThreshold; i++ {
		s.logFFmpegLine(ctx, lines[i%len(lines)])
	}
	if !s.IsSourceRejected() {
		t.Fatal("parameter-set-loss storm did not record the source rejection")
	}
	select {
	case <-markedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("parameter-set-loss storm did not invoke the source-rejected marker")
	}
	sample, count := s.DecodeFailureEvidence()
	if count != decodeErrorThreshold {
		t.Fatalf("evidence count = %d, want %d", count, decodeErrorThreshold)
	}
	if sample == "" {
		t.Fatal("evidence sample is empty")
	}
}

// TestSourceRejectionSuppressedAfterVideoProgress proves the verdict is about
// an undecodable source, not a noisy one: a decoder storm on a generation
// that already muxed segments neither rejects nor marks. A manifest alone, an
// empty segment, or segments older than the generation (a previous
// generation's leftovers) prove nothing and leave the verdict standing.
func TestSourceRejectionSuppressedAfterVideoProgress(t *testing.T) {
	storm := func(s *TranscodeSession) {
		ctx := context.Background()
		for i := 0; i < decodeErrorThreshold; i++ {
			s.logFFmpegLine(ctx, prodSPSMissingLine())
		}
	}
	writeFile := func(t *testing.T, dir, name string, size int, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if !mtime.IsZero() {
			if err := os.Chtimes(filepath.Join(dir, name), mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("recovery after the storm suppresses", func(t *testing.T) {
		dir := t.TempDir()
		s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "hevc"}}
		s.mu.Lock()
		s.outputDir = dir
		s.generationStartedAt = time.Now().Add(-time.Minute)
		s.mu.Unlock()
		// Fake-clock storm in the past; the segment file written afterwards
		// carries a real (newer) mtime, proving the decoder recovered.
		base := time.Unix(7000, 0)
		for i := 0; i < decodeErrorThreshold; i++ {
			s.observeDecodeError(base.Add(time.Duration(i)*100*time.Millisecond), prodSPSMissingLine())
		}
		if !s.IsSourceRejected() {
			t.Fatal("storm with no output did not record the verdict")
		}
		writeFile(t, dir, "seg_00000.ts", 188, time.Time{})
		if s.IsSourceRejected() {
			t.Fatal("video produced after the storm did not lift the verdict")
		}
	})

	t.Run("manifest alone does not suppress", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "stream.m3u8", 64, time.Time{})
		s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "hevc"}}
		s.mu.Lock()
		s.outputDir = dir
		s.generationStartedAt = time.Now().Add(-time.Minute)
		s.mu.Unlock()
		storm(s)
		if !s.IsSourceRejected() {
			t.Fatal("manifest without segments suppressed the verdict")
		}
	})

	t.Run("empty segment does not suppress", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "seg_00000.ts", 0, time.Time{})
		s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "hevc"}}
		s.mu.Lock()
		s.outputDir = dir
		s.generationStartedAt = time.Now().Add(-time.Minute)
		s.mu.Unlock()
		storm(s)
		if !s.IsSourceRejected() {
			t.Fatal("empty segment suppressed the verdict")
		}
	})

	t.Run("previous generation leftovers do not suppress", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Now()
		writeFile(t, dir, "seg_00000.ts", 188, now.Add(-time.Hour))
		s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "hevc"}}
		s.mu.Lock()
		s.outputDir = dir
		s.generationStartedAt = now
		s.mu.Unlock()
		storm(s)
		if !s.IsSourceRejected() {
			t.Fatal("stale segments suppressed the fresh generation's verdict")
		}
	})
}

// rejectedGenerationFixture builds a live-looking session with a stamped
// generation and an empty output directory: waiters block, and a decoder
// storm records the verdict under test control. No ffmpeg process runs,
// so revocation only latches verdict state (cancel is nil-safe).
func rejectedGenerationFixture(t *testing.T) *TranscodeSession {
	t.Helper()
	s := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo:   "hevc",
		CanonicalInputPath: "virtual://movie/tt-test?result=x",
	}}
	s.mu.Lock()
	s.outputDir = t.TempDir()
	s.running = true
	s.generationStartedAt = time.Now()
	s.mu.Unlock()
	return s
}

func stormCurrentGeneration(ctx context.Context, s *TranscodeSession) {
	for i := 0; i < decodeErrorThreshold; i++ {
		s.logFFmpegLine(ctx, prodSPSMissingLine())
	}
}

// TestRejectedGenerationWakesSegmentWaits proves a rejected generation
// terminates every waiter flavor on the typed verdict instead of the
// deadline: segment, opened-segment, and manifest waits must all return
// ErrSourceDecodeRejected promptly once the storm crosses the threshold.
// Waiters observe the verdict through their tick-bounded gated checks.
func TestRejectedGenerationWakesSegmentWaits(t *testing.T) {
	s := rejectedGenerationFixture(t)
	ctx := context.Background()

	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, 3)
	go func() {
		_, err := s.WaitForSegment("seg_00009.ts", 30*time.Second)
		results <- outcome{"segment", err}
	}()
	go func() {
		_, err := s.WaitForOpenSegment("seg_00009.ts", 30*time.Second)
		results <- outcome{"open-segment", err}
	}()
	go func() {
		_, err := s.waitForManifest(ctx, 30*time.Second, true)
		results <- outcome{"manifest", err}
	}()
	// Let the waiters block before the storm lands.
	time.Sleep(200 * time.Millisecond)
	stormCurrentGeneration(ctx, s)

	for i := 0; i < 3; i++ {
		select {
		case got := <-results:
			if !errors.Is(got.err, ErrSourceDecodeRejected) {
				t.Fatalf("%s wait err = %v, want ErrSourceDecodeRejected", got.name, got.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("revoked generation did not wake a waiter promptly")
		}
	}
}

// TestSimultaneousWaitsReceiveSameVerdict proves one rejection fans out to
// every waiter: concurrent segment requests on the same dead generation all
// observe the identical typed verdict.
func TestSimultaneousWaitsReceiveSameVerdict(t *testing.T) {
	s := rejectedGenerationFixture(t)
	ctx := context.Background()

	const waiters = 5
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			_, err := s.WaitForSegment("seg_00007.ts", 30*time.Second)
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	stormCurrentGeneration(ctx, s)

	for i := 0; i < waiters; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrSourceDecodeRejected) {
				t.Fatalf("waiter %d err = %v, want ErrSourceDecodeRejected", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("waiter %d was not woken by the revocation", i)
		}
	}
}

// TestManifestWaitIgnoresRevocationForLocalInput proves the startup manifest
// wait only short-circuits on the verdict for virtual inputs. A local file
// commits and recovers through the replan software path, which requires the
// committed session the verdict must not preempt — so the wait runs to its
// deadline (here shrunk) instead of returning the typed verdict, even though
// IsSourceRejected is true.
func TestManifestWaitIgnoresRevocationForLocalInput(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo:   "hevc",
		CanonicalInputPath: "/media/movies/local.mkv",
	}}
	s.mu.Lock()
	s.outputDir = t.TempDir()
	s.running = true
	s.generationStartedAt = time.Now()
	s.mu.Unlock()

	ctx := context.Background()
	for i := 0; i < decodeErrorThreshold; i++ {
		s.logFFmpegLine(ctx, prodSPSMissingLine())
	}
	if !s.IsSourceRejected() {
		t.Fatal("storm did not record the verdict to ignore")
	}
	start := time.Now()
	_, err := s.waitForManifest(ctx, 300*time.Millisecond, true)
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("local manifest wait returned after %v, want the full deadline", elapsed)
	}
	if err == nil || errors.Is(err, ErrSourceDecodeRejected) {
		t.Fatalf("local manifest wait err = %v, want the deadline error, never the typed verdict", err)
	}
}

// TestRestartRefusedAfterRejection proves a revoked generation cannot be
// rebuilt: restarting the same undecodable bytes is refused with the typed
// verdict so the caller rotates instead. A healthy session keeps its restart.
func TestRestartRefusedAfterRejection(t *testing.T) {
	ctx := context.Background()
	rejected := rejectedGenerationFixture(t)
	stormCurrentGeneration(ctx, rejected)
	if !rejected.IsSourceRejected() {
		t.Fatal("storm did not record the verdict to refuse against")
	}
	if err := rejected.Restart(ctx, 0, 0); !errors.Is(err, ErrSourceDecodeRejected) {
		t.Fatalf("Restart err = %v, want ErrSourceDecodeRejected", err)
	}
	if _, _, err := rejected.RestartSegment(ctx, 9); !errors.Is(err, ErrSourceDecodeRejected) {
		t.Fatalf("RestartSegment err = %v, want ErrSourceDecodeRejected", err)
	}

	healthy := rejectedGenerationFixture(t)
	if healthy.IsSourceRejected() {
		t.Fatal("fresh generation reports a verdict it never earned")
	}
}

// TestRevokeWakesWithoutKillingProcess proves rejection latches the verdict
// while leaving process lifetime to the existing close paths: killing ffmpeg
// at verdict time would convert "manifest will appear" into "manifest never
// appears", engaging the hw_accel=auto HW→software fallback per candidate
// and multiplying transcode starts. Waiters observe the verdict through
// their tick-bounded gated checks; the marker latch fires even with no
// marker callback configured.
func TestRevokeWakesWithoutKillingProcess(t *testing.T) {
	s := rejectedGenerationFixture(t)
	killed := make(chan struct{})
	s.mu.Lock()
	_, innerCancel := context.WithCancel(context.Background())
	s.cancel = func() {
		innerCancel()
		close(killed)
	}
	s.mu.Unlock()

	stormCurrentGeneration(context.Background(), s)
	select {
	case <-killed:
		t.Fatal("revocation killed the process; close paths own process lifetime")
	case <-time.After(200 * time.Millisecond):
	}
	if !s.IsSourceRejected() {
		t.Fatal("verdict missing after revocation")
	}
	s.mu.Lock()
	notified := s.sourceRejectNotified
	s.mu.Unlock()
	if !notified {
		t.Fatal("marker latch not set after revocation")
	}
}

// TestRevocationScopedToGeneration proves a dead generation's verdict cannot
// indict a replacement: after a rejection plus a generation reset, waiters on
// the fresh generation block on the deadline instead of inheriting the old
// verdict.
func TestRevocationScopedToGeneration(t *testing.T) {
	ctx := context.Background()
	s := rejectedGenerationFixture(t)
	stormCurrentGeneration(ctx, s)
	if !s.IsSourceRejected() {
		t.Fatal("first generation did not record the verdict")
	}

	s.mu.Lock()
	s.generationStartedAt = time.Now()
	s.resetDecodeVerdictLocked()
	s.running = true
	s.mu.Unlock()
	if s.IsSourceRejected() {
		t.Fatal("fresh generation inherits the dead generation's verdict")
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitForSegment("seg_00009.ts", 400*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if errors.Is(err, ErrSourceDecodeRejected) {
			t.Fatal("fresh generation waiter inherited the stale verdict")
		}
		if !errors.Is(err, ErrSegmentNotFound) {
			t.Fatalf("fresh waiter err = %v, want deadline ErrSegmentNotFound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fresh generation waiter did not terminate")
	}
}

// TestDecodeWindowResetsOnNewGeneration proves a restart never inherits its
// predecessor's strikes: nine errors, a generation stamp, and one more error
// must not stamp, while ten fresh errors after the stamp must.
func TestDecodeWindowResetsOnNewGeneration(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	base := time.Unix(9000, 0)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		if s.observeDecodeError(base.Add(time.Duration(i)*time.Millisecond), hevcFatalErrorLine()) {
			t.Fatalf("stamped after only %d errors", i+1)
		}
	}
	s.mu.Lock()
	s.generationStartedAt = base.Add(time.Second)
	s.resetDecodeVerdictLocked()
	s.mu.Unlock()
	if s.observeDecodeError(base.Add(2*time.Second), hevcFatalErrorLine()) {
		t.Fatal("fresh generation stamped on the dead generation's count")
	}
	s.mu.Lock()
	first := s.firstDecodeErrorAt
	s.mu.Unlock()
	if !first.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("window did not reopen at the fresh error: %v", first)
	}
	for i := 1; i < decodeErrorThreshold-1; i++ {
		s.observeDecodeError(base.Add(2*time.Second+time.Duration(i)*time.Millisecond), hevcFatalErrorLine())
	}
	if !s.observeDecodeError(base.Add(3*time.Second), hevcFatalErrorLine()) {
		t.Fatal("fresh generation did not stamp after ten new errors")
	}
	if !s.IsSourceRejected() {
		t.Fatal("fresh generation verdict missing after the threshold")
	}
}
