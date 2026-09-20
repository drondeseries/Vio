package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestReplaceVirtualCandidatesHeaderRefreshClearsHeaders pins the nil-vs-empty
// rule on a provider re-list: a re-list that refreshes a candidate's URL with no
// request headers must clear the stored header set, because the previous URL's
// headers do not authenticate the new URL. A re-list that omits the URL keeps
// the stored headers so a header-authenticated URL is not orphaned.
func TestReplaceVirtualCandidatesHeaderRefreshClearsHeaders(t *testing.T) {
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
	contentID := fmt.Sprintf("virtual-transport-headers-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	candidatePath := basePath + "&result=alpha"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Transport Headers %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Transport Headers','matched','{}'::text[])`, contentID); err != nil {
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
		VirtualOwnerInstallationID: 5,
	}
	read := func() (string, string) {
		t.Helper()
		var resolvedURL string
		var headers *string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(resolved_url, ''), provider_request_headers::text
			FROM media_files WHERE content_id=$1 AND file_path=$2 AND virtual_owner_installation_id=5`,
			contentID, candidatePath).Scan(&resolvedURL, &headers); err != nil {
			t.Fatalf("read candidate row: %v", err)
		}
		if headers == nil {
			return resolvedURL, ""
		}
		return resolvedURL, *headers
	}

	// A first listing authenticates its URL with a header set.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                    candidatePath,
		Label:                  "1080p",
		ResolvedURL:            "http://auth.example/stream",
		ProviderRequestHeaders: map[string]string{"Referer": "http://auth.example/"},
	}}); err != nil {
		t.Fatalf("first listing: %v", err)
	}
	if url, headers := read(); url != "http://auth.example/stream" || headers == "" {
		t.Fatalf("first listing stored (%q, %q), want the authenticated URL and headers", url, headers)
	}

	// A re-list refreshes the URL and supplies no headers: the old header set
	// must be cleared, not inherited by the new URL.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:         candidatePath,
		Label:       "1080p",
		ResolvedURL: "http://headerless.example/stream",
	}}); err != nil {
		t.Fatalf("headerless refresh: %v", err)
	}
	if url, headers := read(); url != "http://headerless.example/stream" || headers != "" {
		t.Fatalf("headerless refresh stored (%q, %q), want the new URL with no headers", url, headers)
	}

	// A re-list that omits the URL keeps the stored URL and its header set.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:                    candidatePath,
		Label:                  "1080p",
		ProviderRequestHeaders: map[string]string{"Referer": "http://restored.example/"},
	}}); err != nil {
		t.Fatalf("header-only re-list: %v", err)
	}
	if url, headers := read(); url != "http://headerless.example/stream" || headers == "" {
		t.Fatalf("header-only re-list stored (%q, %q), want the URL preserved and headers updated", url, headers)
	}

	// A re-list with neither URL nor headers preserves both.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI:   candidatePath,
		Label: "1080p",
	}}); err != nil {
		t.Fatalf("metadata-only re-list: %v", err)
	}
	if url, headers := read(); url != "http://headerless.example/stream" || headers == "" {
		t.Fatalf("metadata-only re-list stored (%q, %q), want both preserved", url, headers)
	}
}
