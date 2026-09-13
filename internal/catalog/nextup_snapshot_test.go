package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/userdb"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
)

// snapshotOnlyStore hides the PostgresAnchorStore marker from an otherwise real
// store so tests can drive the snapshot path over Postgres-seeded progress.
type snapshotOnlyStore struct {
	userstore.UserStore
}

// snapshotOnlyProvider wraps a provider and strips the PostgresAnchorStore
// marker from every store it hands out.
type snapshotOnlyProvider struct {
	inner userstore.UserStoreProvider
}

func (p snapshotOnlyProvider) ForUser(ctx context.Context, userID int) (userstore.UserStore, error) {
	store, err := p.inner.ForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return snapshotOnlyStore{UserStore: store}, nil
}

func (p snapshotOnlyProvider) Close() error { return p.inner.Close() }

// sqliteSnapshotProvider returns a fixed per-user SQLite store, which never
// implements PostgresAnchorStore, so ListNextUp takes the snapshot path.
type sqliteSnapshotProvider struct {
	store userstore.UserStore
}

func (p sqliteSnapshotProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return p.store, nil
}

func (p sqliteSnapshotProvider) Close() error { return nil }

func newNextUpSQLiteStore(t *testing.T) *userdb.SQLiteUserStore {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := userdb.InitSchema(db); err != nil {
		t.Fatalf("init sqlite schema: %v", err)
	}
	return userdb.NewSQLiteUserStore(db)
}

func setSQLiteProgress(t *testing.T, store *userdb.SQLiteUserStore, profileID, mediaItemID string, position float64, completed bool, at time.Time) {
	t.Helper()
	if err := store.SetProgressAt(context.Background(), profileID, mediaItemID, position, 1800, completed, at); err != nil {
		t.Fatalf("seed sqlite progress %s: %v", mediaItemID, err)
	}
}

func TestBuildListNextUpSnapshotQuery_Shape(t *testing.T) {
	t.Parallel()

	src := nextUpSnapshotSource{
		completedIDs:         []string{"e1"},
		completedUpdatedAts:  []time.Time{time.Now().UTC()},
		inProgressIDs:        []string{"e2"},
		inProgressUpdatedAts: []time.Time{time.Now().UTC()},
		anyProgressIDs:       []string{"e1", "e2"},
	}
	query, args := buildListNextUpSnapshotQuery(NextUpQuery{UserID: 7, ProfileID: "profile-1"}, 20, nil, src)

	expectedFragments := []string{
		"completed_progress(media_item_id, updated_at) AS (",
		"SELECT * FROM unnest($1::text[], $2::timestamptz[])",
		"nextup_any_progress(media_item_id) AS (",
		"nextup_in_progress(media_item_id, updated_at) AS (",
		"WITH RECURSIVE",
		"(uwp.updated_at, uwp.media_item_id) < (w.updated_at, w.media_item_id)",
		"NOT (e.series_id = ANY(w.seen))",
		fmt.Sprintf("w.n < %d", nextUpAnchorMaxSeries),
		"FROM completed_progress uwp",
		"SELECT 1 FROM nextup_any_progress nap",
		"ORDER BY uwp.updated_at DESC, uwp.media_item_id DESC",
		"LIMIT $3",
	}
	for _, fragment := range expectedFragments {
		if !strings.Contains(query, fragment) {
			t.Fatalf("expected snapshot query to contain %q, got:\n%s", fragment, query)
		}
	}
	// The snapshot path must not reach into the Postgres progress tables or the
	// Postgres hidden-items table; the store already applied both.
	for _, forbidden := range []string{"user_watch_progress", "user_history_hidden_items"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("snapshot query must not reference %s, got:\n%s", forbidden, query)
		}
	}
	if len(args) != 6 {
		t.Fatalf("expected 6 args, got %d (%v)", len(args), args)
	}
	if args[0] == nil || args[1] == nil || args[3] == nil {
		t.Fatalf("completed/any-progress arrays must be bound, got %v", args)
	}
}

func TestBuildListNextUpSnapshotQuery_SeriesScopedKeepsDedupShape(t *testing.T) {
	t.Parallel()

	src := nextUpSnapshotSource{completedIDs: []string{"e1"}, completedUpdatedAts: []time.Time{time.Now().UTC()}}
	query, args := buildListNextUpSnapshotQuery(NextUpQuery{
		UserID:    7,
		ProfileID: "profile-1",
		SeriesID:  "series-42",
	}, 20, nil, src)

	if strings.Contains(query, "WITH RECURSIVE") {
		t.Fatalf("series-scoped snapshot query must not use the walk, got:\n%s", query)
	}
	if !strings.Contains(query, "SELECT DISTINCT ON (e.series_id)") {
		t.Fatalf("series-scoped snapshot query must dedup by series, got:\n%s", query)
	}
	if !strings.Contains(query, "AND e.series_id = $4") {
		t.Fatalf("series-scoped snapshot query must filter by series, got:\n%s", query)
	}
	if strings.Contains(query, "user_watch_progress") {
		t.Fatalf("series-scoped snapshot query must not reference user_watch_progress, got:\n%s", query)
	}
	if len(args) != 7 {
		t.Fatalf("expected 7 args, got %d (%v)", len(args), args)
	}
}

func TestBuildListNextUpSnapshotQuery_DateCutoffAndCursorBind(t *testing.T) {
	t.Parallel()

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	cursor := &nextUpWalkCursor{updatedAt: time.Now().UTC(), mediaItemID: "e9", seen: []string{"s1"}}
	src := nextUpSnapshotSource{completedIDs: []string{"e1"}, completedUpdatedAts: []time.Time{cutoff}}
	query, args := buildListNextUpSnapshotQuery(NextUpQuery{
		UserID:     7,
		ProfileID:  "profile-1",
		DateCutoff: &cutoff,
	}, 20, cursor, src)

	if !strings.Contains(query, "(uwp.updated_at, uwp.media_item_id) < ($5, $6)") {
		t.Fatalf("expected cursor comparison bound after cutoff, got:\n%s", query)
	}
	if !strings.Contains(query, "NOT (e.series_id = ANY($7))") {
		t.Fatalf("expected seen-series exclusion at cursor arg, got:\n%s", query)
	}
	if !strings.Contains(query, "$7::text[] || pick.series_id AS seen") {
		t.Fatalf("expected cursor seen array to seed the walk, got:\n%s", query)
	}
	if !strings.Contains(query, "AND uwp.updated_at >= $4") {
		t.Fatalf("expected date cutoff in every walk step, got:\n%s", query)
	}
	if len(args) != 10 {
		t.Fatalf("expected 10 args, got %d (%v)", len(args), args)
	}
}

func TestBuildListResumableFirstEpisodesSnapshotQuery(t *testing.T) {
	t.Parallel()

	inProgress := []ProgressSnapshot{{ContentID: "e2", UpdatedAt: time.Now().UTC()}}
	completed := []ProgressSnapshot{{ContentID: "e1", UpdatedAt: time.Now().UTC()}}

	global, args := buildListResumableFirstEpisodesSnapshotQuery(NextUpQuery{UserID: 7, ProfileID: "profile-1"}, inProgress, completed)
	if !strings.Contains(global, "nextup_completed(media_item_id) AS (") {
		t.Fatalf("global query must bind completed snapshots, got:\n%s", global)
	}
	if !strings.Contains(global, "e_c.content_id = ANY($3)") {
		t.Fatalf("global query must keep the completed-series gate, got:\n%s", global)
	}
	if strings.Contains(global, "user_watch_progress") {
		t.Fatalf("snapshot resumable query must not reference user_watch_progress, got:\n%s", global)
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d (%v)", len(args), args)
	}

	scoped, scopedArgs := buildListResumableFirstEpisodesSnapshotQuery(NextUpQuery{
		UserID: 7, ProfileID: "profile-1", SeriesID: "series-42",
	}, inProgress, completed)
	if strings.Contains(scoped, "e_c.content_id = ANY($3)") {
		t.Fatalf("series-scoped query must drop the completed-series gate, got:\n%s", scoped)
	}
	if !strings.Contains(scoped, "AND e.series_id = $4") {
		t.Fatalf("series-scoped query must filter by series, got:\n%s", scoped)
	}
	if len(scopedArgs) != 4 {
		t.Fatalf("expected 4 args, got %d (%v)", len(scopedArgs), scopedArgs)
	}
}

// TestNextUpRepository_SnapshotPathMatchesPostgresPath drives the same
// Postgres-seeded fixture through the direct Postgres path and the
// store-snapshot path and asserts identical anchors.
func TestNextUpRepository_SnapshotPathMatchesPostgresPath(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-parity-%d", time.Now().UnixNano())
	seriesA := prefix + "-series-a"
	seriesB := prefix + "-series-b"

	userID, profileID, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesA, seriesB})
		_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
	})

	seedNextUpSeries(t, ctx, pool, seriesA, prefix+" A")
	seedNextUpSeries(t, ctx, pool, seriesB, prefix+" B")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesA + "-e1", seriesA + "-e2", seriesA + "-e3", seriesB + "-e1", seriesB + "-e2", seriesB + "-e3"},
		[]string{seriesA, seriesA, seriesA, seriesB, seriesB, seriesB},
		[]int{1, 2, 3, 1, 2, 3},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesA + "-e2", seriesA + "-e3", seriesB + "-e2", seriesB + "-e3"})

	newest := time.Now().UTC().Truncate(time.Microsecond)
	older := newest.Add(-time.Minute)
	// seriesA: completed e1, so next is e2. seriesB: completed e1 at the same
	// time, plus an OLDER in-progress e2. The older partial does not suppress
	// the series, but the next-episode lateral must still skip e2 (already
	// started) and surface e3.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress (user_id, profile_id, media_item_id, position_seconds, duration_seconds, completed, updated_at)
		VALUES ($1, $2, $3, 0, 1800, TRUE, $5),
		       ($1, $2, $4, 0, 1800, TRUE, $5),
		       ($1, $2, $6, 30, 1800, FALSE, $7)
	`, userID, profileID, seriesA+"-e1", seriesB+"-e1", newest, seriesB+"-e2", older); err != nil {
		t.Fatalf("seed progress: %v", err)
	}

	pgRepo := NewNextUpRepository(pool, nextUpTestStoreProvider{})
	snapRepo := NewNextUpRepository(pool, snapshotOnlyProvider{inner: pgstore.NewPostgresProvider(pool)})

	pgResults, err := pgRepo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("postgres ListNextUp: %v", err)
	}
	snapResults, err := snapRepo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("snapshot ListNextUp: %v", err)
	}

	pgIDs := nextUpContentIDSet(pgResults)
	snapIDs := nextUpContentIDSet(snapResults)
	if len(pgIDs) == 0 {
		t.Fatalf("postgres path returned no rows; fixture invalid")
	}
	if len(pgIDs) != len(snapIDs) {
		t.Fatalf("snapshot path returned %v, postgres path returned %v", snapIDs, pgIDs)
	}
	for id := range pgIDs {
		if !snapIDs[id] {
			t.Fatalf("snapshot path missing %s; postgres=%v snapshot=%v", id, pgIDs, snapIDs)
		}
	}
	if !pgIDs[seriesA+"-e2"] || !pgIDs[seriesB+"-e3"] {
		t.Fatalf("fixture did not exercise anchor and exclusion: %v", pgIDs)
	}
}

func nextUpContentIDSet(results []NextUpResult) map[string]bool {
	set := make(map[string]bool, len(results))
	for _, result := range results {
		set[result.ContentID] = true
	}
	return set
}

func TestNextUpRepository_SQLiteSnapshots_AnchorAndExclusion(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-%d", time.Now().UnixNano())
	seriesA := prefix + "-series-a"
	seriesB := prefix + "-series-b"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesA, seriesB})
	})

	seedNextUpSeries(t, ctx, pool, seriesA, prefix+" A")
	seedNextUpSeries(t, ctx, pool, seriesB, prefix+" B")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesA + "-e1", seriesA + "-e2", seriesA + "-e3", seriesB + "-e1", seriesB + "-e2", seriesB + "-e3"},
		[]string{seriesA, seriesA, seriesA, seriesB, seriesB, seriesB},
		[]int{1, 2, 3, 1, 2, 3},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesA + "-e2", seriesA + "-e3", seriesB + "-e2", seriesB + "-e3"})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	newest := time.Now().UTC().Truncate(time.Second)
	older := newest.Add(-time.Minute)
	setSQLiteProgress(t, store, profileID, seriesA+"-e1", 0, true, newest)
	setSQLiteProgress(t, store, profileID, seriesB+"-e1", 0, true, newest)
	// Older partial watch: the series stays eligible (completed is newer), but
	// e2 is already in progress so the lateral must skip to e3.
	setSQLiteProgress(t, store, profileID, seriesB+"-e2", 30, false, older)

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesA+"-e2", seriesB+"-e3")
}

func TestNextUpRepository_SQLiteSnapshots_HiddenThenRecompletedRestoresEligibility(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-hidden-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
	})

	seedNextUpSeries(t, ctx, pool, seriesID, prefix+" Hidden")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesID + "-e1", seriesID + "-e2"},
		[]string{seriesID, seriesID},
		[]int{1, 2},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesID + "-e2"})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	completedAt := time.Now().UTC().Truncate(time.Second)
	setSQLiteProgress(t, store, profileID, seriesID+"-e1", 0, true, completedAt)

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp before hide: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesID+"-e2")

	// Hiding the only completed episode removes the anchor entirely.
	hiddenBefore := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := store.RemoveHistoryItems(ctx, profileID, []string{seriesID + "-e1"}, hiddenBefore); err != nil {
		t.Fatalf("hide progress: %v", err)
	}
	results, err = repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp after hide: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("hidden anchor must not surface, got %+v", results)
	}

	// Re-completing must win over the watermark: the write path stamps the row
	// at hidden_before + 1s, so the anchor and its successor come back.
	if err := store.MarkWatched(ctx, profileID, seriesID+"-e1", 1800); err != nil {
		t.Fatalf("re-mark watched: %v", err)
	}
	results, err = repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp after recomplete: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesID+"-e2")
}

func TestNextUpRepository_SQLiteSnapshots_ResumableMergePriority(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-resumable-%d", time.Now().UnixNano())
	seriesInProgress := prefix + "-in-progress"
	seriesCompleted := prefix + "-completed"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesInProgress, seriesCompleted})
	})

	seedNextUpSeries(t, ctx, pool, seriesInProgress, prefix+" In Progress")
	seedNextUpSeries(t, ctx, pool, seriesCompleted, prefix+" Completed")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesInProgress + "-e1", seriesCompleted + "-e1", seriesCompleted + "-e2"},
		[]string{seriesInProgress, seriesCompleted, seriesCompleted},
		[]int{1, 1, 2},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesCompleted + "-e2"})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	startedAt := time.Now().UTC().Truncate(time.Second)
	setSQLiteProgress(t, store, profileID, seriesInProgress+"-e1", 30, false, startedAt)
	setSQLiteProgress(t, store, profileID, seriesCompleted+"-e1", 0, true, startedAt.Add(-time.Minute))

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{
		UserID:          userID,
		ProfileID:       profileID,
		Limit:           20,
		EnableResumable: true,
	})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesInProgress+"-e1", seriesCompleted+"-e2")

	byContentID := make(map[string]NextUpResult, len(results))
	for _, result := range results {
		byContentID[result.ContentID] = result
	}
	if !byContentID[seriesInProgress+"-e1"].IsResumable {
		t.Fatalf("in-progress episode must be marked resumable: %+v", byContentID[seriesInProgress+"-e1"])
	}
	if byContentID[seriesCompleted+"-e2"].IsResumable {
		t.Fatalf("completed-next episode must not be marked resumable: %+v", byContentID[seriesCompleted+"-e2"])
	}
}

func TestNextUpRepository_SQLiteSnapshots_ProfileIsolation(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-isolation-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
	})

	seedNextUpSeries(t, ctx, pool, seriesID, prefix+" Isolation")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesID + "-e1", seriesID + "-e2"},
		[]string{seriesID, seriesID},
		[]int{1, 2},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesID + "-e2"})

	store := newNextUpSQLiteStore(t)
	completedAt := time.Now().UTC().Truncate(time.Second)
	setSQLiteProgress(t, store, "profile-a", seriesID+"-e1", 0, true, completedAt)

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})

	resultsA, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: "profile-a", Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp profile-a: %v", err)
	}
	assertNextUpContentIDs(t, resultsA, seriesID+"-e2")

	resultsB, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: "profile-b", Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp profile-b: %v", err)
	}
	if len(resultsB) != 0 {
		t.Fatalf("other profile must not inherit progress, got %+v", resultsB)
	}
}

func TestNextUpRepository_SQLiteSnapshots_WalksPastHundredCompletedRows(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-cap-%d", time.Now().UnixNano())
	seriesFlood := prefix + "-flood"
	seriesOlder := prefix + "-older"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesFlood, seriesOlder})
	})

	seedNextUpSeries(t, ctx, pool, seriesFlood, prefix+" Flood")
	seedNextUpSeries(t, ctx, pool, seriesOlder, prefix+" Older")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesOlder + "-e1", seriesOlder + "-e2"},
		[]string{seriesOlder, seriesOlder},
		[]int{1, 2},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesOlder + "-e2"})

	// 120 newer completions in one series must not push the older eligible
	// series past a 100-row window.
	floodEpisodes := make([]string, 0, 121)
	for i := 1; i <= 121; i++ {
		floodEpisodes = append(floodEpisodes, fmt.Sprintf("%s-e%03d", seriesFlood, i))
	}
	floodSeries := make([]string, 0, 121)
	floodNumbers := make([]int, 0, 121)
	for i := 1; i <= 121; i++ {
		floodSeries = append(floodSeries, seriesFlood)
		floodNumbers = append(floodNumbers, i)
	}
	seedNextUpEpisodes(t, ctx, pool, floodEpisodes, floodSeries, floodNumbers)
	seedNextUpFiles(t, ctx, pool, folderID, []string{floodEpisodes[120]})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	newest := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 120; i++ {
		setSQLiteProgress(t, store, profileID, floodEpisodes[i], 0, true, newest.Add(-time.Duration(i)*time.Second))
	}
	setSQLiteProgress(t, store, profileID, seriesOlder+"-e1", 0, true, newest.Add(-time.Hour))

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesFlood+"-e121", seriesOlder+"-e2")
}

func TestNextUpRepository_SQLiteSnapshots_EqualTimestampAnchorsBothSeries(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-tie-%d", time.Now().UnixNano())
	seriesA := prefix + "-series-a"
	seriesB := prefix + "-series-b"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesA, seriesB})
	})

	seedNextUpSeries(t, ctx, pool, seriesA, prefix+" Tie A")
	seedNextUpSeries(t, ctx, pool, seriesB, prefix+" Tie B")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesA + "-e1", seriesA + "-e2", seriesB + "-e1", seriesB + "-e2"},
		[]string{seriesA, seriesA, seriesB, seriesB},
		[]int{1, 2, 1, 2},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesA + "-e2", seriesB + "-e2"})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	tiedAt := time.Now().UTC().Truncate(time.Second)
	setSQLiteProgress(t, store, profileID, seriesA+"-e1", 0, true, tiedAt)
	setSQLiteProgress(t, store, profileID, seriesB+"-e1", 0, true, tiedAt)

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	assertNextUpContentIDs(t, results, seriesA+"-e2", seriesB+"-e2")
}

func TestNextUpRepository_SQLiteSnapshots_NoNextEpisode(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-none-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
	})

	seedNextUpSeries(t, ctx, pool, seriesID, prefix+" Finished")
	seedNextUpEpisodes(t, ctx, pool,
		[]string{seriesID + "-e1"},
		[]string{seriesID},
		[]int{1},
	)
	seedNextUpFiles(t, ctx, pool, folderID, []string{seriesID + "-e1"})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	setSQLiteProgress(t, store, profileID, seriesID+"-e1", 0, true, time.Now().UTC().Truncate(time.Second))

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("finished series must not surface a next episode, got %+v", results)
	}
}

func TestNextUpRepository_SQLiteSnapshots_MultiSeriesLimit(t *testing.T) {
	pool := newNextUpTestPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-sqlite-limit-%d", time.Now().UnixNano())
	seriesIDs := []string{prefix + "-a", prefix + "-b", prefix + "-c"}

	userID, _, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, seriesIDs)
	})

	store := newNextUpSQLiteStore(t)
	profileID := "sqlite-profile"
	newest := time.Now().UTC().Truncate(time.Second)

	episodeIDs := make([]string, 0, len(seriesIDs)*2)
	episodeSeries := make([]string, 0, len(seriesIDs)*2)
	episodeNumbers := make([]int, 0, len(seriesIDs)*2)
	available := make([]string, 0, len(seriesIDs))
	for i, seriesID := range seriesIDs {
		seedNextUpSeries(t, ctx, pool, seriesID, fmt.Sprintf("%s Series %d", prefix, i))
		episodeIDs = append(episodeIDs, seriesID+"-e1", seriesID+"-e2")
		episodeSeries = append(episodeSeries, seriesID, seriesID)
		episodeNumbers = append(episodeNumbers, 1, 2)
		available = append(available, seriesID+"-e2")
		setSQLiteProgress(t, store, profileID, seriesID+"-e1", 0, true, newest.Add(-time.Duration(i)*time.Minute))
	}
	seedNextUpEpisodes(t, ctx, pool, episodeIDs, episodeSeries, episodeNumbers)
	seedNextUpFiles(t, ctx, pool, folderID, available)

	repo := NewNextUpRepository(pool, sqliteSnapshotProvider{store: store})
	results, err := repo.ListNextUp(ctx, NextUpQuery{UserID: userID, ProfileID: profileID, Limit: 2})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected limit to cap results at 2, got %d: %+v", len(results), results)
	}
	assertNextUpContentIDs(t, results, seriesIDs[0]+"-e2", seriesIDs[1]+"-e2")
}
