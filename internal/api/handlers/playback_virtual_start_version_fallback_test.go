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
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestResolveVirtualPlaybackSourceUserRelinkBypassesFloor pins behavior 1: a
// deliberate user relink (force_relink) marks the resolve as an outage re-list
// so the provider re-lists past the fresh-serve floor and the
// provider-failure fail-fast. An automatic resolve and a session-bound rotation
// are server-initiated and stay on the floor.
func TestResolveVirtualPlaybackSourceUserRelinkBypassesFloor(t *testing.T) {
	newHandler := func(seen *bool) *PlaybackHandler {
		return &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "", errors.New("simple resolver must not be used when the detailed resolver is set")
			}),
			VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				*seen = virtuallibrary.ProviderOutageRelistFromContext(ctx)
				return ResolvedVirtualMedia{URL: "https://cdn.example/x.mkv", URI: uri, CandidateID: "cand-x", OwnerID: 5}, nil
			}),
		}
	}
	file := &models.MediaFile{ID: 1, ContentID: "movie-relink", FilePath: "virtual://movie/movie-relink?result=A", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	userSeen := false
	if _, err := newHandler(&userSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, true, virtualResolveOptionsV3{sessionBound: false}); err != nil {
		t.Fatalf("user relink resolve: %v", err)
	}
	if !userSeen {
		t.Fatal("a user force_relink must mark the resolve as an outage re-list so it bypasses the floor and provider-failure fail-fast")
	}

	autoSeen := false
	if _, err := newHandler(&autoSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, false, virtualResolveOptionsV3{sessionBound: false}); err != nil {
		t.Fatalf("automatic resolve: %v", err)
	}
	if autoSeen {
		t.Fatal("an automatic resolve must stay on the floor")
	}

	rotationSeen := false
	if _, err := newHandler(&rotationSeen).resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "auto", 0, true, virtualResolveOptionsV3{sessionBound: true}); err != nil {
		t.Fatalf("session-bound rotation resolve: %v", err)
	}
	if rotationSeen {
		t.Fatal("a session-bound rotation is server-initiated and must stay on the floor")
	}
}

// TestResolveVirtualStartWithVersionFallbackSkipsAltMountFailedVersion pins
// behavior 2: when the pinned release's listing fails on a fresh start, the
// walk tries alternate versions and returns the first that resolves. A version
// carrying an active AltMount SourceFailed verdict is skipped without a
// listing attempt, and the primary empty-listing failure is the honest cause
// returned only when no other version resolves.
func TestResolveVirtualStartWithVersionFallbackSkipsAltMountFailedVersion(t *testing.T) {
	const (
		primaryURI = "virtual://movie/movie-versions?result=A"
		failedURI  = "virtual://movie/movie-versions?result=B"
		workingURI = "virtual://movie/movie-versions?result=C"
	)
	primary := &models.MediaFile{ID: 1, ContentID: "movie-versions", FilePath: primaryURI, VirtualOwnerInstallationID: 5}
	failedAt := time.Now()
	failedAlt := &models.MediaFile{ID: 2, ContentID: "movie-versions", FilePath: failedURI, VirtualOwnerInstallationID: 5, FailedAt: &failedAt, ProviderVideoHash: "hash-b"}
	workingAlt := &models.MediaFile{ID: 3, ContentID: "movie-versions", FilePath: workingURI, VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-c"}

	var callsA, callsB, callsC int
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			"movie-versions": {primary, failedAlt, workingAlt},
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			switch {
			case strings.Contains(uri, "result=A"):
				callsA++
				return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
			case strings.Contains(uri, "result=B"):
				callsB++
				return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
			case strings.Contains(uri, "result=C"):
				callsC++
				return ResolvedVirtualMedia{URL: "https://cdn.example/c.mkv", URI: uri, CandidateID: "cand-c", OwnerID: 5}, nil
			default:
				return ResolvedVirtualMedia{}, fmt.Errorf("unexpected URI %q", uri)
			}
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	start := playback.StartRequestV3{QualityPreference: "auto"}

	resolved, err := h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", start, false, 0)
	if err != nil {
		t.Fatalf("cross-version fallback resolve: %v", err)
	}
	if resolved.File == nil || !strings.Contains(resolved.URI, "result=C") {
		t.Fatalf("resolved = %#v, want the working alternate version C", resolved)
	}
	if callsA == 0 {
		t.Fatal("the primary pinned release was never attempted")
	}
	if callsC == 0 {
		t.Fatal("the working alternate version was never attempted")
	}
	if callsB != 0 {
		t.Fatalf("the AltMount-failed version was attempted %d times; it must be skipped", callsB)
	}
}

// TestResolveVirtualStartWithVersionFallbackReturnsHonestEmptyListing pins that
// when every alternate's listing is empty, the original empty-listing cause is
// returned (not a fabricated all-versions verdict), so the caller can report
// the honest transient condition.
func TestResolveVirtualStartWithVersionFallbackReturnsHonestEmptyListing(t *testing.T) {
	primary := &models.MediaFile{ID: 1, ContentID: "movie-empty", FilePath: "virtual://movie/movie-empty?result=A", VirtualOwnerInstallationID: 5}
	alt := &models.MediaFile{ID: 2, ContentID: "movie-empty", FilePath: "virtual://movie/movie-empty?result=B", VirtualOwnerInstallationID: 5}
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			"movie-empty": {primary, alt},
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: no streams available from provider")
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	_, err := h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", playback.StartRequestV3{QualityPreference: "auto"}, false, 0)
	if err == nil {
		t.Fatal("expected the empty-listing resolve to fail")
	}
	terminal := virtualStartUnresolvedTerminalV3(err).Terminal
	if terminal == nil || terminal.Reason != "virtual_source_unavailable" || !strings.Contains(terminal.Message, "no streams") {
		t.Fatalf("terminal = %#v, want the honest empty-listing cause", terminal)
	}
	if !terminal.Retryable {
		t.Fatal("an empty provider listing is transient and must stay retryable")
	}
}

// TestVirtualStartUnresolvedTerminalHonestCause pins the exhaustion
// classification: an edge provider failure is the retryable
// provider_unavailable dependency condition, an empty listing names the empty
// answer, and every other cause keeps the generic resolve message instead of a
// fabricated verdict.
func TestVirtualStartUnresolvedTerminalHonestCause(t *testing.T) {
	edge := virtualStartUnresolvedTerminalV3(fmt.Errorf("resolve virtual input: %w: provider listing failed recently", resolver.ErrProviderUnavailable))
	if edge.Terminal == nil || edge.Terminal.Reason != providerUnavailableReasonV3 || !edge.Terminal.Retryable {
		t.Fatalf("edge failure terminal = %#v, want retryable %s", edge.Terminal, providerUnavailableReasonV3)
	}
	empty := virtualStartUnresolvedTerminalV3(errors.New("no streams available from provider"))
	if empty.Terminal == nil || empty.Terminal.Reason != "virtual_source_unavailable" || !strings.Contains(empty.Terminal.Message, "no streams") {
		t.Fatalf("empty listing terminal = %#v, want the empty-listing message", empty.Terminal)
	}
	dead := virtualStartUnresolvedTerminalV3(errors.New("virtual stream provider returned no matching candidate"))
	if dead.Terminal == nil || dead.Terminal.Reason != "virtual_source_unavailable" || strings.Contains(dead.Terminal.Message, "no streams") {
		t.Fatalf("other terminal = %#v, want the generic resolve message", dead.Terminal)
	}
}
