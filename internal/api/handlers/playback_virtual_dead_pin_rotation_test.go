package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// absentSessionPinError mirrors the exact resolver shape: the human message the
// resolver has always returned, wrapped around the sentinel cause so callers can
// distinguish a renumbered/dead anchor from a generic resolve failure.
func absentSessionPinError(candidateID string) error {
	return fmt.Errorf("session-bound virtual candidate %q is no longer listed and candidate rotation was not requested: %w", candidateID, virtuallibrary.ErrSessionBoundCandidateAbsent)
}

// TestHandleStreamRotatesAbsentSessionPin proves the serve path recovers from a
// provider that renumbered or dropped the session's pinned candidate: the first
// resolve refuses with ErrSessionBoundCandidateAbsent, the serve layer retries
// once with a fresh relist and rotation declared, serves the live sibling, and
// commits it to the session binding. Before the fix the same resolve was a hard
// 502 virtual_resolve_failed.
func TestHandleStreamRotatesAbsentSessionPin(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-dead-pin"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("sibling-media"))
	}))
	defer upstream.Close()

	file := &models.MediaFile{ID: 42, ContentID: "movie-dead-pin", FilePath: pinnedURI, VirtualOwnerInstallationID: 7}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowInsecureVirtual = func(int) bool { return true }

	var calls int
	var gotRotate []bool
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		calls++
		rotate := VirtualCandidateRotationAllowed(ctx)
		gotRotate = append(gotRotate, rotate)
		if uri != pinnedURI || ownerInstallationID != 7 || userID != 1 || profileID != "profile-1" {
			t.Fatalf("unexpected resolver arguments: uri=%q owner=%d user=%d profile=%q", uri, ownerInstallationID, userID, profileID)
		}
		if !rotate {
			// Session-bound first attempt: the provider no longer lists the pin.
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		if !forceRefresh || !containsStringExactV3(excludedCandidateIDs, "pinned") {
			t.Fatalf("rotation retry must relist and exclude the dead pin: refresh=%v excluded=%v", forceRefresh, excludedCandidateIDs)
		}
		return ResolvedVirtualMedia{URL: upstream.URL + "/provider/sibling.mp4", URI: siblingURI, CandidateID: "sibling"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	handler.HandleStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "sibling-media" {
		t.Fatalf("body = %q, want sibling-media", rec.Body.String())
	}
	if calls != 2 || len(gotRotate) != 2 || gotRotate[0] || !gotRotate[1] {
		t.Fatalf("resolver calls = %d rotate=%v, want exactly [false true]", calls, gotRotate)
	}
	bound, err := sessionMgr.GetSession(session.ID)
	if err != nil || bound == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if bound.VirtualSourceURI != siblingURI {
		t.Fatalf("session binding = %q, want the rotated sibling %q", bound.VirtualSourceURI, siblingURI)
	}
}

// TestHandleStreamDoesNotRotateOnProviderFailure proves the serve-layer retry is
// narrowly scoped to the absent-pin cause: a generic provider resolve failure
// must keep its 502 and must not spend a misleading rotation attempt.
func TestHandleStreamDoesNotRotateOnProviderFailure(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-provider-down?result=pinned"

	file := &models.MediaFile{ID: 43, ContentID: "movie-provider-down", FilePath: pinnedURI, VirtualOwnerInstallationID: 7}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, errors.New("virtual playback provider returned an unsafe stream URL")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	handler.HandleStream(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (no retry for a provider error)", calls)
	}
	bound, err := sessionMgr.GetSession(session.ID)
	if err != nil || bound == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if bound.VirtualSourceURI != pinnedURI {
		t.Fatalf("session binding = %q, want the unchanged pin %q", bound.VirtualSourceURI, pinnedURI)
	}
}

// TestResolveRehydratedVirtualSourceRotatesAbsentAnchor proves the failure-replan
// rehydration retries the session-bound resolve with rotation declared when the
// anchor is absent from the provider list. Session-bound stays true and the
// retry excludes the dead pin, so only the anchor rotates.
func TestResolveRehydratedVirtualSourceRotatesAbsentAnchor(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-absent"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{ID: 7, ContentID: "movie-replan-absent", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var rotates []bool
	var sessionBound []bool
	var excluded [][]string
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		sessionBound = append(sessionBound, VirtualSessionBinding(ctx))
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling"}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("resolveRehydratedVirtualSourceV3: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved candidate = %q, want the rotated sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
	for i, bound := range sessionBound {
		if !bound {
			t.Fatalf("call %d dropped the session binding; the retry must stay session-bound", i)
		}
	}
	if !containsStringExactV3(excluded[1], "pinned") {
		t.Fatalf("retry exclusions = %v, want the dead pin excluded", excluded[1])
	}
}

// TestResolveRehydratedVirtualSourceDoesNotRotateDisplayRefusal proves the retry
// is scoped to the absent-pin sentinel: the resolver's display-driven
// pinned-candidate refusal (an excluded live pin) is not retried, so a live
// release is never silently swapped.
func TestResolveRehydratedVirtualSourceDoesNotRotateDisplayRefusal(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-display-refusal?result=pinned"
	file := &models.MediaFile{ID: 9, ContentID: "movie-display-refusal", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var calls int
	displayErr := errors.New(`pinned virtual candidate "pinned" is excluded and candidate rotation was not requested`)
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, displayErr
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	_, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, displayErr) {
		t.Fatalf("err = %v, want the original display-driven refusal", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (a display-driven refusal is never retried)", calls)
	}
}

// TestResolveRehydratedVirtualSourceDoesNotRotateProviderError proves the
// rehydration retry does not fire for a generic failure: a provider error is
// returned unchanged without a second resolve.
func TestResolveRehydratedVirtualSourceDoesNotRotateProviderError(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-replan-provider-down?result=pinned"
	file := &models.MediaFile{ID: 8, ContentID: "movie-replan-provider-down", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var calls int
	providerErr := errors.New("virtual playback provider returned an unsafe stream URL")
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, providerErr
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	_, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want the original provider error", err)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (no retry for a provider error)", calls)
	}
}
