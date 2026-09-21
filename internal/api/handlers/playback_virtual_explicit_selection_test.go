package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// explicitSelectionRow is a persisted ?result= candidate the viewer picked
// explicitly. It carries the durable identity and a stored URL. Video evidence
// is intentionally absent unless a test adds it, so the request exercises the
// synchronous resolve/probe path rather than the repeat-play fast path.
func explicitSelectionRow(storedURL string, expiresAt *time.Time) *models.MediaFile {
	return &models.MediaFile{
		ID:                         801,
		ContentID:                  "movie-explicit",
		FilePath:                   "virtual://movie/tt-explicit?result=cand-a",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ResolvedURL:                storedURL,
		ResolvedURLExpiresAt:       expiresAt,
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
	}
}

// TestExplicitSelectionServesPersistedRowWithinTrustWindow pins ladder item 1:
// an explicit version pick of a persisted candidate whose provider listing no
// longer contains it is served from the row's own stored URL inside the trust
// window. Neither the provider lister nor the resolver may run, and the served
// identity must remain the selected candidate (never a sibling).
func TestExplicitSelectionServesPersistedRowWithinTrustWindow(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=stored", &expiresAt)

	listerCalls, detailedCalls := 0, 0
	probed := make(chan struct{}, 1)
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: countingDetailedResolver(&detailedCalls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=sibling", URI: "virtual://movie/tt-explicit?result=cand-b", CandidateID: "cand-b",
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				listerCalls++
				return []VirtualPlaybackStream{{
					ID: "cand-b", URI: "virtual://movie/tt-explicit?result=cand-b",
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}
	// The row has no planner-grade video evidence, so the completeness gate
	// must still force a probe. The stored-URL-first path skips only the
	// provider resolve, never the probe.
	h.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		select {
		case probed <- struct{}{}:
		default:
		}
		return f, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 0 {
		t.Fatalf("detailed resolver called %d times, want 0: the stored row must be served", detailedCalls)
	}
	if listerCalls != 0 {
		t.Fatalf("provider lister called %d times, want 0: the stored row must be served", listerCalls)
	}
	if resolved.URI != file.FilePath {
		t.Fatalf("resolved URI = %q, want the selected candidate %q", resolved.URI, file.FilePath)
	}
	if resolved.File == nil || resolved.File.FilePath != file.FilePath {
		t.Fatalf("resolved file = %#v, want the selected candidate", resolved.File)
	}
	if resolved.URL != file.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the row's stored URL %q", resolved.URL, file.ResolvedURL)
	}
	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("the completeness gate did not force a probe for a row with no planner-grade evidence")
	}
}

// TestExplicitSelectionBeyondWindowKeepsTodayBehavior pins the boundary: with
// the window disabled (0), an expired stored URL carries no trust, so the
// explicit pick resolves as before and may fall to a sibling.
func TestExplicitSelectionBeyondWindowKeepsTodayBehavior(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=old", &expiredAt)

	const siblingURI = "virtual://movie/tt-explicit?result=cand-b"
	listerCalls, detailedCalls := 0, 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 0 },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: countingDetailedResolver(&detailedCalls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=sibling", URI: siblingURI, CandidateID: "cand-b",
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				listerCalls++
				return []VirtualPlaybackStream{{
					ID: "cand-b", URI: siblingURI,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls == 0 {
		t.Fatal("detailed resolver not called: an untrusted expired URL must resolve")
	}
	if resolved.URI != siblingURI {
		t.Fatalf("resolved URI = %q, want the sibling %q (today's behavior outside the window)", resolved.URI, siblingURI)
	}
}

// TestExplicitSelectionWithoutStoredURLNotTrusted pins the trust triple: a row
// with no stored URL is not trusted even when explicitly selected, so the
// ordinary resolve/fallback runs and the selected identity is not fabricated
// from nothing.
func TestExplicitSelectionWithoutStoredURLNotTrusted(t *testing.T) {
	file := explicitSelectionRow("", nil)
	file.ProviderVideoHash = ""

	const siblingURI = "virtual://movie/tt-explicit?result=cand-b"
	listerCalls, detailedCalls := 0, 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: countingDetailedResolver(&detailedCalls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=sibling", URI: siblingURI, CandidateID: "cand-b",
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				listerCalls++
				return []VirtualPlaybackStream{{
					ID: "cand-b", URI: siblingURI,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls == 0 {
		t.Fatal("detailed resolver not called for a row with no persisted evidence")
	}
	if resolved.URI != siblingURI {
		t.Fatalf("resolved URI = %q, want today's fallback %q", resolved.URI, siblingURI)
	}
}

// TestAutoSelectionDoesNotTakeExplicitStoredShortcut pins the scope boundary:
// the stored-URL-first shortcut is for a bound selection only. An auto
// selection with the same row keeps resolving (and may substitute) as before,
// so the feature cannot change auto-pick behavior.
func TestAutoSelectionDoesNotTakeExplicitStoredShortcut(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=stored", &expiresAt)

	listerCalls, detailedCalls := 0, 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: countingDetailedResolver(&detailedCalls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: file.FilePath, CandidateID: "cand-a",
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				listerCalls++
				return []VirtualPlaybackStream{{
					ID: "cand-a", URI: file.FilePath,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: false})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls == 0 {
		t.Fatal("auto selection took the explicit stored-URL shortcut; scope boundary violated")
	}
	if resolved.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly resolved URL", resolved.URL)
	}
}

// TestExplicitSelectionThreadsTrustWithinWindow proves the handler carries the
// persisted-candidate trust into the resolver for an explicit pick inside the
// window, so a dropped same-identity candidate is refused rather than
// substituted. The fake returns the trust sentinel exactly when the context
// carries the flag; the attempt loop must stop instead of serving a sibling.
func TestExplicitSelectionThreadsTrustWithinWindow(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=old", &expiredAt)

	const siblingURI = "virtual://movie/tt-explicit?result=cand-b"
	sawTrust := false
	detailedCalls := 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				detailedCalls++
				sawTrust = virtuallibrary.PersistedCandidateTrusted(ctx)
				return ResolvedVirtualMedia{}, fmt.Errorf("trusted persisted virtual candidate %q is no longer listed: %w", "cand-a", virtuallibrary.ErrPersistedCandidateTrusted)
			}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				return []VirtualPlaybackStream{{
					ID: "cand-b", URI: siblingURI,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	_, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: true})
	if !sawTrust {
		t.Fatal("explicit selection did not thread the persisted-candidate trust into the resolver")
	}
	if err == nil {
		t.Fatal("expected the trusted-absent refusal, got a served source (sibling substitution)")
	}
	if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatalf("err = %v, want ErrPersistedCandidateTrusted", err)
	}
	if detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1", detailedCalls)
	}
}

// TestAutoSelectionDoesNotThreadTrust proves the trust is scoped to a bound
// selection: an auto pick inside the window resolves with the flag unset, so
// its ordinary fallback is unchanged.
func TestAutoSelectionDoesNotThreadTrust(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=old", &expiredAt)

	sawTrust := false
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				sawTrust = virtuallibrary.PersistedCandidateTrusted(ctx)
				return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=fresh", URI: file.FilePath, CandidateID: "cand-a"}, nil
			}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				return []VirtualPlaybackStream{{
					ID: "cand-a", URI: file.FilePath,
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{explicitSelection: false}); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if sawTrust {
		t.Fatal("auto selection must not thread the persisted-candidate trust")
	}
}

// TestExplicitSelectionConcurrentPlaysDoNotFetch proves two plays racing the
// same explicit selection both serve the persisted URL without a provider
// resolve: the stored URL is read-only, so there is no fetch storm and no
// competing write.
func TestExplicitSelectionConcurrentPlaysDoNotFetch(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := explicitSelectionRow("https://93.184.216.34/stream/token=stored", &expiresAt)

	detailedCalls := 0
	h := &PlaybackHandler{
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
				detailedCalls++
				return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=sibling", URI: "virtual://movie/tt-explicit?result=cand-b", CandidateID: "cand-b"}, nil
			}),
	}

	var wg sync.WaitGroup
	urls := make([]string, 2)
	for i := range urls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
			resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
				virtualResolveOptionsV3{explicitSelection: true})
			if err != nil {
				t.Errorf("concurrent resolve %d: %v", i, err)
				return
			}
			urls[i] = resolved.URL
		}(i)
	}
	wg.Wait()
	if detailedCalls != 0 {
		t.Fatalf("detailed resolver called %d times concurrently, want 0", detailedCalls)
	}
	for i, u := range urls {
		if u != file.ResolvedURL {
			t.Fatalf("play %d served URL %q, want the stored URL %q", i, u, file.ResolvedURL)
		}
	}
}
