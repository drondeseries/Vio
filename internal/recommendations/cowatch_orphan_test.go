package recommendations

import (
	"context"
	"testing"
)

// A co-watch row whose neighbor no longer exists in media_items is exactly the
// residue left when an item is deleted after the matrix was computed. It must
// never be returned to a caller.
func TestGetCowatchNeighborsDropsMissingItems(t *testing.T) {
	pool := newEngineTestPool(t)
	ctx := context.Background()

	const prefix = "t7cowatch-"
	cleanupRecoMediaItems(t, pool, prefix)

	sourceID := prefix + "source"
	existingID := prefix + "existing"
	missingID := prefix + "deleted"

	seedRecoMediaItem(t, pool, sourceID, "movie", "matched")
	seedRecoMediaItem(t, pool, existingID, "movie", "matched")

	// item_cowatch has no foreign key, so the missing neighbor can be seeded
	// directly. Cleanup after the media rows are removed.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM item_cowatch WHERE item_id = $1`, sourceID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO item_cowatch (item_id, similar_item_id, jaccard_score, cowatch_count)
		VALUES ($1, $2, 0.9, 10), ($1, $3, 0.8, 8)`,
		sourceID, existingID, missingID); err != nil {
		t.Fatalf("seed cowatch pairs: %v", err)
	}

	repo := NewRepo(pool)
	pairs, err := repo.GetCowatchNeighbors(ctx, sourceID, 10)
	if err != nil {
		t.Fatalf("GetCowatchNeighbors: %v", err)
	}

	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v, want only the existing neighbor", pairs)
	}
	if pairs[0].SimilarItemID != existingID {
		t.Fatalf("returned neighbor = %q, want %q", pairs[0].SimilarItemID, existingID)
	}
}

func TestExistingItemIDsSkipsMissing(t *testing.T) {
	pool := newEngineTestPool(t)
	ctx := context.Background()

	const prefix = "t7exists-"
	cleanupRecoMediaItems(t, pool, prefix)

	existingID := prefix + "existing"
	missingID := prefix + "missing"
	seedRecoMediaItem(t, pool, existingID, "movie", "matched")

	repo := NewRepo(pool)
	existing, err := repo.ExistingItemIDs(ctx, []string{existingID, missingID})
	if err != nil {
		t.Fatalf("ExistingItemIDs: %v", err)
	}
	if _, ok := existing[existingID]; !ok {
		t.Fatalf("existing id %q absent from %+v", existingID, existing)
	}
	if _, ok := existing[missingID]; ok {
		t.Fatalf("missing id %q present in %+v", missingID, existing)
	}
}
