package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestVirtualTransportRestartServesPersistedURLWithoutProviderCall replays the
// live incident: a mid-session HLS seek restart (before_start_segment) re-resolves
// the session's pinned virtual candidate. The catalog row carries a perfectly
// good persisted provider URL inside the trust window, but the provider's current
// listing renumbered its ?result= id. Before the fix the restart force-refreshed,
// discarded the stored URL, hit ErrPersistedCandidateTrusted/ErrSessionBoundCandidateAbsent,
// and every segment request 500'd until hls.js gave up. The restart must serve
// the persisted URL with zero provider calls.
func TestVirtualTransportRestartServesPersistedURLWithoutProviderCall(t *testing.T) {
	tempDir := t.TempDir()
	pinned := "virtual://movie/tt-restart-stored?result=pinned"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                         91,
		ContentID:                  "movie-restart-stored",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
		UpdatedAt:                  time.Now(),
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
	calls := 0
	h := &PlaybackHandler{
		fileResolver:      &fakePinFileResolver{file: row},
		VirtualFileLookup: storedURLLookup(row),
		sessionMgr:        playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "pinned",
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      91,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-stored",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	refresh := session.Opts().RefreshInput
	if refresh == nil {
		t.Fatal("session.Opts().RefreshInput is nil")
	}
	refreshed, cleanup, err := refresh(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if refreshed != row.ResolvedURL {
		t.Fatalf("refreshed URL = %q, want the persisted provider URL %q", refreshed, row.ResolvedURL)
	}
	if calls != 0 {
		t.Fatalf("provider resolve calls = %d, want 0 (the stored URL must be served without a listing)", calls)
	}
}

// TestVirtualTransportRestartRotatesRenumberedSameRelease proves the retry half of
// the restart recovery: when the stored URL is unusable and the pinned id is
// absent from the provider's current listing, the restart retries once with a
// fresh relist, rotation declared, and the row's durable identity threaded, and
// accepts the same release re-identified under a new result id.
func TestVirtualTransportRestartRotatesRenumberedSameRelease(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-restart-rotate"
	pinned := neutral + "?result=pinned"
	renumbered := neutral + "?result=renumbered"
	row := &models.MediaFile{
		ID:                         92,
		ContentID:                  "movie-restart-rotate",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
	phase := "startup"
	var rotates, refreshes []bool
	var excluded [][]string
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
			if phase == "startup" {
				return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/pinned.mp4", URI: pinned, CandidateID: "pinned"}, nil
			}
			rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
			refreshes = append(refreshes, forceRefresh)
			excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
			if !VirtualCandidateRotationAllowed(ctx) {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			return ResolvedVirtualMedia{
				URL: "http://127.0.0.1:9/renumbered.mp4", URI: renumbered, CandidateID: "renumbered",
				IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
			}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      92,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-rotate",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()
	phase = "restart"

	refreshed, cleanup, err := session.Opts().RefreshInput(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if refreshed != "http://127.0.0.1:9/renumbered.mp4" {
		t.Fatalf("refreshed URL = %q, want the renumbered same-release URL", refreshed)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
	if len(refreshes) != 2 || refreshes[0] || !refreshes[1] {
		t.Fatalf("relist intents = %v, want the first attempt stored-first and only the retry forced", refreshes)
	}
	if !containsStringExactV3(excluded[1], "pinned") {
		t.Fatalf("retry exclusions = %v, want the absent pin excluded", excluded[1])
	}
}

// TestVirtualTransportRestartRefusesSiblingRelease proves the restart retry never
// silently swaps an already-planned session onto different bytes: when the rotated
// candidate is a different release (no identity rematch and no matching durable
// identity), RefreshInput returns the original absent-pin failure.
func TestVirtualTransportRestartRefusesSiblingRelease(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-restart-swap"
	pinned := neutral + "?result=pinned"
	sibling := neutral + "?result=sibling"
	row := &models.MediaFile{
		ID:                         93,
		ContentID:                  "movie-restart-swap",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
	phase := "startup"
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if phase == "startup" {
				return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/pinned.mp4", URI: pinned, CandidateID: "pinned"}, nil
			}
			if !VirtualCandidateRotationAllowed(ctx) {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			// A genuinely different release: no identity rematch and a
			// mismatched durable identity.
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling.mp4", URI: sibling, CandidateID: "sibling", ProviderVideoHash: "hash-b"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      93,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-swap",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()
	phase = "restart"

	_, _, err = session.Opts().RefreshInput(context.Background())
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("RefreshInput err = %v, want the absent-pin cause, not a silent sibling restart", err)
	}
}

// TestVirtualTransportRestartReusesPinnedRelay proves the #158 fast path: once a
// virtual session owns a registered relay, an in-place segment restart renews
// its input from that pinned relay without any provider call. Before the fix
// every mid-session restart re-listed the provider and re-registered a new
// relay, so each random seek paid a provider round trip and hung past 2s.
func TestVirtualTransportRestartReusesPinnedRelay(t *testing.T) {
	tempDir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("pinned-media"))
	}))
	defer upstream.Close()

	neutral := "virtual://movie/tt-relay-reuse"
	pinned := neutral + "?result=pinned"
	row := &models.MediaFile{
		ID:                         94,
		ContentID:                  "movie-relay-reuse",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
	var calls int32
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		RemoteStreamRelay: func() *remotestream.Relay {
			r := remotestream.NewRelay()
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			return r
		}(),
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			atomic.AddInt32(&calls, 1)
			return ResolvedVirtualMedia{URL: upstream.URL + "/provider/pinned.mp4", URI: uri, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      94,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-relay-reuse",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	startupCalls := atomic.LoadInt32(&calls)
	startupInput := session.Opts().InputPath
	if !strings.Contains(startupInput, "/source/") {
		t.Fatalf("startup input = %q, want a registered relay URL", startupInput)
	}

	refreshed, cleanup, err := session.Opts().RefreshInput(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if cleanup != nil {
		t.Fatalf("reuse must not hand back a cleanup for a relay the session already owns")
	}
	if refreshed != startupInput {
		t.Fatalf("refreshed URL = %q, want the pinned relay %q", refreshed, startupInput)
	}
	if got := atomic.LoadInt32(&calls); got != startupCalls {
		t.Fatalf("provider resolve calls = %d, want the startup count %d (restart must reuse the pinned relay)", got, startupCalls)
	}

	// A second restart keeps reusing: the relay is still live and no provider
	// call is allowed.
	if again, _, err := session.Opts().RefreshInput(context.Background()); err != nil || again != startupInput {
		t.Fatalf("second restart refreshed = %q err = %v, want the pinned relay", again, err)
	}
	if got := atomic.LoadInt32(&calls); got != startupCalls {
		t.Fatalf("provider resolve calls after second restart = %d, want %d", got, startupCalls)
	}
}

// TestVirtualTransportRestartWithoutRelayKeepsResolvePath pins the scope of the
// reuse fast path: it applies only once a relay is registered. Without a relay
// there is no pinned transport to renew, so the restart keeps the stored-first
// resolve path (and its provider re-list) unchanged.
func TestVirtualTransportRestartWithoutRelayKeepsResolvePath(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-no-relay"
	pinned := neutral + "?result=pinned"
	row := &models.MediaFile{
		ID:                         96,
		ContentID:                  "movie-no-relay",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
	}
	var calls int32
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		// No RemoteStreamRelay: the resolved provider URL is used directly.
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			atomic.AddInt32(&calls, 1)
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/pinned.mp4", URI: uri, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      96,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-no-relay",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	startupCalls := atomic.LoadInt32(&calls)
	if _, _, err := session.Opts().RefreshInput(context.Background()); err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got == startupCalls {
		t.Fatalf("provider resolve calls = %d, want the stored-first path to resolve without a relay", got)
	}
}

// TestPinnedRelayRegistrationClearIfVersionIgnoresStaleFailure pins the #158
// clobber guard: a failure that observed pin version V must not clear a
// replacement installed after it. It is the reason the pin carries a version
// rather than clearing unconditionally.
func TestPinnedRelayRegistrationClearIfVersionIgnoresStaleFailure(t *testing.T) {
	var pin pinnedRelayRegistration
	pin.set("http://relay-a/source/a", "virtual://movie/a")
	_, _, staleVersion := pin.snapshot()

	// A concurrent restart installs a replacement.
	pin.set("http://relay-b/source/b", "virtual://movie/b")

	if pin.clearIfVersion(staleVersion) {
		t.Fatal("a stale failure cleared the replacement pin")
	}
	url, identity, currentVersion := pin.snapshot()
	if url != "http://relay-b/source/b" || identity != "virtual://movie/b" {
		t.Fatalf("pin = %q/%q, want the replacement", url, identity)
	}
	if currentVersion == staleVersion {
		t.Fatal("setting a replacement did not advance the pin version")
	}

	if !pin.clearIfVersion(currentVersion) {
		t.Fatal("the current failure did not clear its own pin")
	}
	if url, identity, _ := pin.snapshot(); url != "" || identity != "" {
		t.Fatalf("pin = %q/%q, want cleared", url, identity)
	}
}

// TestVirtualTransportRestartRenewsRelayAfterUpstreamAuthRejection proves the
// observed-401/403 path: once a request through the pinned relay is refused by
// the upstream, the next restart clears the pin and forces a fresh provider
// listing to renew the signed URL instead of replaying the refusal.
func TestVirtualTransportRestartRenewsRelayAfterUpstreamAuthRejection(t *testing.T) {
	tempDir := t.TempDir()
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	renewed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("renewed-media"))
	}))
	defer renewed.Close()

	neutral := "virtual://movie/tt-relay-renew"
	pinned := neutral + "?result=pinned"
	row := &models.MediaFile{
		ID:                         97,
		ContentID:                  "movie-relay-renew",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
	}
	var calls int32
	var forceRefreshes []bool
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		RemoteStreamRelay: func() *remotestream.Relay {
			r := remotestream.NewRelay()
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			return r
		}(),
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			atomic.AddInt32(&calls, 1)
			forceRefreshes = append(forceRefreshes, forceRefresh)
			if forceRefresh {
				return ResolvedVirtualMedia{URL: renewed.URL + "/provider/renewed.mp4", URI: uri, CandidateID: "pinned"}, nil
			}
			return ResolvedVirtualMedia{URL: rejecting.URL + "/provider/rejected.mp4", URI: uri, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      97,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-relay-renew",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	startupInput := session.Opts().InputPath
	if !strings.Contains(startupInput, "/source/") {
		t.Fatalf("startup input = %q, want a registered relay URL", startupInput)
	}

	// Drive one request through the pinned relay so the upstream 401 is
	// observed and the registration is marked rejected.
	response, err := http.Get(startupInput)
	if err != nil {
		t.Fatalf("request pinned relay: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatalf("pinned relay request status = %d, want the upstream refusal surfaced", response.StatusCode)
	}

	refreshed, cleanup, err := session.Opts().RefreshInput(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if refreshed == startupInput {
		t.Fatal("restart reused a relay whose upstream rejected credentials")
	}
	if cleanup == nil {
		t.Fatal("renewal must hand back a cleanup for the new relay registration")
	}
	defer cleanup()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider resolve calls = %d, want 2 (startup + forced renewal)", got)
	}
	if len(forceRefreshes) != 2 || forceRefreshes[0] || !forceRefreshes[1] {
		t.Fatalf("force-refresh flags = %v, want [false true]", forceRefreshes)
	}

	// The renewed pin is live and must be reused without another provider call.
	if again, _, err := session.Opts().RefreshInput(context.Background()); err != nil || again != refreshed {
		t.Fatalf("post-renewal restart refreshed = %q err = %v, want the renewed relay %q", again, err, refreshed)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider resolve calls after renewal reuse = %d, want 2", got)
	}
}

// TestVirtualTransportRestartResolvesStoredFirstWhenPinnedRelayGone proves a
// pin whose registration is no longer live (expired or evicted) is cleared and
// the restart falls through to the stored-first resolve, then re-pins the
// replacement for the next restart.
func TestVirtualTransportRestartResolvesStoredFirstWhenPinnedRelayGone(t *testing.T) {
	tempDir := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("stored-first-media"))
	}))
	defer upstream.Close()

	neutral := "virtual://movie/tt-relay-gone"
	pinned := neutral + "?result=pinned"
	row := &models.MediaFile{
		ID:                         98,
		ContentID:                  "movie-relay-gone",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
	}
	var pinnedStatus = remotestream.RegistrationAbsent
	var calls int32
	var forceRefreshes []bool
	h := &PlaybackHandler{
		fileResolver: &fakePinFileResolver{file: row},
		sessionMgr:   playback.NewSessionManager(0, 0),
		RemoteStreamRelay: func() *remotestream.Relay {
			r := remotestream.NewRelay()
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			return r
		}(),
		AllowPrivateStreams: func(int) bool { return true },
		RelayRegistrationStatus: func(string) remotestream.RegistrationStatus {
			return pinnedStatus
		},
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			atomic.AddInt32(&calls, 1)
			forceRefreshes = append(forceRefreshes, forceRefresh)
			return ResolvedVirtualMedia{URL: upstream.URL + "/provider/" + strconv.Itoa(int(atomic.LoadInt32(&calls))) + ".mp4", URI: uri, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      98,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "restart-relay-gone",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()
	startupInput := session.Opts().InputPath

	refreshed, cleanup, err := session.Opts().RefreshInput(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if refreshed == startupInput {
		t.Fatal("a gone pin was reused")
	}
	if cleanup == nil {
		t.Fatal("the stored-first resolve must hand back a cleanup for its new relay")
	}
	defer cleanup()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider resolve calls = %d, want 2 (startup + stored-first miss)", got)
	}
	if len(forceRefreshes) != 2 || forceRefreshes[0] || forceRefreshes[1] {
		t.Fatalf("force-refresh flags = %v, want [false false] (a gone pin resolves stored-first)", forceRefreshes)
	}

	// Once the replacement is live it is reused, proving the miss re-pinned.
	pinnedStatus = remotestream.RegistrationLive
	if again, _, err := session.Opts().RefreshInput(context.Background()); err != nil || again != refreshed {
		t.Fatalf("post-repin restart refreshed = %q err = %v, want the replacement %q", again, err, refreshed)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider resolve calls after re-pin reuse = %d, want 2", got)
	}
}
