package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestTranscodeSegmentServedFromRetainedSwitchoverGeneration replays the
// audio/version-switch dead time: a replan publishes a new generation and
// retires the old one, but the client's existing playlist still requests the
// old generation's segments for a moment. Those requests must be served from
// the retained predecessor (its already-produced bytes) instead of 404/503,
// then the new generation serves its own window.
func TestTranscodeSegmentServedFromRetainedSwitchoverGeneration(t *testing.T) {
	const sessionID = "switchover-overlap-session"

	// New (live) generation: has a manifest but not the old segment yet.
	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)
	if err := os.WriteFile(filepath.Join(liveDir, "seg_0.m4s"), []byte("new-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Displaced (predecessor) generation: holds the segment the old playlist is
	// still fetching.
	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_9.m4s"), []byte("old-gen-bytes"), 0o644); err != nil {
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

	// The old playlist requests a segment the new generation does not hold.
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_9.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_9.m4s"},
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the retained generation, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "old-gen-bytes" {
		t.Fatalf("body = %q, want the retained generation's segment bytes", rec.Body.String())
	}

	// The new generation serves its own window normally.
	rec = httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_0.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_0.m4s"},
	))
	if rec.Code != http.StatusOK || rec.Body.String() != "new-gen-bytes" {
		t.Fatalf("live generation segment: %d %q, want 200 new-gen-bytes", rec.Code, rec.Body.String())
	}
}

// TestTranscodeSegmentWithoutRetainedGenerationStaysNotFound proves the
// fallback is inert outside the overlap: with no retained predecessor, a
// missing segment keeps its pre-existing 404.
//
// The live generation is a copy-video session with no seek anchor available
// for the missing segment, so its recovery path resolves no target and never
// spawns ffmpeg. That keeps the assertion independent of an ambient host
// ffmpeg: an encoded session would fall back to the process-wide ffmpeg
// resolver, whose absence on CI surfaces as an unclassified exec error and a
// 500 instead of the pre-existing 404 this test is about.
func TestTranscodeSegmentWithoutRetainedGenerationStaysNotFound(t *testing.T) {
	const sessionID = "no-retained-session"
	liveDir := t.TempDir()
	live, err := playback.NewReadyTranscodeSessionForTesting(liveDir, playback.TranscodeOpts{
		SessionID:        sessionID,
		TargetCodecVideo: "copy",
		SegmentDuration:  2,
	})
	if err != nil {
		t.Fatalf("ready transcode session: %v", err)
	}

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)

	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_9.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_9.m4s"},
	))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no retained generation, body = %s", rec.Code, rec.Body.String())
	}
}
