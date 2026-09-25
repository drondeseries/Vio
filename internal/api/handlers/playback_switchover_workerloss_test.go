package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// routeRequest builds a GET request with a chi route context carrying the
// session_id and segment name params the transcode media routes read.
func routeRequest(target, sessionID, name string, userID int) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if userID != 0 {
		ctx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: userID, Role: "user", TokenType: auth.TokenTypeAccess})
		req = req.WithContext(ctx)
	}
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", sessionID)
	if name != "" {
		routeCtx.URLParams.Add("name", name)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
}

// TestRetainedSwitchoverSegmentDoesNotBypassAuth proves the retained-generation
// fallback serve path runs the same authorization gates as the normal segment
// path: a secure session's retained bytes are refused without an identity and
// for a mismatched owner, and only the owning caller receives them. The fallback
// must not be a hole that serves a predecessor's directory unauthenticated.
func TestRetainedSwitchoverSegmentDoesNotBypassAuth(t *testing.T) {
	const sessionID = "retained-auth-session"

	// A live generation that does not hold the old playlist's segment, so the
	// request must reach the retained fallback.
	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)

	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_9.m4s"), []byte("old-gen-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := playback.NewTranscodeSessionForTest(oldDir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:                        sessionID,
		UserID:                    1,
		ProfileID:                 "profile-1",
		MediaFileID:               91,
		PlayMethod:                playback.PlayTranscode,
		RequireMediaAuthorization: true,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, playback.RetainedGenerationRetention)

	target := "/api/v1/playback/transcode/" + sessionID + "/segment/seg_9.m4s"

	noAuth := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(noAuth, routeRequest(target, sessionID, "seg_9.m4s", 0))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("retained fallback without auth = %d, want 401, body = %s", noAuth.Code, noAuth.Body.String())
	}

	wrongOwner := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(wrongOwner, routeRequest(target, sessionID, "seg_9.m4s", 2))
	if wrongOwner.Code != http.StatusForbidden {
		t.Fatalf("retained fallback for a mismatched owner = %d, want 403, body = %s", wrongOwner.Code, wrongOwner.Body.String())
	}

	owner := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(owner, routeRequest(target, sessionID, "seg_9.m4s", 1))
	if owner.Code != http.StatusOK || owner.Body.String() != "old-gen-bytes" {
		t.Fatalf("owning caller retained fallback = %d %q, want 200 old-gen-bytes", owner.Code, owner.Body.String())
	}
}

// TestRetainedSwitchoverManifestDoesNotBypassAuth proves the retained-generation
// manifest fallback also enforces the normal session authorization: a secure
// session's retained playlist is refused without an identity.
func TestRetainedSwitchoverManifestDoesNotBypassAuth(t *testing.T) {
	const sessionID = "retained-manifest-auth"

	// A live generation whose manifest cannot be built, so the route reaches the
	// retained manifest fallback for the owning caller.
	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)

	oldDir := t.TempDir()
	manifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000000,\nseg_00000.ts\n"
	if err := os.WriteFile(filepath.Join(oldDir, "stream.m3u8"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "seg_00000.ts"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := playback.NewTranscodeSessionForTest(oldDir)

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:                        sessionID,
		UserID:                    1,
		ProfileID:                 "profile-1",
		MediaFileID:               91,
		PlayMethod:                playback.PlayTranscode,
		RequireMediaAuthorization: true,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, live)
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, playback.RetainedGenerationRetention)

	target := "/api/v1/playback/transcode/" + sessionID + "/master.m3u8"

	noAuth := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(noAuth, routeRequest(target, sessionID, "", 0))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("retained manifest fallback without auth = %d, want 401, body = %s", noAuth.Code, noAuth.Body.String())
	}

	wrongOwner := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(wrongOwner, routeRequest(target, sessionID, "", 2))
	if wrongOwner.Code != http.StatusForbidden {
		t.Fatalf("retained manifest fallback for a mismatched owner = %d, want 403, body = %s", wrongOwner.Code, wrongOwner.Body.String())
	}
}

// TestRetainedSwitchoverSegmentMissingFailsDeterministically proves a segment
// neither the live generation nor the retained predecessor holds fails fast with
// the pre-existing 404 rather than hanging or returning stale success once the
// retained fallback is offered.
func TestRetainedSwitchoverSegmentMissingFailsDeterministically(t *testing.T) {
	const sessionID = "retained-missing-session"

	// A copy-video live session whose recovery path resolves no target, so the
	// test does not depend on an ambient host ffmpeg (same shape as the
	// no-retained-generation 404 test).
	liveDir := t.TempDir()
	live, err := playback.NewReadyTranscodeSessionForTesting(liveDir, playback.TranscodeOpts{
		SessionID:        sessionID,
		TargetCodecVideo: "copy",
		SegmentDuration:  2,
	})
	if err != nil {
		t.Fatalf("ready transcode session: %v", err)
	}

	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_00009.m4s"), []byte("old-gen-bytes"), 0o644); err != nil {
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

	// The retained predecessor holds seg_00009; requesting a different missing
	// segment must 404, not hang or 500.
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_00010.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_00010.m4s"},
	))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing segment with a retained predecessor = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

// TestRetainedSwitchoverExpiryNeverServesStaleSuccess proves the retained
// fallback cannot serve a stale generation past its bounded window: once the
// predecessor expires, a request for its segment is no longer answered from the
// retained bytes. This is the handler-level counterpart of the manager's expiry
// reap, closing the "switch worker lost, stale success forever" gap.
func TestRetainedSwitchoverExpiryNeverServesStaleSuccess(t *testing.T) {
	const sessionID = "retained-expiry-session"

	liveDir := t.TempDir()
	live := playback.NewTranscodeSessionForTest(liveDir)

	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, "seg_9.m4s"), []byte("stale-old-bytes"), 0o644); err != nil {
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
	handler.TranscodeManager().RetireTranscodeSessionPredecessor(sessionID, old, 40*time.Millisecond)

	// Inside the window the retained bytes serve.
	inside := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(inside, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_9.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_9.m4s"},
	))
	if inside.Code != http.StatusOK || inside.Body.String() != "stale-old-bytes" {
		t.Fatalf("inside-window retained segment = %d %q, want 200 stale-old-bytes", inside.Code, inside.Body.String())
	}

	// Wait for the entry timer / read reap to remove the retained generation.
	deadline := time.Now().Add(2 * time.Second)
	for handler.TranscodeManager().GetRetainedTranscodeSession(sessionID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("retained generation outlived its bounded window")
		}
		time.Sleep(5 * time.Millisecond)
	}

	after := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(after, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_9.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_9.m4s"},
	))
	if after.Code == http.StatusOK && after.Body.String() == "stale-old-bytes" {
		t.Fatalf("expired retained generation still served stale bytes: %d %q", after.Code, after.Body.String())
	}
}
