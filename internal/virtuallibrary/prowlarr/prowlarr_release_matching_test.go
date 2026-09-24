package prowlarr

import (
	"strings"
	"testing"
)

// The strict cross-provider matcher is the dedup gate for the "not downloaded"
// list, so these cases pin exactly which releases it hides. It must not use the
// weaker title+year fallback (prowlarrReleaseConfirmsCandidate) that exists to
// reorder playback.

func TestIndexerReleaseMatchesProviderExactName(t *testing.T) {
	release := SearchItem{
		Title: "The.Movie.2024.1080p.WEB-DL.x264-GROUP",
		Size:  8_000_000_000,
	}
	if !IndexerReleaseMatchesProvider(release, "The Movie 2024 1080p WEB-DL x264-GROUP", 8_000_000_000) {
		t.Error("exact release-name identity with equal size should match")
	}
}

func TestIndexerReleaseMatchesProviderSizeTolerance(t *testing.T) {
	release := SearchItem{Title: "The.Movie.2024.1080p.BluRay.x264-GROUP", Size: 10_000_000_000}
	// 9% smaller is within the 10% tolerance.
	if !IndexerReleaseMatchesProvider(release, "The.Movie.2024.1080p.BluRay.x264-GROUP", 9_100_000_000) {
		t.Error("sizes within 10% should match")
	}
	// 50% smaller describes a different file.
	if IndexerReleaseMatchesProvider(release, "The.Movie.2024.1080p.BluRay.x264-GROUP", 5_000_000_000) {
		t.Error("sizes differing by 50% must not match")
	}
}

func TestIndexerReleaseMatchesProviderUnknownSizes(t *testing.T) {
	release := SearchItem{Title: "The.Movie.2024.720p.WEBRip.x264-GROUP", Size: 0}
	// Either side unknown: name identity alone is enough to dedup.
	if !IndexerReleaseMatchesProvider(release, "The Movie 2024 720p WEBRip x264-GROUP", 0) {
		t.Error("unknown sizes should not reject an exact name match")
	}
	if !IndexerReleaseMatchesProvider(release, "The Movie 2024 720p WEBRip x264-GROUP", 2_000_000_000) {
		t.Error("a provider size with an unknown release size should still match by name")
	}
}

func TestIndexerReleaseMatchesProviderRejectsDifferentName(t *testing.T) {
	release := SearchItem{Title: "The.Movie.2024.1080p.WEB-DL.x264-GROUP", Size: 8_000_000_000}
	if IndexerReleaseMatchesProvider(release, "The.Movie.2024.2160p.BluRay.x265-OTHER", 8_000_000_000) {
		t.Error("a different release name must not match even at the same size")
	}
}

// The weak fallback matches on normalized title+year with compatible sizes; the
// strict matcher must not, or a title with many quality tiers would hide every
// tier behind one provider release.
func TestIndexerReleaseMatchesProviderIgnoresWeakTitleFallback(t *testing.T) {
	prowlarrRelease := SearchItem{Title: "Some Movie 2024 1080p WEB-DL x264-GROUP", Size: 8_000_000_000}
	// Same normalized title ("some movie", year 2024) and same size, but a
	// different scene name. prowlarrReleaseConfirmsCandidate would accept this.
	candidate := StreamCandidate{
		Name:     "Some Movie 2024 1080p WEB-DL x264-GROUP",
		Title:    "Some Movie 2024 1080p WEB-DL x264-GROUP",
		FileSize: 8_000_000_000,
	}
	if !prowlarrReleaseConfirmsCandidate(prowlarrRelease, candidate) {
		t.Fatal("precondition: weak matcher should confirm the identical name")
	}
	// A distinct release with the same normalized title and size must be hidden
	// from the provider by the weak matcher (same title), so verify the strict
	// matcher rejects when names differ but normalized titles agree.
	differentName := SearchItem{Title: "Some.Movie.2024.1080p.WEB-DL.x264-OTHERGROUP", Size: 8_000_000_000}
	normalized, _ := normalizeReleaseTitle(differentName.Title)
	want, _ := normalizeReleaseTitle(prowlarrRelease.Title)
	if normalized != want {
		t.Fatalf("precondition: normalized titles differ (%q vs %q)", normalized, want)
	}
	if IndexerReleaseMatchesProvider(differentName, "Some Movie 2024 1080p WEB-DL x264-GROUP", 8_000_000_000) {
		t.Error("strict matcher must not accept a different release name on a title-only basis")
	}
}

func TestIndexerReleaseMatchesProviderEmptyNames(t *testing.T) {
	release := SearchItem{Title: "", Size: 1}
	if IndexerReleaseMatchesProvider(release, "anything", 1) {
		t.Error("an empty release name must not match")
	}
	if IndexerReleaseMatchesProvider(SearchItem{Title: "something", Size: 1}, "", 1) {
		t.Error("an empty provider name must not match")
	}
}

func TestParseProwlarrSearchReadsProtocol(t *testing.T) {
	body := `[
		{"guid":"a","title":"A","size":1,"protocol":"usenet","indexer":"idx","indexerId":7},
		{"guid":"b","title":"B","size":2,"protocol":"torrent","indexer":"idx","indexerId":7}
	]`
	releases, err := parseProwlarrSearch(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseProwlarrSearch: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(releases))
	}
	if releases[0].Protocol != "usenet" {
		t.Errorf("protocol = %q, want usenet (JSON key %q)", releases[0].Protocol, "protocol")
	}
	if releases[1].Protocol != "torrent" {
		t.Errorf("protocol = %q, want torrent", releases[1].Protocol)
	}
}
