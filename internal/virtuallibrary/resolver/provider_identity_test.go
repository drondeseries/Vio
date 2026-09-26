package resolver

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// TestCandidateDedupKeyUsesInfoHashAndVideoSize proves the dedup hash tier
// accepts a torrent infoHash and the name tier reads the videoSize hint, so a
// provider that declares identity only through those fields still collapses and
// re-matches.
func TestCandidateDedupKeyUsesInfoHashAndVideoSize(t *testing.T) {
	candidate := StreamCandidate{
		Name:     "AltMount 1080p",
		Title:    "My.Movie.2024.1080p.WEB-DL.x264\n💾 8.50 GB 🌐 NZBgeek",
		URL:      "https://provider.example/my.movie.mkv",
		InfoHash: "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333",
	}
	candidate.BehaviorHints.VideoSize = 8_500_000_000

	if got, want := candidateDedupKey(candidate), "vidhash:aaaabbbbccccddddeeeeffff0000111122223333"; got != want {
		t.Fatalf("dedup key = %q, want the infoHash tier %q", got, want)
	}

	// With the hash removed, the name tier still carries the size hint so two
	// postings of the same release collapse by name+size.
	candidate.InfoHash = ""
	key := candidateDedupKey(candidate)
	if key == "" {
		t.Fatal("name+size tier is empty despite a title and a videoSize hint")
	}
	other := candidate
	other.BehaviorHints.VideoSize = 0
	if candidateDedupKey(other) == key {
		t.Fatal("name tier ignored the videoSize hint: sizes collapsed as equal")
	}
}

// TestPersistedIdentityFromInfoHashCandidate is the end-to-end shape: a
// candidate carrying infoHash + videoSize yields a durable identity a persisted
// row can hold.
func TestPersistedIdentityFromInfoHashCandidate(t *testing.T) {
	candidate := StreamCandidate{
		Name:     "AltMount 1080p",
		Title:    "My.Movie.2024.1080p.WEB-DL.x264\n💾 8.50 GB 🌐 NZBgeek",
		URL:      "https://provider.example/my.movie.mkv",
		InfoHash: "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333",
	}
	candidate.BehaviorHints.VideoSize = 8_500_000_000

	videoHash := stream.CandidateVideoHash(candidate)
	if videoHash != "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333" {
		t.Fatalf("provider video hash = %q, want the infoHash", videoHash)
	}
	releaseName := CandidateReleaseName(candidate)
	if releaseName == "" {
		t.Fatal("release name is empty for an infoHash candidate")
	}
	if got, want := PersistedDedupKey(videoHash, "", releaseName, stream.CandidateDeclaredSize(candidate)), "vidhash:aaaabbbbccccddddeeeeffff0000111122223333"; got != want {
		t.Fatalf("persisted key = %q, want the hash tier %q", got, want)
	}
}
