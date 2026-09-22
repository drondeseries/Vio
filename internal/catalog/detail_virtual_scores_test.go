package catalog

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestAttachVirtualCandidateScoresStampsMatchingVersions(t *testing.T) {
	var gotContentID string
	svc := &DetailService{virtualScoreSource: func(_ context.Context, contentID string, _ []*models.MediaFile) map[int]int {
		gotContentID = contentID
		return map[int]int{7: 850}
	}}

	versions := []FileVersion{{FileID: 7}, {FileID: 8}}
	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, []*models.MediaFile{{ID: 7}})

	if gotContentID != "movie:heat" {
		t.Fatalf("scorer content id = %q, want movie:heat", gotContentID)
	}
	if versions[0].FormatScore == nil || *versions[0].FormatScore != 850 {
		t.Fatalf("scored version score = %v, want 850", versions[0].FormatScore)
	}
	if versions[1].FormatScore != nil {
		t.Fatalf("unscored version score = %v, want nil", versions[1].FormatScore)
	}
}

func TestAttachVirtualCandidateScoresWithoutSourceIsNoop(t *testing.T) {
	svc := &DetailService{}
	versions := []FileVersion{{FileID: 7}}

	svc.attachVirtualCandidateScores(context.Background(), "movie:heat", versions, nil)

	if versions[0].FormatScore != nil {
		t.Fatalf("score = %v, want nil when no source is wired", versions[0].FormatScore)
	}
}
