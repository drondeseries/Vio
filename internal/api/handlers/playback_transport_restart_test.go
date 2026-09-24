package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
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
