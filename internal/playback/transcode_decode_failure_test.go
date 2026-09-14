package playback

import (
	"context"
	"testing"
	"time"
)

// hevcPOCErrorLine mirrors the prod stderr shape from a hardware HEVC decoder
// that cannot construct a frame reference list and fails from frame zero.
func hevcPOCErrorLine() string {
	return `[hevc @ 0x55d0] Could not find ref with POC 16`
}

// TestDecodeErrorLineMatchesOnlyDecoderFailures proves which stderr lines count
// as a decoder rejecting the source and which are encoder/output/HLS/network
// chatter. The negative set mirrors TestNonDemuxErrorsDoNotCount's discipline.
func TestDecodeErrorLineMatchesOnlyDecoderFailures(t *testing.T) {
	positive := []string{
		`[hevc @ 0x55d0] Could not find ref with POC 16`,
		`[hevc @ 0x55d0] Error constructing the frame RPS`,
		`[h264 @ 0x55d0] Failed to decode picture`,
		`[hevc @ 0x55d0] decode_slice_header error`,
	}
	for _, line := range positive {
		if !decodeErrorLine(line) {
			t.Errorf("decodeErrorLine(%q) = false, want true", line)
		}
	}
	negative := []string{
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

// TestDecodeFailureStampsAfterThreshold covers the threshold and idempotence:
// the stamp is not set before decodeErrorThreshold matched lines and never
// reports twice.
func TestDecodeFailureStampsAfterThreshold(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	base := time.Unix(2000, 0)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		if s.observeDecodeError(base.Add(time.Duration(i)*time.Millisecond), hevcPOCErrorLine()) {
			t.Fatalf("stamped after only %d errors", i+1)
		}
	}
	if s.IsDecodeFailed() {
		t.Fatal("session marked decode-failed before the threshold")
	}
	if !s.observeDecodeError(base.Add(time.Second), hevcPOCErrorLine()) {
		t.Fatal("not stamped when the threshold is reached")
	}
	if !s.IsDecodeFailed() {
		t.Fatal("session not marked decode-failed after the threshold")
	}
	if s.observeDecodeError(base.Add(2*time.Second), hevcPOCErrorLine()) {
		t.Fatal("stamp reported more than once")
	}
}

// TestDecodeFailureDecaysAfterQuietWindow proves a healthy stretch resets the
// counter so failures spread past the window never accumulate into a stamp.
func TestDecodeFailureDecaysAfterQuietWindow(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	base := time.Unix(3000, 0)
	for i := 0; i < decodeErrorThreshold-1; i++ {
		s.observeDecodeError(base, hevcPOCErrorLine())
	}
	// More than decodeErrorDecay after the previous failure: the counter resets,
	// so this is a fresh first strike and must not stamp.
	decayed := base.Add(decodeErrorDecay + time.Second)
	if s.observeDecodeError(decayed, hevcPOCErrorLine()) {
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
				if s.observeDecodeError(time.Unix(4000, 0), hevcPOCErrorLine()) {
					t.Fatal("software/copy session stamped on decoder failures")
				}
			}
			if s.IsDecodeFailed() {
				t.Fatal("software/copy session marked decode-failed")
			}
		})
	}
}

// TestLogFFmpegLineObservesDecodeFailure exercises the stderr plumbing the
// transcode process uses, including the diagnostic sample kept for the replan
// decision log.
func TestLogFFmpegLineObservesDecodeFailure(t *testing.T) {
	s := &TranscodeSession{opts: TranscodeOpts{TargetCodecVideo: "h264"}}
	ctx := context.Background()
	for i := 0; i < decodeErrorThreshold; i++ {
		s.logFFmpegLine(ctx, hevcPOCErrorLine())
	}
	if !s.IsDecodeFailed() {
		t.Fatal("logFFmpegLine did not observe the decoder failure")
	}
	sample, count := s.DecodeFailureEvidence()
	if sample != hevcPOCErrorLine() || count != decodeErrorThreshold {
		t.Fatalf("evidence = (%q, %d), want (%q, %d)", sample, count, hevcPOCErrorLine(), decodeErrorThreshold)
	}
}
