package resolver

import "testing"

// TestMatchCandidateIdentityForBackfillRequiresSizeCorroboration proves the
// backfill's central rule: a release name alone is not enough to invent durable
// identity. Both sides must carry a size that plausibly describes the same
// file. This is deliberately stricter than SharedTier, whose unknown-size
// neutrality is safe for serving but not for writing new identity.
func TestMatchCandidateIdentityForBackfillRequiresSizeCorroboration(t *testing.T) {
	candidate := NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 8_000_000_000)

	if tier, ok := MatchCandidateIdentityForBackfill(NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 8_200_000_000), candidate); !ok || tier != "release_name" {
		t.Fatalf("name+size within drift = %q ok=%v, want release_name match", tier, ok)
	}

	// A row that never recorded a size cannot corroborate the name.
	if tier, ok := MatchCandidateIdentityForBackfill(NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 0), candidate); ok {
		t.Fatalf("bare release name matched at tier %q; size corroboration is required", tier)
	}

	// A candidate with no size cannot corroborate either.
	if _, ok := MatchCandidateIdentityForBackfill(NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 8_000_000_000), NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 0)); ok {
		t.Fatal("candidate with an unknown size corroborated a name")
	}

	// A materially different size is not the same release.
	if _, ok := MatchCandidateIdentityForBackfill(NewPersistedIdentityTiers("", "", "Movie.2024.1080p.WEB-DL.x264", 4_000_000_000), candidate); ok {
		t.Fatal("a half-size release corroborated the name")
	}

	// A different name never matches, even with a plausible size.
	if _, ok := MatchCandidateIdentityForBackfill(NewPersistedIdentityTiers("", "", "Other.Movie.2025.1080p", 8_000_000_000), candidate); ok {
		t.Fatal("a different release name matched")
	}
}

// TestMatchCandidateIdentityForBackfillAdoptsStrongerCandidateTier proves the
// #145 asymmetry carries into the backfill: a name-only row may adopt the hash
// or GUID a fresh candidate offers for the same release, which is exactly the
// point of the backfill (a value the row never had becomes durable).
func TestMatchCandidateIdentityForBackfillAdoptsStrongerCandidateTier(t *testing.T) {
	candidate := NewPersistedIdentityTiers("hash-1", "guid-1", "Movie.2024.1080p", 8_000_000_000)
	identity := NewPersistedIdentityTiers("", "", "Movie.2024.1080p", 8_000_000_000)

	tier, ok := MatchCandidateIdentityForBackfill(identity, candidate)
	if !ok || tier != "release_name" {
		t.Fatalf("name-only row against stronger candidate = %q ok=%v, want release_name", tier, ok)
	}
}

// TestMatchCandidateIdentityForBackfillStrongerStoredTierVetoes proves a row
// that already carries a hash is not re-identified by a name coincidence when
// the listing cannot corroborate the hash. The backfill never weakens an
// existing identity.
func TestMatchCandidateIdentityForBackfillStrongerStoredTierVetoes(t *testing.T) {
	candidate := NewPersistedIdentityTiers("", "", "Movie.2024.1080p", 8_000_000_000)
	identity := NewPersistedIdentityTiers("stored-hash", "", "Movie.2024.1080p", 8_000_000_000)
	if _, ok := MatchCandidateIdentityForBackfill(identity, candidate); ok {
		t.Fatal("a stored hash the candidate could not corroborate was satisfied by the name tier")
	}

	identity = NewPersistedIdentityTiers("", "stored-guid", "Movie.2024.1080p", 8_000_000_000)
	if _, ok := MatchCandidateIdentityForBackfill(identity, candidate); ok {
		t.Fatal("a stored GUID the candidate could not corroborate was satisfied by the name tier")
	}
}
