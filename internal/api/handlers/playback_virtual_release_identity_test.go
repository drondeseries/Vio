package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// releaseIdentityProber returns a usable probed row so a fallback candidate
// that resolves reaches the verified/adopt branch.
func releaseIdentityProber(_ context.Context, _ string, transient *models.MediaFile) (*models.MediaFile, error) {
	return transient, nil
}

// TestFallbackEligibilityEnforcesBeforeResolvingSibling pins blocker 1's
// "enforce before resolving a sibling": a session-bound fallback without
// declared rotation refreshes only its anchored release and never contacts a
// healthy sibling, even when the provider lists one.
func TestFallbackEligibilityEnforcesBeforeResolvingSibling(t *testing.T) {
	const (
		sessionURI = "virtual://movie/tt-enforce?result=session"
		siblingURI = "virtual://movie/tt-enforce?result=sibling"
	)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var resolved []string
	saved := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "sibling", URI: siblingURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolved = append(resolved, path)
			if path == sessionURI {
				return "", errors.New("session credentials rotated")
			}
			return "http://localhost:8080/sibling.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			saved++
			return 1, nil
		},
	}
	file := &models.MediaFile{ID: 30, ContentID: "movie-tt-enforce", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"})
	if got != nil {
		t.Fatalf("fallback returned %#v, want nil without rotation", got)
	}
	for _, path := range resolved {
		if path == siblingURI {
			t.Fatalf("sibling was resolved under a session binding without rotation: %v", resolved)
		}
	}
	if saved != 0 {
		t.Fatalf("fallback persisted %d replacement(s), want 0", saved)
	}
	if !strings.Contains(logs.String(), "refusing to substitute a different release") {
		t.Fatalf("refusal was not logged: %s", logs.String())
	}
}

// TestFallbackRefreshesSameReleaseWithoutRotation pins "a stale URL may refresh
// within the same release": the anchored candidate re-resolves, keeps its
// release identity, and never persists a replacement.
func TestFallbackRefreshesSameReleaseWithoutRotation(t *testing.T) {
	const sessionURI = "virtual://movie/tt-refresh?result=session"
	var resolved []string
	saved := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "sibling", URI: "virtual://movie/tt-refresh?result=sibling"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolved = append(resolved, path)
			return "http://localhost:8080/session.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			saved++
			return 1, nil
		},
	}
	file := &models.MediaFile{ID: 31, ContentID: "movie-tt-refresh", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"})
	if got == nil {
		t.Fatal("fallback returned nil, want the refreshed session release")
	}
	if id := virtualResultCandidateID(got.URI); id != "session" {
		t.Fatalf("refreshed release identity = %q, want session", id)
	}
	if len(resolved) != 1 || resolved[0] != sessionURI {
		t.Fatalf("resolved paths = %v, want only the session release", resolved)
	}
	if saved != 0 {
		t.Fatalf("a same-release refresh persisted %d replacement(s), want 0", saved)
	}
}

// TestFallbackAudioSubtitleDegradeNeverMovesRelease pins the owner invariant
// from blocker 1 from the fallback side: an audio/subtitle problem on the
// session's release is not an indictment of the video, so the fallback must not
// substitute a sibling release even though one is listed and healthy.
func TestFallbackAudioSubtitleDegradeNeverMovesRelease(t *testing.T) {
	const (
		sessionURI = "virtual://movie/tt-tracks?result=session"
		siblingURI = "virtual://movie/tt-tracks?result=sibling"
	)
	var resolvedSibling bool
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "sibling", URI: siblingURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			if path == siblingURI {
				resolvedSibling = true
			}
			return "", errors.New("the session release did not resolve")
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{
		ID: 32, ContentID: "movie-tt-tracks", FilePath: sessionURI, VirtualOwnerInstallationID: 5,
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Language: "eng"}},
		SubtitleTracks: []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"})
	if got != nil {
		t.Fatalf("fallback returned %#v, want nil: track problems must not move the release", got)
	}
	if resolvedSibling {
		t.Fatal("a sibling release was resolved for an audio/subtitle problem")
	}
}

// TestFallbackFailedPinNotResolved pins blocker 2's "failed pins": an anchored
// candidate with an active failed_at verdict is not re-resolved or adopted.
func TestFallbackFailedPinNotResolved(t *testing.T) {
	const sessionURI = "virtual://movie/tt-failed-pin?result=session"
	failedAt := time.Now().Add(-time.Minute)
	resolverCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "session", URI: sessionURI}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == sessionURI {
				return &models.MediaFile{ID: 40, FilePath: sessionURI, FailedAt: &failedAt}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/session.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{ID: 40, ContentID: "movie-tt-failed-pin", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"})
	if got != nil {
		t.Fatalf("fallback returned %#v for an active failed pin, want nil", got)
	}
	if resolverCalls != 0 {
		t.Fatalf("failed pin was resolved %d time(s), want 0", resolverCalls)
	}
}

// TestFallbackExplicitRetryResolvesFailedPin pins the defined retry policy:
// allowFailed (an explicit user retry / declared rotation) is the only path
// that re-resolves an active failed verdict.
func TestFallbackExplicitRetryResolvesFailedPin(t *testing.T) {
	const sessionURI = "virtual://movie/tt-retry?result=session"
	failedAt := time.Now().Add(-time.Minute)
	resolverCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "session", URI: sessionURI}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == sessionURI {
				return &models.MediaFile{ID: 41, FilePath: sessionURI, FailedAt: &failedAt}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/session.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{ID: 41, ContentID: "movie-tt-retry", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, allowFailed: true, releaseID: "session"})
	if got == nil {
		t.Fatal("explicit retry did not resolve the failed pin")
	}
	if id := virtualResultCandidateID(got.URI); id != "session" {
		t.Fatalf("retried release identity = %q, want session", id)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1 on explicit retry", resolverCalls)
	}
}

// TestFallbackFailedSiblingSkippedHealthyAdoptedWithCAS pins both the failed
// sibling skip and the exact database state of the adopted replacement: the
// healthy sibling is persisted under its own path, CAS-fenced on the original
// row identity.
func TestFallbackFailedSiblingSkippedHealthyAdoptedWithCAS(t *testing.T) {
	const (
		anchorURI  = "virtual://movie/tt-adopt?result=anchor"
		failedURI  = "virtual://movie/tt-adopt?result=failed"
		healthyURI = "virtual://movie/tt-adopt?result=healthy"
	)
	failedAt := time.Now().Add(-time.Minute)
	anchorUpdated := time.Now().Add(-2 * time.Hour).UTC()
	var resolvedPaths []string
	var mu sync.Mutex
	var saved []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "failed", URI: failedURI, Resolution: "1080p"},
				{ID: "healthy", URI: healthyURI, Resolution: "720p"},
			}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == failedURI {
				return &models.MediaFile{ID: 2, FilePath: failedURI, FailedAt: &failedAt}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			mu.Lock()
			resolvedPaths = append(resolvedPaths, path)
			mu.Unlock()
			return "http://localhost:8080/" + virtualResultCandidateID(path) + ".mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			mu.Lock()
			saved = append(saved, args)
			mu.Unlock()
			return 1, nil
		},
	}
	file := &models.MediaFile{
		ID: 42, ContentID: "movie-tt-adopt", FilePath: anchorURI,
		VirtualOwnerInstallationID: 5, MediaFolderID: 9, UpdatedAt: anchorUpdated,
	}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: false})
	if got == nil {
		t.Fatal("fallback returned nil, want the healthy sibling")
	}
	if id := virtualResultCandidateID(got.URI); id != "healthy" {
		t.Fatalf("adopted release = %q, want healthy", id)
	}
	for _, path := range resolvedPaths {
		if path == failedURI {
			t.Fatalf("failed sibling was resolved: %v", resolvedPaths)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved %d rows, want exactly 1", len(saved))
	}
	got0 := saved[0]
	if got0.AdoptPath != healthyURI {
		t.Fatalf("AdoptPath = %q, want healthy sibling %q", got0.AdoptPath, healthyURI)
	}
	if got0.ExpectedFilePath != anchorURI {
		t.Fatalf("ExpectedFilePath = %q, want the CAS anchor %q", got0.ExpectedFilePath, anchorURI)
	}
	if !got0.UpdatedAt.Equal(anchorUpdated) || got0.OwnerID != 5 || got0.LibraryID != 9 || !got0.StampProbe {
		t.Fatalf("persist fence fields = updated_at %v owner %d library %d stamp %v, want snapshot %v/5/9/true",
			got0.UpdatedAt, got0.OwnerID, got0.LibraryID, got0.StampProbe, anchorUpdated)
	}
}

// TestFallbackAllFailedCandidatesReturnNil pins the all-failed case: when every
// listed sibling carries an active verdict the fallback resolves none of them
// and returns nil so the caller preserves its original error.
func TestFallbackAllFailedCandidatesReturnNil(t *testing.T) {
	const (
		anchorURI = "virtual://movie/tt-all-failed?result=anchor"
		firstURI  = "virtual://movie/tt-all-failed?result=one"
		secondURI = "virtual://movie/tt-all-failed?result=two"
	)
	failedAt := time.Now().Add(-time.Minute)
	resolverCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "one", URI: firstURI},
				{ID: "two", URI: secondURI},
			}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == firstURI || path == secondURI {
				return &models.MediaFile{ID: 2, FilePath: path, FailedAt: &failedAt}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/x.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{ID: 43, ContentID: "movie-tt-all-failed", FilePath: anchorURI, VirtualOwnerInstallationID: 5}

	if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: false}); got != nil {
		t.Fatalf("fallback returned %#v, want nil when all candidates are failed", got)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolved %d failed candidate(s), want 0", resolverCalls)
	}
}

// TestFailedVerdictExpiresAfterMaxAge pins the defined automatic expiry: once a
// verdict is older than virtualFailedVerdictMaxAge it no longer excludes the
// candidate from automatic selection, so a stale pin can recover without an
// explicit retry.
func TestFailedVerdictExpiresAfterMaxAge(t *testing.T) {
	previousTTL := virtualFailedVerdictMaxAge
	virtualFailedVerdictMaxAge = time.Minute
	t.Cleanup(func() { virtualFailedVerdictMaxAge = previousTTL })

	const sessionURI = "virtual://movie/tt-expiry?result=session"
	failedAt := time.Now().Add(-2 * time.Hour)
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "session", URI: sessionURI}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == sessionURI {
				return &models.MediaFile{ID: 44, FilePath: sessionURI, FailedAt: &failedAt}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/session.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{ID: 44, ContentID: "movie-tt-expiry", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	if got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"}); got == nil {
		t.Fatal("expired verdict still excluded the candidate, want automatic retry")
	}
}

// TestFallbackVerdictRecheckedAfterResolveForConcurrentFailure pins the
// "no fallback-only bypass" property under concurrent failure recording: a
// verdict recorded after the pre-resolve check but during the provider call is
// still caught on the resolved identity, so the candidate is neither served nor
// adopted.
func TestFallbackVerdictRecheckedAfterResolveForConcurrentFailure(t *testing.T) {
	const sessionURI = "virtual://movie/tt-recheck?result=session"
	failedAt := time.Now()
	var lookups int
	resolverCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "session", URI: sessionURI}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path != sessionURI {
				return nil, nil
			}
			lookups++
			if lookups == 1 {
				return &models.MediaFile{ID: 46, FilePath: sessionURI}, nil // healthy at pre-check
			}
			return &models.MediaFile{ID: 46, FilePath: sessionURI, FailedAt: &failedAt}, nil // failed mid-resolve
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/session.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
	}
	file := &models.MediaFile{ID: 46, ContentID: "movie-tt-recheck", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: false, releaseID: "session"})
	if got != nil {
		t.Fatalf("fallback returned %#v, want nil: a verdict recorded during resolve must block", got)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (the concurrent failure is recorded during this call)", resolverCalls)
	}
	if lookups < 2 {
		t.Fatalf("verdict lookups = %d, want the pre- and post-resolve checks", lookups)
	}
}

// TestFallbackConcurrentPersistenceUsesSnapshotFence pins that the replacement
// adoption is CAS-fenced against concurrent mutation: the saver receives the
// row identity snapshotted before resolution, not the mutated row, so a
// concurrent failure/rotation cannot be overwritten by a stale adoption.
func TestFallbackConcurrentPersistenceUsesSnapshotFence(t *testing.T) {
	const (
		anchorURI  = "virtual://movie/tt-concurrent?result=anchor"
		healthyURI = "virtual://movie/tt-concurrent?result=healthy"
	)
	anchorUpdated := time.Now().Add(-3 * time.Hour).UTC()
	captured := make(chan models.VirtualFilePersistArgs, 1)
	release := make(chan struct{})
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "healthy", URI: healthyURI}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/healthy.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			captured <- args
			<-release
			return 1, nil
		},
	}
	file := &models.MediaFile{
		ID: 45, ContentID: "movie-tt-concurrent", FilePath: anchorURI,
		VirtualOwnerInstallationID: 5, MediaFolderID: 9, UpdatedAt: anchorUpdated,
	}

	type outcome struct {
		resolved *resolvedVirtualPlaybackSource
	}
	done := make(chan outcome, 1)
	go func() {
		done <- outcome{h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
			virtualFallbackEligibility{sessionBound: false})}
	}()

	var args models.VirtualFilePersistArgs
	select {
	case args = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback never persisted the substitute")
	}

	// Concurrent mutation while the save is in flight: a newer failure and a
	// rotated update timestamp. The snapshot already handed to the saver must
	// neither see them nor be able to clobber them (the SQL fence is keyed on
	// the snapshot).
	rotated := time.Now().UTC()
	newFailure := time.Now().UTC()
	file.UpdatedAt = rotated
	file.FailedAt = &newFailure
	file.FilePath = healthyURI
	close(release)

	select {
	case out := <-done:
		if out.resolved == nil {
			t.Fatal("fallback returned nil, want the healthy substitute")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback did not return")
	}

	if !args.UpdatedAt.Equal(anchorUpdated) {
		t.Fatalf("saved UpdatedAt = %v, want the pre-resolution snapshot %v", args.UpdatedAt, anchorUpdated)
	}
	if args.ExpectedFilePath != anchorURI {
		t.Fatalf("saved ExpectedFilePath = %q, want the pre-resolution anchor %q", args.ExpectedFilePath, anchorURI)
	}
	if args.AdoptPath != healthyURI {
		t.Fatalf("saved AdoptPath = %q, want %q", args.AdoptPath, healthyURI)
	}
}
