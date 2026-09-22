package catalog

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func testVirtualRanking() VirtualRanking {
	return VirtualRanking{
		ProfileLabel: "4K HDR",
		Source:       VirtualRankingSourceProfile,
		Criteria: []VirtualRankingCriterion{
			{Attribute: "score", Direction: "desc"},
			{Attribute: "resolution", Direction: "desc"},
		},
	}
}

// TestAttachVirtualRankingStampsDetailAndVariant proves the ranking is
// projected at the item level and on each playback variant whose versions came
// from the same virtual listing.
func TestAttachVirtualRankingStampsDetailAndVariant(t *testing.T) {
	var gotContentID string
	ranking := testVirtualRanking()
	svc := &DetailService{virtualRankingSource: func(_ context.Context, contentID string, _ []*models.MediaFile) map[int]VirtualRanking {
		gotContentID = contentID
		return map[int]VirtualRanking{42: ranking}
	}}

	detail := &WatchDetail{
		Versions: []FileVersion{{FileID: 42}, {FileID: 43}},
		PlaybackVariants: []PlaybackVariant{{
			VariantID: "v1",
			Parts:     []PlaybackVariantPart{{PartIndex: 1, Versions: []FileVersion{{FileID: 43}, {FileID: 42}}}},
		}},
	}
	svc.attachVirtualRanking(context.Background(), "movie:heat", detail, []*models.MediaFile{{ID: 42}})

	if gotContentID != "movie:heat" {
		t.Fatalf("ranking content id = %q, want movie:heat", gotContentID)
	}
	if detail.VirtualRanking == nil || detail.VirtualRanking.ProfileLabel != "4K HDR" || detail.VirtualRanking.Source != VirtualRankingSourceProfile {
		t.Fatalf("item ranking = %+v, want the profile ranking", detail.VirtualRanking)
	}
	if len(detail.VirtualRanking.Criteria) != 2 || detail.VirtualRanking.Criteria[0].Attribute != "score" {
		t.Fatalf("item criteria = %+v", detail.VirtualRanking.Criteria)
	}
	variantRanking := detail.PlaybackVariants[0].VirtualRanking
	if variantRanking == nil || variantRanking.ProfileLabel != "4K HDR" {
		t.Fatalf("variant ranking = %+v, want the shared listing ranking", variantRanking)
	}
}

// TestAttachVirtualRankingPicksFirstRankedVersion proves an unranked file at
// the head of the version list does not suppress the ranking carried by a
// later version of the same listing.
func TestAttachVirtualRankingPicksFirstRankedVersion(t *testing.T) {
	ranking := VirtualRanking{Source: VirtualRankingSourceDefault}
	svc := &DetailService{virtualRankingSource: func(context.Context, string, []*models.MediaFile) map[int]VirtualRanking {
		return map[int]VirtualRanking{9: ranking}
	}}
	detail := &WatchDetail{Versions: []FileVersion{{FileID: 8}, {FileID: 9}}}

	svc.attachVirtualRanking(context.Background(), "movie:heat", detail, nil)

	if detail.VirtualRanking == nil || detail.VirtualRanking.Source != VirtualRankingSourceDefault {
		t.Fatalf("ranking = %+v, want the later version's default ranking", detail.VirtualRanking)
	}
}

// TestAttachVirtualRankingWithoutSourceIsNoop proves a local item or an
// unwired source leaves the ranking absent for the omitempty contract.
func TestAttachVirtualRankingWithoutSourceIsNoop(t *testing.T) {
	svc := &DetailService{}
	detail := &WatchDetail{Versions: []FileVersion{{FileID: 7}}}

	svc.attachVirtualRanking(context.Background(), "movie:heat", detail, nil)

	if detail.VirtualRanking != nil {
		t.Fatalf("ranking = %+v, want nil with no source", detail.VirtualRanking)
	}
}
