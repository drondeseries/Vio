package prowlarr

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func preparedRelease(guid, title string, size int64) prowlarrRelease {
	release := prowlarrRelease{
		GUID:        guid,
		Title:       title,
		Size:        size,
		DownloadURL: "https://indexer.example/download/" + guid,
	}
	prepareProwlarrRelease(&release)
	return release
}

// referenceClassify is the original candidates x releases linear scan, kept as
// the ground truth the indexed classifier must reproduce. It is deliberately
// unoptimized: prowlarrReleaseConfirmsCandidate recomputes every derived field.
func referenceClassify(candidates []StreamCandidate, releases []prowlarrRelease) []StreamCandidate {
	out := make([]StreamCandidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		for j := range releases {
			if prowlarrReleaseConfirmsCandidate(releases[j], out[i]) {
				out[i].SourceConfirmed = true
				if strings.TrimSpace(out[i].SourceGUID) == "" && releases[j].GUID != "" {
					out[i].SourceGUID = releases[j].GUID
				}
				break
			}
		}
	}
	return out
}

// TestClassifyCandidatesMatchesLinearReference pins the indexed classifier to the
// original linear scan across exact matches, the weak title fallback, a size
// mismatch, a year mismatch, an unmatched candidate, and a candidate whose
// existing GUID must survive.
func TestClassifyCandidatesMatchesLinearReference(t *testing.T) {
	releases := []prowlarrRelease{
		preparedRelease("weak-guid", "Some.Movie.2024.2160p.BluRay.x265-OTHER", 8_000_000_000),
		preparedRelease("exact-guid", "Some.Movie.2024.1080p.WEB-DL.x264-GRP", 8_000_000_000),
		preparedRelease("another-guid", "Another.Film.2021.1080p.WEB-DL.x264-GRP", 4_000_000_000),
		preparedRelease("third-guid", "Third.Film.2019.720p.WEBRip.x264-GRP", 2_000_000_000),
		preparedRelease("fourth-guid", "Fourth.Film.2018.1080p.WEB-DL.x264-GRP", 3_000_000_000),
	}
	candidates := []StreamCandidate{
		// Weak title match at index 0 must win over the exact match at index 1:
		// the linear scan returns the earliest confirming release.
		{Title: "Some Movie 2024 1080p WEB-DL x264-GRP", FileSize: 8_000_000_000},
		{Title: "Another Film 2021 1080p WEB-DL x264-GRP", FileSize: 4_000_000_000, SourceGUID: "altmount:keepme"},
		// Same normalized title as third-guid but a distinct release name and a
		// compatible size: the weak fallback confirms it.
		{Title: "Third Film 2019 720p WEBRip x264-ALT", FileSize: 2_000_000_000},
		// Year differs from fourth-guid: the weak fallback must not confirm.
		{Title: "Fourth Film 2017 1080p WEB-DL x264-GRP", FileSize: 3_000_000_000},
		{Title: "Unknown Movie 2024 1080p WEB-DL x264-GRP", FileSize: 1_000_000_000},
	}

	want := referenceClassify(candidates, releases)
	client := &prowlarrSearchClient{releases: releases}
	got := append([]StreamCandidate(nil), candidates...)
	client.ClassifyCandidates(got)

	for i := range got {
		if got[i].SourceConfirmed != want[i].SourceConfirmed {
			t.Errorf("candidate %d SourceConfirmed = %v, want %v", i, got[i].SourceConfirmed, want[i].SourceConfirmed)
		}
		if got[i].SourceGUID != want[i].SourceGUID {
			t.Errorf("candidate %d SourceGUID = %q, want %q", i, got[i].SourceGUID, want[i].SourceGUID)
		}
	}
	// Pin the ordering rule explicitly so a future "prefer exact" rewrite fails
	// here rather than silently moving a row's durable GUID.
	if got[0].SourceGUID != "weak-guid" {
		t.Errorf("earliest weak match should win: SourceGUID = %q, want weak-guid", got[0].SourceGUID)
	}
	if !got[1].SourceConfirmed || got[1].SourceGUID != "altmount:keepme" {
		t.Errorf("existing GUID must survive: confirmed=%v GUID=%q", got[1].SourceConfirmed, got[1].SourceGUID)
	}
}

// TestClassifyCandidatesBoundedOnLargeSnapshot guards the regression that made a
// 500 x 20000 snapshot take ~14s per list and resolve: the classifier must stay
// far below the old quadratic cost. The setup is outside the timed window.
func TestClassifyCandidatesBoundedOnLargeSnapshot(t *testing.T) {
	releases, candidates := largeClassificationFixture()

	client := &prowlarrSearchClient{releases: releases}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		client.ClassifyCandidates(candidates)
		close(done)
	}()

	const budget = 3 * time.Second
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > budget {
			t.Fatalf("classify took %s, want under %s", elapsed, budget)
		}
	case <-time.After(budget):
		t.Fatalf("classify did not finish within %s on %d candidates x %d releases", budget, len(candidates), len(releases))
	}
}

// BenchmarkClassifyCandidatesLargeSnapshot measures the steady-state cost of the
// cache-hit path the resolver calls on every list and resolve.
func BenchmarkClassifyCandidatesLargeSnapshot(b *testing.B) {
	releases, candidates := largeClassificationFixture()
	client := &prowlarrSearchClient{releases: releases}
	// Prime the cached index so the benchmark isolates the per-serve work.
	client.ClassifyCandidates(candidates)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range candidates {
			candidates[j].SourceConfirmed = false
		}
		client.ClassifyCandidates(candidates)
	}
}

// largeClassificationFixture builds the 500 candidates x 20000 releases scale the
// production snapshot reached. Releases carry the identity fields
// prepareProwlarrRelease derives but skip the stream parse the classifier never
// reads, keeping fixture setup cheap. The last release is an exact match for the
// first candidate so the run exercises a real confirmation, not only misses.
func largeClassificationFixture() ([]prowlarrRelease, []StreamCandidate) {
	const releaseCount = 20000
	const candidateCount = 500

	releases := make([]prowlarrRelease, 0, releaseCount)
	for i := 0; i < releaseCount; i++ {
		releases = append(releases, identityOnlyRelease(
			fmt.Sprintf("guid-%d", i),
			fmt.Sprintf("Catalog.Release.%05d.2024.1080p.WEB-DL.x264-GRP", i),
			5_000_000_000,
		))
	}
	// Replace the last release with an exact match for candidate 0.
	releases[releaseCount-1] = identityOnlyRelease("guid-target", "Target.Movie.2024.1080p.WEB-DL.x264-GRP", 7_000_000_000)

	candidates := make([]StreamCandidate, 0, candidateCount)
	candidates = append(candidates, StreamCandidate{
		Title:    "Target Movie 2024 1080p WEB-DL x264-GRP",
		FileSize: 7_000_000_000,
	})
	for i := 1; i < candidateCount; i++ {
		candidates = append(candidates, StreamCandidate{
			Title:    fmt.Sprintf("Unmatched.Candidate.%03d.2024.1080p.WEB-DL.x264-GRP", i),
			FileSize: 1_000_000_000,
		})
	}
	return releases, candidates
}

// identityOnlyRelease sets the fields prepareProwlarrRelease precomputes without
// running the stream parse; the classifier reads only these identity fields.
func identityOnlyRelease(guid, title string, size int64) prowlarrRelease {
	release := prowlarrRelease{GUID: guid, Title: title, Size: size, DownloadURL: "https://indexer.example/download/" + guid}
	release.normalizedTitle, release.normalizedYear = normalizeReleaseTitle(title)
	release.nameKey = releaseNameKey(title)
	return release
}
