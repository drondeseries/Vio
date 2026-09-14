package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/storetest"
)

// TestPostgresProgressSince runs the offline-sync progress-reconciliation
// conformance test (invariant 1) against the Postgres backend, exercising the
// user_watch_progress synced_seq trigger and event_at LWW comparison. Skips
// unless SILO_TEST_DATABASE_URL is set and the migration is applied.
func TestPostgresProgressSince(t *testing.T) {
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

	var col *string
	err = pool.QueryRow(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'user_watch_progress' AND column_name = 'synced_seq'`).Scan(&col)
	if errors.Is(err, pgx.ErrNoRows) || col == nil {
		t.Skip("watch_progress offline-sync migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	storetest.RunProgressSince(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}

// TestPostgresDeviceProfiles runs the per-device capability registry
// conformance tests against the Postgres backend. Skips unless
// SILO_TEST_DATABASE_URL is set and the migration is applied.
func TestPostgresDeviceProfiles(t *testing.T) {
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

	var table *string
	err = pool.QueryRow(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'user_device_profiles'`).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) || table == nil {
		t.Skip("user_device_profiles migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	storetest.RunDeviceProfiles(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-devprof-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_device_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_devices WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}

// TestPostgresMarkWatchedBatch runs the batch mark-watched conformance test
// (series/season mark-watched) against the Postgres backend. The per-user
// SQLite backend runs the same suite in internal/userdb, which is what keeps
// the two transactional implementations from drifting. Skips unless
// SILO_TEST_DATABASE_URL is set.
func TestPostgresMarkWatchedBatch(t *testing.T) {
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

	storetest.RunMarkWatchedBatch(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-markwatched-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_history WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_history_hidden_items WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}

// TestPostgresSettingValues runs the canonical settings-contract storage
// conformance tests against the Postgres backend. The per-user SQLite backend
// runs the same suite in internal/userdb, which is what keeps the two from
// drifting on scope identity, partial uniqueness and delete behavior. Skips
// unless SILO_TEST_DATABASE_URL is set and the migration is applied.
func TestPostgresSettingValues(t *testing.T) {
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

	var table *string
	err = pool.QueryRow(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'user_setting_values'`).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) || table == nil {
		t.Skip("settings contract storage migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	storetest.RunSettingValues(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-settings-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// user_setting_values and user_setting_mutations cascade from users.
		// The cleanup asserts the cascade actually fired: silently discarding
		// this error would let a dropped FK leak seeded rows into the shared
		// test database run after run with no failure signal.
		t.Cleanup(func() {
			deleteUserAssertingCascade(t, pool, userID,
				"user_setting_values", "user_setting_mutations", "user_profiles")
		})
		return newStore(pool, userID)
	})
}

// SeedHiddenHistoryItem implements storetest's Next Up hidden-history test
// seam against the raw shared Postgres table. It lives in the test build only:
// the public store API timestamp-adjusts or suppresses any write at or before
// a hidden watermark, so the conformance suite cannot construct a hidden
// progress row any other way.
func (s *PostgresUserStore) SeedHiddenHistoryItem(ctx context.Context, profileID, mediaItemID string, hiddenBefore time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_history_hidden_items (user_id, profile_id, media_item_id, hidden_before, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE SET
			hidden_before = EXCLUDED.hidden_before,
			updated_at = EXCLUDED.updated_at`,
		s.userID, profileID, mediaItemID, hiddenBefore.UTC())
	return err
}

// TestPostgresNextUpState runs the Next Up state provider conformance suite
// against the Postgres backend. The per-user SQLite backend runs the same suite
// in internal/userdb. Skips unless SILO_TEST_DATABASE_URL is set and the
// hidden-history migration is applied.
func TestPostgresNextUpState(t *testing.T) {
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

	var table *string
	err = pool.QueryRow(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'user_history_hidden_items'`).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) || table == nil {
		t.Skip("user_history_hidden_items migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	storetest.RunNextUpState(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-nextup-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_history_hidden_items WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}

// TestPostgresNextUpStateAccountIsolation pins the account half of the scope
// rule: two accounts may share a profile id and media item ids, but one
// account's progress and hidden watermarks must never leak into the other's
// page. The shared suite covers profile isolation within one account.
func TestPostgresNextUpStateAccountIsolation(t *testing.T) {
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

	var table *string
	err = pool.QueryRow(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'user_history_hidden_items'`).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) || table == nil {
		t.Skip("user_history_hidden_items migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	userIDs := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-nextup-account-%d-%d", time.Now().UnixNano(), i),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user %d: %v", i, err)
		}
		userIDs = append(userIDs, userID)
	}
	t.Cleanup(func() {
		for _, userID := range userIDs {
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_history_hidden_items WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		}
	})

	const profileID = "shared-profile"
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	storeA := newStore(pool, userIDs[0])
	storeB := newStore(pool, userIDs[1])
	if err := storeA.SetProgressAt(ctx, profileID, "item-a", 60, 3600, false, base); err != nil {
		t.Fatalf("storeA SetProgressAt: %v", err)
	}
	if err := storeB.SetProgressAt(ctx, profileID, "item-b", 60, 3600, false, base); err != nil {
		t.Fatalf("storeB SetProgressAt: %v", err)
	}

	pageA, err := storeA.ListNextUpStatePage(ctx, profileID, nil, 10)
	if err != nil {
		t.Fatalf("storeA page: %v", err)
	}
	if len(pageA.Entries) != 1 || pageA.Entries[0].MediaItemID != "item-a" {
		t.Fatalf("storeA entries = %+v, want [item-a]", pageA.Entries)
	}
	pageB, err := storeB.ListNextUpStatePage(ctx, profileID, nil, 10)
	if err != nil {
		t.Fatalf("storeB page: %v", err)
	}
	if len(pageB.Entries) != 1 || pageB.Entries[0].MediaItemID != "item-b" {
		t.Fatalf("storeB entries = %+v, want [item-b]", pageB.Entries)
	}

	// The exact item-scoped reader holds the same account boundary as the page.
	itemsA, err := storeA.ListNextUpStateForItems(ctx, profileID, []string{"item-a", "item-b"})
	if err != nil {
		t.Fatalf("storeA items: %v", err)
	}
	if len(itemsA) != 1 || itemsA[0].MediaItemID != "item-a" {
		t.Fatalf("storeA items = %+v, want [item-a]", itemsA)
	}
	itemsB, err := storeB.ListNextUpStateForItems(ctx, profileID, []string{"item-a", "item-b"})
	if err != nil {
		t.Fatalf("storeB items: %v", err)
	}
	if len(itemsB) != 1 || itemsB[0].MediaItemID != "item-b" {
		t.Fatalf("storeB items = %+v, want [item-b]", itemsB)
	}

	// A watermark written by account B, even for account A's media item id,
	// must not hide account A's row.
	if err := storeB.SeedHiddenHistoryItem(ctx, profileID, "item-a", base.Add(time.Hour)); err != nil {
		t.Fatalf("storeB seed hidden: %v", err)
	}
	pageA, err = storeA.ListNextUpStatePage(ctx, profileID, nil, 10)
	if err != nil {
		t.Fatalf("storeA page after cross-account watermark: %v", err)
	}
	if len(pageA.Entries) != 1 || pageA.Entries[0].MediaItemID != "item-a" {
		t.Fatalf("storeA entries after cross-account watermark = %+v, want [item-a]", pageA.Entries)
	}
	itemsA, err = storeA.ListNextUpStateForItems(ctx, profileID, []string{"item-a"})
	if err != nil {
		t.Fatalf("storeA items after cross-account watermark: %v", err)
	}
	if len(itemsA) != 1 || itemsA[0].MediaItemID != "item-a" {
		t.Fatalf("storeA items after cross-account watermark = %+v, want [item-a]", itemsA)
	}
}

// deleteUserAssertingCascade removes a seeded user and fails the test if the
// delete errors or any named child table still holds the user's rows — i.e.
// if an ON DELETE CASCADE these cleanups rely on ever goes missing.
func deleteUserAssertingCascade(t *testing.T, pool *pgxpool.Pool, userID int, childTables ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Errorf("cleanup: deleting user %d: %v", userID, err)
		return
	}
	for _, table := range childTables {
		var leaked int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE user_id = $1`, userID,
		).Scan(&leaked); err != nil {
			t.Errorf("cleanup: counting %s rows for user %d: %v", table, userID, err)
			continue
		}
		if leaked != 0 {
			t.Errorf("cleanup: %d %s rows survived deleting user %d — the ON DELETE CASCADE this cleanup relies on is gone",
				leaked, table, userID)
		}
	}
}

// TestPostgresJellycompatDisplayPrefs runs the Jellyfin DisplayPreferences
// storage conformance tests against the Postgres backend; the per-user SQLite
// backend runs the same suite in internal/userdb. Skips unless
// SILO_TEST_DATABASE_URL is set and the migration is applied.
func TestPostgresJellycompatDisplayPrefs(t *testing.T) {
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

	var table *string
	err = pool.QueryRow(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'jellycompat_displayprefs'`).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) || table == nil {
		t.Skip("jellycompat_displayprefs migration has not been applied")
	}
	if err != nil {
		t.Fatalf("check migration: %v", err)
	}

	storetest.RunJellycompatDisplayPrefs(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-displayprefs-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// jellycompat_displayprefs cascades from users; assert it, as above.
		t.Cleanup(func() {
			deleteUserAssertingCascade(t, pool, userID, "jellycompat_displayprefs")
		})
		return newStore(pool, userID)
	})
}

func TestPostgresCollectionSortPreferences(t *testing.T) {
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
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.user_collection_sort_preferences')::text`).Scan(&tableName); err != nil {
		t.Fatalf("check preference table: %v", err)
	}
	if tableName == nil || *tableName == "" {
		t.Skip("collection sort preference migration has not been applied")
	}

	storetest.RunCollectionSortPreferences(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("sort-pref-conf-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			deleteUserAssertingCascade(t, pool, userID,
				"user_collection_sort_preferences", "user_profiles")
		})
		return newStore(pool, userID)
	})
}

// TestPostgresPersonalListPage runs the keyset favorites/watchlist paging
// conformance test against the Postgres backend.
// Skips unless SILO_TEST_DATABASE_URL is set.
func TestPostgresPersonalListPage(t *testing.T) {
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

	storetest.RunPersonalListPage(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-listpage-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_favorites WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_watchlist WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}

// TestPostgresProgressPage runs the keyset progress paging conformance test
// against the Postgres backend. Skips unless SILO_TEST_DATABASE_URL is set.
func TestPostgresProgressPage(t *testing.T) {
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

	storetest.RunProgressPage(t, func(t *testing.T) userstore.UserStore {
		var userID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
			fmt.Sprintf("conf-progresspage-%d", time.Now().UnixNano()),
		).Scan(&userID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, userID)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		})
		return newStore(pool, userID)
	})
}
