package playback

import (
	"strings"
	"testing"
)

// TestBuildFFmpegArgs_AudioTrackIndexIsAudioOnlyOrdinal verifies the transcode
// builder passes TranscodeOpts.AudioTrackIndex through to ffmpeg's `0:a:N`
// specifier verbatim. The API layer is responsible for converting the selected
// track's absolute container stream index to this audio-only ordinal before it
// reaches TranscodeOpts; the builder must never re-derive it from the container
// layout.
func TestBuildFFmpegArgs_AudioTrackIndexIsAudioOnlyOrdinal(t *testing.T) {
	tests := []struct {
		name            string
		audioTrackIndex int
		wantMap         string
	}{
		{name: "default first audio", audioTrackIndex: -1, wantMap: "0:a:0?"},
		{name: "first audio", audioTrackIndex: 0, wantMap: "0:a:0?"},
		{name: "second audio", audioTrackIndex: 1, wantMap: "0:a:1?"},
		{name: "third audio", audioTrackIndex: 2, wantMap: "0:a:2?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildFFmpegArgs(TranscodeOpts{
				InputPath:        "/media/movie.mkv",
				OutputDir:        "/tmp/out",
				SessionID:        "session-audio-ordinal",
				SourceVideoCodec: "h264",
				TargetCodecVideo: "h264",
				TargetCodecAudio: "aac",
				SegmentDuration:  2,
				HWAccel:          "none",
				AudioTrackIndex:  tt.audioTrackIndex,
			})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-map "+tt.wantMap) {
				t.Fatalf("args missing -map %s: %s", tt.wantMap, joined)
			}
		})
	}
}
