package virtuallibrary_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// identityRematchProvider serves two distinct releases so a re-list can be
// matched by the durable identity of one of them. Result ids are stable within
// a test run; the "absent pin" is a fabricated id, which is what the provider
// produces when it renumbers results between listings.
func identityRematchProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"streams": [
				{"name": "1080p", "title": "Alpha.Movie.2024.1080p.WEB-DL.x264", "fileSize": 8000000000, "url": "http://192.168.1.100:8080/alpha.mkv"},
				{"name": "720p", "title": "Beta.Movie.2024.720p.WEB-DL.x264", "fileSize": 4000000000, "url": "http://192.168.1.100:8080/beta.mkv"}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	return server
}

const absentResultID = "ffffffffffffffffffffffff"

// TestResolveDetailedRematchesSameReleaseByIdentity proves a pinned id absent
// from a fresh listing is treated as the same release re-identified when a
// listed candidate carries the row's durable identity, and that the resolver
// reports the re-match rather than a substitution.
func TestResolveDetailedRematchesSameReleaseByIdentity(t *testing.T) {
	server := identityRematchProvider(t)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       server.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)
	ctx := context.Background()
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("ListStreams: count=%d err=%v, want 2", len(streams), err)
	}
	target := streams[0]
	identity := virtuallibrary.PersistedCandidateIdentity{
		ReleaseName: target.ProviderReleaseName,
		ReleaseSize: target.FileSize,
	}
	if identity.ReleaseName == "" || identity.ReleaseSize == 0 {
		t.Fatalf("fixture did not carry a name+size identity: %+v", identity)
	}

	res, err := svc.ResolveDetailed(
		virtuallibrary.WithPersistedCandidateIdentity(ctx, identity),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err != nil {
		t.Fatalf("rematch resolve: %v", err)
	}
	if !res.IdentityRematched {
		t.Fatal("resolver did not report the same-release re-identification")
	}
	if res.CandidateID != target.ID {
		t.Fatalf("rematched candidate = %q, want %q", res.CandidateID, target.ID)
	}
	if res.URL != target.ProviderURL {
		t.Fatalf("rematched URL = %q, want the matched candidate's %q", res.URL, target.ProviderURL)
	}
	if !strings.Contains(res.URI, "result="+target.ID) {
		t.Fatalf("rematched URI = %q, want the new result id", res.URI)
	}
}

// TestResolveDetailedRematchRejectsSameNameDifferentSize proves a provider that
// reuses a release name cannot be treated as the same release when the size
// differs and there is no stronger identity tier.
func TestResolveDetailedRematchRejectsSameNameDifferentSize(t *testing.T) {
	server := identityRematchProvider(t)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       server.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)
	ctx := context.Background()
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("ListStreams: count=%d err=%v, want 2", len(streams), err)
	}
	identity := virtuallibrary.PersistedCandidateIdentity{
		ReleaseName: streams[0].ProviderReleaseName,
		ReleaseSize: streams[0].FileSize + 1,
	}

	_, err = svc.ResolveDetailed(
		virtuallibrary.WithPersistedCandidateIdentity(ctx, identity),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err == nil {
		t.Fatal("expected the dead-pin refusal when only the name matches but the size differs")
	}
	if !strings.Contains(err.Error(), "no longer listed") {
		t.Fatalf("error = %v, want the session-bound dead-pin refusal", err)
	}
}

// TestResolveDetailedRematchRejectsStrongerTierMismatch proves a stored hash that
// no listed candidate carries is a miss even when the name+size coincide: the
// stronger tier constrains the match so a re-used release name cannot be gamed.
func TestResolveDetailedRematchRejectsStrongerTierMismatch(t *testing.T) {
	server := identityRematchProvider(t)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       server.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)
	ctx := context.Background()
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("ListStreams: count=%d err=%v, want 2", len(streams), err)
	}
	identity := virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "a-hash-no-candidate-carries",
		ReleaseName: streams[0].ProviderReleaseName,
		ReleaseSize: streams[0].FileSize,
	}

	_, err = svc.ResolveDetailed(
		virtuallibrary.WithPersistedCandidateIdentity(ctx, identity),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err == nil {
		t.Fatal("expected refusal when the stored hash matches no candidate")
	}
}

// TestResolveDetailedLegacyRowStaysRefused proves a row with no durable
// identity keeps today's behavior: the absent pin is refused.
func TestResolveDetailedLegacyRowStaysRefused(t *testing.T) {
	server := identityRematchProvider(t)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       server.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)

	_, err := svc.ResolveDetailed(
		context.Background(),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err == nil {
		t.Fatal("expected the dead-pin refusal for a legacy row with no identity")
	}
	if !strings.Contains(err.Error(), "no longer listed") {
		t.Fatalf("error = %v, want the session-bound dead-pin refusal", err)
	}
}
