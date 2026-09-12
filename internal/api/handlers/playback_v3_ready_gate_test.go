package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// blockingTranscodeSpawnV3 installs a StartTranscodeFunc that records the call
// and then parks until the returned channel is closed. It models the slow
// ffmpeg spawn / manifest wait that the eager local-HLS transports pay during
// commit. The locally-servable identity and progressive-remux routes must never
// reach it: their URL is /stream/<session>, served lazily by StreamHandler, so
// the start response must be produced without spawning anything.
func blockingTranscodeSpawnV3(handler *PlaybackHandler) (release chan struct{}, called *atomic.Bool) {
	release = make(chan struct{})
	called = &atomic.Bool{}
	handler.StartTranscodeFunc = func(ctx context.Context, _ playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		called.Store(true)
		select {
		case <-release:
			return nil, errors.New("blocked transport spawn released")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return release, called
}

// startMustNotSpawnTransportV3 runs the start handler and fails if it either
// blocks on the spawn gate or invokes it at all.
func startMustNotSpawnTransportV3(t *testing.T, handler *PlaybackHandler, rr *httptest.ResponseRecorder, req *http.Request, release chan struct{}, called *atomic.Bool) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		handler.HandleStartPlayback(rr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("playback start did not return; it blocked on the local transport readiness gate")
	}
	close(release)
	if called.Load() {
		t.Fatal("locally-servable start invoked the local transcode spawn")
	}
}

func assertLocalStreamRouteV3(t *testing.T, response playback.DecisionResponseV3) {
	t.Helper()
	if response.PlaybackPlan == nil || response.SessionID == "" {
		t.Fatalf("response has no playable session: %#v", response)
	}
	parsed, err := url.Parse(response.PlaybackPlan.Stream.URL)
	if err != nil {
		t.Fatalf("parse stream URL %q: %v", response.PlaybackPlan.Stream.URL, err)
	}
	if parsed.IsAbs() || parsed.Path != "/stream/"+response.SessionID {
		t.Fatalf("stream URL = %q, want API-local /stream/%s route", response.PlaybackPlan.Stream.URL, response.SessionID)
	}
}

// A direct-play start mints /stream/<session> and must return without touching
// the local transcode spawn, even when a spawn is gated shut.
func TestStartPlaybackV3LocalDirectDoesNotWaitForTransportReadiness(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}

	release, called := blockingTranscodeSpawnV3(handler)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, v3HandlerStartRequest()))).WithContext(newAuthorizedPlaybackContext())
	startMustNotSpawnTransportV3(t, handler, rr, req, release, called)

	var response playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &response) != nil {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	if response.PlaybackPlan == nil || response.PlaybackPlan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("plan = %#v, want direct play", response.PlaybackPlan)
	}
	assertLocalStreamRouteV3(t, response)
	if handler.tm.GetTranscodeSession(response.SessionID) != nil {
		t.Fatal("direct-play start registered a local transcode session")
	}
}

// A progressive-remux start (the identity route, not HLS) mints
// /stream/<session?seek=> and must return without spawning the remux ffmpeg,
// which StreamHandler starts lazily on the first request.
func TestStartPlaybackV3LocalProgressiveDoesNotWaitForTransportReadiness(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.Container = "mkv"
	file.FilePath = writePlaybackTestMediaFile(t, "movie.mkv")
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3(nil))
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	stubCopySeekAnchorV3(handler)

	start := v3HandlerStartRequest()
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassProgressiveV3: {Enabled: true, SupportedOnDevice: true},
	}

	release, called := blockingTranscodeSpawnV3(handler)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext())
	startMustNotSpawnTransportV3(t, handler, rr, req, release, called)

	var response playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &response) != nil {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	if response.PlaybackPlan == nil || response.PlaybackPlan.Delivery != playback.DeliveryRemuxProgressiveV3 {
		t.Fatalf("plan = %#v, want progressive remux", response.PlaybackPlan)
	}
	assertLocalStreamRouteV3(t, response)
	if handler.tm.GetTranscodeSession(response.SessionID) != nil {
		t.Fatal("progressive-remux start registered a local transcode session")
	}
}
