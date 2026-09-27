package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A successful subtitle in-place degrade replans the same release with the
// subtitle dropped. It is a replacement input, so it must carry the selected
// source's inventory provenance rather than dropping it to empty.
func TestDegradeStartSubtitleInPlaceV3KeepsInventoryProvenance(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	file := v3HandlerFixtureFile(t)
	req := v3HandlerStartRequest()

	_, result, _, ok := handler.degradeStartSubtitleInPlaceV3(
		httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil).WithContext(newAuthorizedPlaybackContext()),
		req, file, file, 0, playback.PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true}, ProbeProvenanceDeclared,
	)
	if !ok {
		t.Fatal("subtitle in-place degrade did not produce a playable plan")
	}
	if result.Plan == nil {
		t.Fatalf("degraded result = %#v, want a plan", result)
	}
	if result.Plan.InventoryProvenance != string(ProbeProvenanceDeclared) {
		t.Fatalf("degraded provenance = %q, want declared carried from the selected source", result.Plan.InventoryProvenance)
	}
}

// A failed subtitle degrade reports no plan; the provenance must not leak onto
// the caller's original terminal result through a successful plan either.
func TestDegradeStartSubtitleInPlaceV3FailureLeavesProvenanceToCaller(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "false"}}
	file := v3HandlerFixtureFile(t)
	// A bitrate far above the client cap with transcoding disabled makes the
	// degraded plan refuse, so the helper reports false and the caller keeps its
	// own result.
	file.Bitrate = 80_000
	req := v3HandlerStartRequest()
	req.Capabilities.MaxResolution = "480p"
	req.Capabilities.VideoDecode[0].MaxWidth = 854
	req.Capabilities.VideoDecode[0].MaxHeight = 480

	_, result, _, ok := handler.degradeStartSubtitleInPlaceV3(
		httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil).WithContext(newAuthorizedPlaybackContext()),
		req, file, file, 0, playback.PlannerSettingsV3{TranscodeEnabled: false}, ProbeProvenanceDeclared,
	)
	if ok {
		t.Fatalf("degrade unexpectedly succeeded on a refused plan: %#v", result)
	}
	if result.Plan != nil {
		t.Fatalf("failed degrade returned a plan %#v, want none", result.Plan)
	}
}

// A successful audio in-place degrade replans the same release with another
// audio track. It too is a replacement input and must keep the selected
// source's provenance.
func TestDegradeStartAudioInPlaceV3KeepsInventoryProvenance(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig("", t.TempDir())
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
	}))
	file := v3HandlerFixtureFile(t)
	// Track 0 is dts, which this aac-only client cannot render, so the in-place
	// resolution picks track 1 (aac) and yields a playable plan.
	file.AudioTracks = []models.AudioTrack{
		{Index: 0, Codec: "dts", Channels: 6, Layout: "5.1", Language: "eng", Default: true},
		{Index: 1, Codec: "aac", Channels: 2, Layout: "stereo", Language: "spa"},
	}
	file.CodecAudio = "dts"
	req := v3HandlerStartRequest()
	req.QualityPreference = "auto"
	req.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}
	req.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}

	_, _, result, _, ok := handler.degradeStartAudioInPlaceV3(
		httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil).WithContext(newAuthorizedPlaybackContext()),
		req, file, file, 0, playback.PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true}, ProbeProvenancePending,
	)
	if !ok {
		t.Fatal("audio in-place degrade did not produce a playable plan")
	}
	if result.Plan == nil {
		t.Fatalf("degraded result = %#v, want a plan", result)
	}
	if result.Plan.InventoryProvenance != string(ProbeProvenancePending) {
		t.Fatalf("degraded provenance = %q, want pending carried from the selected source", result.Plan.InventoryProvenance)
	}
}
