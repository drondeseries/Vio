package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// providerOutageRow builds a session-bound virtual catalog row that is inside
// the trust window: durable identity plus persisted transport evidence (a
// stored URL and a delivery stamp), the precondition for the bounded outage
// retry.
func providerOutageRow(id int, pinned string) *models.MediaFile {
	delivered := time.Now().Add(-time.Minute)
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "movie-provider-outage",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       new(time.Now().Add(2 * time.Hour)),
		LastDeliveredAt:            &delivered,
		UpdatedAt:                  time.Now(),
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
}

// providerEmptyListing returns the provider's "title not found" shape: a
// successful answer with no candidates. This is what altmount's Stremio
// listing did during the live incident's 40s blackout.
func providerEmptyListing() error {
	return errors.New("no streams available from provider")
}

// provider502 wraps the resolver's transient provider sentinel the way a real
// upstream 5xx does, so callers can branch on resolver.ErrProviderUnavailable.
func provider502() error {
	return fmt.Errorf("%w: streaming provider returned status 502", resolver.ErrProviderUnavailable)
}

// TestVirtualTransportStartupServesStoredURLDuringProviderBlackout is the live
// incident replay: a transport (re)start for an existing session reaches
// getPlaybackMedia, and the provider listing is empty while the row still holds
// a persisted URL inside the trust window. The startup must serve the stored
// URL (zero provider resolves) instead of failing the transport.
func TestVirtualTransportStartupServesStoredURLDuringProviderBlackout(t *testing.T) {
	tempDir := t.TempDir()
	pinned := "virtual://movie/tt-outage-stored?result=pinned"
	row := providerOutageRow(101, pinned)

	calls := 0
	h := &PlaybackHandler{
		fileResolver:      &fakePinFileResolver{file: row},
		VirtualFileLookup: storedURLLookup(row),
		sessionMgr:        playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			calls++
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      101,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "outage-stored",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed during provider blackout: %v", err)
	}
	defer func() { _ = session.Close() }()
	if calls != 0 {
		t.Fatalf("provider resolves = %d, want 0: the stored URL must serve without a listing", calls)
	}
}

// TestVirtualTransportStartupRetriesTransientProviderFailure proves the bounded
// retry half: the provider fails twice with a 502 then recovers, and the
// startup serves the recovered candidate instead of returning an error. The
// row is inside the trust window so the retry is authorized.
func TestVirtualTransportStartupRetriesTransientProviderFailure(t *testing.T) {
	tempDir := t.TempDir()
	pinned := "virtual://movie/tt-outage-retry?result=pinned"
	row := providerOutageRow(102, pinned)

	// No stored URL: force the provider resolve path so the retry is exercised.
	row.ResolvedURL = ""

	calls := 0
	h := &PlaybackHandler{
		fileResolver:                &fakePinFileResolver{file: row},
		VirtualFileLookup:           storedURLLookup(row),
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		sessionMgr:                  playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			calls++
			if calls <= 2 {
				return ResolvedVirtualMedia{}, provider502()
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/recovered.mp4", URI: pinned, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      102,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "outage-retry",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed after the provider recovered: %v", err)
	}
	defer func() { _ = session.Close() }()
	// Exactly the initial attempt plus the two bounded retries: the failover
	// loop's neutral substitution must not have run.
	if calls != 3 {
		t.Fatalf("provider resolves = %d, want exactly 3 (initial + 2 bounded retries), not a failover substitution", calls)
	}
}

// TestVirtualTransportStartupProviderOutageIsRetryable503 proves the final
// classification: a provider that stays dead yields the retryable
// provider_unavailable classification (not a bare permanent failure), so the
// client keeps retrying the release it picked.
func TestVirtualTransportStartupProviderOutageIsRetryable503(t *testing.T) {
	pinned := "virtual://movie/tt-outage-dead?result=pinned"
	row := providerOutageRow(103, pinned)
	row.ResolvedURL = ""

	trusted := virtualCandidateTrustedForOutageRetry(row, 720*time.Hour)
	if !trusted {
		t.Fatal("row inside the trust window with delivery evidence must be outage-retry trusted")
	}
	err := classifyVirtualProviderOutage(provider502(), trusted)
	if !errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("classified error = %v, want errVirtualProviderUnavailable", err)
	}
	if !virtualProviderListingOutage(err) {
		t.Fatalf("classified error = %v, want it still recognized as a provider outage", err)
	}

	// A row outside the window must not be classified as retryable: it keeps
	// the pre-existing resolve failure.
	untrusted := providerOutageRow(104, pinned)
	untrusted.UpdatedAt = time.Now().Add(-30 * 24 * time.Hour)
	untrustedTrusted := virtualCandidateTrustedForOutageRetry(untrusted, 720*time.Hour)
	if untrustedTrusted {
		t.Fatal("a row outside the trust window must not be outage-retry trusted")
	}
	if errors.Is(classifyVirtualProviderOutage(provider502(), untrustedTrusted), errVirtualProviderUnavailable) {
		t.Fatal("a row outside the trust window must not be classified as a retryable provider outage")
	}
}

// TestHandleStreamProviderOutageAnswersProviderUnavailable proves the serve
// layer answers 503 provider_unavailable for a trusted session-bound row when
// the provider listing stays empty, instead of the 502 virtual_resolve_failed
// that would be read as a permanent release failure.
func TestHandleStreamProviderOutageAnswersProviderUnavailable(t *testing.T) {
	const pinned = "virtual://movie/tt-serve-outage?result=cand-a"
	row := providerOutageRow(105, pinned)
	row.ResolvedURL = ""

	file := &models.MediaFile{
		ID:                         row.ID,
		ContentID:                  row.ContentID,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, &storedURLStreamFileResolver{row: row})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{}, providerEmptyListing()
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 provider_unavailable, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider_unavailable") {
		t.Fatalf("body = %q, want the provider_unavailable error code", rr.Body.String())
	}
}

// TestVirtualTransportStartupRefusesSilentSiblingOnExistingSession proves
// requirement (c): the startup fallback loop for an existing session must not
// silently swap to a different release. A sibling that does not re-match the
// row's durable identity is refused rather than served under the viewer's pin.
func TestVirtualTransportStartupRefusesSilentSiblingOnExistingSession(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-outage-swap"
	pinned := neutral + "?result=pinned"
	sibling := neutral + "?result=sibling"
	row := providerOutageRow(106, pinned)
	row.ResolvedURL = ""

	// The first resolve fails with a genuine absent-pin cause (not a transient
	// outage), so the loop advances to the substitution attempt; that attempt
	// returns a different release with no identity match.
	h := &PlaybackHandler{
		fileResolver:      &fakePinFileResolver{file: row},
		VirtualFileLookup: storedURLLookup(row),
		sessionMgr:        playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if strings.Contains(uri, "result=pinned") {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling.mp4", URI: sibling, CandidateID: "sibling"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	// Bind an existing session so sessionVirtualURI is non-empty.
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", row.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	h.sessionMgr = sessionMgr

	opts := playback.TranscodeOpts{
		MediaFileID:                      106,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        session.ID,
		OutputDir:                        tempDir,
	}
	_, err = h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err == nil {
		t.Fatal("startup served a sibling release; an existing session must refuse a silent swap")
	}
	if !strings.Contains(err.Error(), "different release") {
		t.Fatalf("startup error = %v, want the silent-release-swap refusal", err)
	}
}

// TestVirtualProviderListingOutageClassification pins the outage predicate so a
// future resolver error rename cannot silently disable the retry.
func TestVirtualProviderListingOutageClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"provider sentinel", provider502(), true},
		{"empty listing", providerEmptyListing(), true},
		{"trusted persisted absent", fmt.Errorf("no longer listed: %w", virtuallibrary.ErrPersistedCandidateTrusted), true},
		{"session-bound absent", absentSessionPinError("pinned"), false},
		{"nil", nil, false},
		{"other", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualProviderListingOutage(tc.err); got != tc.want {
				t.Fatalf("virtualProviderListingOutage(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
