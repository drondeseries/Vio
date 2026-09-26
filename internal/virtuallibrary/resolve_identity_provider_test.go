package virtuallibrary_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// infoHashIdentityProvider answers with one torrent-style release whose only
// durable identity is the infoHash and a behaviorHints.videoSize hint. This is
// the shape an addon uses when it has no behaviorHints.videoHash and no
// Prowlarr GUID, which is exactly the case that used to persist no identity.
func infoHashIdentityProvider(t *testing.T, infoHash string) *httptest.Server {
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
				{
					"name": "AltMount 1080p",
					"title": "My.Movie.2024.1080p.WEB-DL.x264\n💾 8.50 GB 🌐 NZBgeek",
					"url": "https://provider.example/my.movie.mkv",
					"infoHash": "` + infoHash + `",
					"behaviorHints": {"videoSize": 8500000000}
				}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func newInfoHashService(t *testing.T, infoHash string) *virtuallibrary.Service {
	t.Helper()
	server := infoHashIdentityProvider(t, infoHash)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:             true,
		ManifestURL:         server.URL + "/manifest.json",
		AllowInsecureHTTP:   true,
		AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil service")
	}
	return svc
}

// TestResolveDetailedCarriesInfoHashIdentity proves a candidate whose only
// provider identity is an infoHash + videoSize hint still resolves with a
// durable identity, so the persistence path stores something re-matchable.
func TestResolveDetailedCarriesInfoHashIdentity(t *testing.T) {
	svc := newInfoHashService(t, "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333")

	res, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != nil {
		t.Fatalf("ResolveDetailed: %v", err)
	}
	if res.ProviderVideoHash != "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333" {
		t.Fatalf("ProviderVideoHash = %q, want the infoHash", res.ProviderVideoHash)
	}
	if res.ProviderReleaseName == "" {
		t.Fatal("ProviderReleaseName is empty; the name tier must be derived from the title")
	}
	if res.ProviderReleaseSize != 8_500_000_000 {
		t.Fatalf("ProviderReleaseSize = %d, want the videoSize hint", res.ProviderReleaseSize)
	}
}

// TestListStreamsCarriesInfoHashIdentity is the listing half of the persistence
// path: the sink stores what ListStreams/ListStreamsFresh report, so the record
// must carry the same durable identity.
func TestListStreamsCarriesInfoHashIdentity(t *testing.T) {
	svc := newInfoHashService(t, "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333")

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("len(streams) = %d, want 1", len(streams))
	}
	got := streams[0]
	if got.ProviderVideoHash != "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333" {
		t.Fatalf("ProviderVideoHash = %q, want the infoHash", got.ProviderVideoHash)
	}
	if got.ProviderReleaseName == "" {
		t.Fatal("ProviderReleaseName is empty; the name tier must be derived from the title")
	}
	if got.FileSize != 8_500_000_000 {
		t.Fatalf("FileSize = %d, want the videoSize hint", got.FileSize)
	}
}

// TestResolveDetailedRematchesInfoHashUnderNewResultID is the acceptance case:
// the pinned result id is renumbered by the provider, but the row's durable
// infoHash identity re-identifies the same release and reports IdentityRematched
// instead of a dead pin.
func TestResolveDetailedRematchesInfoHashUnderNewResultID(t *testing.T) {
	svc := newInfoHashService(t, "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333")
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 1 {
		t.Fatalf("ListStreams: count=%d err=%v, want 1", len(streams), err)
	}
	identity := virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   streams[0].ProviderVideoHash,
		ReleaseName: streams[0].ProviderReleaseName,
		ReleaseSize: streams[0].FileSize,
	}
	if identity.VideoHash == "" {
		t.Fatal("fixture did not carry an infoHash identity")
	}

	res, err := svc.ResolveDetailed(
		virtuallibrary.WithPersistedCandidateIdentity(ctx, identity),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err != nil {
		t.Fatalf("an infoHash row must rematch a renumbered result id: %v", err)
	}
	if !res.IdentityRematched {
		t.Fatal("resolver did not report the infoHash same-release re-identification")
	}
	if res.ProviderVideoHash != streams[0].ProviderVideoHash {
		t.Fatalf("rematched hash = %q, want the candidate's own", res.ProviderVideoHash)
	}
}
