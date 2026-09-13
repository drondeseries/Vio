package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// writePlaybackTestFFmpegDecodeFailure writes a fake ffmpeg that emits the
// hardware decoder's fatal HEVC lines to stderr from the first frame and then
// produces a ready manifest, so the real TranscodeSession observes the decode
// failure while the local HLS transport starts normally.
func writePlaybackTestFFmpegDecodeFailure(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-decode-failure.sh")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 20 ]; do echo \"[hevc @ 0x1] Could not find ref with POC $i\" >&2; i=$((i+1)); done\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/init.mp4\"; printf x > \"$out/seg_0.m4s\"; " +
		"printf x > \"$out/seg_1.m4s\"; printf x > \"$out/seg_2.m4s\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:7\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXT-X-MAP:URI=\"init.mp4\"\\n" +
		"#EXTINF:2.0,\\nseg_0.m4s\\n#EXTINF:2.0,\\nseg_1.m4s\\n" +
		"#EXTINF:2.0,\\nseg_2.m4s\\n' > \"$last\" ;;\n" +
		"esac\n" +
		"sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// hardwareDecodeFailureHandler builds a handler whose local transcode executes
// with hw_accel=qsv and whose source is an HEVC file the client cannot direct
// play, so the only viable route is a server HLS transcode.
func hardwareDecodeFailureHandler(t *testing.T) (*PlaybackHandler, *models.MediaFile) {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	source.Container = "mkv"
	source.FilePath = writePlaybackTestMediaFile(t, "movie-hevc.mkv")
	source.CodecVideo = "hevc"
	source.Resolution = "1080p"
	source.Bitrate = 8_000
	source.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", Profile: "Main", Level: 120, Width: 1920, Height: 1080,
		FrameRate: "24000/1001", Bitrate: 8_000, BitDepth: 8,
		VideoRange: "SDR", VideoRangeType: "SDR",
	}}

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: source})
	baseConfig := playbackTestConfig(writePlaybackTestFFmpegDecodeFailure(t), t.TempDir())
	handler.PlaybackConfig = func() config.PlaybackConfig {
		cfg := baseConfig()
		cfg.HWAccel = "qsv"
		return cfg
	}
	stubCopySeekAnchorV3(handler)
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"transcode_enabled": "true", "allow_4k_transcode": "true"}}
	return handler, source
}

// TestHandleReplanPlaybackV3FailureRecoveryFallsBackToSoftwareDecode drives a
// start whose hardware decoder rejects the source, then a failure_recovery
// replan. The replan must keep the server-transcode delivery (not demote it
// before the software decode mode is tried), return a plan marked
// software_video_decode, freeze that mode into the executable recipe, and
// re-spawn ffmpeg with SoftwareVideoDecode set. A second failure_recovery must
// not retry software decode.
func TestHandleReplanPlaybackV3FailureRecoveryFallsBackToSoftwareDecode(t *testing.T) {
	handler, source := hardwareDecodeFailureHandler(t)

	start := v3HandlerStartRequest()
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264"},
		AudioDecodeCodecs: []string{"aac"},
	}
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	if started.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("fixture expected a server transcode start, got %s (%s)", started.PlaybackPlan.Delivery, started.PlaybackPlan.DecisionReason)
	}
	if started.PlaybackPlan.EffectiveRecipe.SoftwareVideoDecode {
		t.Fatal("start plan was already software-decode before any decoder failure")
	}
	if started.PlaybackPlan.EffectiveMediaFileID != source.ID {
		t.Fatalf("start effective file = %d, want %d", started.PlaybackPlan.EffectiveMediaFileID, source.ID)
	}

	// Wait on the observable decode-failure stamp rather than a fixed sleep.
	live := handler.tm.GetTranscodeSession(started.SessionID)
	if live == nil {
		t.Fatal("start registered no live transcode session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !live.IsDecodeFailed() {
		if time.Now().After(deadline) {
			t.Fatal("hardware decode failure was never observed on the live session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Capture the replan's spawn so the execution flag can be asserted.
	var mu sync.Mutex
	var captured []playback.TranscodeOpts
	handler.StartTranscodeFunc = func(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		mu.Lock()
		captured = append(captured, opts)
		mu.Unlock()
		return playback.StartTranscode(ctx, opts)
	}

	recovery := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "software-decode-recovery-0001", FailedPlanID: started.PlaybackPlan.PlanID,
		PlanAttemptID: "software-decode-attempt-0001", PlanAttemptKey: started.PlaybackPlan.PlanAttemptKey,
		AttemptedPlanKeys: []string{started.PlaybackPlan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decode_error"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	recovered := postPlaybackReplanV3(t, handler, started.SessionID, recovery)
	if recovered.Terminal != nil {
		t.Fatalf("failure recovery returned a terminal: %#v", recovered.Terminal)
	}
	if recovered.PlaybackPlan == nil {
		t.Fatal("failure recovery returned no plan")
	}
	if recovered.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("software retry abandoned the server-transcode delivery: %s (%s)", recovered.PlaybackPlan.Delivery, recovered.PlaybackPlan.DecisionReason)
	}
	if !recovered.PlaybackPlan.EffectiveRecipe.SoftwareVideoDecode {
		t.Fatal("recovered plan is not marked software_video_decode")
	}

	record, err := handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.FrozenRecipe.SoftwareVideoDecode {
		t.Fatal("frozen executable recipe did not carry software_video_decode")
	}
	caps := record.NormalizedRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3]
	if !caps.Enabled || caps.FailureReason == demoteDeliveryReasonV3 {
		t.Fatalf("HLS delivery was demoted before the software decode retry: %+v", caps)
	}

	mu.Lock()
	if len(captured) == 0 {
		mu.Unlock()
		t.Fatal("replan did not spawn a replacement transcode")
	}
	last := captured[len(captured)-1]
	mu.Unlock()
	if !last.SoftwareVideoDecode {
		t.Fatal("replacement transcode was not spawned with SoftwareVideoDecode")
	}

	// A second failure recovery must not loop back into software decode. The
	// software variant is now the exhausted plan, so the delivery demotes and
	// the hardware plan must not be reselected either.
	second := recovery
	second.ReplanRequestID = "software-decode-recovery-0002"
	second.FailedPlanID = recovered.PlaybackPlan.PlanID
	second.PlanAttemptID = "software-decode-attempt-0002"
	second.PlanAttemptKey = recovered.PlaybackPlan.PlanAttemptKey
	second.AttemptedPlanKeys = []string{recovered.PlaybackPlan.PlanAttemptKey}
	second.AttemptCount = 2
	second.SelectedTracks = recovered.PlaybackPlan.SelectedTracks
	secondReplan := postPlaybackReplanV3(t, handler, started.SessionID, second)
	if secondReplan.Terminal != nil && strings.TrimSpace(secondReplan.Terminal.Detail) == "" {
		t.Fatalf("exhausted recovery terminal has an empty detail: %#v", secondReplan.Terminal)
	}
	if secondReplan.PlaybackPlan != nil {
		if secondReplan.PlaybackPlan.EffectiveRecipe.SoftwareVideoDecode {
			t.Fatal("second failure recovery retried software decode")
		}
		if secondReplan.PlaybackPlan.Delivery == playback.DeliveryTranscodeHLSV3 {
			t.Fatalf("second failure recovery reselected a server transcode instead of honoring the demotion: %#v", secondReplan.PlaybackPlan)
		}
	}
	mu.Lock()
	softwareSpawns := 0
	for _, opts := range captured {
		if opts.SoftwareVideoDecode {
			softwareSpawns++
		}
	}
	mu.Unlock()
	if softwareSpawns != 1 {
		t.Fatalf("software decode spawned %d times, want exactly one", softwareSpawns)
	}
}
