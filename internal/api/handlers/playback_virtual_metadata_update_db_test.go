package handlers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// virtualMetadataUpdateTestPool connects to the disposable migrated test
// database provisioned by TestMain and serializes fixture access with the same
// advisory lock the other destructive handler fixtures use. It skips cleanly
// when SILO_TEST_DATABASE_URL is unset.
func virtualMetadataUpdateTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("identify test database: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "purge") {
		t.Fatalf("refusing destructive handler fixture database %q", databaseName)
	}
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x53494c4f5f544553)); err != nil {
		lockConn.Release()
		t.Fatalf("lock test database fixtures: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x53494c4f5f544553))
		lockConn.Release()
	})
	return pool
}

// seedVirtualMetadataUpdateFolder inserts a throwaway library and returns its
// id together with a cleanup that removes every row the test created.
func seedVirtualMetadataUpdateFolder(t *testing.T, pool *pgxpool.Pool, folderID, ownerID int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM media_files WHERE virtual_owner_installation_id = $1`, ownerID); err != nil {
		t.Fatalf("reset virtual rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID); err != nil {
		t.Fatalf("reset library: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id, name, type, enabled) VALUES($1, 'probe evidence guard', 'movies', true)`, folderID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE virtual_owner_installation_id = $1`, ownerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})
}

// runVirtualMetadataUpdate executes the real VirtualFileMetadataUpdateSQL with
// the same argument shape the router saver uses.
func runVirtualMetadataUpdate(t *testing.T, pool *pgxpool.Pool, candidateID int, expectedPath, adoptPath string, ownerID, folderID int, updatedAt time.Time, probeUpdatedAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	tag, err := pool.Exec(ctx, VirtualFileMetadataUpdateSQL,
		`[]`, `[]`, `[]`,
		"2160p", "hevc", "eac3", "mkv", true, 8000000, 5400,
		candidateID, expectedPath, true,
		updatedAt, probeUpdatedAt, ownerID, folderID, adoptPath,
	)
	if err != nil {
		t.Fatalf("VirtualFileMetadataUpdateSQL: %v", err)
	}
	if got := tag.RowsAffected(); got != 1 {
		t.Fatalf("rows affected = %d, want 1", got)
	}
}

// Adopting a candidate path onto the requested row is the normal case: with no
// sibling owning the resolved path, file_path moves and the evidence lands.
func TestVirtualFileMetadataUpdateAdoptsPathWithoutSibling(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994311, 7001
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-adopt-control"
	candidatePath := adoptPath + "?result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-adopt-control', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != adoptPath {
		t.Fatalf("file_path = %q, want adopted %q", path, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped")
	}
}

// When a sibling row (same virtual owner and library) already owns the resolved
// path, the update must not adopt it: the unique index would reject the write and
// drop the probe evidence. The row keeps its path but the evidence and stamp
// still land.
func TestVirtualFileMetadataUpdateSkipsAdoptionWhenSiblingOwnsPath(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994312, 7002
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-guard"
	candidatePath := adoptPath + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-guard', $1, $2, 'virtual', $3, 'virtual')`, folderID, adoptPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-guard', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != candidatePath {
		t.Fatalf("file_path = %q, want unchanged candidate %q (sibling owns %q)", path, candidatePath, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted despite skipped adoption", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped despite skipped adoption")
	}
}
