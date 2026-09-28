package scanner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedBackfillFile inserts one virtual media_files row for the backfill
// selection tests and returns its id. Identity fields are inserted verbatim so
// a test can pin a legacy (NULL hash/GUID) or already-identified row.
func seedBackfillFile(t *testing.T, pool *pgxpool.Pool, contentID string, folderID int, owner int, path string, hash, guid, releaseName string, releaseSize int64) int {
	t.Helper()
	ctx := context.Background()
	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files (
			content_id, media_folder_id, file_path, file_size, container,
			virtual_owner_installation_id,
			provider_video_hash, provider_guid, provider_release_name, provider_release_size
		) VALUES ($1,$2,$3,0,'virtual',$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8::bigint,0))
		RETURNING id`,
		contentID, folderID, path, owner, hash, guid, releaseName, releaseSize,
	).Scan(&id); err != nil {
		t.Fatalf("seed backfill file: %v", err)
	}
	return id
}

// TestListVirtualIdentityBackfillRowsSelectsOnlyLegacyRows pins the selection
// predicate: only virtual rows with an owner and neither a hash nor a GUID are
// eligible, and the keyset cursor is id-ordered.
func TestListVirtualIdentityBackfillRowsSelectsOnlyLegacyRows(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	repo := NewFileRepository(pool)
	source, folderID, base := seedVirtualResolutionFixture(t, pool, "identity-backfill-select")
	suffix := time.Now().UnixNano()
	contentID := source.ContentID

	legacyA := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=a-%d", base, suffix), "", "", "Movie.A.2024.1080p", 8_000_000_000)
	identified := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=b-%d", base, suffix), "hash-b", "", "Movie.B.2024.1080p", 4_000_000_000)
	guidOnly := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=c-%d", base, suffix), "", "guid-c", "Movie.C.2024.1080p", 2_000_000_000)
	legacyD := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=d-%d", base, suffix), "", "", "Movie.D.2024.1080p", 1_000_000_000)

	rows, err := repo.ListVirtualIdentityBackfillRows(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[int]bool{}
	for _, row := range rows {
		if row.ContentID == contentID {
			got[row.ID] = true
		}
	}
	if !got[legacyA] || !got[legacyD] {
		t.Fatalf("eligible rows missing: got=%v want %d,%d", got, legacyA, legacyD)
	}
	if got[identified] || got[guidOnly] {
		t.Fatalf("an already-identified row was selected: %v", got)
	}

	// The keyset cursor skips everything at or below it.
	after, err := repo.ListVirtualIdentityBackfillRows(context.Background(), legacyA, 100)
	if err != nil {
		t.Fatalf("list after cursor: %v", err)
	}
	for _, row := range after {
		if row.ContentID == contentID && row.ID <= legacyA {
			t.Fatalf("cursor returned row %d at or below the cursor", row.ID)
		}
	}

	count, err := repo.CountVirtualIdentityBackfillRows(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count < 2 {
		t.Fatalf("count = %d, want at least the two legacy rows", count)
	}
}

// TestFillVirtualProviderIdentityFillsOnlyEmptyTiers pins the write rule: a
// legacy row gains the confident tiers, an already-identified row is untouched,
// and an all-empty identity is a no-op.
func TestFillVirtualProviderIdentityFillsOnlyEmptyTiers(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	repo := NewFileRepository(pool)
	source, folderID, base := seedVirtualResolutionFixture(t, pool, "identity-backfill-fill")
	suffix := time.Now().UnixNano()
	contentID := source.ContentID
	ctx := context.Background()

	// A legacy row whose release name is already present but hash/GUID are not.
	legacy := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=legacy-%d", base, suffix), "", "", "Movie.Legacy.2024.1080p", 8_000_000_000)
	wrote, err := repo.FillVirtualProviderIdentity(ctx, legacy, fmt.Sprintf("%s?result=legacy-%d", base, suffix), VirtualProviderIdentity{
		VideoHash: "new-hash", GUID: "new-guid", ReleaseName: "Movie.Legacy.2024.1080p", ReleaseSize: 8_000_000_000,
	})
	if err != nil {
		t.Fatalf("fill legacy: %v", err)
	}
	if !wrote {
		t.Fatal("fill legacy wrote nothing")
	}
	var hash, guid, releaseName string
	var releaseSize int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(provider_video_hash,''), COALESCE(provider_guid,''), COALESCE(provider_release_name,''), COALESCE(provider_release_size,0) FROM media_files WHERE id=$1`, legacy).
		Scan(&hash, &guid, &releaseName, &releaseSize); err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if hash != "new-hash" || guid != "new-guid" || releaseName != "Movie.Legacy.2024.1080p" || releaseSize != 8_000_000_000 {
		t.Fatalf("legacy identity = %q/%q/%q/%d", hash, guid, releaseName, releaseSize)
	}

	// A second fill with different values must not overwrite the stored tiers.
	wrote, err = repo.FillVirtualProviderIdentity(ctx, legacy, fmt.Sprintf("%s?result=legacy-%d", base, suffix), VirtualProviderIdentity{
		VideoHash: "other-hash", GUID: "other-guid", ReleaseName: "Other.Release", ReleaseSize: 1,
	})
	if err != nil {
		t.Fatalf("second fill: %v", err)
	}
	if wrote {
		t.Fatal("a present identity was overwritten")
	}

	// An already-identified row is out of the legacy predicate entirely.
	identified := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=identified-%d", base, suffix), "stored-hash", "", "Movie.Identified.2024.1080p", 4_000_000_000)
	wrote, err = repo.FillVirtualProviderIdentity(ctx, identified, fmt.Sprintf("%s?result=identified-%d", base, suffix), VirtualProviderIdentity{VideoHash: "replace"})
	if err != nil {
		t.Fatalf("fill identified: %v", err)
	}
	if wrote {
		t.Fatal("an already-identified row was written")
	}

	// An empty identity has nothing to add and must be a no-op.
	empty := seedBackfillFile(t, pool, contentID, folderID, 5, fmt.Sprintf("%s?result=empty-%d", base, suffix), "", "", "", 0)
	wrote, err = repo.FillVirtualProviderIdentity(ctx, empty, fmt.Sprintf("%s?result=empty-%d", base, suffix), VirtualProviderIdentity{})
	if err != nil {
		t.Fatalf("fill empty: %v", err)
	}
	if wrote {
		t.Fatal("an empty identity was written")
	}
}
