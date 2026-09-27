package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The requested/effective split must survive every response surface. A start
// that substituted the served candidate row publishes the requested id on
// requested_media_file_id and the served row on effective_media_file_id and
// source.media_file_id; the live session follows the served row, and the
// durable attempt record keeps the requested id. Its idempotent replay returns
// the same split rather than re-resolving it.
func TestHandleStartPlaybackV3ReplayKeepsRequestedEffectiveSplit(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 150
	source.FilePath = "virtual://movie/source-150"
	source.VirtualOwnerInstallationID = 5
	servedCandidateID := source.ID + 1000

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualCandidateFileLookup = func(_ context.Context, path, _ string, _ string, _ int) (*models.MediaFile, error) {
		candidate := *source
		candidate.ID = servedCandidateID
		candidate.FilePath = path
		candidate.Container = "mp4"
		return &candidate, nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	body := marshalV3StartRequest(t, start)

	recorder := httptest.NewRecorder()
	handler.HandleStartPlayback(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(body)).WithContext(newAuthorizedPlaybackContext()))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// Replay the same attempt: the idempotency lookup must return the stored
	// response unchanged, so the split cannot drift on a retry.
	replay := httptest.NewRecorder()
	handler.HandleStartPlayback(replay, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(body)).WithContext(newAuthorizedPlaybackContext()))
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != recorder.Body.String() {
		t.Fatalf("replay body diverged from the original:\nfirst=%s\nreplay=%s", recorder.Body.String(), replay.Body.String())
	}

	var response playback.DecisionResponseV3
	if err := json.Unmarshal(replay.Body.Bytes(), &response); err != nil {
		t.Fatalf("replay response invalid: %v", err)
	}
	if response.PlaybackPlan == nil {
		t.Fatalf("replay response has no playback plan: %#v", response)
	}
	if response.PlaybackPlan.RequestedMediaFileID != source.ID ||
		response.PlaybackPlan.EffectiveMediaFileID != servedCandidateID ||
		response.PlaybackPlan.Source.MediaFileID != servedCandidateID {
		t.Fatalf("replayed plan identity = requested %d effective %d source %d, want requested %d effective/source %d",
			response.PlaybackPlan.RequestedMediaFileID, response.PlaybackPlan.EffectiveMediaFileID,
			response.PlaybackPlan.Source.MediaFileID, source.ID, servedCandidateID)
	}

	record, err := handler.PlanStoreV3.GetAttemptByPlaybackAttemptID(context.Background(), start.PlaybackAttemptID)
	if err != nil {
		t.Fatalf("attempt lookup failed: %v", err)
	}
	if record.RequestedMediaFileID != source.ID || record.EffectiveMediaFileID != servedCandidateID {
		t.Fatalf("attempt identity = requested %d effective %d, want requested %d effective %d",
			record.RequestedMediaFileID, record.EffectiveMediaFileID, source.ID, servedCandidateID)
	}
}
