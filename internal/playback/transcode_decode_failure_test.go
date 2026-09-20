package playback

import (
	"context"
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
