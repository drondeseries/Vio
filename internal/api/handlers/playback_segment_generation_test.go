package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A generation-scoped segment request that a matching retained generation
// cannot answer is terminal: the live generation is not the stream the URL
// named, so neither waiting on it nor restarting it can produce servable bytes.
// The handler must reject with the stale-generation verdict instead of running
// the missing-segment recovery against the live generation.
func TestTranscodeSegmentStaleRetainedGenerationIsTerminal(t *testing.T) {
	const sessionID = "stale-retained-session"

	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)
	if err := os.WriteFile(filepath.Join(liveDir, "seg_0.m4s"), []byte("new-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The displaced generation holds seg_5 but never produced the seg_9 the
	// stale playlist asks for.
	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_5.m4s"), []byte("old-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := playback.NewTranscodeSessionForTest(oldDir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, playback.RetainedGenerationRetention)

	// The URL names the retained generation, which does not own the segment.
	oldToken := old.GenerationToken()
	if live.MatchesGenerationToken(oldToken) {
		t.Fatal("test setup: live generation unexpectedly matches the retained token")
	}

	const segmentName = "seg_9.m4s"
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(oldToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d (stale generation); body = %s", rec.Code, http.StatusPreconditionFailed, rec.Body.String())
	}
}
