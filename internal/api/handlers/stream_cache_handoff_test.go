package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/altmount"
)

// cacheHandoffFileResolver returns the single fixture row by id.
type cacheHandoffFileResolver struct {
	file *models.MediaFile
}

func (r cacheHandoffFileResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if r.file == nil || r.file.ID != id {
		return nil, errors.New("file not found")
	}
	return r.file, nil
}

// cacheProgressRecorder captures download.progress events.
type cacheProgressRecorder struct {
	mu       sync.Mutex
	payloads []playback.DownloadProgressPayload
}

func (r *cacheProgressRecorder) PublishDownloadProgress(_ string, payload playback.DownloadProgressPayload) bool {
	r.mu.Lock()
	r.payloads = append(r.payloads, payload)
	r.mu.Unlock()
	return true
}

func (r *cacheProgressRecorder) states() []playback.DownloadProgressState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]playback.DownloadProgressState, 0, len(r.payloads))
	for _, payload := range r.payloads {
		out = append(out, payload.State)
	}
	return out
}

func newCacheHandoffFixture(t *testing.T, releaseName string) (*StreamHandler, *playback.SessionManager, *playback.Session, *models.MediaFile) {
	t.Helper()
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-tt1",
		FilePath:                   "virtual://movie/tt1?result=cand-a",
		VirtualOwnerInstallationID: 5,
		ProviderReleaseName:        releaseName,
	}
	if err := manager.SetVirtualSource(session.ID, file.FilePath, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler := &StreamHandler{
		sessionMgr:   manager,
		fileResolver: cacheHandoffFileResolver{file: file},
	}
	return handler, manager, session, file
}

// TestHandoffVirtualSessionToCachedRebindsSameRelease proves the happy path:
// the same candidate id is re-resolved, the cached URL is adopted, and the
// session binding moves without changing the release.
func TestHandoffVirtualSessionToCachedRebindsSameRelease(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !forceRefresh {
			t.Error("cache handoff must list afresh")
		}
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/cached",
			URI:                 file.FilePath,
			CandidateID:         "cand-a",
			OwnerID:             5,
			ProviderReleaseName: file.ProviderReleaseName,
		}, nil
	})

	_, cleanup, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if !applied {
		t.Fatal("handoff was not applied")
	}
	if cleanup != nil {
		cleanup()
	}
	current, err := handler.sessionMgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if current.VirtualSourceURI != file.FilePath {
		t.Fatalf("binding = %q, want the pinned URI %q", current.VirtualSourceURI, file.FilePath)
	}
}

// TestHandoffVirtualSessionToCachedRefusesReleaseSwap proves a resolve that
// names a different candidate id is refused rather than silently switching the
// release under the session binding.
func TestHandoffVirtualSessionToCachedRefusesReleaseSwap(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/other",
			URI:                 "virtual://movie/tt1?result=cand-b",
			CandidateID:         "cand-b",
			OwnerID:             5,
			ProviderReleaseName: "Another.Release.2020",
		}, nil
	})

	if _, _, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false); err == nil || applied {
		t.Fatalf("release swap applied=%v err=%v, want refused", applied, err)
	}
	current, _ := handler.sessionMgr.GetSession(session.ID)
	if current.VirtualSourceURI != file.FilePath {
		t.Fatalf("binding moved to %q on a refused swap", current.VirtualSourceURI)
	}
}

// TestHandoffVirtualSessionToCachedRefusesUnverifiablePin proves a session with
// no concrete pinned candidate id is never handed off: a force-refresh resolve
// ranks candidates anew, so committing one would silently substitute another
// release under a neutral row's session binding.
func TestHandoffVirtualSessionToCachedRefusesUnverifiablePin(t *testing.T) {
	handler, manager, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	const neutral = "virtual://movie/tt1"
	file.FilePath = neutral
	if err := manager.SetVirtualSource(session.ID, neutral, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !forceRefresh {
			t.Error("cache handoff must list afresh")
		}
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/cached",
			URI:                 neutral + "?result=cand-b",
			CandidateID:         "cand-b",
			OwnerID:             5,
			ProviderReleaseName: "Another.Release.2020",
		}, nil
	})

	if _, _, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false); err == nil || applied {
		t.Fatalf("unverifiable handoff applied=%v err=%v, want refused", applied, err)
	}
	current, _ := handler.sessionMgr.GetSession(session.ID)
	if current.VirtualSourceURI != neutral {
		t.Fatalf("binding moved to %q on a refused unverifiable handoff", current.VirtualSourceURI)
	}
}

// TestHandoffVirtualSessionToCachedHonorsGenerationFence proves a binding move
// that lands while the handoff is resolving leaves the newer binding in place.
func TestHandoffVirtualSessionToCachedHonorsGenerationFence(t *testing.T) {
	handler, manager, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return true, true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		// Simulate a concurrent rotation that lands mid-resolve.
		if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt1?result=cand-b", 5); err != nil {
			t.Errorf("concurrent SetVirtualSource: %v", err)
		}
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/cached",
			URI:                 file.FilePath,
			CandidateID:         "cand-a",
			OwnerID:             5,
			ProviderReleaseName: file.ProviderReleaseName,
		}, nil
	})

	if _, _, applied, err := handler.handoffVirtualSessionToCached(context.Background(), session, file, false); err != nil || applied {
		t.Fatalf("fenced handoff applied=%v err=%v, want false/nil", applied, err)
	}
	current, _ := handler.sessionMgr.GetSession(session.ID)
	if current.VirtualSourceURI != "virtual://movie/tt1?result=cand-b" {
		t.Fatalf("binding = %q, want the newer binding", current.VirtualSourceURI)
	}
}

// TestBeginVirtualCacheHandoffStreamsDownloadProgress proves pressing play on a
// pinned release asks the provider for a fill and reports queued ->
// downloading progress to the realtime publisher.
func TestBeginVirtualCacheHandoffStreamsDownloadProgress(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	recorder := &cacheProgressRecorder{}
	handler.DownloadProgress = recorder
	done := make(chan struct{})
	handler.VirtualReleaseCacheFiller = VirtualReleaseCacheFillFunc(func(_ context.Context, virtualURI string, ownerID int, report func(playback.DownloadProgressPayload)) error {
		if virtualURI != file.FilePath || ownerID != 5 {
			t.Errorf("filler args = (%q, %d), want (%q, 5)", virtualURI, ownerID, file.FilePath)
		}
		report(playback.DownloadProgressPayload{State: playback.DownloadProgressStateDownloading, Bytes: 50, TotalBytes: 100})
		close(done)
		return nil
	})

	release := handler.beginVirtualCacheHandoff(context.Background(), session, file, ResolvedVirtualMedia{
		URI:                 file.FilePath,
		OwnerID:             5,
		ProviderReleaseName: file.ProviderReleaseName,
	})
	defer release()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cache fill did not run")
	}
	// Allow the detached goroutine's progress callback to settle.
	deadline := time.Now().Add(2 * time.Second)
	for {
		states := recorder.states()
		if len(states) >= 2 {
			if states[0] != playback.DownloadProgressStateQueued || states[1] != playback.DownloadProgressStateDownloading {
				t.Fatalf("progress states = %v, want queued then downloading", states)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress states = %v, want queued then downloading", states)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandleVirtualReleaseConfirmedRebindsWaiter proves the monitor signal
// hands a registered, still-pinned session off to its cached copy and reports
// completion.
func TestHandleVirtualReleaseConfirmedRebindsWaiter(t *testing.T) {
	handler, _, session, file := newCacheHandoffFixture(t, "Movie.2024.1080p.WEB-DL")
	recorder := &cacheProgressRecorder{}
	handler.DownloadProgress = recorder
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL:                 "http://127.0.0.1:9/cached",
			URI:                 file.FilePath,
			CandidateID:         "cand-a",
			OwnerID:             5,
			ProviderReleaseName: file.ProviderReleaseName,
		}, nil
	})

	release := handler.beginVirtualCacheHandoff(context.Background(), session, file, ResolvedVirtualMedia{
		URI:                 file.FilePath,
		OwnerID:             5,
		ProviderReleaseName: file.ProviderReleaseName,
	})
	defer release()

	handler.HandleVirtualReleaseConfirmed(altmount.ReleaseKey(file.ProviderReleaseName))

	// The handler dispatches the handoff asynchronously so the observer callback
	// stays non-blocking; wait for the completion event before asserting.
	waitForCondition(t, func() bool {
		states := recorder.states()
		return len(states) > 0 && states[len(states)-1] == playback.DownloadProgressStateCompleted
	}, "release-confirmed handoff did not report completion")

	current, _ := handler.sessionMgr.GetSession(session.ID)
	if current.VirtualSourceURI != file.FilePath {
		t.Fatalf("binding = %q, want the pinned URI", current.VirtualSourceURI)
	}
	states := recorder.states()
	if len(states) == 0 || states[len(states)-1] != playback.DownloadProgressStateCompleted {
		t.Fatalf("progress states = %v, want a completed event", states)
	}
}
