package virtuallibrary_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

func TestResolveNilServiceReturnsUnavailable(t *testing.T) {
	var s *virtuallibrary.Service
	_, err := s.Resolve(context.Background(), "virtual://movie/tt100")
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("Resolve: err = %v, want ErrVirtualLibraryUnavailable", err)
	}

	_, err = s.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("ResolveDetailed: err = %v, want ErrVirtualLibraryUnavailable", err)
	}

	_, err = s.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != virtuallibrary.ErrVirtualLibraryUnavailable {
		t.Fatalf("ListStreams: err = %v, want ErrVirtualLibraryUnavailable", err)
	}
}

func TestCandidateVariantIDConsistency(t *testing.T) {
	c1 := stream.StreamCandidate{
		URL:        "https://stream.example.com/video.mkv",
		Name:       "Movie 1080p",
		Title:      "Movie.2024.1080p.WEBRip.x264",
		Resolution: "1080p",
		CodecVideo: "h264",
		CodecAudio: "aac",
	}
	id1 := stream.CandidateVariantID(c1)
	if len(id1) != 24 {
		t.Fatalf("id length = %d, want 24 (12 hex bytes)", len(id1))
	}
	id2 := stream.CandidateVariantID(c1)
	if id1 != id2 {
		t.Fatalf("candidate ID not deterministic: %q != %q", id1, id2)
	}

	// Different stream URL yields distinct ID
	c2 := c1
	c2.URL = "https://stream.example.com/video2.mkv"
	id3 := stream.CandidateVariantID(c2)
	if id1 == id3 {
		t.Fatalf("different candidate produced same ID: %q", id1)
	}
}

func TestResolvedVirtualProvenanceInference(t *testing.T) {
	// Virtual file with owner <= 0 infers Core provenance across DB reloads
	coreFile := &models.MediaFile{
		Container:                  "virtual",
		FilePath:                   "virtual://movie/tt100",
		VirtualOwnerInstallationID: 0,
	}
	if prov := coreFile.ResolvedVirtualProvenance(); prov != models.VirtualProvenanceCore {
		t.Fatalf("resolved provenance = %q, want %q", prov, models.VirtualProvenanceCore)
	}

	// Virtual file with positive owner infers Plugin provenance
	pluginFile := &models.MediaFile{
		Container:                  "virtual",
		FilePath:                   "virtual://movie/tt100",
		VirtualOwnerInstallationID: 42,
	}
	if prov := pluginFile.ResolvedVirtualProvenance(); prov != models.VirtualProvenancePlugin {
		t.Fatalf("resolved provenance = %q, want %q", prov, models.VirtualProvenancePlugin)
	}

	// Explicitly stamped provenance is preserved as-is
	stampedFile := &models.MediaFile{
		VirtualProvenance:          models.VirtualProvenanceCore,
		VirtualOwnerInstallationID: 99,
	}
	if prov := stampedFile.ResolvedVirtualProvenance(); prov != models.VirtualProvenanceCore {
		t.Fatalf("resolved provenance = %q, want %q", prov, models.VirtualProvenanceCore)
	}

	// Non-virtual local file infers Local
	localFile := &models.MediaFile{
		Container: "mkv",
		FilePath:  "/media/movies/movie.mkv",
	}
	if prov := localFile.ResolvedVirtualProvenance(); prov != models.VirtualProvenanceLocal {
		t.Fatalf("resolved provenance = %q, want %q", prov, models.VirtualProvenanceLocal)
	}
}

// TestResolveDetailedPinnedExcludedCandidateSubstitution pins the release-swap
// invariant: a pinned candidate the caller excluded is only substitutable when
// rotation was explicitly requested. A dead pin (absent from the provider list)
// still falls back so a genuinely unavailable release recovers.
func TestResolveDetailedPinnedExcludedCandidateSubstitution(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"streams": [
				{"name": "1080p", "title": "First", "url": "http://192.168.1.100:8080/stream1.mkv"},
				{"name": "720p", "title": "Second", "url": "http://192.168.1.100:8080/stream2.mkv"}
			]
		}`))
	}))
	defer mockServer.Close()

	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       mockServer.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	ctx := context.Background()
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("ListStreams: count=%d err=%v, want 2", len(streams), err)
	}
	pinned := streams[0].ID
	sibling := streams[1].ID
	pinnedURI := "virtual://movie/tt100?result=" + pinned

	// Excluding the pinned candidate without a rotation request must refuse
	// rather than silently serving a different release.
	if _, err := svc.ResolveDetailed(ctx, pinnedURI, false, []string{pinned}, "", false, false); err == nil {
		t.Fatal("expected refusal when the pinned candidate is excluded without rotation")
	}

	// An explicit rotation request may substitute the sibling.
	rotated, err := svc.ResolveDetailed(ctx, pinnedURI, false, []string{pinned}, "", false, true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if rotated.CandidateID != sibling {
		t.Fatalf("rotated candidate = %q, want sibling %q", rotated.CandidateID, sibling)
	}

	// A pinned id absent from the provider list is a dead release: the
	// dead-provider fallback still substitutes even without a rotation request.
	dead, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=ffffffffffffffffffffffff", false, nil, "", false, false)
	if err != nil {
		t.Fatalf("dead-pin resolve: %v", err)
	}
	if dead.CandidateID == "" {
		t.Fatal("expected a fallback candidate for a dead pin")
	}
}

func TestResolveDetailedWithFakeProvider(t *testing.T) {
	// Create a mock HTTP server serving a valid Stremio streams response
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		// Stream endpoint: /stream/movie/tt100.json
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"streams": [
				{
					"name": "1080p",
					"title": "Movie 1080p Stream",
					"url": "http://192.168.1.100:8080/stream1.mkv"
				},
				{
					"name": "720p",
					"title": "Movie 720p Stream",
					"url": "http://192.168.1.100:8080/stream2.mkv"
				}
			]
		}`))
	}))
	defer mockServer.Close()

	// Case 1: AllowInsecureHTTP = false. Private IPs rejected by SSRF validation.
	svcSecure := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       mockServer.URL + "/manifest.json",
		AllowInsecureHTTP: false,
	}, nil, nil)
	if svcSecure == nil {
		t.Fatal("expected non-nil service")
	}
	_, errSecure := svcSecure.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if errSecure == nil {
		t.Fatal("expected SSRF error when AllowInsecureHTTP is false for private stream URL")
	}

	// Case 2: AllowInsecureHTTP = true. Private IP streams allowed.
	svcInsecure := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       mockServer.URL + "/manifest.json",
		AllowInsecureHTTP: true,
	}, nil, nil)
	if svcInsecure == nil {
		t.Fatal("expected non-nil service")
	}
	res, err := svcInsecure.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != nil {
		t.Fatalf("ResolveDetailed failed: %v", err)
	}
	if res.URL != "http://192.168.1.100:8080/stream1.mkv" {
		t.Fatalf("resolved URL = %q, want stream1", res.URL)
	}
	if res.CandidateID == "" {
		t.Fatal("expected non-empty candidate ID")
	}
	// A stream with no videoHash/GUID still carries a durable identity: the
	// normalized release name derived from its display text.
	if res.ProviderReleaseName == "" {
		t.Fatal("expected a derived provider release name for a candidate with no hash/GUID")
	}
	if res.ProviderGUID != "" || res.ProviderVideoHash != "" {
		t.Fatalf("unexpected provider identity: guid=%q hash=%q", res.ProviderGUID, res.ProviderVideoHash)
	}

	// Case 3: ListStreams returns streams with OwnerInstallationID = 0
	streams, err := svcInsecure.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams failed: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("len(streams) = %d, want 2", len(streams))
	}
	for i, s := range streams {
		if s.OwnerInstallationID != 0 {
			t.Errorf("stream[%d].OwnerInstallationID = %d, want 0", i, s.OwnerInstallationID)
		}
		if !s.Visible {
			t.Errorf("stream[%d].Visible = false, want true", i)
		}
		// The provider URL and durable identity must reach the persistence
		// sink for every listed candidate, even with no hash/GUID.
		if s.ProviderURL == "" {
			t.Errorf("stream[%d].ProviderURL is empty, want the provider URL", i)
		}
		if s.ProviderReleaseName == "" {
			t.Errorf("stream[%d].ProviderReleaseName is empty, want the name+size fallback", i)
		}
	}
}
