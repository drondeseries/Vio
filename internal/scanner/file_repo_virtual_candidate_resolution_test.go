package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

// seedVirtualResolutionFixture creates a throwaway library and item and returns
// a source row plus the folder id. It is scoped per-test by a unique suffix so
// the destructive cleanup cannot touch another suite's rows.
func seedVirtualResolutionFixture(t *testing.T, pool *pgxpool.Pool, name string) (*models.MediaFile, int, string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-resolve-%s-%d", name, suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Resolve %s %d", name, suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Resolve Fixture','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}
	return &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 5,
	}, folderID, basePath
}

func virtualResolutionTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestReplaceVirtualCandidatesPersistsResolvedResolution proves a replace
// writes the provider URL, its expiry, and the durable identity tiers onto the
// candidate row without touching the neutral `?result=` listing identity.
func TestReplaceVirtualCandidatesPersistsResolvedResolution(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	ctx := context.Background()
	source, _, basePath := seedVirtualResolutionFixture(t, pool, "persist")
	candidatePath := basePath + "&result=persist"
	expiresAt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	repo := NewFileRepository(pool)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                  candidatePath,
		Label:                "2160p",
		FileSize:             8_000_000_000,
		ResolvedURL:          "https://provider.example/movie.mkv?token=abc",
		ResolvedURLExpiresAt: &expiresAt,
		ProviderVideoHash:    "HASH123",
		ProviderGUID:         "guid-release-1",
		ProviderReleaseName:  "movie20232160p",
		ProviderReleaseSize:  8_000_000_000,
	}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var (
		filePath  string
		url       *string
		urlExpiry *time.Time
		hash      *string
		guid      *string
		name      *string
		size      *int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT file_path, resolved_url, resolved_url_expires_at,
		       provider_video_hash, provider_guid, provider_release_name, provider_release_size
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5 AND file_path=$2`,
		source.ContentID, candidatePath,
	).Scan(&filePath, &url, &urlExpiry, &hash, &guid, &name, &size); err != nil {
		t.Fatalf("inspect candidate row: %v", err)
	}
	if filePath != candidatePath {
		t.Fatalf("file_path = %q, want the neutral ?result= listing identity %q", filePath, candidatePath)
	}
	if url == nil || *url != "https://provider.example/movie.mkv?token=abc" {
		t.Fatalf("resolved_url = %v, want the provider URL", url)
	}
	if urlExpiry == nil || !urlExpiry.Equal(expiresAt) {
		t.Fatalf("resolved_url_expires_at = %v, want %v", urlExpiry, expiresAt)
	}
	if hash == nil || *hash != "HASH123" || guid == nil || *guid != "guid-release-1" {
		t.Fatalf("hash=%v guid=%v, want HASH123/guid-release-1", hash, guid)
	}
	if name == nil || *name != "movie20232160p" {
		t.Fatalf("provider_release_name = %v, want movie20232160p", name)
	}
	if size == nil || *size != 8_000_000_000 {
		t.Fatalf("provider_release_size = %v, want 8000000000", size)
	}
}

// TestReplaceVirtualCandidatesReListPreservesStoredResolution proves a re-list
// that no longer reports the URL or identity preserves the last stored values
// instead of erasing them. Only a newer successful resolution may overwrite.
func TestReplaceVirtualCandidatesReListPreservesStoredResolution(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	ctx := context.Background()
	source, _, basePath := seedVirtualResolutionFixture(t, pool, "preserve")
	candidatePath := basePath + "&result=preserve"
	firstExpiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	repo := NewFileRepository(pool)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                  candidatePath,
		Label:                "1080p",
		FileSize:             4_000_000_000,
		ResolvedURL:          "https://provider.example/first.mkv?token=one",
		ResolvedURLExpiresAt: &firstExpiry,
		ProviderVideoHash:    "FIRSTHASH",
		ProviderGUID:         "first-guid",
		ProviderReleaseName:  "movie20231080p",
		ProviderReleaseSize:  4_000_000_000,
	}}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// The re-list carries no URL and no identity for the same listing identity.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:      candidatePath,
		Label:    "1080p",
		FileSize: 4_000_000_000,
	}}); err != nil {
		t.Fatalf("re-list replace: %v", err)
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
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5 AND file_path=$2`,
		source.ContentID, candidatePath,
	).Scan(&url, &urlExpiry, &hash, &guid, &name, &size); err != nil {
		t.Fatalf("inspect re-listed row: %v", err)
	}
	if url == nil || *url != "https://provider.example/first.mkv?token=one" {
		t.Fatalf("resolved_url = %v, want the preserved first URL", url)
	}
	if urlExpiry == nil || !urlExpiry.Equal(firstExpiry) {
		t.Fatalf("resolved_url_expires_at = %v, want preserved %v", urlExpiry, firstExpiry)
	}
	if hash == nil || *hash != "FIRSTHASH" || guid == nil || *guid != "first-guid" {
		t.Fatalf("identity erased: hash=%v guid=%v", hash, guid)
	}
	if name == nil || *name != "movie20231080p" || size == nil || *size != 4_000_000_000 {
		t.Fatalf("release identity erased: name=%v size=%v", name, size)
	}
}

// TestReplaceVirtualCandidatesIdentityFallsBackToNameAndSize proves a stream
// with no video hash and no source GUID still persists a durable identity: the
// release name and size, leaving the stronger tiers NULL.
func TestReplaceVirtualCandidatesIdentityFallsBackToNameAndSize(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	ctx := context.Background()
	source, _, basePath := seedVirtualResolutionFixture(t, pool, "fallback")
	candidatePath := basePath + "&result=fallback"

	repo := NewFileRepository(pool)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                 candidatePath,
		Label:               "1080p",
		FileSize:            3_000_000_000,
		ProviderReleaseName: "movie20231080p",
		ProviderReleaseSize: 3_000_000_000,
	}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var (
		hash *string
		guid *string
		name *string
		size *int64
	)
	if err := pool.QueryRow(ctx, `
		SELECT provider_video_hash, provider_guid, provider_release_name, provider_release_size
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5 AND file_path=$2`,
		source.ContentID, candidatePath,
	).Scan(&hash, &guid, &name, &size); err != nil {
		t.Fatalf("inspect fallback row: %v", err)
	}
	if hash != nil || guid != nil {
		t.Fatalf("hash=%v guid=%v, want NULL when the stream carries neither", hash, guid)
	}
	if name == nil || *name != "movie20231080p" {
		t.Fatalf("provider_release_name = %v, want the name+size fallback identity", name)
	}
	if size == nil || *size != 3_000_000_000 {
		t.Fatalf("provider_release_size = %v, want 3000000000", size)
	}
}

// TestVirtualCandidateResolutionReadPath proves the catalog read the resume
// path uses (GetByPath) exposes the persisted resolution fields on MediaFile.
func TestVirtualCandidateResolutionReadPath(t *testing.T) {
	pool := virtualResolutionTestPool(t)
	ctx := context.Background()
	source, _, basePath := seedVirtualResolutionFixture(t, pool, "read")
	candidatePath := basePath + "&result=read"
	expiresAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	repo := NewFileRepository(pool)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                  candidatePath,
		Label:                "1080p",
		FileSize:             1_000_000_000,
		ResolvedURL:          "https://provider.example/read.mkv?token=read",
		ResolvedURLExpiresAt: &expiresAt,
		ProviderGUID:         "read-guid",
		ProviderReleaseName:  "movie20231080p",
		ProviderReleaseSize:  1_000_000_000,
	}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	file, err := repo.GetByPath(ctx, candidatePath)
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if file.ResolvedURL != "https://provider.example/read.mkv?token=read" {
		t.Fatalf("ResolvedURL = %q, want the persisted URL", file.ResolvedURL)
	}
	if file.ResolvedURLExpiresAt == nil || !file.ResolvedURLExpiresAt.Equal(expiresAt) {
		t.Fatalf("ResolvedURLExpiresAt = %v, want %v", file.ResolvedURLExpiresAt, expiresAt)
	}
	if file.ProviderGUID != "read-guid" || file.ProviderReleaseName != "movie20231080p" || file.ProviderReleaseSize != 1_000_000_000 {
		t.Fatalf("identity not exposed on read: guid=%q name=%q size=%d",
			file.ProviderGUID, file.ProviderReleaseName, file.ProviderReleaseSize)
	}
}
