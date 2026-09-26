package resolver

import "testing"

// candidateWithIdentity builds a listed candidate carrying the given durable
// identity fields. It exercises the same candidateDedupKey the deduplication
// chain uses, so the assertions below pin the re-match precedence.
func candidateWithIdentity(url, title, videoHash, guid string, size int64) StreamCandidate {
	candidate := StreamCandidate{
		URL:        url,
		Name:       title,
		Title:      title,
		FileSize:   size,
		SourceGUID: guid,
	}
	candidate.BehaviorHints.VideoHash = videoHash
	return candidate
}

func TestPersistedDedupKeyPrecedence(t *testing.T) {
	if got, want := PersistedDedupKey("ABC", "guid-1", "name", 100), "vidhash:abc"; got != want {
		t.Fatalf("hash tier = %q, want %q", got, want)
	}
	if got, want := PersistedDedupKey("", "guid-1", "name", 100), "guid:guid-1"; got != want {
		t.Fatalf("guid tier = %q, want %q", got, want)
	}
	nameKey := PersistedDedupKey("", "", "release.name", 100)
	if nameKey == "" || nameKey == PersistedDedupKey("", "", "release.name", 0) {
		t.Fatalf("name+size tier = %q; size must distinguish", nameKey)
	}
	if got := PersistedDedupKey("", "", "", 100); got != "" {
		t.Fatalf("identity with no tier = %q, want empty", got)
	}
}

// TestMatchCandidateByPersistedIdentityTierOrder pins that the re-match uses the
// dedup chain's tier order: a stronger stored tier is never satisfied by a
// weaker candidate, and a weaker stored tier only matches name+size.
func TestMatchCandidateByPersistedIdentityTierOrder(t *testing.T) {
	hashCandidate := candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "HASH1", "", 8_000_000_000)
	nameCandidate := candidateWithIdentity("https://p.example/b.mkv", "Movie.2024.1080p", "", "", 8_000_000_000)
	candidates := []StreamCandidate{hashCandidate, nameCandidate}

	// A stored hash matches the hash candidate even when the stored name/size
	// disagree: the strongest tier wins.
	matched, ok := MatchCandidateByPersistedIdentity(candidates, "hash1", "", "different.name", 1)
	if !ok || matched.URL != hashCandidate.URL {
		t.Fatalf("hash match = %v ok=%v, want the hash candidate", matched.URL, ok)
	}

	// A stored name+size identity matches the name candidate.
	matched, ok = MatchCandidateByPersistedIdentity(candidates, "", "", CandidateReleaseName(nameCandidate), nameCandidate.FileSize)
	if !ok || matched.URL != nameCandidate.URL {
		t.Fatalf("name+size match = %v ok=%v, want the name candidate", matched.URL, ok)
	}

	// A stored hash that no candidate carries is a miss: the weaker name+size
	// coincidence must NOT be used. This is what stops a provider that reuses a
	// release name from being treated as the same release.
	if _, ok := MatchCandidateByPersistedIdentity(candidates, "different-hash", "", CandidateReleaseName(nameCandidate), nameCandidate.FileSize); ok {
		t.Fatal("a stored hash did not match, yet a name+size candidate was accepted")
	}

	// A stored name+size with a different size is a miss.
	if _, ok := MatchCandidateByPersistedIdentity(candidates, "", "", CandidateReleaseName(nameCandidate), nameCandidate.FileSize+1); ok {
		t.Fatal("same release name with a different size matched")
	}
}

// TestMatchCandidateByPersistedIdentityEmptyNeverMatches proves a legacy
// identity (all tiers empty) can never re-match.
func TestMatchCandidateByPersistedIdentityEmptyNeverMatches(t *testing.T) {
	candidates := []StreamCandidate{candidateWithIdentity("https://p.example/a.mkv", "Movie.2024.1080p", "", "", 0)}
	if _, ok := MatchCandidateByPersistedIdentity(candidates, "", "", "", 0); ok {
		t.Fatal("an empty persisted identity matched a candidate")
	}
}
