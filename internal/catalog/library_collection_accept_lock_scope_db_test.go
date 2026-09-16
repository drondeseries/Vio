package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acceptLockScopeAdvisoryLimit is the headroom the accept transaction is
// allowed above zero advisory locks. A non-virtual accept of pass-through
// members should hold none; the constant only absorbs unrelated locks a future
// change might add deliberately.
const acceptLockScopeAdvisoryLimit = 16

type acceptLockScopeFixture struct {
	libraryID int
	appName   string
	snapshot  *models.LibraryCollection
	members   []LibraryCollectionItemInput
}

// TestAcceptPreparedItemsAdvisoryLocksDoNotScaleWithMembership is the decisive
// regression test for lock-table exhaustion: a re-sync of a large non-virtual
// collection used to take two exclusive plus one shared transaction-scoped
// advisory lock per member, exhausting max_locks_per_transaction. The accept
// transaction is paused after its pre-lock phase so the test can count the
// advisory locks it holds, then run again with a much larger membership and
// assert the count is unchanged.
func TestAcceptPreparedItemsAdvisoryLocksDoNotScaleWithMembership(t *testing.T) {
	pool := collectionSortTestPool(t)
	ctx := context.Background()

	counts := make(map[int]int, 2)
	for _, n := range []int{1, 300} {
		t.Run(fmt.Sprintf("members-%d", n), func(t *testing.T) {
			fixture := seedAcceptLockScopeCollection(t, pool, n)

			cfg, err := pgxpool.ParseConfig(os.Getenv("SILO_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.RuntimeParams["application_name"] = fixture.appName
			cfg.MaxConns = 1
			acceptPool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(acceptPool.Close)

			started := make(chan struct{})
			release := make(chan struct{})
			acceptPreparedItemsPostLockHook = func() {
				close(started)
				<-release
			}
			t.Cleanup(func() { acceptPreparedItemsPostLockHook = nil })

			done := make(chan error, 1)
			go func() {
				repo := NewLibraryCollectionRepository(acceptPool)
				done <- repo.AcceptPreparedItems(ctx, fixture.snapshot, fixture.members, map[string]preparedCollectionItem{}, NewItemRepository(pool))
			}()

			select {
			case <-started:
			case err := <-done:
				t.Fatalf("accept finished before the pre-lock hook: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the accept pre-lock phase")
			}

			count := countAcceptAdvisoryLocks(t, pool, fixture.appName)
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("accept failed: %v", err)
			}
			if count >= acceptLockScopeAdvisoryLimit {
				t.Fatalf("advisory locks held = %d, want < %d for %d members", count, acceptLockScopeAdvisoryLimit, n)
			}
			t.Logf("members=%d advisory_locks_held=%d", n, count)
			counts[n] = count
		})
	}

	if counts[1] != counts[300] {
		t.Fatalf("advisory lock count scales with membership: 1 member -> %d, 300 members -> %d", counts[1], counts[300])
	}
}

// TestAcceptFailureLeavesMembershipUnchangedDB proves the accept transaction is
// atomic for membership: a forced failure after the DELETE but during the
// INSERT must roll the previous membership back intact, so a lock-exhausted
// accept cannot leave a partial collection.
func TestAcceptFailureLeavesMembershipUnchangedDB(t *testing.T) {
	pool := collectionSortTestPool(t)
	ctx := context.Background()

	fixture := seedAcceptLockScopeCollection(t, pool, 3)
	original := membershipIDs(t, pool, fixture.snapshot.ID)

	trigger := fmt.Sprintf("accept_fail_%d", fixture.libraryID)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected lock table exhaustion' USING ERRCODE = '53200';
		END; $$;
		CREATE TRIGGER %s BEFORE INSERT ON library_collection_items
		FOR EACH ROW WHEN (NEW.collection_id = '%s')
		EXECUTE FUNCTION %s();`, trigger, trigger, fixture.snapshot.ID, trigger)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON library_collection_items; DROP FUNCTION IF EXISTS %s();`, trigger, trigger))
	})

	repo := NewLibraryCollectionRepository(pool)
	// A different desired set: a successful accept would replace membership, so
	// the assertion below is meaningful rather than a no-op.
	newMembers := []LibraryCollectionItemInput{
		{MediaItemID: fmt.Sprintf("accept-lock-scope-%d-100", fixture.libraryID), Position: 0},
		{MediaItemID: fmt.Sprintf("accept-lock-scope-%d-101", fixture.libraryID), Position: 1},
	}
	err := repo.AcceptPreparedItems(ctx, fixture.snapshot, newMembers, map[string]preparedCollectionItem{}, NewItemRepository(pool))
	if err == nil {
		t.Fatal("expected the injected accept failure")
	}
	if !isRetryableCollectionSyncError(err) {
		t.Fatalf("injected 53200 was not classified retryable: %v", err)
	}
	if got := membershipIDs(t, pool, fixture.snapshot.ID); !slices.Equal(got, original) {
		t.Fatalf("membership changed after failed accept: got %v, want %v", got, original)
	}
}

func seedAcceptLockScopeCollection(t *testing.T, pool *pgxpool.Pool, members int) acceptLockScopeFixture {
	t.Helper()
	ctx := context.Background()

	var libraryID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name,enabled) VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("accept-lock-scope-%d", time.Now().UnixNano())).Scan(&libraryID); err != nil {
		t.Fatal(err)
	}
	collectionID := fmt.Sprintf("accept-lock-scope-%d", libraryID)
	prefix := fmt.Sprintf("accept-lock-scope-%d-", libraryID)
	sourceConfig := json.RawMessage(`{"virtual_playback":false}`)

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Accept lock scope','manual',$2,$3)`,
		collectionID, libraryID, sourceConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,$2)`,
		collectionID, libraryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,sort_title)
		SELECT $1||g, 'movie', 'scope '||g, 'scope '||g FROM generate_series(0,$2-1) g`, prefix, members); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		SELECT $1, $2||g, g FROM generate_series(0,$3-1) g`, collectionID, prefix, members); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// Membership cascades from the collection; delete it before the items
		// and folder it references.
		_, _ = pool.Exec(bg, `DELETE FROM library_collections WHERE id=$1`, collectionID)
		_, _ = pool.Exec(bg, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%")
		_, _ = pool.Exec(bg, `DELETE FROM media_folders WHERE id=$1`, libraryID)
	})

	memberInputs := make([]LibraryCollectionItemInput, 0, members)
	for i := range members {
		memberInputs = append(memberInputs, LibraryCollectionItemInput{MediaItemID: fmt.Sprintf("%s%d", prefix, i), Position: i})
	}
	return acceptLockScopeFixture{
		libraryID: libraryID,
		appName:   fmt.Sprintf("accept-lock-scope-%d", libraryID),
		snapshot:  &models.LibraryCollection{ID: collectionID, LibraryID: libraryID, LibraryIDs: []int{libraryID}, CollectionType: "manual", SourceConfig: sourceConfig},
		members:   memberInputs,
	}
}

func countAcceptAdvisoryLocks(t *testing.T, pool *pgxpool.Pool, appName string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM pg_locks locks
		JOIN pg_stat_activity activity ON activity.pid = locks.pid
		WHERE locks.locktype = 'advisory' AND activity.application_name = $1`, appName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func membershipIDs(t *testing.T, pool *pgxpool.Pool, collectionID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT media_item_id FROM library_collection_items WHERE collection_id=$1`, collectionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}
