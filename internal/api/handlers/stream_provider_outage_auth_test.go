package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// streamAuthContext builds a request context carrying the given authenticated
// account and profile, the way the API middleware would.
func streamAuthContext(userID int, profileID string) context.Context {
	ctx := apimw.SetClaims(context.Background(), &auth.Claims{UserID: userID, Role: "user", TokenType: auth.TokenTypeAccess})
	return apimw.SetProfileID(ctx, profileID)
}

// TestStreamOutageRetryPathDoesNotBypassAuth proves the outage-retry resolve on
// the stream fallback runs behind the same authorization gates as the normal
// stream path: an unauthenticated request is refused before any provider resolve
// (the retry path cannot be a token/auth bypass), and a mismatched-profile
// request is refused after the session is loaded.
func TestStreamOutageRetryPathDoesNotBypassAuth(t *testing.T) {
	const pinned = "virtual://movie/tt-outage-auth?result=cand-a"
	row := providerOutageRow(301, pinned)
	row.ResolvedURL = ""

	resolves := 0
	file := &models.MediaFile{ID: row.ID, ContentID: row.ContentID, FilePath: pinned, VirtualOwnerInstallationID: 5}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, &storedURLStreamFileResolver{row: row})
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolves++
		return ResolvedVirtualMedia{}, providerEmptyListing()
	})

	// Unauthenticated: the route must 401 without touching the provider.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated outage-path request = %d, want 401, body = %s", rr.Code, rr.Body.String())
	}
	if resolves != 0 {
		t.Fatalf("provider resolves = %d, want 0: auth must gate the outage retry", resolves)
	}

	// Wrong profile: refused by the owning-profile gate after the session loads,
	// again before any resolve is attempted.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(streamAuthContext(1, "profile-2"))
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr = httptest.NewRecorder()
	handler.HandleStream(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("mismatched-profile outage-path request = %d, want 403, body = %s", rr.Code, rr.Body.String())
	}
	if resolves != 0 {
		t.Fatalf("provider resolves = %d, want 0: profile ownership must gate the outage retry", resolves)
	}
}
