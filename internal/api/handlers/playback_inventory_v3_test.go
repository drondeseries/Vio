package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestHandleGetPlaybackInventoryV3(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-inv-test",
		FilePath:       "/media/test.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 3, Codec: "subrip", Language: "eng", Default: true},
		},
	}

	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})

	// Unauthenticated request -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(unauthReq, "session_id", session.ID))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rr.Code)
	}

	// Missing session -> 404
	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	missingReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/missing-session/inventory", nil).WithContext(authCtx)
	rr = httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(missingReq, "session_id", "missing-session"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want 404", rr.Code)
	}

	// Authorized request -> 200 with ETag
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr = httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag header")
	}

	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.SessionID != session.ID {
		t.Errorf("session ID = %q, want %q", inv.SessionID, session.ID)
	}
	if inv.InventoryStatus != "verified" {
		t.Errorf("inventory status = %q, want verified", inv.InventoryStatus)
	}
	if len(inv.AudioTracks) != 2 {
		t.Fatalf("audio tracks = %d, want 2", len(inv.AudioTracks))
	}
	if len(inv.SubtitleInventory) != 1 {
		t.Fatalf("subtitle inventory = %d, want 1", len(inv.SubtitleInventory))
	}

	// Conditional request matching ETag -> 304 Not Modified
	condReq := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	condReq.Header.Set("If-None-Match", etag)
	rrCond := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rrCond, withPlaybackRouteParam(condReq, "session_id", session.ID))
	if rrCond.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", rrCond.Code)
	}
	if rrCond.Body.Len() != 0 {
		t.Fatalf("304 response body must be empty, got %d bytes", rrCond.Body.Len())
	}
}

func TestComputeInventoryRevisionDeterministic(t *testing.T) {
	audio := []playback.AudioInventoryItemV3{
		{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
	}
	subs := []playback.SubtitleInventoryItemV3{
		{TrackID: "sub-1", Codec: "subrip", Language: "eng", Default: true},
	}

	r1 := playback.ComputeInventoryRevisionV3("verified", audio, subs)
	r2 := playback.ComputeInventoryRevisionV3("verified", audio, subs)
	if r1 != r2 {
		t.Fatalf("revision not deterministic: %q != %q", r1, r2)
	}

	rDeclared := playback.ComputeInventoryRevisionV3("declared", audio, subs)
	if rDeclared == r1 {
		t.Fatal("revision must differ when status changes from declared to verified")
	}

	audioUpdated := append(audio, playback.AudioInventoryItemV3{Index: 2, Codec: "ac3", Channels: 6, Language: "fre"})
	rUpdated := playback.ComputeInventoryRevisionV3("verified", audioUpdated, subs)
	if rUpdated == r1 {
		t.Fatal("revision must differ when audio tracks change")
	}
}
