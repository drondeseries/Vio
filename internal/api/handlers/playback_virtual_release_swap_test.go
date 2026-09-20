package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
)

// TestFallbackResolveStaleVirtualSourceRefusesSessionReleaseSwapWithoutRotation
// pins the no-silent-swap invariant for the stale-source fallback: a
// session-bound candidate whose rotation was not declared must not be replaced
// by a sibling release. The fallback retries the session's own candidate once,
// and when that fails it refuses and reports why instead of substituting.
func TestFallbackResolveStaleVirtualSourceRefusesSessionReleaseSwapWithoutRotation(t *testing.T) {
	const (
		sessionURI    = "virtual://movie/tt-swap?result=session"
		substituteURI = "virtual://movie/tt-swap?result=substitute"
	)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var resolvedPaths []string
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "substitute", URI: substituteURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolvedPaths = append(resolvedPaths, path)
			if path == sessionURI {
				return "", errors.New("provider dropped the pinned release")
			}
			return "http://localhost:8080/substitute.mp4", nil
		}),
	}
	file := &models.MediaFile{ID: 10, ContentID: "movie-tt-swap", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	ctx := context.Background()

	result := h.fallbackResolveStaleVirtualSource(ctx, file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	if result != nil {
		t.Fatalf("fallback returned %#v, want nil (no silent release swap)", result)
	}
	if len(resolvedPaths) != 1 || resolvedPaths[0] != sessionURI {
		t.Fatalf("resolved paths = %v, want only the session candidate %q", resolvedPaths, sessionURI)
	}
	if !strings.Contains(logs.String(), "refusing to substitute a different release") {
		t.Fatalf("refusal reason was not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), sessionURI) {
		t.Fatalf("refusal log does not name the session candidate: %s", logs.String())
	}
}

// TestFallbackResolveStaleVirtualSourceReusesSessionCandidate proves the
// positive fallback for a stale session candidate: when the chosen release
// still resolves it is reused (refreshing stale credentials), and no sibling is
// ever contacted under a session binding without declared rotation.
func TestFallbackResolveStaleVirtualSourceReusesSessionCandidate(t *testing.T) {
	const (
		sessionURI    = "virtual://movie/tt-reuse?result=session"
		substituteURI = "virtual://movie/tt-reuse?result=substitute"
	)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var resolvedPaths []string
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "substitute", URI: substituteURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolvedPaths = append(resolvedPaths, path)
			return "http://localhost:8080/session.mp4", nil
		}),
	}
	file := &models.MediaFile{ID: 11, ContentID: "movie-tt-reuse", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	ctx := context.Background()

	result := h.fallbackResolveStaleVirtualSource(ctx, file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	if result == nil {
		t.Fatal("fallback returned nil, want the re-resolved session candidate")
	}
	if got := virtualResultCandidateID(result.URI); got != "session" {
		t.Fatalf("resolved candidate = %q, want the session candidate", got)
	}
	if len(resolvedPaths) != 1 || resolvedPaths[0] != sessionURI {
		t.Fatalf("resolved paths = %v, want only %q", resolvedPaths, sessionURI)
	}
	if !strings.Contains(logs.String(), "re-resolved the session-bound candidate") {
		t.Fatalf("session-candidate reuse was not logged: %s", logs.String())
	}
}

// TestFallbackResolveStaleVirtualSourceSkipsFailedSubstitute proves the
// fallback never selects a candidate whose failed verdict is still active. The
// first sibling carries a live failed_at stamp; with rotation declared the
// fallback must skip it and resolve the healthy sibling instead.
func TestFallbackResolveStaleVirtualSourceSkipsFailedSubstitute(t *testing.T) {
	const (
		sessionURI = "virtual://movie/tt-failed?result=session"
		failedURI  = "virtual://movie/tt-failed?result=failed"
		healthyURI = "virtual://movie/tt-failed?result=healthy"
	)
	failedStamp := time.Now().Add(-time.Hour)
	var resolvedPaths []string
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "failed", URI: failedURI, Resolution: "1080p"},
				{ID: "healthy", URI: healthyURI, Resolution: "720p"},
			}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == failedURI {
				return &models.MediaFile{ID: 2, FilePath: failedURI, FailedAt: &failedStamp}, nil
			}
			return nil, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			resolvedPaths = append(resolvedPaths, path)
			return "http://localhost:8080/" + virtualResultCandidateID(path) + ".mp4", nil
		}),
	}
	file := &models.MediaFile{ID: 12, ContentID: "movie-tt-failed", FilePath: sessionURI, VirtualOwnerInstallationID: 5}

	// Rotation declared: the fallback is allowed to substitute a sibling.
	ctx := context.Background()

	result := h.fallbackResolveStaleVirtualSource(ctx, file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: true})
	if result == nil {
		t.Fatal("fallback returned nil, want the healthy substitute")
	}
	if got := virtualResultCandidateID(result.URI); got != "healthy" {
		t.Fatalf("resolved candidate = %q, want the healthy sibling", got)
	}
	for _, path := range resolvedPaths {
		if path == failedURI {
			t.Fatalf("fallback resolved the failed candidate: %v", resolvedPaths)
		}
	}
}

// TestVirtualCandidateFailureLogNamesAttemptedCandidateNotSubstitute pins the
// log-attribution fix: when the resolver substitutes a sibling for the
// candidate being tried and that sibling is marked failed, the failure entry
// for the attempted candidate must name the attempted candidate, not only the
// substitute it collapsed to.
func TestVirtualCandidateFailureLogNamesAttemptedCandidateNotSubstitute(t *testing.T) {
	const (
		originalURI   = "virtual://movie/tt-log?result=original"
		substituteURI = "virtual://movie/tt-log?result=substitute"
	)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	failedStamp := time.Now().Add(-time.Hour)
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID: "substitute", URI: substituteURI,
				Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac",
			}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == substituteURI {
				return &models.MediaFile{ID: 2, FilePath: substituteURI, FailedAt: &failedStamp}, nil
			}
			return nil, nil
		},
		// Mirror the detailed resolver's dedup/keep behavior: the attempted
		// candidate collapses to the substitute named by the provider list.
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/substitute.mp4",
				URI:         substituteURI,
				CandidateID: "substitute",
			}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/stream?path=" + path, nil
		}),
	}
	file := &models.MediaFile{ID: 13, ContentID: "movie-tt-log", FilePath: originalURI, VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)

	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false, virtualResolveOptionsV3{sessionBound: true}); err == nil {
		t.Fatal("expected the marked-failed substitute to fail the resolve")
	}

	type failureEntry struct {
		Msg          string `json:"msg"`
		CandidateURI string `json:"candidate_uri"`
		Index        int    `json:"candidate_index"`
		Error        string `json:"error"`
	}
	var attempted *failureEntry
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry failureEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry.Msg == "virtual playback candidate failed" && entry.Index == 0 {
			copied := entry
			attempted = &copied
		}
	}
	if attempted == nil {
		t.Fatalf("no candidate_index=0 failure entry logged; logs=%s", logs.String())
	}
	if attempted.CandidateURI != originalURI {
		t.Fatalf("index=0 candidate_uri = %q, want the attempted %q", attempted.CandidateURI, originalURI)
	}
	if !strings.Contains(attempted.Error, originalURI) {
		t.Fatalf("index=0 error %q does not name the attempted candidate %q", attempted.Error, originalURI)
	}
	if !strings.Contains(attempted.Error, substituteURI) {
		t.Fatalf("index=0 error %q does not name the substituted candidate %q", attempted.Error, substituteURI)
	}
}
