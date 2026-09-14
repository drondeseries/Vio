package catalog

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestVirtualMediaRotationPreservesFileID requires a migrated database via
// SILO_TEST_DATABASE_URL. It proves that re-registering the same virtual movie
// with a fresh provider result hash updates the existing media_files row in
// place — keeping the stable ID clients cache — instead of inserting a
// sibling version that invalidates them.
func TestVirtualMediaRotationPreservesFileID(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	r := NewVirtualMediaRegistrar(pool)
	libID := ensureVirtualTestLibrary(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, libID) })

	movie := VirtualMedia{
		MediaType:      "movie",
		Source:         "test-source",
		LibraryID:      strconv.Itoa(libID),
		Title:          "Rotation Probe Film",
		IMDbID:         "tt99999991",
		RuntimeMinutes: 100,
		VirtualURI:     "virtual://movie/tt99999991?result=hash-one",
		Container:      "mkv",
		Resolution:     "2160p",
		CodecVideo:     "hevc",
		CodecAudio:     "truehd",
	}

	first, err := r.UpsertVirtualMedia(ctx, 0, movie)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var firstFileID int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM media_files WHERE content_id=$1 ORDER BY id`, first.MediaID).Scan(&firstFileID); err != nil {
		t.Fatalf("load first file row: %v", err)
	}

	// Provider rotates: same release, new result hash.
	movie.VirtualURI = "virtual://movie/tt99999991?result=hash-two"
	if _, err := r.UpsertVirtualMedia(ctx, 0, movie); err != nil {
		t.Fatalf("rotation upsert: %v", err)
	}

	var rotatedFileID int
	var path string
	if err := pool.QueryRow(ctx,
		`SELECT id, file_path FROM media_files WHERE content_id=$1 ORDER BY id`, first.MediaID).
		Scan(&rotatedFileID, &path); err != nil {
		t.Fatalf("load rotated file row: %v", err)
	}
	if rotatedFileID != firstFileID {
		t.Fatalf("rotation changed media file id %d -> %d; clients caching the old id break",
			firstFileID, rotatedFileID)
	}
	if path != movie.VirtualURI {
		t.Fatalf("adopted row path = %q, want refreshed uri %q", path, movie.VirtualURI)
	}
}

// ensureVirtualTestLibrary creates (or reuses) an isolated movies library for
// the test so registrations have a valid destination folder.
func ensureVirtualTestLibrary(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := context.Background()
	var id int
	err := pool.QueryRow(ctx, `SELECT id FROM media_folders WHERE name='test-virtual-rotation'`).Scan(&id)
	if err == nil {
		return id
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders(name, type) VALUES('test-virtual-rotation', 'movies') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return id
}
