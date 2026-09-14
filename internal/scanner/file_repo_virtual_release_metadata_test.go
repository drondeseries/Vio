package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestReplaceVirtualCandidatesDoesNotPolluteReleaseMetadata verifies that the
// virtual-candidate refresh keeps the provider display label out of
// release_name and release_group on both the initial insert and the
// conflict-update path. Virtual files have no filename to parse a release name
// from, so those columns stay empty (the column default); the label lives in
// edition_raw, the version flyout's display fallback — matching
// upsertVirtualFileVariant and the cleanup migration
// 20260908140000_cleanup_virtual_release_metadata.sql.
func TestReplaceVirtualCandidatesDoesNotPolluteReleaseMetadata(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-release-metadata-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	candidatePath := basePath + "&result=first"
	const label = "Release Name · 4K · Provider"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Virtual Release Metadata %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Virtual Release Metadata','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 7,
	}

	// Brand-new candidate: the label must not leak into release_name or
	// release_group; edition_raw is the designated display column.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: candidatePath, Label: label}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}
	assertVirtualReleaseMetadata(t, ctx, pool, contentID, folderID, candidatePath, label)

	// Refreshed (conflict-updated) candidate: same invariant holds.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: candidatePath, Label: label}}); err != nil {
		t.Fatalf("refresh virtual candidates: %v", err)
	}
	assertVirtualReleaseMetadata(t, ctx, pool, contentID, folderID, candidatePath, label)
}

func assertVirtualReleaseMetadata(t *testing.T, ctx context.Context, pool *pgxpool.Pool, contentID string, folderID int, candidatePath, label string) {
	t.Helper()
	var releaseName, releaseGroup, editionRaw string
	if err := pool.QueryRow(ctx, `
		SELECT release_name, release_group, edition_raw
		FROM media_files
		WHERE content_id=$1 AND media_folder_id=$2 AND file_path=$3`,
		contentID, folderID, candidatePath,
	).Scan(&releaseName, &releaseGroup, &editionRaw); err != nil {
		t.Fatalf("read virtual candidate metadata: %v", err)
	}
	if releaseName != "" {
		t.Fatalf("release_name = %q, want empty (display label must not pollute it)", releaseName)
	}
	if releaseGroup != "" {
		t.Fatalf("release_group = %q, want empty (display label must not pollute it)", releaseGroup)
	}
	if editionRaw != label {
		t.Fatalf("edition_raw = %q, want %q (designated display column)", editionRaw, label)
	}
}
