package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// fakeIndexerReleaseDetail is the access-checked detail seam. It records the
// identity and answers a fixed error.
type fakeIndexerReleaseDetail struct {
	calls     int
	userID    int
	profileID string
	contentID string
	err       error
}

func (f *fakeIndexerReleaseDetail) WatchDetail(_ context.Context, userID int, profileID, contentID string, _ catalog.AccessFilter) (*catalog.WatchDetail, error) {
	f.calls++
	f.userID = userID
	f.profileID = profileID
	f.contentID = contentID
	if f.err != nil {
		return nil, f.err
	}
	return &catalog.WatchDetail{ContentID: contentID}, nil
}

func indexerReleaseTestStore(t *testing.T) *virtuallibrary.IndexerReleaseStore {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return virtuallibrary.NewIndexerReleaseStore(pool)
}

func seedIndexerRelease(t *testing.T, store *virtuallibrary.IndexerReleaseStore, scope virtuallibrary.IndexerReleaseScope, guid string) virtuallibrary.IndexerRelease {
	t.Helper()
	score := 7
	release := virtuallibrary.IndexerRelease{
		GUID: guid, Title: "Heat 1995 2160p WEB-DL x265-GRP", NormalizedName: "heat 1995",
		Protocol: "usenet", Indexer: "idx", IndexerID: 3, SizeBytes: 8_000_000_000,
		FormatScore: &score, DownloadURL: "https://indexer.example/secret-nzb",
		ExpireAt: time.Now().Add(time.Hour),
	}
	if err := store.UpsertIndexerReleases(t.Context(), scope, []virtuallibrary.IndexerRelease{release}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil || len(list) == 0 {
		t.Fatalf("seed lookup: %v (%d rows)", err, len(list))
	}
	return list[0]
}

// TestRequestIndexerReleaseEnqueuesStoredURL covers the happy path: the row is
// resolved by id, access is checked through the detail seam, the server-stored
// (never client-supplied) URL is handed to the provider, and no URL is echoed.
func TestRequestIndexerReleaseEnqueuesStoredURL(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:heat-1995", MediaFolderID: 3}
	row := seedIndexerRelease(t, store, scope, "guid-1")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	var enqueuedURL, enqueuedName string
	detail := &fakeIndexerReleaseDetail{}
	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: detail,
		Enqueue: func(_ context.Context, downloadURL, name string) (string, error) {
			enqueuedURL, enqueuedName = downloadURL, name
			return "nzo_1", nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{{ID: 1, ContentID: "movie:heat-1995", FilePath: "virtual://movie/heat-1995", Container: "virtual", MediaFolderID: 3}}, nil
		},
	}
	result, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:heat-1995", row.ID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("RequestIndexerRelease: %v", err)
	}
	if result.State != "queued" || result.ReleaseID != strconv.FormatInt(row.ID, 10) {
		t.Fatalf("result = %+v, want release id %d", result, row.ID)
	}
	if enqueuedURL != "https://indexer.example/secret-nzb" || enqueuedName == "" {
		t.Fatalf("enqueued url=%q name=%q", enqueuedURL, enqueuedName)
	}
	if strings.Contains(result.ReleaseID, "indexer.example") {
		t.Fatal("result echoed the URL")
	}
	if detail.calls != 1 || detail.contentID != "movie:heat-1995" {
		t.Fatalf("detail calls = %+v", detail)
	}
	list, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].EnqueueState != "queued" || list[0].NZOID != "nzo_1" {
		t.Fatalf("row after request = %+v", list)
	}
}

// TestRequestIndexerReleaseAlreadyQueuedIsIdempotent proves a second request
// returns the same state without a second enqueue.
func TestRequestIndexerReleaseAlreadyQueuedIsIdempotent(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:idem", MediaFolderID: 3}
	row := seedIndexerRelease(t, store, scope, "guid-idem")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	enqueues := 0
	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: &fakeIndexerReleaseDetail{},
		Enqueue: func(context.Context, string, string) (string, error) {
			enqueues++
			return "nzo_idem", nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{{ID: 1, ContentID: "movie:idem", FilePath: "virtual://movie/idem", Container: "virtual", MediaFolderID: 3}}, nil
		},
	}
	first, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:idem", row.ID, catalog.AccessFilter{})
	if err != nil || first.State != "queued" {
		t.Fatalf("first request: %+v %v", first, err)
	}
	second, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:idem", row.ID, catalog.AccessFilter{})
	if err != nil || second.State != "queued" {
		t.Fatalf("second request: %+v %v", second, err)
	}
	if enqueues != 1 {
		t.Fatalf("enqueues = %d, want 1 (idempotent)", enqueues)
	}
}

// TestRequestIndexerReleaseUnknownOrForeignIsNotFound proves a release id that
// belongs to another title (or none) is a 404 and never enqueues.
func TestRequestIndexerReleaseUnknownOrForeignIsNotFound(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:foreign", MediaFolderID: 3}
	row := seedIndexerRelease(t, store, scope, "guid-foreign")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	enqueues := 0
	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: &fakeIndexerReleaseDetail{},
		Enqueue: func(context.Context, string, string) (string, error) {
			enqueues++
			return "nzo", nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{{ID: 1, ContentID: "movie:heat-1995", FilePath: "virtual://movie/heat-1995", Container: "virtual", MediaFolderID: 3}}, nil
		},
	}
	// The release exists under another content id; asking for it on this item
	// must not resolve.
	_, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:heat-1995", row.ID, catalog.AccessFilter{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
	// An id that names nothing is also a 404.
	_, err = svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:foreign", 999999, catalog.AccessFilter{})
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unknown id err = %v, want 404", err)
	}
	if enqueues != 0 {
		t.Fatalf("enqueues = %d, want 0", enqueues)
	}
}

// TestRequestIndexerReleaseAccessDenied proves the detail seam runs before any
// enqueue and its error is surfaced unchanged.
func TestRequestIndexerReleaseAccessDenied(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:denied", MediaFolderID: 3}
	row := seedIndexerRelease(t, store, scope, "guid-denied")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	enqueues := 0
	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: &fakeIndexerReleaseDetail{err: &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}},
		Enqueue: func(context.Context, string, string) (string, error) {
			enqueues++
			return "nzo", nil
		},
	}
	_, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:denied", row.ID, catalog.AccessFilter{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
	if enqueues != 0 {
		t.Fatalf("enqueues = %d, want 0 after access denial", enqueues)
	}
}

// TestRequestIndexerReleaseFailureStaysRetryable proves a provider refusal
// records the row failed and answers state=failed without echoing the URL.
func TestRequestIndexerReleaseFailureStaysRetryable(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:fail", MediaFolderID: 3}
	row := seedIndexerRelease(t, store, scope, "guid-fail")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: &fakeIndexerReleaseDetail{},
		Enqueue: func(context.Context, string, string) (string, error) {
			return "", errors.New("provider refused https://indexer.example/secret-nzb")
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{{ID: 1, ContentID: "movie:fail", FilePath: "virtual://movie/fail", Container: "virtual", MediaFolderID: 3}}, nil
		},
	}
	result, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "movie:fail", row.ID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("RequestIndexerRelease: %v", err)
	}
	if result.State != "failed" {
		t.Fatalf("state = %q, want failed", result.State)
	}
	if strings.Contains(result.Message, "indexer.example") {
		t.Fatalf("message leaks the URL: %q", result.Message)
	}
	list, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].EnqueueState != "failed" {
		t.Fatalf("row after failure = %+v", list)
	}
	if list[0].DownloadURL == "" {
		t.Fatal("stored URL was cleared")
	}
}

// TestRequestIndexerReleaseEpisodeScope proves an episode's media_id (the
// episode id) resolves a row persisted under the (series id, episode id,
// folder) scope its virtual source rows carry, and does not resolve a row under
// a wrong scope.
func TestRequestIndexerReleaseEpisodeScope(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "series:abc", EpisodeID: "episode:abc:1:2", MediaFolderID: 7}
	row := seedIndexerRelease(t, store, scope, "guid-episode")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	enqueues := 0
	svc := &VirtualIndexerReleaseService{
		Store:  store,
		Detail: &fakeIndexerReleaseDetail{},
		Enqueue: func(context.Context, string, string) (string, error) {
			enqueues++
			return "nzo_ep", nil
		},
		// The episode's media_id is the episode id; the source rows carry the
		// series id as content id and the episode id.
		EpisodeFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{{
				ID: 1, ContentID: "series:abc", EpisodeID: "episode:abc:1:2",
				FilePath: "virtual://series/abc/1/2", Container: "virtual", MediaFolderID: 7,
			}}, nil
		},
	}
	result, err := svc.RequestIndexerRelease(t.Context(), 1, "p", "episode:abc:1:2", row.ID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("RequestIndexerRelease: %v", err)
	}
	if result.State != "queued" || enqueues != 1 {
		t.Fatalf("result = %+v enqueues=%d", result, enqueues)
	}
	list, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil || len(list) != 1 || list[0].EnqueueState != "queued" {
		t.Fatalf("row after request = %+v %v", list, err)
	}
}

// TestIndexerReleasesForWatchProjection proves the watch seam projects the
// parsed display metadata and never the URL.
func TestIndexerReleasesForWatchProjection(t *testing.T) {
	store := indexerReleaseTestStore(t)
	scope := virtuallibrary.IndexerReleaseScope{ContentID: "movie:project", MediaFolderID: 3}
	seedIndexerRelease(t, store, scope, "guid-project")
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	svc := &VirtualIndexerReleaseService{Store: store}
	views, err := svc.IndexerReleasesForWatch(t.Context(), "movie:project")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %+v", views)
	}
	view := views[0]
	if view.Resolution != "2160p" || view.CodecVideo == "" {
		t.Fatalf("parsed metadata = %+v", view)
	}
	if view.HDR {
		t.Fatalf("HDR set without an HDR tag: %+v", view)
	}
	if strings.Contains(view.ReleaseID, "indexer.example") {
		t.Fatal("release id is a URL")
	}
}
