package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userdb"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
)

// --- Test database ---------------------------------------------------------

func newNextUpTestPool(t *testing.T) *pgxpool.Pool {
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
	t.Cleanup(pool.Close)

	var tableName *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.media_items')::text`).Scan(&tableName); err != nil {
		t.Fatalf("check media_items table: %v", err)
	}
	if tableName == nil || *tableName == "" {
		t.Skip("test database has not applied base schema")
	}

	return pool
}

// --- Backend parameterization ---------------------------------------------

// nextUpBackend supplies the user-state backend under test. The catalog is
// always the package's Postgres test database; only progress storage varies.
type nextUpBackend struct {
	name        string
	newProvider func(t *testing.T, pool *pgxpool.Pool) userstore.UserStoreProvider
}

func nextUpBackends() []nextUpBackend {
	return []nextUpBackend{
		{
			name: "sqlite_state_postgres_catalog",
			newProvider: func(t *testing.T, _ *pgxpool.Pool) userstore.UserStoreProvider {
				return newNextUpSQLiteProvider(t)
			},
		},
		{
			name: "postgres_state_postgres_catalog",
			newProvider: func(_ *testing.T, pool *pgxpool.Pool) userstore.UserStoreProvider {
				return pgstore.NewPostgresProvider(pool)
			},
		},
	}
}

func runNextUpBackends(t *testing.T, fn func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider)) {
	t.Helper()
	for _, backend := range nextUpBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			pool := newNextUpTestPool(t)
			provider := backend.newProvider(t, pool)
			fn(t, pool, provider)
		})
	}
}

// fixedUserStoreProvider returns one store for every user id. It models the
// per-user SQLite backend, whose progress never appears in Postgres.
type fixedUserStoreProvider struct {
	store userstore.UserStore
}

func (p fixedUserStoreProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return p.store, nil
}

func (p fixedUserStoreProvider) Close() error { return nil }

func newNextUpSQLiteProvider(t *testing.T) userstore.UserStoreProvider {
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
	return fixedUserStoreProvider{store: userdb.NewSQLiteUserStore(db)}
}

// --- Fixture ---------------------------------------------------------------

type nextUpFixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	provider  userstore.UserStoreProvider
	prefix    string
	userID    int
	profileID string
	folderID  int
}

func newNextUpFixture(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) *nextUpFixture {
	t.Helper()

	ctx := context.Background()
	prefix := fmt.Sprintf("nextup-%d", time.Now().UnixNano())
	userID, profileID, folderID := seedNextUpTestOwner(t, ctx, pool, prefix)
	return &nextUpFixture{
		ctx:       ctx,
		pool:      pool,
		provider:  provider,
		prefix:    prefix,
		userID:    userID,
		profileID: profileID,
		folderID:  folderID,
	}
}

func (f *nextUpFixture) store(t *testing.T) userstore.UserStore {
	t.Helper()
	store, err := f.provider.ForUser(f.ctx, f.userID)
	if err != nil {
		t.Fatalf("resolve user store: %v", err)
	}
	return store
}

func (f *nextUpFixture) seedProgress(t *testing.T, mediaItemID string, position float64, completed bool, at time.Time) {
	t.Helper()
	f.seedProgressFor(t, f.profileID, mediaItemID, position, completed, at)
}

func (f *nextUpFixture) seedProgressFor(t *testing.T, profileID, mediaItemID string, position float64, completed bool, at time.Time) {
	t.Helper()
	if err := f.store(t).SetProgressAt(f.ctx, profileID, mediaItemID, position, 1800, completed, at); err != nil {
		t.Fatalf("seed progress %s: %v", mediaItemID, err)
	}
}

func (f *nextUpFixture) repo(t *testing.T) *NextUpRepository {
	t.Helper()
	return NewNextUpRepository(f.pool, f.provider)
}

func (f *nextUpFixture) list(t *testing.T, q NextUpQuery) []NextUpResult {
	t.Helper()
	q.UserID = f.userID
	if q.ProfileID == "" {
		q.ProfileID = f.profileID
	}
	results, err := f.repo(t).ListNextUp(f.ctx, q)
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	return results
}

// --- Catalog seed helpers --------------------------------------------------

func seedNextUpTestOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefix string) (int, string, int) {
	t.Helper()

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders (type, name, enabled)
		VALUES ('series', $1, TRUE)
		RETURNING id
	`, prefix+" Library").Scan(&folderID); err != nil {
		t.Fatalf("seed media folder: %v", err)
	}

	var userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, role)
		VALUES ($1, 'user')
		RETURNING id
	`, prefix+"-user").Scan(&userID); err != nil {
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
		t.Fatalf("seed user: %v", err)
	}

	profileID := fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_profiles (id, user_id, name)
		VALUES ($1, $2, 'Next Up Regression')
	`, profileID, userID); err != nil {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
		t.Fatalf("seed profile: %v", err)
	}

	return userID, profileID, folderID
}

func seedNextUpProfile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int, profileID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_profiles (id, user_id, name)
		VALUES ($1, $2, 'Next Up Sibling')
	`, profileID, userID); err != nil {
		t.Fatalf("seed sibling profile: %v", err)
	}
}

func seedNextUpSeries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, seriesID, title string) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres)
		VALUES ($1, 'series', $2, 'matched', '{}'::text[])
	`, seriesID, title); err != nil {
		t.Fatalf("seed series %s: %v", seriesID, err)
	}
}

func seedNextUpSeriesBatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, seriesIDs []string, title string) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres)
		SELECT id, 'series', $2, 'matched', '{}'::text[]
		FROM unnest($1::text[]) AS fixture(id)
	`, seriesIDs, title); err != nil {
		t.Fatalf("seed series batch: %v", err)
	}
}

func seedNextUpEpisodes(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	episodeIDs []string,
	seriesIDs []string,
	episodeNumbers []int,
) {
	t.Helper()
	seasons := make([]int, len(episodeIDs))
	for i := range seasons {
		seasons[i] = 1
	}
	seedNextUpEpisodesSeasoned(t, ctx, pool, episodeIDs, seriesIDs, seasons, episodeNumbers)
}

func seedNextUpEpisodesSeasoned(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	episodeIDs []string,
	seriesIDs []string,
	seasons []int,
	episodeNumbers []int,
) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
		SELECT episode_id, series_id, season_number, episode_number, 'Next Up Episode'
		FROM unnest($1::text[], $2::text[], $3::int[], $4::int[])
			AS fixture(episode_id, series_id, season_number, episode_number)
	`, episodeIDs, seriesIDs, seasons, episodeNumbers); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
}

func seedNextUpFiles(t *testing.T, ctx context.Context, pool *pgxpool.Pool, folderID int, episodeIDs []string) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files (episode_id, media_folder_id, file_path, duration)
		SELECT episode_id, $2, '/nextup-regression/' || episode_id || '.mkv', 1800
		FROM unnest($1::text[]) AS fixture(episode_id)
	`, episodeIDs, folderID); err != nil {
		t.Fatalf("seed media files: %v", err)
	}
}

func repeatString(value string, count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = value
	}
	return values
}

// --- Assertions ------------------------------------------------------------

func assertNextUpContentIDs(t *testing.T, results []NextUpResult, want ...string) {
	t.Helper()

	if len(results) != len(want) {
		t.Fatalf("ListNextUp returned %d rows, want %d: %+v", len(results), len(want), results)
	}
	got := make(map[string]bool, len(results))
	for _, result := range results {
		got[result.ContentID] = true
	}
	for _, contentID := range want {
		if !got[contentID] {
			t.Errorf("ListNextUp missing %s: %+v", contentID, results)
		}
	}
}

func assertNextUpOrderedIDs(t *testing.T, results []NextUpResult, want ...string) {
	t.Helper()

	if len(results) != len(want) {
		t.Fatalf("ListNextUp returned %d rows, want %d: %+v", len(results), len(want), results)
	}
	for i, contentID := range want {
		if results[i].ContentID != contentID {
			t.Fatalf("ListNextUp[%d] = %s, want %s (full: %+v)", i, results[i].ContentID, contentID, results)
		}
	}
}

func nextUpResultByContentID(results []NextUpResult) map[string]NextUpResult {
	byID := make(map[string]NextUpResult, len(results))
	for _, result := range results {
		byID[result.ContentID] = result
	}
	return byID
}

// --- Anchor selection ------------------------------------------------------

func TestNextUp_AnchorOrdersByEpisodeNotContentID(t *testing.T) {
	// A bulk mark-watched series writes every row with one timestamp. The anchor
	// must then be the highest (season, episode): production-shape ids can put a
	// season-1 backfill above season 2, and anchoring on content id would
	// surface an already-watched episode's successor.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Ordering")

		// want is s02e04 (highest available episode after the anchor); trap is
		// s01e04, which a content-id-ordered anchor would wrongly pick.
		id := func(base string) string { return f.prefix + "-" + base }
		episodeIDs := []string{
			id("100000000000000001"), id("100000000000000002"),
			id("100000000000000003"), id("100000000000000004"),
			id("900000000000000001"), id("900000000000000002"),
			id("900000000000000003"), id("900000000000000004"),
		}
		seasons := []int{2, 2, 2, 2, 1, 1, 1, 1}
		numbers := []int{1, 2, 3, 4, 1, 2, 3, 4}
		seedNextUpEpisodesSeasoned(t, f.ctx, pool, episodeIDs, repeatString(seriesID, 8), seasons, numbers)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, episodeIDs)

		bulkAt := time.Now().UTC().Truncate(time.Microsecond)
		for _, ep := range episodeIDs[:3:3] {
			f.seedProgress(t, ep, 0, true, bulkAt)
		}
		for _, ep := range episodeIDs[4:7:7] {
			f.seedProgress(t, ep, 0, true, bulkAt)
		}

		results := f.list(t, NextUpQuery{Limit: 20})
		assertNextUpContentIDs(t, results, episodeIDs[3])
	})
}

// --- Starvation / traversal ------------------------------------------------

func TestNextUp_HeavilyWatchedSeriesDoesNotStarveOlderSeries(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		floodSeries := f.prefix + "-flood"
		olderSeries := f.prefix + "-older"
		seedNextUpSeries(t, f.ctx, pool, floodSeries, "Flood")
		seedNextUpSeries(t, f.ctx, pool, olderSeries, "Older")

		// >100 newer completions in one series must not consume the budget.
		floodEpisodes := make([]string, 0, 121)
		for i := 1; i <= 121; i++ {
			floodEpisodes = append(floodEpisodes, fmt.Sprintf("%s-e%03d", floodSeries, i))
		}
		seedNextUpEpisodes(t, f.ctx, pool, floodEpisodes, repeatString(floodSeries, 121), sequence(1, 121))
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{floodEpisodes[120]})

		olderEpisodes := []string{olderSeries + "-e1", olderSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, repeatString(olderSeries, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i := 0; i < 120; i++ {
			f.seedProgress(t, floodEpisodes[i], 0, true, newest.Add(-time.Duration(i)*time.Second))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-time.Hour))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, floodEpisodes[120], olderEpisodes[1])
	})
}

func TestNextUp_WalksPastIneligibleSeries(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		const ineligibleCount = 100
		seriesIDs := make([]string, 0, ineligibleCount+2)
		episodeIDs := make([]string, 0, ineligibleCount+2)
		for i := 0; i < ineligibleCount; i++ {
			seriesID := fmt.Sprintf("%s-ineligible-%03d", f.prefix, i)
			seriesIDs = append(seriesIDs, seriesID)
			episodeIDs = append(episodeIDs, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Ineligible")
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, ones(ineligibleCount))

		olderA := f.prefix + "-older-a"
		olderB := f.prefix + "-older-b"
		seedNextUpSeries(t, f.ctx, pool, olderA, "Older A")
		seedNextUpSeries(t, f.ctx, pool, olderB, "Older B")
		olderEpisodes := []string{olderA + "-e1", olderA + "-e2", olderB + "-e1", olderB + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, []string{olderA, olderA, olderB, olderB}, []int{1, 2, 1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1], olderEpisodes[3]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-(ineligibleCount+1)*time.Minute))
		f.seedProgress(t, olderEpisodes[2], 0, true, newest.Add(-(ineligibleCount+2)*time.Minute))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, olderEpisodes[1], olderEpisodes[3])
	})
}

func TestNextUp_NoStarvationBeyondAnchorSeriesCap(t *testing.T) {
	// The removed 960-series anchor ceiling let an older eligible series starve
	// behind a wall of ineligible ones. Walk past 960 distinct series.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		const ineligibleCount = 970
		seriesIDs := make([]string, 0, ineligibleCount+1)
		episodeIDs := make([]string, 0, ineligibleCount+1)
		for i := 0; i < ineligibleCount; i++ {
			seriesID := fmt.Sprintf("%s-ineligible-%04d", f.prefix, i)
			seriesIDs = append(seriesIDs, seriesID)
			episodeIDs = append(episodeIDs, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Ineligible")
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, ones(ineligibleCount))

		olderSeries := f.prefix + "-older"
		seedNextUpSeries(t, f.ctx, pool, olderSeries, "Older")
		olderEpisodes := []string{olderSeries + "-e1", olderSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, []string{olderSeries, olderSeries}, []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Second))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-time.Duration(ineligibleCount+1)*time.Second))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, olderEpisodes[1])
	})
}

func TestNextUp_ExhaustedHistoryReturnsNothing(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Finished")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, []int{1, 1, 1})

		at := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, at.Add(-time.Duration(i)*time.Minute))
		}

		results := f.list(t, NextUpQuery{Limit: 20})
		if len(results) != 0 {
			t.Fatalf("ListNextUp returned %d rows for finished history: %+v", len(results), results)
		}
	})
}

// --- Ties and pagination ---------------------------------------------------

func TestNextUp_SameTimestampSeriesStayOrdered(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesA := f.prefix + "-series-a"
		seriesB := f.prefix + "-series-b"
		seedNextUpSeries(t, f.ctx, pool, seriesA, "Tie A")
		seedNextUpSeries(t, f.ctx, pool, seriesB, "Tie B")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesA + "-e1", seriesA + "-e2", seriesB + "-e1", seriesB + "-e2"},
			[]string{seriesA, seriesA, seriesB, seriesB},
			[]int{1, 2, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesA + "-e2", seriesB + "-e2"})

		tiedAt := time.Now().UTC().Truncate(time.Microsecond)
		f.seedProgress(t, seriesA+"-e1", 0, true, tiedAt)
		f.seedProgress(t, seriesB+"-e1", 0, true, tiedAt)

		q := NextUpQuery{Limit: 20}
		results := f.list(t, q)
		assertNextUpOrderedIDs(t, results, seriesA+"-e2", seriesB+"-e2")

		for i := 0; i < 5; i++ {
			repeat := f.list(t, q)
			assertNextUpOrderedIDs(t, repeat, seriesA+"-e2", seriesB+"-e2")
		}
	})
}

func TestNextUp_TiesSpanningPagesStayDeterministic(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		// Episode ids sort c > b > a, so with a tiny page size the smallest
		// series (a) lands on a later page than the tie window. The final
		// (updated_at DESC, series_id) order must still put a first.
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Tie")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		successors := []string{seriesIDs[0] + "-e2", seriesIDs[1] + "-e2", seriesIDs[2] + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool,
			append(append([]string{}, episodeIDs...), successors...),
			[]string{seriesIDs[0], seriesIDs[1], seriesIDs[2], seriesIDs[0], seriesIDs[1], seriesIDs[2]},
			[]int{1, 1, 1, 2, 2, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, successors)

		tiedAt := time.Now().UTC().Truncate(time.Microsecond)
		for _, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, tiedAt)
		}

		repo := f.repo(t)
		repo.statePageSize = 2
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{UserID: f.userID, ProfileID: f.profileID, Limit: 2})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpOrderedIDs(t, results, seriesIDs[0]+"-e2", seriesIDs[1]+"-e2")
	})
}

// --- Filters ---------------------------------------------------------------

func TestNextUp_GlobalDateCutoffOnSecondPage(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Cutoff")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		successors := []string{seriesIDs[0] + "-e2", seriesIDs[1] + "-e2", seriesIDs[2] + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool,
			append(append([]string{}, episodeIDs...), successors...),
			[]string{seriesIDs[0], seriesIDs[1], seriesIDs[2], seriesIDs[0], seriesIDs[1], seriesIDs[2]},
			[]int{1, 1, 1, 2, 2, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, successors)

		recent := time.Now().UTC().Truncate(time.Microsecond)
		old := recent.Add(-2 * time.Hour)
		// b and c are recent; a is older than the cutoff and must be dropped.
		f.seedProgress(t, seriesIDs[1]+"-e1", 0, true, recent)
		f.seedProgress(t, seriesIDs[2]+"-e1", 0, true, recent)
		f.seedProgress(t, seriesIDs[0]+"-e1", 0, true, old)

		cutoff := recent.Add(-30 * time.Minute)
		repo := f.repo(t)
		repo.statePageSize = 2
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, Limit: 5, DateCutoff: &cutoff,
		})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpContentIDs(t, results, seriesIDs[1]+"-e2", seriesIDs[2]+"-e2")
	})
}

func TestNextUp_SeriesScopedFindsOldestAnchor(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		// A wall of newer unrelated completions must not hide the target
		// series' long-idle anchor.
		const noise = 40
		noiseSeries := make([]string, 0, noise)
		noiseEpisodes := make([]string, 0, noise)
		for i := 0; i < noise; i++ {
			seriesID := fmt.Sprintf("%s-noise-%03d", f.prefix, i)
			noiseSeries = append(noiseSeries, seriesID)
			noiseEpisodes = append(noiseEpisodes, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, noiseSeries, "Noise")
		seedNextUpEpisodes(t, f.ctx, pool, noiseEpisodes, noiseSeries, ones(noise))

		targetSeries := f.prefix + "-target"
		seedNextUpSeries(t, f.ctx, pool, targetSeries, "Target")
		targetEpisodes := []string{targetSeries + "-e1", targetSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, targetEpisodes, repeatString(targetSeries, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{targetEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range noiseEpisodes {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}
		f.seedProgress(t, targetEpisodes[0], 0, true, newest.Add(-24*time.Hour))

		repo := f.repo(t)
		repo.statePageSize = 5
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, SeriesID: targetSeries, Limit: 20,
		})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpContentIDs(t, results, targetEpisodes[1])
	})
}

func TestNextUp_SeriesAndDateFilters(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesA := f.prefix + "-a"
		seriesB := f.prefix + "-b"
		seedNextUpSeries(t, f.ctx, pool, seriesA, "A")
		seedNextUpSeries(t, f.ctx, pool, seriesB, "B")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesA + "-e1", seriesA + "-e2", seriesB + "-e1", seriesB + "-e2"},
			[]string{seriesA, seriesA, seriesB, seriesB},
			[]int{1, 2, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesA + "-e2", seriesB + "-e2"})

		older := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
		newer := older.Add(time.Hour)
		f.seedProgress(t, seriesA+"-e1", 0, true, older)
		f.seedProgress(t, seriesB+"-e1", 0, true, newer)

		results := f.list(t, NextUpQuery{SeriesID: seriesA, Limit: 20})
		assertNextUpContentIDs(t, results, seriesA+"-e2")

		cutoff := older.Add(30 * time.Minute)
		results = f.list(t, NextUpQuery{DateCutoff: &cutoff, Limit: 20})
		assertNextUpContentIDs(t, results, seriesB+"-e2")

		results = f.list(t, NextUpQuery{SeriesID: seriesA, DateCutoff: &cutoff, Limit: 20})
		if len(results) != 0 {
			t.Fatalf("series+date should exclude the older series, got %+v", results)
		}
	})
}

// --- Hidden history --------------------------------------------------------

func TestNextUp_HiddenHistoryWatermarkAndReappearance(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Hidden")
		e1, e2, e3 := seriesID+"-e1", seriesID+"-e2", seriesID+"-e3"
		seedNextUpEpisodes(t, f.ctx, pool, []string{e1, e2, e3}, repeatString(seriesID, 3), []int{1, 2, 3})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{e2, e3})

		older := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
		newer := older.Add(time.Hour)
		f.seedProgress(t, e1, 0, true, older)
		f.seedProgress(t, e2, 0, true, newer)

		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e3)

		// Hiding the newest completion deletes it and records the watermark; the
		// older completion anchors the series, so the hidden episode returns.
		hiddenBefore := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
		if err := f.store(t).RemoveHistoryItems(f.ctx, f.profileID, []string{e2}, hiddenBefore); err != nil {
			t.Fatalf("hide progress: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e2)

		// A completion at the watermark is suppressed (inclusive comparison).
		if err := f.store(t).SetProgressAt(f.ctx, f.profileID, e2, 0, 1800, true, hiddenBefore); err != nil {
			t.Fatalf("completion at watermark: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e2)

		// A completion after the watermark restores the newest anchor.
		if err := f.store(t).SetProgressAt(f.ctx, f.profileID, e2, 0, 1800, true, hiddenBefore.Add(time.Second)); err != nil {
			t.Fatalf("completion after watermark: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e3)
	})
}

// --- Profile isolation -----------------------------------------------------

func TestNextUp_SiblingProfilesAreIsolated(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		profileA := f.prefix + "-profile-a"
		profileB := f.prefix + "-profile-b"
		seedNextUpProfile(t, f.ctx, pool, f.userID, profileA)
		seedNextUpProfile(t, f.ctx, pool, f.userID, profileB)

		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Isolation")
		e1, e2 := seriesID+"-e1", seriesID+"-e2"
		seedNextUpEpisodes(t, f.ctx, pool, []string{e1, e2}, repeatString(seriesID, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{e2})

		f.seedProgressFor(t, profileA, e1, 0, true, time.Now().UTC().Truncate(time.Microsecond))

		assertNextUpContentIDs(t, f.list(t, NextUpQuery{ProfileID: profileA, Limit: 20}), e2)
		if results := f.list(t, NextUpQuery{ProfileID: profileB, Limit: 20}); len(results) != 0 {
			t.Fatalf("sibling profile inherited progress: %+v", results)
		}
	})
}

// --- Resumable merge -------------------------------------------------------

func TestNextUp_EmptyHistoryResumableEnabled(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		results := f.list(t, NextUpQuery{EnableResumable: true, Limit: 20})
		if len(results) != 0 {
			t.Fatalf("empty history must return no anchors, got %+v", results)
		}
	})
}

func TestNextUp_ResumableMergePriority(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesInProgress := f.prefix + "-in-progress"
		seriesCompleted := f.prefix + "-completed"
		seedNextUpSeries(t, f.ctx, pool, seriesInProgress, "In Progress")
		seedNextUpSeries(t, f.ctx, pool, seriesCompleted, "Completed")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesInProgress + "-e1", seriesCompleted + "-e1", seriesCompleted + "-e2"},
			[]string{seriesInProgress, seriesCompleted, seriesCompleted},
			[]int{1, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesCompleted + "-e2"})

		startedAt := time.Now().UTC().Truncate(time.Microsecond)
		f.seedProgress(t, seriesInProgress+"-e1", 30, false, startedAt)
		f.seedProgress(t, seriesCompleted+"-e1", 0, true, startedAt.Add(-time.Minute))

		results := f.list(t, NextUpQuery{EnableResumable: true, Limit: 20})
		assertNextUpContentIDs(t, results, seriesInProgress+"-e1", seriesCompleted+"-e2")

		byID := nextUpResultByContentID(results)
		if !byID[seriesInProgress+"-e1"].IsResumable {
			t.Fatalf("in-progress episode must be marked resumable: %+v", byID[seriesInProgress+"-e1"])
		}
		if byID[seriesCompleted+"-e2"].IsResumable {
			t.Fatalf("completed-next episode must not be marked resumable: %+v", byID[seriesCompleted+"-e2"])
		}
	})
}

// --- Limits and ordering ---------------------------------------------------

func TestNextUp_MultiSeriesLimit(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Limit")
		episodeIDs := make([]string, 0, len(seriesIDs)*2)
		episodeSeries := make([]string, 0, len(seriesIDs)*2)
		episodeNumbers := make([]int, 0, len(seriesIDs)*2)
		available := make([]string, 0, len(seriesIDs))
		for _, seriesID := range seriesIDs {
			episodeIDs = append(episodeIDs, seriesID+"-e1", seriesID+"-e2")
			episodeSeries = append(episodeSeries, seriesID, seriesID)
			episodeNumbers = append(episodeNumbers, 1, 2)
			available = append(available, seriesID+"-e2")
		}
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, episodeSeries, episodeNumbers)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, available)

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, seriesID := range seriesIDs {
			f.seedProgress(t, seriesID+"-e1", 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}

		results := f.list(t, NextUpQuery{Limit: 2})
		assertNextUpOrderedIDs(t, results, seriesIDs[0]+"-e2", seriesIDs[1]+"-e2")
	})
}

// --- Helpers for bulk fixtures --------------------------------------------

func ones(count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = 1
	}
	return values
}

func sequence(start, count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = start + i
	}
	return values
}

// --- Build-shape guard -----------------------------------------------------

// TestNextUpRepositoryHasNoPostgresUserStateSQL pins the refactor's core
// invariant: the catalog package must not carry Next Up SQL that joins the
// Postgres user-state tables. It scans the package sources so a reintroduced
// join fails the build even if no behavioral test exercises it.
func TestNextUpRepositoryHasNoPostgresUserStateSQL(t *testing.T) {
	raw, err := os.ReadFile("nextup_repo.go")
	if err != nil {
		t.Fatalf("read nextup_repo.go: %v", err)
	}
	source := string(raw)
	for _, forbidden := range []string{"user_watch_progress", "user_history_hidden_items", "nextUpAnchorMax", "buildListNextUpQuery", "buildListNextUpSnapshotQuery"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("nextup_repo.go still references %q", forbidden)
		}
	}
}
