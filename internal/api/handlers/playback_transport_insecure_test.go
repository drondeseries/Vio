package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

func TestResolveVirtualInputRelayRespectsAllowInsecureOptIn(t *testing.T) {
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	h := &PlaybackHandler{
		RemoteStreamRelay: relay,
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			return "http://altmount:8080/stremio/test/play?url=http%3A%2F%2Fprowlarr%3A9696%2F19%2Fdownload", nil
		}),
	}

	t.Run("opt-in off rejects private host", func(t *testing.T) {
		h.AllowInsecureVirtual = nil
		res, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 5, 1, "profile", false, nil, "")
		if err == nil {
			cleanup()
			t.Fatalf("expected private host to be rejected without allow_insecure_http, got relay %q", res.URL)
		}
	})

	t.Run("opt-in on accepts private host", func(t *testing.T) {
		h.AllowInsecureVirtual = func(installationID int) bool { return installationID == 5 }
		res, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 5, 1, "profile", false, nil, "")
		if err != nil {
			t.Fatalf("expected private host to be accepted with allow_insecure_http: %v", err)
		}
		defer cleanup()
		if res.URL == "" {
			t.Fatal("expected a relay URL")
		}
	})

	t.Run("opt-in on for other installation stays strict", func(t *testing.T) {
		h.AllowInsecureVirtual = func(installationID int) bool { return installationID == 5 }
		if _, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 7, 1, "profile", false, nil, ""); err == nil {
			cleanup()
			t.Fatal("expected private host to be rejected for an installation without the opt-in")
		}
	})
}

type fakePinFileResolver struct {
	file        *models.MediaFile
	replacedOld string
	replacedNew string
}

func (r *fakePinFileResolver) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	return r.file, nil
}

func (r *fakePinFileResolver) ReplaceVirtualResultPin(ctx context.Context, fileID int, expectedPath, replacementPath string) (bool, error) {
	r.replacedOld = expectedPath
	r.replacedNew = replacementPath
	if r.file != nil {
		r.file.FilePath = replacementPath
	}
	return true, nil
}

func TestVirtualTranscodeStartupFailsOverOnResolutionError(t *testing.T) {
	attempts := make([]string, 0)
	cache := NewVirtualBestResultCache(time.Minute, 10)
	cacheKey := bestResultCacheKey("content-1", "virtual://series/tt1/1/1", 5)
	cache.set(cacheKey, []VirtualPlaybackStream{
		{URI: "virtual://series/tt1/1/1?result=dead"},
		{URI: "virtual://series/tt1/1/1?result=live"},
	}, time.Now())

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-1",
			FilePath:                   "virtual://series/tt1/1/1?result=dead",
			VirtualOwnerInstallationID: 5,
		},
	}

	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			attempts = append(attempts, virtualURI)
			if virtualURI == "virtual://series/tt1/1/1?result=dead" {
				return "", errors.New("candidate link expired")
			}
			return "http://localhost:8080/stream.mp4", nil
		}),
		VirtualMediaRefreshResolver: VirtualMediaRefreshResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			attempts = append(attempts, virtualURI)
			return "http://localhost:8080/stream.mp4", nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1?result=dead",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-retry-test",
	}

	// startLocalPlaybackTransportOnce runs attempt 0 (which fails at resolve)
	// and attempt 1 (which falls back to neutral URI).
	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d: %#v", len(attempts), attempts)
	}
	if attempts[0] != "virtual://series/tt1/1/1?result=dead" {
		t.Fatalf("attempt 0 = %q, want pinned dead URI", attempts[0])
	}
	if attempts[1] != "virtual://series/tt1/1/1" {
		t.Fatalf("attempt 1 = %q, want fallback neutral URI", attempts[1])
	}

	// Dead candidate must be evicted from the BestResultCache
	cached := cache.get(cacheKey, time.Now())
	for _, c := range cached {
		if c.URI == "virtual://series/tt1/1/1?result=dead" {
			t.Fatalf("dead candidate was not evicted from BestResultCache: %#v", cached)
		}
	}
	foundLive := false
	for _, c := range cached {
		if c.URI == "virtual://series/tt1/1/1?result=live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live candidate was incorrectly evicted from BestResultCache: %#v", cached)
	}
}

func TestVirtualTranscodeEvictsCandidateStoredWithDeviceFingerprint(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 10)
	fingerprint := "device-fp-123"
	cacheKey := bestResultCacheKey("content-fp", "virtual://series/tt1/1/1", 5, fingerprint)
	cache.setWithDetails(cacheKey, "content-fp", "virtual://series/tt1/1/1", 5, []VirtualPlaybackStream{
		{URI: "virtual://series/tt1/1/1?result=dead"},
		{URI: "virtual://series/tt1/1/1?result=live"},
	}, time.Now())

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-fp",
			FilePath:                   "virtual://series/tt1/1/1?result=dead",
			VirtualOwnerInstallationID: 5,
		},
	}

	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			if virtualURI == "virtual://series/tt1/1/1?result=dead" {
				return "", errors.New("candidate link expired")
			}
			return "http://localhost:8080/stream.mp4", nil
		}),
		VirtualMediaRefreshResolver: VirtualMediaRefreshResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			return "http://localhost:8080/stream.mp4", nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1?result=dead",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-fp-test",
	}

	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	cached := cache.get(cacheKey, time.Now())
	for _, c := range cached {
		if c.URI == "virtual://series/tt1/1/1?result=dead" {
			t.Fatalf("dead candidate was not evicted from fingerprint-keyed cache: %#v", cached)
		}
	}
	foundLive := false
	for _, c := range cached {
		if c.URI == "virtual://series/tt1/1/1?result=live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live candidate was incorrectly evicted: %#v", cached)
	}
}

func TestVirtualTranscodeEvictionOwnerExactIsolation(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 10)
	keyOwner5 := bestResultCacheKey("content-iso", "virtual://series/tt1/1/1", 5)
	cache.setWithDetails(keyOwner5, "content-iso", "virtual://series/tt1/1/1", 5, []VirtualPlaybackStream{
		{ID: "cand-same-id", URI: "virtual://series/tt1/1/1?result=cand-same-id"},
	}, time.Now())

	keyOwner7 := bestResultCacheKey("content-iso", "virtual://series/tt1/1/1", 7)
	cache.setWithDetails(keyOwner7, "content-iso", "virtual://series/tt1/1/1", 7, []VirtualPlaybackStream{
		{ID: "cand-same-id", URI: "virtual://series/tt1/1/1?result=cand-same-id"},
	}, time.Now())

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-iso",
			FilePath:                   "virtual://series/tt1/1/1?result=cand-same-id",
			VirtualOwnerInstallationID: 5,
		},
	}

	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			if ownerInstallationID == 5 {
				return "", errors.New("owner 5 link expired")
			}
			return "http://localhost:8080/stream.mp4", nil
		}),
		VirtualMediaRefreshResolver: VirtualMediaRefreshResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			return "http://localhost:8080/stream.mp4", nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1?result=cand-same-id",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-iso-test",
	}

	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	cached5 := cache.get(keyOwner5, time.Now())
	if len(cached5) != 0 {
		t.Fatalf("expected owner 5 entry to be evicted, got %#v", cached5)
	}

	cached7 := cache.get(keyOwner7, time.Now())
	if len(cached7) != 1 || cached7[0].ID != "cand-same-id" {
		t.Fatalf("owner 7 entry was incorrectly evicted by owner 5 failure: %#v", cached7)
	}
}

func TestVirtualTranscodeNeutralResolutionFailureEvictsExactCandidate(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 10)
	cacheKey := bestResultCacheKey("content-1", "virtual://series/tt1/1/1", 5)
	cache.set(cacheKey, []VirtualPlaybackStream{
		{URI: "virtual://series/tt1/1/1?result=broken"},
		{URI: "virtual://series/tt1/1/1?result=live"},
	}, time.Now())
	fileRes := &fakePinFileResolver{file: &models.MediaFile{ID: 10, ContentID: "content-1", FilePath: "virtual://series/tt1/1/1", VirtualOwnerInstallationID: 5}}
	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			if len(excluded) == 0 {
				return ResolvedVirtualMedia{CandidateID: "broken"}, errors.New("candidate unavailable")
			}
			return ResolvedVirtualMedia{}, errors.New("all candidates unavailable")
		}),
	}
	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), playback.TranscodeOpts{MediaFileID: 10, InputPath: "virtual://series/tt1/1/1", VirtualSourceOwnerInstallationID: 5, SessionID: "neutral-exact-eviction"})
	cached := cache.get(cacheKey, time.Now())
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=broken" {
			t.Fatalf("failed concrete candidate remained cached: %#v", cached)
		}
	}
	foundLive := false
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("unrelated live candidate was removed: %#v", cached)
	}
}

func TestVirtualTranscodeStartupFailureEvictsExactCandidate(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewVirtualBestResultCache(time.Minute, 10)
	cacheKey := bestResultCacheKey("content-1", "virtual://series/tt1/1/1", 5)
	cache.set(cacheKey, []VirtualPlaybackStream{
		{URI: "virtual://series/tt1/1/1?result=broken"},
		{URI: "virtual://series/tt1/1/1?result=live"},
	}, time.Now())
	fileRes := &fakePinFileResolver{file: &models.MediaFile{ID: 10, ContentID: "content-1", FilePath: "virtual://series/tt1/1/1?result=broken", VirtualOwnerInstallationID: 5}}
	attemptsExcluded := make([][]string, 0)
	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			attemptsExcluded = append(attemptsExcluded, append([]string(nil), excluded...))
			if len(excluded) == 0 {
				return ResolvedVirtualMedia{URL: "http://localhost:8080/broken.mp4", URI: uri, CandidateID: "broken"}, nil
			}
			return ResolvedVirtualMedia{URL: "http://localhost:8080/live.mp4", URI: "virtual://series/tt1/1/1?result=live", CandidateID: "live"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			if strings.Contains(opts.InputPath, "broken.mp4") {
				return nil, errors.New("manifest startup failed")
			}
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), playback.TranscodeOpts{MediaFileID: 10, InputPath: "virtual://series/tt1/1/1?result=broken", VirtualSourceOwnerInstallationID: 5, SessionID: "manifest-exact-eviction", OutputDir: tempDir})
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()
	if len(attemptsExcluded) < 2 || len(attemptsExcluded[1]) != 1 || attemptsExcluded[1][0] != "broken" {
		t.Fatalf("failed candidate was not excluded on retry: %#v", attemptsExcluded)
	}
	cached := cache.get(cacheKey, time.Now())
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=broken" {
			t.Fatalf("failed concrete candidate remained cached: %#v", cached)
		}
	}
	foundLive := false
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("unrelated live candidate was removed: %#v", cached)
	}
}

func TestVirtualTranscodeManifestFailureEvictsExactCandidate(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewVirtualBestResultCache(time.Minute, 10)
	cacheKey := bestResultCacheKey("content-1", "virtual://series/tt1/1/1", 5)
	cache.set(cacheKey, []VirtualPlaybackStream{
		{URI: "virtual://series/tt1/1/1?result=broken"},
		{URI: "virtual://series/tt1/1/1?result=live"},
	}, time.Now())
	fileRes := &fakePinFileResolver{file: &models.MediaFile{ID: 10, ContentID: "content-1", FilePath: "virtual://series/tt1/1/1?result=broken", VirtualOwnerInstallationID: 5}}
	attemptsExcluded := make([][]string, 0)
	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			attemptsExcluded = append(attemptsExcluded, append([]string(nil), excluded...))
			if len(excluded) == 0 {
				return ResolvedVirtualMedia{URL: "http://localhost:8080/broken.mp4", URI: uri, CandidateID: "broken"}, nil
			}
			return ResolvedVirtualMedia{URL: "http://localhost:8080/live.mp4", URI: "virtual://series/tt1/1/1?result=live", CandidateID: "live"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			dir := filepath.Join(tempDir, strings.TrimSuffix(filepath.Base(opts.InputPath), filepath.Ext(opts.InputPath)))
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
			opts.OutputDir = dir
			if strings.Contains(opts.InputPath, "broken.mp4") {
				opts.TargetCodecVideo = "copy"
				session, err := playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
				if err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(opts.OutputDir, "stream.m3u8"), []byte("#EXTM3U\n"), 0644); err != nil {
					return nil, err
				}
				return session, nil
			}
			return playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
		},
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), playback.TranscodeOpts{MediaFileID: 10, InputPath: "virtual://series/tt1/1/1?result=broken", VirtualSourceOwnerInstallationID: 5, SessionID: "manifest-exact-eviction", OutputDir: tempDir})
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()
	if len(attemptsExcluded) < 2 || len(attemptsExcluded[1]) != 1 || attemptsExcluded[1][0] != "broken" {
		t.Fatalf("failed candidate was not excluded on retry: %#v", attemptsExcluded)
	}
	cached := cache.get(cacheKey, time.Now())
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=broken" {
			t.Fatalf("failed concrete candidate remained cached: %#v", cached)
		}
	}
	foundLive := false
	for _, candidate := range cached {
		if candidate.URI == "virtual://series/tt1/1/1?result=live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("unrelated live candidate was removed: %#v", cached)
	}
}

func TestVirtualTranscodeStartupFailsOverAndPinsWinningCandidateOnManifestReady(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewVirtualBestResultCache(time.Minute, 10)

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-1",
			FilePath:                   "virtual://series/tt1/1/1?result=dead",
			VirtualOwnerInstallationID: 5,
		},
	}

	var attemptsExcluded [][]string
	var refreshedURI string
	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
			attemptsExcluded = append(attemptsExcluded, append([]string(nil), excludedCandidateIDs...))
			if virtualURI == "virtual://series/tt1/1/1?result=dead" {
				return ResolvedVirtualMedia{CandidateID: "dead"}, errors.New("stream 404")
			}
			if forceRefresh && len(excludedCandidateIDs) == 0 {
				refreshedURI = virtualURI
				return ResolvedVirtualMedia{
					URL:            "http://localhost:8080/stream-refreshed.mp4",
					URI:            virtualURI,
					CandidateID:    "live-winner",
					RequestHeaders: map[string]string{"Referer": "https://stream.example/renewed"},
				}, nil
			}
			return ResolvedVirtualMedia{
				URL:            "http://localhost:8080/stream.mp4",
				URI:            "virtual://series/tt1/1/1?result=live-winner",
				CandidateID:    "live-winner",
				RequestHeaders: map[string]string{"Referer": "https://stream.example/"},
			}, nil
		}),
		StartTranscodeFunc: func(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1?result=dead",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-winner-pin-test",
		OutputDir:                        tempDir,
	}

	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed: %v", err)
	}
	defer func() { _ = session.Close() }()

	if len(attemptsExcluded) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(attemptsExcluded))
	}
	// Attempt 0 should have no exclusions
	if len(attemptsExcluded[0]) != 0 {
		t.Fatalf("attempt 0 should have no exclusions, got %#v", attemptsExcluded[0])
	}
	// Attempt 1 should exclude "dead"
	if len(attemptsExcluded[1]) == 0 || attemptsExcluded[1][0] != "dead" {
		t.Fatalf("attempt 1 did not exclude 'dead': got %#v", attemptsExcluded[1])
	}

	// Winning candidate must be CAS-pinned to the database row
	if fileRes.replacedOld != "virtual://series/tt1/1/1?result=dead" || fileRes.replacedNew != "virtual://series/tt1/1/1?result=live-winner" {
		t.Fatalf("CAS pin update mismatch: got old=%q new=%q, want old=dead new=live-winner", fileRes.replacedOld, fileRes.replacedNew)
	}
	if fileRes.file.FilePath != "virtual://series/tt1/1/1?result=live-winner" {
		t.Fatalf("file.FilePath = %q, want live-winner", fileRes.file.FilePath)
	}

	// Verify RefreshInput renews the exact live-winner candidate
	refreshInput := session.Opts().RefreshInput
	if refreshInput == nil {
		t.Fatal("session.Opts().RefreshInput is nil")
	}
	renewedURL, cleanup, err := refreshInput(context.Background())
	if err != nil {
		t.Fatalf("RefreshInput error: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if renewedURL != "http://localhost:8080/stream-refreshed.mp4" {
		t.Fatalf("renewedURL = %q, want refreshed stream", renewedURL)
	}
	if refreshedURI != "virtual://series/tt1/1/1?result=live-winner" {
		t.Fatalf("refreshedURI = %q, want winning candidate URI", refreshedURI)
	}
}

func TestVirtualTranscodeStartupAllFailuresUnpinsToNeutral(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 10)

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-1",
			FilePath:                   "virtual://series/tt1/1/1?result=dead",
			VirtualOwnerInstallationID: 5,
		},
	}

	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{CandidateID: "dead"}, errors.New("stream 404")
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1?result=dead",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-unpin-test",
	}

	_, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err == nil {
		t.Fatal("expected start failure when all attempts fail")
	}

	// When all attempts fail transport start, dead pin must be cleared to neutral
	if fileRes.replacedOld != "virtual://series/tt1/1/1?result=dead" || fileRes.replacedNew != "virtual://series/tt1/1/1" {
		t.Fatalf("failed transcode unpin: got old=%q new=%q", fileRes.replacedOld, fileRes.replacedNew)
	}
}

func TestVirtualTranscodeStartupExcludesCandidateOnNeutralResolveError(t *testing.T) {
	cache := NewVirtualBestResultCache(time.Minute, 10)

	fileRes := &fakePinFileResolver{
		file: &models.MediaFile{
			ID:                         10,
			ContentID:                  "content-1",
			FilePath:                   "virtual://series/tt1/1/1",
			VirtualOwnerInstallationID: 5,
		},
	}

	var attemptsExcluded [][]string
	h := &PlaybackHandler{
		BestResultCache: cache,
		fileResolver:    fileRes,
		sessionMgr:      playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
			attemptsExcluded = append(attemptsExcluded, append([]string(nil), excludedCandidateIDs...))
			if len(excludedCandidateIDs) == 0 {
				// First attempt fails but returns the candidate ID that failed
				return ResolvedVirtualMedia{CandidateID: "cand-broken-1"}, errors.New("debrid link expired")
			}
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/stream-live.mp4",
				URI:         "virtual://series/tt1/1/1?result=cand-live-2",
				CandidateID: "cand-live-2",
			}, nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID:                      10,
		InputPath:                        "virtual://series/tt1/1/1",
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "sess-neutral-exclusion-test",
	}

	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	if len(attemptsExcluded) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(attemptsExcluded))
	}
	// Attempt 1 must exclude "cand-broken-1" discovered during neutral resolution failure
	if len(attemptsExcluded[1]) == 0 || attemptsExcluded[1][0] != "cand-broken-1" {
		t.Fatalf("attempt 1 did not exclude 'cand-broken-1': got %#v", attemptsExcluded[1])
	}
}

func TestDeviceAwareStickyParity(t *testing.T) {
	h := &PlaybackHandler{}
	key := "content-1"

	tvCandidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=4k-dv", Resolution: "2160p", CodecVideo: "hevc", HDR: "dv"},
		{URI: "virtual://movie/1?result=1080p-sdr", Resolution: "1080p", CodecVideo: "h264"},
	}

	phoneCandidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=1080p-sdr", Resolution: "1080p", CodecVideo: "h264"},
		{URI: "virtual://movie/1?result=4k-dv", Resolution: "2160p", CodecVideo: "hevc", HDR: "dv"},
	}

	// 1. TV plays and pins 4k-dv
	h.pinVirtualSticky(key, "virtual://movie/1?result=4k-dv")

	// 2. SDR Phone requests candidates: SDR device capabilities
	phoneCaps := plugins.DeviceCapabilities{
		CodecsVideo:   []string{"h264"},
		MaxResolution: "1080p",
		HDR:           false,
		DolbyVision:   false,
	}

	// applyVirtualStickyPin should NOT promote the 4k-dv pin for the SDR phone because 1080p-sdr has higher score!
	applied := h.applyVirtualStickyPin(key, "virtual://movie/1?result=4k-dv", phoneCandidates, phoneCaps)
	if applied[0].URI != "virtual://movie/1?result=1080p-sdr" {
		t.Fatalf("sticky pin improperly overrode phone ranking: got %q, want 1080p-sdr", applied[0].URI)
	}

	// 3. 4K DV TV requests candidates: DV TV device capabilities
	tvCaps := plugins.DeviceCapabilities{
		CodecsVideo:   []string{"h264", "hevc"},
		MaxResolution: "2160p",
		HDR:           true,
		DolbyVision:   true,
	}
	appliedTV := h.applyVirtualStickyPin(key, "virtual://movie/1?result=4k-dv", tvCandidates, tvCaps)
	if appliedTV[0].URI != "virtual://movie/1?result=4k-dv" {
		t.Fatalf("sticky pin was not promoted for compatible TV: got %q, want 4k-dv", appliedTV[0].URI)
	}

	// 4. Unknown device (no profile) has no capabilities and uses clean key
	keyClean := bestResultCacheKey("content-1", "virtual://movie/1", 5)
	keyEmptyFingerprint := bestResultCacheKey("content-1", "virtual://movie/1", 5, "")
	if keyClean != keyEmptyFingerprint {
		t.Fatalf("clean key %q != empty fingerprint key %q", keyClean, keyEmptyFingerprint)
	}
}

func TestVirtualInputRelayForwardsHeaders(t *testing.T) {
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	h := &PlaybackHandler{
		RemoteStreamRelay:    relay,
		AllowInsecureVirtual: func(id int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL:            "http://stream.example/media.mp4",
				URI:            virtualURI,
				CandidateID:    "cand-1",
				RequestHeaders: map[string]string{"Referer": "https://stream.example/app"},
			}, nil
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://movie/1", 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI failed: %v", err)
	}
	defer cleanup()

	if res.URL == "" || res.URL == "http://stream.example/media.mp4" {
		t.Fatalf("expected loopback relay URL, got %q", res.URL)
	}
	if res.RequestHeaders["Referer"] != "https://stream.example/app" {
		t.Fatalf("res.RequestHeaders = %#v", res.RequestHeaders)
	}
}
