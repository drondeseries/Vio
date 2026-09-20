package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The retention guards exist because user-visible playback state references
// catalog content by bare ID (or by media_files.id) with no foreign key, so a
// background virtual-catalog sweep that deletes the item silently strands
// Continue Watching/history rows. These tests pin the observable outcome: a
// referenced item (and the file a live session or the user's last-played
// version depends on) survives reconciliation, while an unreferenced item is
// still swept.

// seedRetentionUser creates a dedicated login for retention fixtures. Deleting
// the username first removes any per-user rows from an earlier failed run so
// reruns stay deterministic.
func seedRetentionUser(t *testing.T, pool *pgxpool.Pool, username string) int {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE username = $1`, username); err != nil {
		t.Fatalf("reset retention user: %v", err)
	}
	var userID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`, username,
	).Scan(&userID); err != nil {
		t.Fatalf("seed retention user: %v", err)
	}
	return userID
}

func seedRetentionFolder(t *testing.T, pool *pgxpool.Pool, folderID int, name string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media_folders(id,name,type,enabled) VALUES($1,$2,'movies',true)`, folderID, name,
	); err != nil {
		t.Fatalf("seed retention folder: %v", err)
	}
}

// seedRetentionVirtualItem inserts a virtual movie plus the source claim a
// reconciliation sweep needs to classify it as stale. When withFile is true it
// also inserts one virtual media file and its file claim, which is what the
// file-level guard has to protect.
func seedRetentionVirtualItem(t *testing.T, pool *pgxpool.Pool, contentID, tmdbID, source string, folderID, ownerID int, withFile bool) (fileID int, filePath string) {
	t.Helper()
	ctx := context.Background()
	filePath = "virtual://movie/" + contentID
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(
			content_id,type,title,tmdb_id,status,virtual_owner_installation_id,virtual_source,virtual_last_seen_at
		) VALUES($1,'movie','Retention Fixture',$2,'matched',$3,$4,NOW())`,
		contentID, tmdbID, ownerID, source); err != nil {
		t.Fatalf("seed retention item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata,last_seen_at
		) VALUES($1,$2,$3,$4,true,NOW())`, ownerID, source, contentID, folderID); err != nil {
		t.Fatalf("seed retention source claim: %v", err)
	}
	if !withFile {
		return 0, ""
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id
		) VALUES($1,$2,$3,0,'virtual','virtual',$4) RETURNING id`,
		contentID, folderID, filePath, ownerID).Scan(&fileID); err != nil {
		t.Fatalf("seed retention file: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path,last_seen_at
		) VALUES($1,$2,$3,$4,$5,NOW())`, ownerID, source, contentID, folderID, filePath); err != nil {
		t.Fatalf("seed retention file claim: %v", err)
	}
	return fileID, filePath
}

func countRetentionRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count retention rows: %v", err)
	}
	return n
}

// TestVirtualReconcilePreservesWatchedItem covers the reported data-loss: a
// movie that still has user_watch_history must not be deleted by a
// reconciliation that no longer lists it.
func TestVirtualReconcilePreservesWatchedItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 960, "RetentionWatched")

	const (
		contentID = "movie-tmdb-retain-watch"
		source    = "provider-retain"
	)
	_, _ = seedRetentionVirtualItem(t, pool, contentID, "9001", source, 960, 11, true)
	userID := seedRetentionUser(t, pool, "retention-watch-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_history(id,user_id,profile_id,media_item_id,watched_at)
		VALUES($1,$2,'default',$3,NOW())`, "hist-"+contentID, userID, contentID); err != nil {
		t.Fatalf("seed watch history: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{960})
	if err != nil {
		t.Fatalf("reconcile watched item: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0 for a watched item", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 1 {
		t.Fatalf("watched item rows=%d, want 1 (deleted by sweep)", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM user_watch_history WHERE media_item_id=$1`, contentID); got != 1 {
		t.Fatalf("watch history rows=%d, want 1", got)
	}
}

// TestVirtualReconcilePreservesItemForPlayedFile covers a user_watch_progress
// row whose media_item_id is elsewhere but whose last_file_id points at this
// item's file. The file sweep runs before the item delete, so the file-level
// guard must retain the played file for the item to survive at all.
func TestVirtualReconcilePreservesItemForPlayedFile(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 961, "RetentionPlayedFile")

	const (
		contentID = "movie-tmdb-retain-played-file"
		source    = "provider-retain"
	)
	fileID, _ := seedRetentionVirtualItem(t, pool, contentID, "9002", source, 961, 11, true)
	userID := seedRetentionUser(t, pool, "retention-played-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress(user_id,profile_id,media_item_id,position_seconds,duration_seconds,last_file_id)
		VALUES($1,'default','movie-tmdb-elsewhere',30,3600,$2)`, userID, fileID); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{961})
	if err != nil {
		t.Fatalf("reconcile played-file item: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 1 {
		t.Fatalf("played-file item rows=%d, want 1", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_files WHERE id=$1`, fileID); got != 1 {
		t.Fatalf("last-played file rows=%d, want 1 (file sweep deleted it)", got)
	}
}

// TestVirtualReconcilePreservesOpenSessionItemWithoutFile isolates the
// item-level guard: the item has no media file at all, and an open ABS session
// names its content_id. Deleting the item would strand the live session.
func TestVirtualReconcilePreservesOpenSessionItemWithoutFile(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 962, "RetentionOpenSession")

	const (
		contentID = "movie-tmdb-retain-open-session"
		source    = "provider-retain"
	)
	_, _ = seedRetentionVirtualItem(t, pool, contentID, "9003", source, 962, 11, false)
	userID := seedRetentionUser(t, pool, "retention-session-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO abs_playback_sessions(id,user_id,profile_id,content_id,media_file_id)
		VALUES($1,$2,'default',$3,NULL)`, "abs-"+contentID, userID, contentID); err != nil {
		t.Fatalf("seed open abs session: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{962})
	if err != nil {
		t.Fatalf("reconcile open-session item: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 1 {
		t.Fatalf("open-session item rows=%d, want 1", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM abs_playback_sessions WHERE content_id=$1 AND closed_at IS NULL`, contentID); got != 1 {
		t.Fatalf("open abs session rows=%d, want 1", got)
	}
}

// TestVirtualReconcilePreservesFileForOpenSession covers an open ABS session
// that names the media file directly. The file has ON DELETE SET NULL, so
// without the file-level guard the sweep would strip the live session's file
// identity.
func TestVirtualReconcilePreservesFileForOpenSession(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 963, "RetentionSessionFile")

	const (
		contentID = "movie-tmdb-retain-session-file"
		source    = "provider-retain"
	)
	fileID, _ := seedRetentionVirtualItem(t, pool, contentID, "9004", source, 963, 11, true)
	userID := seedRetentionUser(t, pool, "retention-session-file-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO abs_playback_sessions(id,user_id,profile_id,content_id,media_file_id)
		VALUES($1,$2,'default',$3,$4)`, "abs-file-"+contentID, userID, contentID, fileID); err != nil {
		t.Fatalf("seed open abs session: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{963})
	if err != nil {
		t.Fatalf("reconcile session-file item: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_files WHERE id=$1`, fileID); got != 1 {
		t.Fatalf("session media file rows=%d, want 1 (file sweep deleted it)", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM abs_playback_sessions WHERE media_file_id=$1 AND closed_at IS NULL`, fileID); got != 1 {
		t.Fatalf("session rows with file identity=%d, want 1", got)
	}
}

// TestVirtualReconcilePreservesFreshAttemptItem covers an unexpired protocol-v3
// attempt. The attempt's effective_media_file_id has ON DELETE CASCADE, so the
// file-level guard is what keeps the file (and therefore the attempt and item)
// alive; an expired attempt must not.
func TestVirtualReconcilePreservesFreshAttemptItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 964, "RetentionFreshAttempt")

	const (
		contentID = "movie-tmdb-retain-fresh-attempt"
		source    = "provider-retain"
	)
	fileID, _ := seedRetentionVirtualItem(t, pool, contentID, "9005", source, 964, 11, true)
	userID := seedRetentionUser(t, pool, "retention-attempt-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id,session_id,user_id,profile_id,
			requested_media_file_id,effective_media_file_id,
			current_plan_id,current_plan,normalized_request,expires_at)
		VALUES('attempt-retain-fresh',gen_random_uuid(),$1,'default',$2,$2,'plan-retain','{}'::jsonb,'{}'::jsonb,NOW()+INTERVAL '1 hour')`,
		userID, fileID); err != nil {
		t.Fatalf("seed fresh attempt: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{964})
	if err != nil {
		t.Fatalf("reconcile fresh-attempt item: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_files WHERE id=$1`, fileID); got != 1 {
		t.Fatalf("attempt media file rows=%d, want 1", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM playback_v3_attempts WHERE effective_media_file_id=$1`, fileID); got != 1 {
		t.Fatalf("fresh attempt rows=%d, want 1", got)
	}
}

// TestVirtualReconcileRemovesItemWithOnlyExpiredAttempt is the negative half:
// an expired attempt is already cleanup and must not retain the item.
func TestVirtualReconcileRemovesItemWithOnlyExpiredAttempt(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 965, "RetentionExpiredAttempt")

	const (
		contentID = "movie-tmdb-retain-expired-attempt"
		source    = "provider-retain"
	)
	fileID, _ := seedRetentionVirtualItem(t, pool, contentID, "9006", source, 965, 11, true)
	userID := seedRetentionUser(t, pool, "retention-expired-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id,session_id,user_id,profile_id,
			requested_media_file_id,effective_media_file_id,
			current_plan_id,current_plan,normalized_request,expires_at)
		VALUES('attempt-retain-expired',gen_random_uuid(),$1,'default',$2,$2,'plan-retain','{}'::jsonb,'{}'::jsonb,NOW()-INTERVAL '1 hour')`,
		userID, fileID); err != nil {
		t.Fatalf("seed expired attempt: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{965})
	if err != nil {
		t.Fatalf("reconcile expired-attempt item: %v", err)
	}
	if result.ItemsRemoved != 1 {
		t.Fatalf("items_removed=%d, want 1 for an item guarded only by an expired attempt", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 0 {
		t.Fatalf("item rows=%d, want 0", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_files WHERE id=$1`, fileID); got != 0 {
		t.Fatalf("file rows=%d, want 0", got)
	}
}

// TestVirtualReconcileRemovesUnreferencedItem proves the guards do not make
// reconciliation a no-op: an item with no user or playback references is still
// removed when it leaves the keep set.
func TestVirtualReconcileRemovesUnreferencedItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 966, "RetentionUnreferenced")

	const (
		contentID = "movie-tmdb-retain-unreferenced"
		source    = "provider-retain"
	)
	fileID, _ := seedRetentionVirtualItem(t, pool, contentID, "9007", source, 966, 11, true)

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, []string{"movie-tmdb-keep"}, []int{966})
	if err != nil {
		t.Fatalf("reconcile unreferenced item: %v", err)
	}
	if result.ItemsRemoved != 1 {
		t.Fatalf("items_removed=%d, want 1", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 0 {
		t.Fatalf("item rows=%d, want 0", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_files WHERE id=$1`, fileID); got != 0 {
		t.Fatalf("file rows=%d, want 0", got)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1`, contentID); got != 0 {
		t.Fatalf("source claim rows=%d, want 0", got)
	}
}

// TestVirtualReconcileRequestSourceStillWithdrawsUnreferencedItem pins the
// conclusion on the request: bypass: an empty keep set for a single-request
// source is legitimate and must still withdraw an item with no user state.
func TestVirtualReconcileRequestSourceStillWithdrawsUnreferencedItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 967, "RetentionRequestEmpty")

	const (
		contentID = "movie-tmdb-retain-request-empty"
		source    = "request:movie:req-retain-1"
	)
	_, _ = seedRetentionVirtualItem(t, pool, contentID, "9008", source, 967, 11, true)

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, nil, []int{967})
	if err != nil {
		t.Fatalf("reconcile request source with empty keep set: %v", err)
	}
	if result.ItemsRemoved != 1 {
		t.Fatalf("items_removed=%d, want 1", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 0 {
		t.Fatalf("request item rows=%d, want 0", got)
	}
}

// TestVirtualReconcileRequestSourceRetainsWatchedItem proves the request:
// bypass only skips the empty-keep-set refusal; it does not authorize deleting
// an item the new guards protect.
func TestVirtualReconcileRequestSourceRetainsWatchedItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 968, "RetentionRequestWatched")

	const (
		contentID = "movie-tmdb-retain-request-watched"
		source    = "request:movie:req-retain-2"
	)
	_, _ = seedRetentionVirtualItem(t, pool, contentID, "9009", source, 968, 11, true)
	userID := seedRetentionUser(t, pool, "retention-request-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress(user_id,profile_id,media_item_id,position_seconds,duration_seconds)
		VALUES($1,'default',$2,30,3600)`, userID, contentID); err != nil {
		t.Fatalf("seed request watch progress: %v", err)
	}

	result, err := NewVirtualMediaRegistrar(pool).ReconcileVirtualMedia(ctx, 11, source, nil, []int{968})
	if err != nil {
		t.Fatalf("reconcile watched request with empty keep set: %v", err)
	}
	if result.ItemsRemoved != 0 {
		t.Fatalf("items_removed=%d, want 0", result.ItemsRemoved)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 1 {
		t.Fatalf("watched request item rows=%d, want 1", got)
	}
}

// TestCleanupRequestVirtualMediaRetainsWatchedItem guards the sibling
// request-cancellation path: canceling a request must not delete an item the
// user has watch history for.
func TestCleanupRequestVirtualMediaRetainsWatchedItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	seedRetentionFolder(t, pool, 969, "RetentionRequestCleanup")

	const contentID = "movie-tmdb-retain-request-cleanup"
	_, _ = seedRetentionVirtualItem(t, pool, contentID, "9010", "request:movie:req-retain-3", 969, 11, true)
	userID := seedRetentionUser(t, pool, "retention-request-cleanup-user")
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_history(id,user_id,profile_id,media_item_id,watched_at)
		VALUES($1,$2,'default',$3,NOW())`, "hist-"+contentID, userID, contentID); err != nil {
		t.Fatalf("seed request watch history: %v", err)
	}

	if err := NewItemRepository(pool).CleanupRequestVirtualMedia(ctx, "movie", 9010, "", ""); err != nil {
		t.Fatalf("cleanup request virtual media: %v", err)
	}
	if got := countRetentionRows(t, pool, `SELECT count(*) FROM media_items WHERE content_id=$1`, contentID); got != 1 {
		t.Fatalf("watched request item rows=%d, want 1 (cancellation deleted it)", got)
	}
}

// TestVirtualRetentionGuardsCoverVisibleState is the cheap shape check: it fails
// fast (without a database) when a future edit drops a reference from either
// guard.
func TestVirtualRetentionGuardsCoverVisibleState(t *testing.T) {
	item := normalizePredicateSQL(virtualItemRetentionGuard)
	for _, want := range []string{
		"NOT EXISTS (SELECT 1 FROM user_watch_history uwh WHERE uwh.media_item_id = mi.content_id)",
		"SELECT 1 FROM user_watch_progress uwp WHERE uwp.media_item_id = mi.content_id",
		"uwp.last_file_id IN (SELECT mf.id FROM media_files mf WHERE mf.content_id = mi.content_id)",
		"SELECT 1 FROM abs_playback_sessions aps WHERE aps.content_id = mi.content_id AND aps.closed_at IS NULL",
		"SELECT 1 FROM playback_v3_attempts pva WHERE pva.expires_at > NOW()",
	} {
		if !strings.Contains(item, normalizePredicateSQL(want)) {
			t.Fatalf("item retention guard missing %q:\n%s", want, item)
		}
	}
	file := normalizePredicateSQL(virtualFileRetentionGuard)
	for _, want := range []string{
		"NOT EXISTS (SELECT 1 FROM user_watch_progress uwp WHERE uwp.last_file_id = mf.id)",
		"NOT EXISTS (SELECT 1 FROM abs_playback_sessions aps WHERE aps.media_file_id = mf.id AND aps.closed_at IS NULL)",
		"SELECT 1 FROM playback_v3_attempts pva WHERE pva.expires_at > NOW()",
	} {
		if !strings.Contains(file, normalizePredicateSQL(want)) {
			t.Fatalf("file retention guard missing %q:\n%s", want, file)
		}
	}
}
