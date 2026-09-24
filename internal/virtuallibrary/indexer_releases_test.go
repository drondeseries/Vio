package virtuallibrary

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/testdb"
)

// TestMain provisions a migrated disposable database when
// SILO_TEST_DATABASE_URL is set; otherwise the DB-backed tests below skip and
// the rest of the package runs unchanged.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, "virtuallibrary"))
}

func testStore(t *testing.T) *IndexerReleaseStore {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewIndexerReleaseStore(pool)
}

func testScope(contentID, episodeID string) IndexerReleaseScope {
	return IndexerReleaseScope{ContentID: contentID, EpisodeID: episodeID, MediaFolderID: 1}
}

func testRelease(guid string, size int64, score int) IndexerRelease {
	return IndexerRelease{
		GUID:           guid,
		Title:          "Title " + guid,
		NormalizedName: "title " + guid,
		Protocol:       "usenet",
		Indexer:        "idx",
		IndexerID:      3,
		SizeBytes:      size,
		FormatScore:    &score,
		DownloadURL:    "https://indexer.example/" + guid,
		ExpireAt:       time.Now().Add(time.Hour),
	}
}

func TestIndexerReleaseUpsertIdempotency(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-1", "")
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 100, 5)}); err != nil {
		t.Fatal(err)
	}
	// Re-upsert the same guid with refreshed fields.
	updated := testRelease("g1", 200, 9)
	updated.Title = "Title g1 remastered"
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{updated}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d rows, want 1 (upsert must be idempotent)", len(list))
	}
	if list[0].SizeBytes != 200 || list[0].Title != "Title g1 remastered" {
		t.Errorf("row not refreshed: %+v", list[0])
	}
}

func TestIndexerReleaseUpsertPreservesQueuedState(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-queued", "")
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 100, 5)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkIndexerReleaseQueued(ctx, scope, "g1", "nzo_1"); err != nil {
		t.Fatal(err)
	}
	// A re-list of the same release must not reset the user's request.
	relisted := testRelease("g1", 150, 7)
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{relisted}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d rows, want 1", len(list))
	}
	if list[0].EnqueueState != "queued" || list[0].NZOID != "nzo_1" {
		t.Errorf("queued state not preserved: state=%q nzo=%q", list[0].EnqueueState, list[0].NZOID)
	}
	if list[0].SizeBytes != 150 {
		t.Errorf("mutable fields should still refresh on re-upsert, size=%d", list[0].SizeBytes)
	}
}

func TestIndexerReleaseUpsertPreservesFailedState(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-failed", "")
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 100, 5)}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkIndexerReleaseFailed(ctx, scope, "g1", "provider down"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 120, 6)}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].EnqueueState != "failed" {
		t.Errorf("failed state not preserved: %q", list[0].EnqueueState)
	}
}

func TestIndexerReleaseMarkQueuedIdempotent(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-idem", "")
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 100, 5)}); err != nil {
		t.Fatal(err)
	}
	nzo, already, err := store.MarkIndexerReleaseQueued(ctx, scope, "g1", "nzo_first")
	if err != nil {
		t.Fatal(err)
	}
	if already || nzo != "nzo_first" {
		t.Fatalf("first mark: nzo=%q already=%v, want nzo_first/false", nzo, already)
	}
	nzo, already, err = store.MarkIndexerReleaseQueued(ctx, scope, "g1", "nzo_second")
	if err != nil {
		t.Fatal(err)
	}
	if !already {
		t.Error("second mark should report alreadyQueued=true")
	}
	if nzo != "nzo_first" {
		t.Errorf("second mark returned nzo=%q, want the original nzo_first (no re-enqueue)", nzo)
	}
}

func TestIndexerReleaseMarkQueuedNotFound(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-missing", "")
	_, _, err := store.MarkIndexerReleaseQueued(ctx, scope, "nope", "nzo")
	if !errors.Is(err, ErrIndexerReleaseNotFound) {
		t.Fatalf("err = %v, want ErrIndexerReleaseNotFound", err)
	}
	if err := store.MarkIndexerReleaseFailed(ctx, scope, "nope", "reason"); !errors.Is(err, ErrIndexerReleaseNotFound) {
		t.Fatalf("failed: err = %v, want ErrIndexerReleaseNotFound", err)
	}
}

func TestIndexerReleaseExpiryFiltering(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-expire", "")
	expired := testRelease("old", 100, 1)
	expired.ExpireAt = time.Now().Add(-time.Minute)
	live := testRelease("new", 100, 1)
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{expired}); err != nil {
		t.Fatal(err)
	}
	// Upserting the live row prunes the expired one as part of the batch.
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{live}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GUID != "new" {
		t.Fatalf("list = %+v, want only the live row", list)
	}
}

func TestIndexerReleasePrune(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-prune", "")
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{testRelease("g1", 100, 1)}); err != nil {
		t.Fatal(err)
	}
	pool, err := store.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release()
	if _, err := pool.Exec(ctx, `UPDATE virtual_indexer_releases SET expire_at = now() - interval '1 minute' WHERE content_id = $1`, scope.ContentID); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.PruneExpiredIndexerReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("list after prune = %+v, want empty", list)
	}
}

func TestIndexerReleasePerContentCap(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-cap", "")
	releases := make([]IndexerRelease, 0, maxIndexerReleasesPerContent+10)
	for i := 0; i < maxIndexerReleasesPerContent+10; i++ {
		// Increasing score so the retained set is deterministic.
		releases = append(releases, testRelease(fmt.Sprintf("g%02d", i), int64(1000+i), i))
	}
	if err := store.UpsertIndexerReleases(ctx, scope, releases); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxIndexerReleasesPerContent {
		t.Fatalf("got %d rows, want the cap %d", len(list), maxIndexerReleasesPerContent)
	}
	if list[0].GUID != fmt.Sprintf("g%02d", maxIndexerReleasesPerContent+9) {
		t.Errorf("best-scored release missing from the retained set: first=%s", list[0].GUID)
	}
}

func TestIndexerReleaseCapKeepsQueued(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	scope := testScope("movie-cap-queued", "")
	// Queue the lowest-scored release, then flood the title past the cap.
	low := testRelease("low", 1, 0)
	if err := store.UpsertIndexerReleases(ctx, scope, []IndexerRelease{low}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkIndexerReleaseQueued(ctx, scope, "low", "nzo_low"); err != nil {
		t.Fatal(err)
	}
	releases := make([]IndexerRelease, 0, maxIndexerReleasesPerContent+5)
	for i := 0; i < maxIndexerReleasesPerContent+5; i++ {
		releases = append(releases, testRelease(fmt.Sprintf("hi%02d", i), int64(1000+i), 100+i))
	}
	if err := store.UpsertIndexerReleases(ctx, scope, releases); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, release := range list {
		if release.GUID == "low" {
			found = true
			if release.EnqueueState != "queued" || release.NZOID != "nzo_low" {
				t.Errorf("queued row mutated by cap: %+v", release)
			}
		}
	}
	if !found {
		t.Errorf("queued release dropped by the per-content cap (%d rows)", len(list))
	}
}

func TestIndexerReleaseEpisodeScoping(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	movie := testScope("series-1", "")
	episode := testScope("series-1", "s01e01")
	if err := store.UpsertIndexerReleases(ctx, movie, []IndexerRelease{testRelease("movie", 10, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertIndexerReleases(ctx, episode, []IndexerRelease{testRelease("ep", 20, 2)}); err != nil {
		t.Fatal(err)
	}
	movieList, err := store.ListIndexerReleases(ctx, movie)
	if err != nil {
		t.Fatal(err)
	}
	if len(movieList) != 1 || movieList[0].GUID != "movie" {
		t.Fatalf("movie scope = %+v, want only the movie row", movieList)
	}
	episodeList, err := store.ListIndexerReleases(ctx, episode)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodeList) != 1 || episodeList[0].GUID != "ep" {
		t.Fatalf("episode scope = %+v, want only the episode row", episodeList)
	}
}
