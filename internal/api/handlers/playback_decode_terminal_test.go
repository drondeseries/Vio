package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestTranscodeServeReportsDecoderRejectedSource drives a start whose hardware
// decoder rejects the source, then requests the manifest and a segment through
// the live serve handlers. Both must answer a permanent, machine-readable
// decode failure instead of serving a manifest the client can never play (which
// would leave hls.js to exhaust its own recovery budget and report only a
// startup timeout).
func TestTranscodeServeReportsDecoderRejectedSource(t *testing.T) {
	handler, _ := hardwareDecodeFailureHandler(t)
	// Delay the storm past the commit: the start must observe a healthy
	// generation (manifest ready, no verdict) so the rejection under test
	// lands on the committed session whose manifest and segment routes are
	// asserted below. An immediate storm would fail the start itself before
	// anything commits.
	prevConfig := handler.PlaybackConfig
	delayedFFmpeg := writePlaybackTestFFmpegDelayedDecodeFailure(t)
	handler.PlaybackConfig = func() config.PlaybackConfig {
		cfg := prevConfig()
		cfg.FFmpegPath = delayedFFmpeg
		return cfg
	}

	start := v3HandlerStartRequest()
	start.QualityPreference = "auto"
	start.ClientFeatures = append(start.ClientFeatures, playback.FeatureHeaderAuthenticatedMediaV3)
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
		t.Fatalf("fixture expected a server transcode start, got %s", started.PlaybackPlan.Delivery)
	}
	defer handler.tm.CloseTranscodeSession(started.SessionID, "")

	live := handler.tm.GetTranscodeSession(started.SessionID)
	if live == nil {
		t.Fatal("start registered no live transcode session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !live.IsSourceRejected() {
		if time.Now().After(deadline) {
			t.Fatal("the decoder rejection was never observed on the live session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	manifestRecorder := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(manifestRecorder, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+started.SessionID+"/master.m3u8",
		nil,
		map[string]string{"session_id": started.SessionID},
	))
	assertDecodeFailureResponse(t, manifestRecorder)

	segmentRecorder := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(segmentRecorder, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+started.SessionID+"/segment/seg_0.m4s",
		nil,
		map[string]string{"session_id": started.SessionID, "name": "seg_0.m4s"},
	))
	assertDecodeFailureResponse(t, segmentRecorder)
}

func assertDecodeFailureResponse(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
	if got := rr.Header().Get(transcodeDecodeErrorHeader); got != transcodeDecodeErrorCode {
		t.Fatalf("%s = %q, want %q", transcodeDecodeErrorHeader, got, transcodeDecodeErrorCode)
	}
	var resp errorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v; body = %s", err, rr.Body.String())
	}
	if resp.Error != "decode_failed" {
		t.Fatalf("error code = %q, want decode_failed", resp.Error)
	}
}
