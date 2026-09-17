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

	_, err = s.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "")
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
	_, errSecure := svcSecure.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "")
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
	res, err := svcInsecure.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "")
	if err != nil {
		t.Fatalf("ResolveDetailed failed: %v", err)
	}
	if res.URL != "http://192.168.1.100:8080/stream1.mkv" {
		t.Fatalf("resolved URL = %q, want stream1", res.URL)
	}
	if res.CandidateID == "" {
		t.Fatal("expected non-empty candidate ID")
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
	}
}
