package prowlarr

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// TestClassifyCandidatesPreservesAltmountGUID proves Prowlarr corroborates a
// candidate AltMount already identified instead of re-identifying it: the
// namespaced AltMount key must survive so the row's durable GUID does not move
// between providers on every classification pass.
func TestClassifyCandidatesPreservesAltmountGUID(t *testing.T) {
	release := prowlarrRelease{
		GUID:        "prowlarr-guid-1",
		Title:       "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		Size:        8_500_000_000,
		DownloadURL: "https://indexer.example/download/1",
	}
	prepareProwlarrRelease(&release)
	client := &prowlarrSearchClient{releases: []prowlarrRelease{release}}

	candidate := stream.StreamCandidate{
		Name:       "AltMount 1080p",
		Title:      "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		URL:        "https://provider.example/my.movie.mkv",
		FileSize:   8_500_000_000,
		SourceGUID: "altmount:mymovie20241080pwebdlx264grp",
	}
	candidates := []stream.StreamCandidate{candidate}
	client.ClassifyCandidates(candidates)

	if !candidates[0].SourceConfirmed {
		t.Fatal("Prowlarr did not confirm the candidate")
	}
	if candidates[0].SourceGUID != "altmount:mymovie20241080pwebdlx264grp" {
		t.Fatalf("SourceGUID = %q, want the AltMount key preserved", candidates[0].SourceGUID)
	}
}

// TestClassifyCandidatesFillsEmptyGUID proves Prowlarr still supplies its own
// release GUID when the candidate has none, so the GUID tier is populated when
// AltMount is unwired.
func TestClassifyCandidatesFillsEmptyGUID(t *testing.T) {
	release := prowlarrRelease{
		GUID:        "prowlarr-guid-1",
		Title:       "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		Size:        8_500_000_000,
		DownloadURL: "https://indexer.example/download/1",
	}
	prepareProwlarrRelease(&release)
	client := &prowlarrSearchClient{releases: []prowlarrRelease{release}}

	candidates := []stream.StreamCandidate{{
		Name:     "AltMount 1080p",
		Title:    "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		URL:      "https://provider.example/my.movie.mkv",
		FileSize: 8_500_000_000,
	}}
	client.ClassifyCandidates(candidates)

	if candidates[0].SourceGUID != "prowlarr-guid-1" {
		t.Fatalf("SourceGUID = %q, want the Prowlarr GUID", candidates[0].SourceGUID)
	}
}
