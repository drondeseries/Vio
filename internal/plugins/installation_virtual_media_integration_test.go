package plugins

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInstallationStoreUpdateReplacementCleansVirtualMediaAtomically(t *testing.T) {
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

	var installationID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO plugin_installations(plugin_id,version,install_path,enabled,update_policy)
		VALUES('test.virtual-replacement','0.0.1','/tmp/test-virtual-replacement',true,'manual')
		RETURNING id`).Scan(&installationID); err != nil {
		t.Fatalf("seed plugin installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM plugin_installations WHERE id=$1`, installationID)
	})

	const folderID = 969
	const contentID = "movie-test-virtual-replacement"
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES($1,'Atomic Replacement','movies',true)`, folderID); err != nil {
		t.Fatalf("seed virtual replacement folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,virtual_owner_installation_id,virtual_source) VALUES($1,'movie','Atomic Replacement',$2,'test-source')`, contentID, installationID); err != nil {
		t.Fatalf("seed virtual replacement item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id) VALUES($1,$2,'virtual://movie/test-virtual-replacement',0,'virtual','virtual',$3)`, contentID, folderID, installationID); err != nil {
		t.Fatalf("seed virtual replacement file: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata) VALUES($1,'test-source',$2,$3,true)`, installationID, contentID, folderID); err != nil {
		t.Fatalf("seed virtual replacement source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path) VALUES($1,'test-source',$2,$3,'virtual://movie/test-virtual-replacement')`, installationID, contentID, folderID); err != nil {
		t.Fatalf("seed virtual replacement file claim: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	version := "0.0.2"
	path := "/tmp/test-virtual-replacement-new"
	store := NewInstallationStore(pool)
	if err := store.Update(ctx, installationID, UpdateInstallationInput{
		Version:            &version,
		InstallPath:        &path,
		RemoveVirtualMedia: true,
	}); err != nil {
		t.Fatalf("update replacement installation: %v", err)
	}

	var gotVersion, gotPath string
	if err := pool.QueryRow(ctx, `SELECT version,install_path FROM plugin_installations WHERE id=$1`, installationID).Scan(&gotVersion, &gotPath); err != nil {
		t.Fatalf("inspect updated installation: %v", err)
	}
	if gotVersion != version || gotPath != path {
		t.Fatalf("updated installation version=%q path=%q, want %q/%q", gotVersion, gotPath, version, path)
	}

	var files, sourceClaims, fileClaims, items int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM media_items WHERE content_id=$1)`, contentID,
	).Scan(&files, &sourceClaims, &fileClaims, &items); err != nil {
		t.Fatalf("inspect cleaned virtual replacement catalog: %v", err)
	}
	if files != 0 || sourceClaims != 0 || fileClaims != 0 || items != 0 {
		t.Fatalf("cleaned virtual replacement files=%d source_claims=%d file_claims=%d items=%d", files, sourceClaims, fileClaims, items)
	}
}

func TestInstallationStoreUpdateReplacementRollsBackVirtualCleanupOnFailure(t *testing.T) {
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

	var installationID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO plugin_installations(plugin_id,version,install_path,enabled,update_policy)
		VALUES('test.virtual-replacement-rollback','0.0.1','/tmp/test-virtual-replacement-rollback',true,'manual')
		RETURNING id`).Scan(&installationID); err != nil {
		t.Fatalf("seed plugin installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM plugin_installations WHERE id=$1`, installationID)
	})

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("movie-test-virtual-replacement-rollback-%d", suffix)
	var folderID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(name,type,enabled) VALUES($1,'movies',true) RETURNING id`, fmt.Sprintf("Atomic Replacement Rollback %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed rollback virtual folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM virtual_media_file_source_claims WHERE plugin_installation_id=$1`, installationID)
		_, _ = pool.Exec(ctx, `DELETE FROM virtual_media_source_claims WHERE plugin_installation_id=$1`, installationID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE virtual_owner_installation_id=$1`, installationID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE virtual_owner_installation_id=$1`, installationID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,virtual_owner_installation_id,virtual_source) VALUES($1,'movie','Atomic Replacement Rollback',$2,'test-source')`, contentID, installationID); err != nil {
		t.Fatalf("seed rollback virtual item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id) VALUES($1,$2,$3,0,'virtual','virtual',$4)`, contentID, folderID, "virtual://movie/"+contentID, installationID); err != nil {
		t.Fatalf("seed rollback virtual file: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata) VALUES($1,'test-source',$2,$3,true)`, installationID, contentID, folderID); err != nil {
		t.Fatalf("seed rollback virtual source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path) VALUES($1,'test-source',$2,$3,$4)`, installationID, contentID, folderID, "virtual://movie/"+contentID); err != nil {
		t.Fatalf("seed rollback virtual file claim: %v", err)
	}

	version := "0.0.2"
	store := NewInstallationStore(pool)
	err = store.Update(ctx, installationID, UpdateInstallationInput{
		Version:            &version,
		RemoveVirtualMedia: true,
		Capabilities: []Capability{
			{Type: "test", ID: "duplicate"},
			{Type: "test", ID: "duplicate"},
		},
	})
	if err == nil {
		t.Fatal("replacement update with duplicate capabilities = nil, want error")
	}

	var gotVersion string
	if err := pool.QueryRow(ctx, `SELECT version FROM plugin_installations WHERE id=$1`, installationID).Scan(&gotVersion); err != nil {
		t.Fatalf("inspect rolled-back installation: %v", err)
	}
	if gotVersion != "0.0.1" {
		t.Fatalf("rolled-back installation version=%q, want 0.0.1", gotVersion)
	}

	var files, sourceClaims, fileClaims, items int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM media_items WHERE content_id=$1)`, contentID,
	).Scan(&files, &sourceClaims, &fileClaims, &items); err != nil {
		t.Fatalf("inspect rolled-back virtual catalog: %v", err)
	}
	if files != 1 || sourceClaims != 1 || fileClaims != 1 || items != 1 {
		t.Fatalf("rolled-back virtual catalog files=%d source_claims=%d file_claims=%d items=%d, want 1/1/1/1", files, sourceClaims, fileClaims, items)
	}
}
