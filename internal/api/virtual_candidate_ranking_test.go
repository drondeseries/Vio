package api

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
)

// fakeRankingResolver returns a ranking per listing path and records the paths
// it was asked to resolve.
type fakeRankingResolver struct {
	byPath map[string]virtuallibrary.VirtualRanking
	paths  []string
}

func (f *fakeRankingResolver) RankingForPath(virtualPath string) virtuallibrary.VirtualRanking {
	f.paths = append(f.paths, virtualPath)
	if ranking, ok := f.byPath[virtualPath]; ok {
		return ranking
	}
	return virtuallibrary.VirtualRanking{Source: "default", Criteria: []quality.SortCriterion{{Attribute: "score", Direction: "desc"}}}
}

// TestVirtualCandidateRankingSourceMapsByListing proves the ranking is keyed by
// file ID, derived per listing (profile selector kept, result stripped), and
// shared by every file of one listing.
func TestVirtualCandidateRankingSourceMapsByListing(t *testing.T) {
	profilePath := "virtual://movie/tt1?profile=4k"
	resolver := &fakeRankingResolver{byPath: map[string]virtuallibrary.VirtualRanking{
		profilePath: {
			ProfileLabel: "4K HDR", Source: "profile",
			Criteria: []quality.SortCriterion{{Attribute: "size", Direction: "desc"}},
		},
	}}
	source := virtualCandidateRankingSource(resolver)

	files := []*models.MediaFile{
		{ID: 42, FilePath: "virtual://movie/tt1?profile=4k&result=abc"},
		{ID: 43, FilePath: "virtual://movie/tt1?profile=4k&result=def"},
		{ID: 44, FilePath: "/media/Movies/Heat.mkv"},
	}
	rankings := source(context.Background(), "movie:heat", files)

	if len(resolver.paths) != 1 || resolver.paths[0] != profilePath {
		t.Fatalf("resolved paths = %v, want [%s]", resolver.paths, profilePath)
	}
	if len(rankings) != 2 {
		t.Fatalf("rankings = %v, want the two virtual files", rankings)
	}
	for _, id := range []int{42, 43} {
		ranking, ok := rankings[id]
		if !ok {
			t.Fatalf("file %d has no ranking", id)
		}
		if ranking.ProfileLabel != "4K HDR" || ranking.Source != "profile" {
			t.Fatalf("file %d ranking = %+v", id, ranking)
		}
		if len(ranking.Criteria) != 1 || ranking.Criteria[0].Attribute != "size" {
			t.Fatalf("file %d criteria = %+v", id, ranking.Criteria)
		}
	}
	if _, ok := rankings[44]; ok {
		t.Fatalf("local file was ranked: %v", rankings)
	}
}

// TestVirtualCandidateRankingSourceResolvesEachListingOnce proves two profile
// selectors on one item resolve independently.
func TestVirtualCandidateRankingSourceResolvesEachListingOnce(t *testing.T) {
	resolver := &fakeRankingResolver{byPath: map[string]virtuallibrary.VirtualRanking{
		"virtual://movie/tt1?profile=4k": {ProfileLabel: "4K", Source: "profile"},
	}}
	source := virtualCandidateRankingSource(resolver)

	files := []*models.MediaFile{
		{ID: 1, FilePath: "virtual://movie/tt1?profile=4k&result=a"},
		{ID: 2, FilePath: "virtual://movie/tt1?profile=4k&result=b"},
		{ID: 3, FilePath: "virtual://movie/tt1?profile=fhd&result=c"},
	}
	rankings := source(context.Background(), "movie:heat", files)

	if len(resolver.paths) != 2 {
		t.Fatalf("resolved paths = %v, want one per listing", resolver.paths)
	}
	if len(rankings) != 3 {
		t.Fatalf("rankings = %v, want all three files", rankings)
	}
	if rankings[3].Source != "default" || rankings[3].ProfileLabel != "" {
		t.Fatalf("unconfigured profile ranking = %+v, want the default order", rankings[3])
	}
}

// TestVirtualCandidateRankingSourceSkipsNonVirtualFiles proves a fully local
// item produces no ranking projection.
func TestVirtualCandidateRankingSourceSkipsNonVirtualFiles(t *testing.T) {
	resolver := &fakeRankingResolver{}
	source := virtualCandidateRankingSource(resolver)

	if rankings := source(context.Background(), "movie:heat", []*models.MediaFile{{ID: 1, FilePath: "/media/Heat.mkv"}}); rankings != nil {
		t.Fatalf("rankings = %v, want nil", rankings)
	}
	if len(resolver.paths) != 0 {
		t.Fatalf("resolved paths = %v, want none", resolver.paths)
	}
}

// TestVirtualCandidateRankingSourceNilIsNil proves an unwired service yields a
// nil source rather than a panic.
func TestVirtualCandidateRankingSourceNilIsNil(t *testing.T) {
	if source := virtualCandidateRankingSource(nil); source != nil {
		t.Fatalf("source = %v, want nil", source)
	}
}
