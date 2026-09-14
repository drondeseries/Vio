package scanner

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// ffprobe legitimately omits format.bit_rate for some containers. The implied
// size/duration fallback must fill the field, because the playback planner
// requires a known bitrate and the repair trigger would otherwise consider the
// row complete while every play attempt fails forever.
func TestApplyProbeDataDerivesImpliedBitrateWhenProbeOmitsIt(t *testing.T) {
	mf := &models.MediaFile{FileSize: 8_000_000_000}
	applyProbeData(mf, &ProbeData{Duration: 7200}, "local")

	if mf.Bitrate != 8_888 {
		t.Fatalf("bitrate = %d kbps, want implied 8888 from size and duration", mf.Bitrate)
	}
	if mf.Duration != 7200 {
		t.Fatalf("duration = %d, want the probed duration", mf.Duration)
	}
}

func TestApplyProbeDataKeepsReportedBitrate(t *testing.T) {
	mf := &models.MediaFile{FileSize: 8_000_000_000}
	applyProbeData(mf, &ProbeData{Duration: 7200, Bitrate: 5_500}, "local")

	if mf.Bitrate != 5_500 {
		t.Fatalf("bitrate = %d kbps, want the reported 5500", mf.Bitrate)
	}
}

func TestNeedsCriticalProbeRepair_ProbedVideoMissingBitrateRepairsOnce(t *testing.T) {
	now := time.Now()
	f := &models.MediaFile{
		ProbeSource:    "local",
		ProbeUpdatedAt: &now,
		FileSize:       8_000_000_000,
		Duration:       7200,
		Container:      "mkv",
		CodecVideo:     "hevc",
		Resolution:     "1080p",
		VideoTracks:    []models.VideoTrack{{Codec: "hevc", ColorRange: "tv"}},
		Chapters:       []models.MediaChapter{},
	}

	if !NeedsCriticalProbeRepair(f) {
		t.Fatal("a probed video without any bitrate must be reprobed; the planner refuses such rows forever while this check calls them complete")
	}

	f.Bitrate = 8_888
	if NeedsCriticalProbeRepair(f) {
		t.Fatal("a converged bitrate must not trigger another repair probe")
	}
}

// Without a known size the implied-bitrate fallback could never populate a
// value, so flagging the row would re-run ffprobe on every playback decision
// forever instead of converging.
func TestNeedsCriticalProbeRepair_VideoMissingBitrateWithoutKnownSizeDoesNotLoop(t *testing.T) {
	now := time.Now()
	f := &models.MediaFile{
		ProbeSource:    "local",
		ProbeUpdatedAt: &now,
		Duration:       7200,
		Container:      "mkv",
		CodecVideo:     "hevc",
		Resolution:     "1080p",
		VideoTracks:    []models.VideoTrack{{Codec: "hevc", ColorRange: "tv"}},
		Chapters:       []models.MediaChapter{},
	}

	if NeedsCriticalProbeRepair(f) {
		t.Fatal("a video row with unknown size must not be flagged for missing bitrate")
	}
}
