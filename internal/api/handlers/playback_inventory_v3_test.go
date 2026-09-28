package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestPlaybackInventoryPublishesEffectiveVersionIdentity pins that the live
// inventory response names the effective version the transport is bound to, so
// a client polling it can re-key menus without waiting for a replan.
func TestPlaybackInventoryPublishesEffectiveVersionIdentity(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-identity",
		FilePath:       "/media/identity.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})

	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.EffectiveMediaFileID != file.ID {
		t.Fatalf("effective_media_file_id = %d, want %d", inv.EffectiveMediaFileID, file.ID)
	}
	if inv.EffectiveVirtualURI != "" {
		t.Fatalf("effective_virtual_uri = %q, want empty for a local file", inv.EffectiveVirtualURI)
	}
}

// TestPlaybackInventoryFollowsRotatedVirtualSource pins the failover case: after
// a serve-layer rotation the session's binding names release B while the row the
// session id was planned against is still release A. The response must name B
// and publish B's own declared inventory — never A's tracks — even though B has
// not been probed yet.
func TestPlaybackInventoryFollowsRotatedVirtualSource(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now()
	releaseA := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-rotation",
		FilePath:       "virtual://movie/rotation?result=A",
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	// Release B is unprobed: it carries declared provider metadata only.
	releaseB := &models.MediaFile{
		ID:           200,
		ContentID:    "movie-rotation",
		FilePath:     "virtual://movie/rotation?result=B",
		AudioTracks:  []models.AudioTrack{{Index: 1, Codec: "eac3", Language: "deu"}},
		VideoTracks:  []models.VideoTrack{{Codec: "hevc", Width: 1920, Height: 1080}},
		Resolution:   "1080p",
		CodecVideo:   "hevc",
		CodecAudio:   "eac3",
		Container:    "mkv",
		ProviderGUID: "guid-b",
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: releaseA})
	h.VirtualFileLookup = func(_ context.Context, path string) (*models.MediaFile, error) {
		if strings.TrimSpace(path) == releaseB.FilePath {
			return releaseB, nil
		}
		return nil, nil
	}
	if err := sessionMgr.SetVirtualSource(session.ID, releaseB.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	authCtx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	authCtx = apimw.SetProfileID(authCtx, "profile-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v2/playback/"+session.ID+"/inventory", nil).WithContext(authCtx)
	rr := httptest.NewRecorder()
	h.HandleGetPlaybackInventoryV3(rr, withPlaybackRouteParam(req, "session_id", session.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var inv playback.PlaybackInventoryV3
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if inv.EffectiveMediaFileID != releaseB.ID {
		t.Fatalf("effective_media_file_id = %d, want the rotated release %d", inv.EffectiveMediaFileID, releaseB.ID)
	}
	if inv.EffectiveVirtualURI != releaseB.FilePath {
		t.Fatalf("effective_virtual_uri = %q, want %q", inv.EffectiveVirtualURI, releaseB.FilePath)
	}
	if inv.InventoryStatus != "declared" {
		t.Fatalf("inventory_status = %q, want declared for an unprobed sibling", inv.InventoryStatus)
	}
	if len(inv.AudioTracks) != 1 || inv.AudioTracks[0].Language != "deu" {
		t.Fatalf("audio tracks = %#v, want release B's deu track only", inv.AudioTracks)
	}
	for _, track := range inv.AudioTracks {
		if track.Language == "eng" {
			t.Fatal("the previous release's audio track leaked into the rotated inventory")
		}
	}
}

// TestPublishSourceCommittedEmitsEffectiveVersion proves the commit-time push
// names the effective version and its declared inventory on the session's
// realtime connection.
func TestPublishSourceCommittedEmitsEffectiveVersion(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	file := &models.MediaFile{
		ID:          100,
		ContentID:   "movie-commit",
		FilePath:    "virtual://movie/commit?result=A",
		AudioTracks: []models.AudioTrack{{Index: 1, Codec: "aac", Language: "eng"}},
	}
	h := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	h.PublishSourceCommitted(context.Background(), session.ID)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	if event.Name != playback.RealtimeEventSourceCommitted {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventSourceCommitted)
	}
	var payload playback.SourceCommittedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.SessionID != session.ID || payload.EffectiveMediaFileID != file.ID {
		t.Fatalf("payload identity = %#v, want session %q file %d", payload, session.ID, file.ID)
	}
	if payload.EffectiveVirtualURI != file.FilePath {
		t.Fatalf("payload effective_virtual_uri = %q, want %q", payload.EffectiveVirtualURI, file.FilePath)
	}
	if len(payload.AudioTracks) != 1 || payload.AudioTracks[0].Language != "eng" {
		t.Fatalf("payload audio tracks = %#v, want the declared eng track", payload.AudioTracks)
	}
}

type sourceCommittedTestConn struct {
	messages []any
}

func (c *sourceCommittedTestConn) WriteJSON(v any) error {
	c.messages = append(c.messages, v)
	return nil
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

	// Delimiter collision test: titles containing colons must not produce identical digests
	audioCol1 := []playback.AudioInventoryItemV3{
		{Title: "a:b", EmbeddedTitle: "c", TrackID: "1"},
	}
	audioCol2 := []playback.AudioInventoryItemV3{
		{Title: "a", EmbeddedTitle: "b:c", TrackID: "1"},
	}
	rCol1 := playback.ComputeInventoryRevisionV3("verified", audioCol1, subs)
	rCol2 := playback.ComputeInventoryRevisionV3("verified", audioCol2, subs)
	if rCol1 == rCol2 {
		t.Fatalf("delimiter collision: %q == %q", rCol1, rCol2)
	}
}
