package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestCatalogVersionsCheckHTTP exercises the batched version liveness check
// against a real database: dead pins are stamped failed_at and reported
// unavailable, live candidates are cleared and reported available, local
// missing files are reported unavailable without any provider probe, and
// ambiguous provider errors leave the stamp unchanged.
func TestCatalogVersionsCheckHTTP(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("versions-check-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	insertFile := func(path, container string, owner int, failedAt, missingSince any) int {
		t.Helper()
		var id int
		if err := pool.QueryRow(t.Context(), `
			INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at,missing_since)
			VALUES ($1,$2,$3,1000,$4,$5,$6,$7) RETURNING id`,
			prefix+"movie", library, path, container, owner, failedAt, missingSince).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	deadPinID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=dead", time.Now().UnixNano()), "virtual", 7, nil, nil)
	livePinID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=live", time.Now().UnixNano()), "virtual", 7, time.Now(), nil)
	missingLocalID := insertFile(fmt.Sprintf("/media/%s-missing.mkv", prefix), "", 0, nil, time.Now())
	presentLocalID := insertFile(fmt.Sprintf("/media/%s-present.mkv", prefix), "", 0, nil, nil)
	ambiguousID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=ambiguous", time.Now().UnixNano()), "virtual", 7, nil, nil)
	joinedAmbiguousID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=joined", time.Now().UnixNano()), "virtual", 7, nil, nil)

	var resolveCalls atomic.Int64
	var deadPinCalls atomic.Int64
	resolver := VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolveCalls.Add(1)
		switch {
		case strings.Contains(uri, "result=dead"):
			deadPinCalls.Add(1)
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		case strings.Contains(uri, "result=live"):
			return ResolvedVirtualMedia{URL: "http://provider.test/live.mp4", URI: uri, CandidateID: "live"}, nil
		case strings.Contains(uri, "result=ambiguous"):
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider 7 request failed")
		case strings.Contains(uri, "result=joined"):
			// Owner provider RPC-failed while a fallback answered with no
			// matching candidate: the pin's owner may simply be down.
			return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: virtual stream provider 7 request failed; virtual stream provider returned no matching candidate")
		default:
			return ResolvedVirtualMedia{}, errors.New("unexpected uri")
		}
	})

	repo := scanner.NewFileRepository(pool)
	itemRepo := catalog.NewItemRepository(pool)
	h := &CatalogResourceHandler{
		FileResolver:       repo,
		VirtualResolver:    resolver,
		MarkVirtualFailed:  repo.MarkVirtualCandidateFailed,
		ClearVirtualFailed: repo.ClearVirtualCandidateFailed,
		ItemAccess:         itemRepo,
		EpisodeLookup:      catalog.NewEpisodeRepository(pool),
		ExtraLookup:        catalog.NewExtraRepository(pool),
	}
	router := chi.NewRouter()
	router.Post("/catalog/versions/check", h.HandleCheckVersions)

	post := func(ids ...int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string][]int{"file_ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	readFailedAt := func(fileID int) bool {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(t.Context(), `SELECT failed_at FROM media_files WHERE id=$1`, fileID).Scan(&failedAt); err != nil {
			t.Fatal(err)
		}
		return failedAt != nil
	}

	t.Run("dead pin stamped and reported unavailable", func(t *testing.T) {
		rec := post(deadPinID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != deadPinID || resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if !readFailedAt(deadPinID) {
			t.Fatal("dead pin was not stamped failed_at")
		}
		if deadPinCalls.Load() != 1 {
			t.Fatalf("dead pin resolver calls = %d, want 1", deadPinCalls.Load())
		}
	})

	t.Run("live candidate reported available without clearing transport evidence", func(t *testing.T) {
		if !readFailedAt(livePinID) {
			t.Fatal("precondition: live candidate must start stamped failed")
		}
		rec := post(livePinID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		// Listing availability is true (the pinned candidate resolved), but a
		// metadata-only check never opens media, so the transport-failure
		// stamp survives — only a real delivery clears it.
		if len(resp.Results) != 1 || resp.Results[0].FileID != livePinID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if !readFailedAt(livePinID) {
			t.Fatal("metadata-only check erased transport-failure evidence")
		}
	})

	t.Run("local missing file unavailable without probe", func(t *testing.T) {
		before := resolveCalls.Load()
		rec := post(missingLocalID, presentLocalID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		byID := map[int]bool{}
		for _, r := range resp.Results {
			byID[r.FileID] = r.Available
		}
		if byID[missingLocalID] {
			t.Fatalf("missing local file reported available: %#v", resp.Results)
		}
		if !byID[presentLocalID] {
			t.Fatalf("present local file reported unavailable: %#v", resp.Results)
		}
		if resolveCalls.Load() != before {
			t.Fatalf("local files triggered %d provider probes, want 0", resolveCalls.Load()-before)
		}
	})

	t.Run("ambiguous provider error leaves stamp unchanged", func(t *testing.T) {
		rec := post(ambiguousID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != ambiguousID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if readFailedAt(ambiguousID) {
			t.Fatal("ambiguous provider error must not stamp failed_at")
		}
	})

	t.Run("joined owner-down error leaves stamp unchanged", func(t *testing.T) {
		rec := post(joinedAmbiguousID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != joinedAmbiguousID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if readFailedAt(joinedAmbiguousID) {
			t.Fatal("joined owner-down error must not stamp failed_at")
		}
	})

	t.Run("unknown file id reported unavailable", func(t *testing.T) {
		rec := post(999999999)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != 999999999 || resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
	})

	t.Run("batch over cap rejected", func(t *testing.T) {
		ids := make([]int, maxVersionCheckFiles+1)
		for i := range ids {
			ids[i] = i + 1
		}
		rec := post(ids...)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty batch rejected", func(t *testing.T) {
		rec := post()
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCatalogVersionsCheckAccess verifies the liveness check authorizes each
// file for the requesting profile BEFORE resolving or stamping anything: a
// profile without access to a file gets the same available=false shape as an
// unknown ID, with no resolver call and no health-stamping callback, while a
// profile with access behaves as before.
func TestCatalogVersionsCheckAccess(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("versions-check-access-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check Access','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	var fileID int
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES ($1,$2,$3,1000,'virtual',7) RETURNING id`,
		prefix+"movie", library, fmt.Sprintf("virtual://movie/tt%d?result=live", time.Now().UnixNano())).Scan(&fileID); err != nil {
		t.Fatal(err)
	}

	var resolveCalls atomic.Int64
	var markCalls atomic.Int64
	var clearCalls atomic.Int64
	resolver := VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolveCalls.Add(1)
		return ResolvedVirtualMedia{URL: "http://provider.test/live.mp4", URI: uri, CandidateID: "live"}, nil
	})
	repo := scanner.NewFileRepository(pool)
	itemRepo := catalog.NewItemRepository(pool)
	h := &CatalogResourceHandler{
		FileResolver:    repo,
		VirtualResolver: resolver,
		MarkVirtualFailed: func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error {
			markCalls.Add(1)
			return repo.MarkVirtualCandidateFailed(ctx, fileID, expectedFilePath, observedFailedAt)
		},
		ClearVirtualFailed: func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error {
			clearCalls.Add(1)
			return repo.ClearVirtualCandidateFailed(ctx, fileID, expectedFilePath, observedFailedAt)
		},
		ItemAccess:    itemRepo,
		EpisodeLookup: catalog.NewEpisodeRepository(pool),
		ExtraLookup:   catalog.NewExtraRepository(pool),
	}
	router := chi.NewRouter()
	router.Post("/catalog/versions/check", h.HandleCheckVersions)

	post := func(scope access.Scope) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string][]int{"file_ids": {fileID}})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(access.SetScope(r.Context(), scope))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	t.Run("profile without access is indistinguishable from unknown", func(t *testing.T) {
		rec := post(access.Scope{AllowedLibraryIDs: []int{}})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if resolveCalls.Load() != 0 {
			t.Fatalf("resolver invoked %d times for an inaccessible file, want 0", resolveCalls.Load())
		}
		if markCalls.Load() != 0 || clearCalls.Load() != 0 {
			t.Fatalf("health stamping invoked for an inaccessible file: mark=%d clear=%d, want 0/0", markCalls.Load(), clearCalls.Load())
		}
	})

	t.Run("profile with access resolves and reports available", func(t *testing.T) {
		rec := post(access.Scope{})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if resolveCalls.Load() != 1 {
			t.Fatalf("resolver calls = %d, want 1", resolveCalls.Load())
		}
		// A metadata-only check resolves a URL but never opens media, so it
		// must NOT clear a transport-failure stamp — only a real delivery does.
		if clearCalls.Load() != 0 {
			t.Fatalf("clear calls = %d, want 0 (listing availability is not transport evidence)", clearCalls.Load())
		}
	})
}

// TestCatalogVersionsCheckInFlightFencing exercises the version-check handler
// while the DB row changes concurrently with an in-flight resolution: the
// fake resolver signals `started` once the handler has read the row and is
// blocked inside the provider call, the test then rotates the candidate
// (file_path) or stamps a newer failure directly through the repository, and
// only then releases the resolver. The handler's fenced conditional write
// (expectedFilePath + observedFailedAt) must be a no-op against the mutated
// row, so the row reflects ONLY the concurrent mutation — never the stale
// verdict the check computed from the pre-mutation identity. This is the
// handler-level counterpart to the repository-level CAS tests in
// internal/scanner/file_repo_clear_virtual_failed_test.go, which mutate and
// call sequentially and therefore never have a resolution in flight.
func TestCatalogVersionsCheckInFlightFencing(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("versions-check-fencing-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check Fencing','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	originalPath := fmt.Sprintf("virtual://movie/tt%d?result=original", time.Now().UnixNano())
	replacementPath := fmt.Sprintf("virtual://movie/tt%d?result=replacement", time.Now().UnixNano())
	var fileID int
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES ($1,$2,$3,1000,'virtual',7) RETURNING id`,
		prefix+"movie", library, originalPath).Scan(&fileID); err != nil {
		t.Fatal(err)
	}

	repo := scanner.NewFileRepository(pool)
	itemRepo := catalog.NewItemRepository(pool)

	readRow := func() (path string, failedAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT file_path, failed_at FROM media_files WHERE id=$1`, fileID).Scan(&path, &failedAt); err != nil {
			t.Fatal(err)
		}
		return path, failedAt
	}

	// newBlockingResolver returns a resolver that signals `started` once the
	// handler is inside the provider call (the row has been read and the
	// fenced identity captured) and then blocks until `release` is closed.
	newBlockingResolver := func(verdict error) (VirtualMediaDetailedResolver, *sync.WaitGroup, chan struct{}, chan struct{}) {
		var wg sync.WaitGroup
		started := make(chan struct{})
		release := make(chan struct{})
		wg.Add(1)
		resolver := VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			defer wg.Done()
			close(started)
			<-release
			if verdict != nil {
				return ResolvedVirtualMedia{}, verdict
			}
			return ResolvedVirtualMedia{URL: "http://provider.test/live.mp4", URI: uri, CandidateID: "live"}, nil
		})
		return resolver, &wg, started, release
	}

	post := func(router *chi.Mux) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string][]int{"file_ids": {fileID}})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	// Subtest 1: the check resolves the ORIGINAL path as a confirmed dead pin
	// while the provider rotates the candidate to replacementPath. The stale
	// mark must be a no-op: failed_at stays NULL and file_path stays the
	// replacement, and the response verdict (computed from the stale identity)
	// is available=false.
	t.Run("stale dead-pin mark is a no-op while candidate rotates", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NULL WHERE id=$2`, originalPath, fileID)
		resolver, wg, started, release := newBlockingResolver(errors.New("virtual stream provider returned no matching candidate"))
		h := &CatalogResourceHandler{
			FileResolver:       repo,
			VirtualResolver:    resolver,
			MarkVirtualFailed:  repo.MarkVirtualCandidateFailed,
			ClearVirtualFailed: repo.ClearVirtualCandidateFailed,
			ItemAccess:         itemRepo,
			EpisodeLookup:      catalog.NewEpisodeRepository(pool),
			ExtraLookup:        catalog.NewExtraRepository(pool),
		}
		router := chi.NewRouter()
		router.Post("/catalog/versions/check", h.HandleCheckVersions)

		recCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { recCh <- post(router) }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("resolver never started")
		}
		// The handler is now blocked inside the provider call with the stale
		// identity (originalPath, failed_at nil) captured. Rotate the row.
		exec(`UPDATE media_files SET file_path=$1 WHERE id=$2`, replacementPath, fileID)
		close(release)
		rec := <-recCh
		wg.Wait()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || resp.Results[0].Available {
			t.Fatalf("stale dead-pin verdict must report unavailable: %#v", resp.Results)
		}
		path, failedAt := readRow()
		if path != replacementPath {
			t.Fatalf("stale mark modified file_path: %q, want %q", path, replacementPath)
		}
		if failedAt != nil {
			t.Fatal("stale dead-pin mark stamped the rotated replacement candidate")
		}
	})

	// Subtest 2: the check resolves the ORIGINAL path as live while another
	// session stamps the candidate dead. The stale clear must be a no-op:
	// failed_at stays set and file_path stays the original, and the response
	// verdict (computed from the stale identity) is available=true.
	t.Run("stale live clear is a no-op while a newer failure lands", func(t *testing.T) {
		exec(`UPDATE media_files SET file_path=$1, failed_at=NULL WHERE id=$2`, originalPath, fileID)
		resolver, wg, started, release := newBlockingResolver(nil)
		h := &CatalogResourceHandler{
			FileResolver:       repo,
			VirtualResolver:    resolver,
			MarkVirtualFailed:  repo.MarkVirtualCandidateFailed,
			ClearVirtualFailed: repo.ClearVirtualCandidateFailed,
			ItemAccess:         itemRepo,
			EpisodeLookup:      catalog.NewEpisodeRepository(pool),
			ExtraLookup:        catalog.NewExtraRepository(pool),
		}
		router := chi.NewRouter()
		router.Post("/catalog/versions/check", h.HandleCheckVersions)

		recCh := make(chan *httptest.ResponseRecorder, 1)
		go func() { recCh <- post(router) }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("resolver never started")
		}
		// The handler is blocked inside the provider call with the stale
		// identity (originalPath, failed_at nil) captured. Stamp the row dead.
		exec(`UPDATE media_files SET failed_at=NOW() WHERE id=$1`, fileID)
		close(release)
		rec := <-recCh
		wg.Wait()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || !resp.Results[0].Available {
			t.Fatalf("stale live verdict must report available: %#v", resp.Results)
		}
		path, failedAt := readRow()
		if path != originalPath {
			t.Fatalf("stale clear modified file_path: %q, want %q", path, originalPath)
		}
		if failedAt == nil {
			t.Fatal("stale live clear erased a newer failure stamp")
		}
	})
}

// TestCatalogVersionsCheckStrictCandidateIdentity pins the strict check
// semantics: a metadata-only check must verify that the resolver's answer
// names the REQUESTED candidate. Ordinary playback may substitute candidates[0]
// for an absent pin, but a substituted answer is evidence the pin is gone, not
// that it recovered — and a metadata-only check must never clear a
// transport-failure stamp regardless of the verdict.
func TestCatalogVersionsCheckStrictCandidate(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("versions-check-strict-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check Strict','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	// A transport-failed candidate: failed_at is set (no bytes at stream open).
	pinnedPath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=A", time.Now().UnixNano())
	var fileID int
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
		VALUES ($1,$2,$3,1000,'virtual',7,NOW()) RETURNING id`,
		prefix+"movie", library, pinnedPath).Scan(&fileID); err != nil {
		t.Fatal(err)
	}

	repo := scanner.NewFileRepository(pool)
	itemRepo := catalog.NewItemRepository(pool)

	readRow := func() (path string, failedAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT file_path, failed_at FROM media_files WHERE id=$1`, fileID).Scan(&path, &failedAt); err != nil {
			t.Fatal(err)
		}
		return path, failedAt
	}

	newHandler := func(resolver VirtualMediaDetailedResolver) (*CatalogResourceHandler, *chi.Mux) {
		h := &CatalogResourceHandler{
			FileResolver:       repo,
			VirtualResolver:    resolver,
			MarkVirtualFailed:  repo.MarkVirtualCandidateFailed,
			ClearVirtualFailed: repo.ClearVirtualCandidateFailed,
			ItemAccess:         itemRepo,
			EpisodeLookup:      catalog.NewEpisodeRepository(pool),
			ExtraLookup:        catalog.NewExtraRepository(pool),
		}
		router := chi.NewRouter()
		router.Post("/catalog/versions/check", h.HandleCheckVersions)
		return h, router
	}
	post := func(router *chi.Mux) versionCheckResponse {
		t.Helper()
		body, err := json.Marshal(map[string][]int{"file_ids": {fileID}})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("substituted candidate is not recovery for the requested pin", func(t *testing.T) {
		// Request pinned A; the provider (playing ordinary playback semantics)
		// substitutes candidate B. The strict check must not treat B as
		// evidence A recovered: no clear, stamp survives, verdict unavailable.
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, pinnedPath, fileID)
		var sawForceRefresh atomic.Bool
		_, router := newHandler(VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			// Strict check semantics require a forced refresh: the ordinary
			// candidates[0] substitution branch requires !forceRefresh.
			sawForceRefresh.Store(forceRefresh)
			// Ordinary fallback semantics: requested A absent → substitute B.
			return ResolvedVirtualMedia{URL: "http://provider.test/b.mp4", URI: "virtual://movie/substituted?result=B", CandidateID: "B"}, nil
		}))
		resp := post(router)
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || resp.Results[0].Available {
			t.Fatalf("substituted candidate must not report the pinned A as available: %#v", resp.Results)
		}
		if !sawForceRefresh.Load() {
			t.Fatal("strict check resolved without forceRefresh=true; the candidates[0] substitution branch was not disabled")
		}
		path, failedAt := readRow()
		if path != pinnedPath || failedAt == nil {
			t.Fatalf("substituted resolution must not clear the transport failure: path=%q failed_at=%v", path, failedAt)
		}
	})

	// Unqualified pin form: ?result=A without a profile parameter — the
	// identity verification must hold for both URI shapes.
	t.Run("unqualified pinned form also requires returned identity", func(t *testing.T) {
		unqualifiedPath := fmt.Sprintf("virtual://movie/tt%d?result=A", time.Now().UnixNano())
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, unqualifiedPath, fileID)
		_, router := newHandler(VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://provider.test/b.mp4", URI: "virtual://movie/substituted?result=B", CandidateID: "B"}, nil
		}))
		resp := post(router)
		if len(resp.Results) != 1 || resp.Results[0].FileID != fileID || resp.Results[0].Available {
			t.Fatalf("substituted candidate must not report the unqualified pinned A as available: %#v", resp.Results)
		}
		path, failedAt := readRow()
		if path != unqualifiedPath || failedAt == nil {
			t.Fatalf("substituted resolution must not clear the transport failure: path=%q failed_at=%v", path, failedAt)
		}
	})

	t.Run("identity-verified success reports available without clearing", func(t *testing.T) {
		// The resolver genuinely names the requested candidate A. The check
		// reports listing-availability (true) but a metadata-only resolution
		// still never clears the transport-failure stamp.
		exec(`UPDATE media_files SET file_path=$1, failed_at=NOW() WHERE id=$2`, pinnedPath, fileID)
		_, router := newHandler(VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://provider.test/a.mp4", URI: pinnedPath, CandidateID: "A"}, nil
		}))
		resp := post(router)
		if len(resp.Results) != 1 || !resp.Results[0].Available {
			t.Fatalf("identity-verified resolution must report available: %#v", resp.Results)
		}
		path, failedAt := readRow()
		if path != pinnedPath || failedAt == nil {
			t.Fatalf("listing availability must not clear transport evidence: path=%q failed_at=%v", path, failedAt)
		}
	})
}
