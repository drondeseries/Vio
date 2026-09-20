package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedResolvedCandidateRow inserts a virtual candidate row carrying the
// persisted resolution fields so an adoption write can be tested against it.
func seedResolvedCandidateRow(t *testing.T, pool *pgxpool.Pool, folderID, ownerID int, contentID, candidatePath string, resolved models.MediaFile) (int, time.Time, *time.Time) {
	t.Helper()
	ctx := context.Background()
	var id int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(
			content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			resolved_url, resolved_url_expires_at,
			provider_video_hash, provider_guid, provider_release_name, provider_release_size
		)
		VALUES($1,$2,$3,'virtual',$4,'virtual',$5,$6,$7,$8,$9,$10)
		RETURNING id, updated_at, probe_updated_at`,
		contentID, folderID, candidatePath, ownerID,
		nullIfEmpty(resolved.ResolvedURL), resolved.ResolvedURLExpiresAt,
		nullIfEmpty(resolved.ProviderVideoHash), nullIfEmpty(resolved.ProviderGUID),
		nullIfEmpty(resolved.ProviderReleaseName), resolved.ProviderReleaseSize,
	).Scan(&id, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert resolved candidate row: %v", err)
	}
	return id, updatedAt, probeUpdatedAt
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// TestVirtualFileMetadataUpdateAdoptPreservesResolvedResolution proves a
// metadata-only adoption write (no resolution result carried) does not erase
// the persisted provider URL, expiry, or durable identity on the row.
func TestVirtualFileMetadataUpdateAdoptPreservesResolvedResolution(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994421, 7011
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-adopt-preserve?result=cand-a"
	expiresAt := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	id, updatedAt, probeUpdatedAt := seedResolvedCandidateRow(t, pool, folderID, ownerID, "movie-adopt-preserve", candidatePath, models.MediaFile{
		ResolvedURL:          "https://provider.example/first.mkv?token=one",
		ResolvedURLExpiresAt: &expiresAt,
		ProviderVideoHash:    "PRESERVEHASH",
		ProviderGUID:         "preserve-guid",
		ProviderReleaseName:  "movie20231080p",
		ProviderReleaseSize:  4_000_000_000,
	})

	args := models.VirtualFilePersistArgs{
		FileID:           id,
		ExpectedFilePath: candidatePath,
		UpdatedAt:        updatedAt,
		ProbeUpdatedAt:   probeUpdatedAt,
		OwnerID:          ownerID,
		LibraryID:        folderID,
		AdoptPath:        candidatePath,
		RequireAdopt:     true,
		HDR:              true,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if err != nil {
		t.Fatalf("adopt without resolution: %v", err)
	}
	if !result.MetadataUpdated {
		t.Fatal("adopt without resolution did not update the row")
	}

	var (
		url       *string
		urlExpiry *time.Time
		hash      *string
		guid      *string
		name      *string
		size      *int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT resolved_url, resolved_url_expires_at,
		       provider_video_hash, provider_guid, provider_release_name, provider_release_size
		FROM media_files WHERE id=$1`, id,
	).Scan(&url, &urlExpiry, &hash, &guid, &name, &size); err != nil {
		t.Fatalf("inspect adopted row: %v", err)
	}
	if url == nil || *url != "https://provider.example/first.mkv?token=one" {
		t.Fatalf("resolved_url erased by metadata-only adopt: %v", url)
	}
	if urlExpiry == nil || !urlExpiry.Equal(expiresAt) {
		t.Fatalf("resolved_url_expires_at erased: %v", urlExpiry)
	}
	if hash == nil || *hash != "PRESERVEHASH" || guid == nil || *guid != "preserve-guid" {
		t.Fatalf("identity erased by metadata-only adopt: hash=%v guid=%v", hash, guid)
	}
	if name == nil || *name != "movie20231080p" || size == nil || *size != 4_000_000_000 {
		t.Fatalf("release identity erased: name=%v size=%v", name, size)
	}
}

// TestVirtualFileMetadataUpdateAdoptRefreshesResolvedResolution proves a newer
// successful resolution overwrites the stored URL and identity in the same
// adoption write, including replacing the expiry that belonged to the old URL.
func TestVirtualFileMetadataUpdateAdoptRefreshesResolvedResolution(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994422, 7012
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-adopt-refresh?result=cand-a"
	oldExpiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	id, updatedAt, probeUpdatedAt := seedResolvedCandidateRow(t, pool, folderID, ownerID, "movie-adopt-refresh", candidatePath, models.MediaFile{
		ResolvedURL:          "https://provider.example/old.mkv?token=old",
		ResolvedURLExpiresAt: &oldExpiry,
		ProviderGUID:         "old-guid",
		ProviderReleaseName:  "movie20231080p",
		ProviderReleaseSize:  4_000_000_000,
	})

	newExpiry := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	args := models.VirtualFilePersistArgs{
		FileID:               id,
		ExpectedFilePath:     candidatePath,
		UpdatedAt:            updatedAt,
		ProbeUpdatedAt:       probeUpdatedAt,
		OwnerID:              ownerID,
		LibraryID:            folderID,
		AdoptPath:            candidatePath,
		RequireAdopt:         true,
		HDR:                  true,
		ResolvedURL:          "https://provider.example/new.mkv?token=new",
		ResolvedURLExpiresAt: &newExpiry,
		ProviderVideoHash:    "NEWHASH",
		ProviderGUID:         "new-guid",
		ProviderReleaseName:  "movie20232160p",
		ProviderReleaseSize:  8_000_000_000,
	}
	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args); err != nil {
		t.Fatalf("adopt with new resolution: %v", err)
	}

	var (
		url       *string
		urlExpiry *time.Time
		hash      *string
		guid      *string
		name      *string
		size      *int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT resolved_url, resolved_url_expires_at,
		       provider_video_hash, provider_guid, provider_release_name, provider_release_size
		FROM media_files WHERE id=$1`, id,
	).Scan(&url, &urlExpiry, &hash, &guid, &name, &size); err != nil {
		t.Fatalf("inspect refreshed row: %v", err)
	}
	if url == nil || *url != "https://provider.example/new.mkv?token=new" {
		t.Fatalf("resolved_url = %v, want the newer URL", url)
	}
	if urlExpiry == nil || !urlExpiry.Equal(newExpiry) {
		t.Fatalf("resolved_url_expires_at = %v, want %v (old expiry must not linger)", urlExpiry, newExpiry)
	}
	if hash == nil || *hash != "NEWHASH" || guid == nil || *guid != "new-guid" {
		t.Fatalf("identity not refreshed: hash=%v guid=%v", hash, guid)
	}
	if name == nil || *name != "movie20232160p" || size == nil || *size != 8_000_000_000 {
		t.Fatalf("release identity not refreshed: name=%v size=%v", name, size)
	}
}
