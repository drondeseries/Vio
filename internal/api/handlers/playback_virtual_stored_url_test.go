package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// storedURLLookup returns the fixed row for the exact candidate path and a
// not-found error otherwise, mirroring the production exact-path lookup.
func storedURLLookup(row *models.MediaFile) VirtualFileLookup {
	return func(_ context.Context, path string) (*models.MediaFile, error) {
		if row == nil || row.FilePath != path {
			return nil, ErrVirtualCandidateNotFound
		}
		return row, nil
	}
}

// countingDetailedResolver counts provider listings and returns a fixed
// resolution, standing in for the provider round-trip the shortcut must skip.
func countingDetailedResolver(calls *int, resolved ResolvedVirtualMedia) VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		*calls++
		return resolved, nil
	})
}

func TestResolveVirtualInputUsesStoredURLWithoutListing(t *testing.T) {
	const pinned = "virtual://movie/tt-stored?result=cand-a"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                         71,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 0 {
		t.Fatalf("provider listing calls = %d, want 0 for a usable stored URL", calls)
	}
	if res.URL != row.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the stored URL %q", res.URL, row.ResolvedURL)
	}
	if res.CandidateID != "cand-a" {
		t.Fatalf("candidate id = %q, want cand-a", res.CandidateID)
	}
}

func TestResolveVirtualInputForceRefreshBypassesStoredURL(t *testing.T) {
	const pinned = "virtual://movie/tt-force?result=cand-a"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                   72,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", true, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an explicit forceRefresh", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
}

func TestResolveVirtualInputExpiredStoredURLFallsBackAndRefreshes(t *testing.T) {
	const pinned = "virtual://movie/tt-expired?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	newExpiry := time.Now().Add(3 * time.Hour)
	row := &models.MediaFile{
		ID:                   73,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=new", URI: pinned, CandidateID: "cand-a",
			ExpiresAt: newExpiry,
		}),
	}
	var saved []models.VirtualFilePersistArgs
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		saved = append(saved, args)
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an expired stored URL", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if len(saved) != 1 {
		t.Fatalf("stored-URL refresh writes = %d, want 1", len(saved))
	}
	if saved[0].ResolvedURL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("refreshed resolved_url = %q, want the fresh URL", saved[0].ResolvedURL)
	}
	if saved[0].ResolvedURLExpiresAt == nil || !saved[0].ResolvedURLExpiresAt.Equal(newExpiry) {
		t.Fatalf("refreshed expiry = %v, want %v", saved[0].ResolvedURLExpiresAt, newExpiry)
	}
	if saved[0].FileID != row.ID || saved[0].ExpectedFilePath != pinned {
		t.Fatalf("refresh targeted file %d %q, want %d %q", saved[0].FileID, saved[0].ExpectedFilePath, row.ID, pinned)
	}
}

func TestResolveVirtualInputStoredURLFailingValidationFallsBack(t *testing.T) {
	const pinned = "virtual://movie/tt-invalid?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	row := &models.MediaFile{
		ID:                   74,
		FilePath:             pinned,
		ResolvedURL:          "http://127.0.0.1/private/stream",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 after the stored URL failed validation", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL, never the unvalidated one", res.URL)
	}
}

func TestResolveVirtualInputNoStoredURLBehavesAsBefore(t *testing.T) {
	const pinned = "virtual://movie/tt-none?result=cand-a"
	row := &models.MediaFile{
		ID:       75,
		FilePath: pinned,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}
	saves := 0
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		saves++
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 when no URL is stored", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if saves != 0 {
		t.Fatalf("refresh writes = %d, want 0 for a row that never stored a URL", saves)
	}
}

func TestResolveVirtualInputStoredURLFromDifferentRowNotUsed(t *testing.T) {
	const pinned = "virtual://movie/tt-other?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	// The lookup returns a row for a different concrete release, as could
	// happen if a caller resolved the path to the wrong owner row.
	otherRow := &models.MediaFile{
		ID:                   76,
		FilePath:             "virtual://movie/tt-other?result=cand-b",
		ResolvedURL:          "https://93.184.216.34/stream/token=other",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(otherRow),
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1; another row's stored URL must not be served", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL, never another row's URL", res.URL)
	}
}

func TestResolveVirtualInputRefreshSkipsSubstitutedCandidate(t *testing.T) {
	const pinned = "virtual://movie/tt-sub?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	row := &models.MediaFile{
		ID:                   77,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		// The resolver substituted a different release; its URL must not be
		// deposited onto cand-a's row.
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=sibling", URI: "virtual://movie/tt-sub?result=cand-b", CandidateID: "cand-b",
		}),
	}
	saves := 0
	h.VirtualFileSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
		saves++
		return 1, nil
	}

	_, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1", calls)
	}
	if saves != 0 {
		t.Fatalf("refresh writes = %d, want 0 for a substituted sibling", saves)
	}
}

// storedURLStreamFileResolver is a serve-layer file resolver that supports the
// exact-path lookup the stored-URL shortcut uses.
type storedURLStreamFileResolver struct {
	row *models.MediaFile
}

func (r *storedURLStreamFileResolver) GetByID(_ context.Context, _ int) (*models.MediaFile, error) {
	if r.row == nil {
		return nil, ErrVirtualCandidateNotFound
	}
	return r.row, nil
}

func (r *storedURLStreamFileResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	if r.row == nil || r.row.FilePath != path {
		return nil, ErrVirtualCandidateNotFound
	}
	return r.row, nil
}

func TestStreamResolveVirtualInputUsesStoredURLWithoutListing(t *testing.T) {
	const pinned = "virtual://movie/tt-serve?result=cand-a"
	expiresAt := time.Now().Add(time.Hour)
	row := &models.MediaFile{
		ID:                   81,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expiresAt,
	}
	calls := 0
	h := NewStreamHandler(nil, &storedURLStreamFileResolver{row: row})
	h.VirtualMediaDetailedResolver = countingDetailedResolver(&calls, ResolvedVirtualMedia{
		URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
	})

	res, cleanup, err := h.resolveVirtualInputURIExcluding(context.Background(), row, 1, "profile", false, nil, false)
	if err != nil {
		t.Fatalf("resolveVirtualInputURIExcluding error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 0 {
		t.Fatalf("provider listing calls = %d, want 0 for a usable stored URL", calls)
	}
	if res.URL != row.ResolvedURL {
		t.Fatalf("resolved URL = %q, want the stored URL %q", res.URL, row.ResolvedURL)
	}
	if res.CandidateID != "cand-a" {
		t.Fatalf("candidate id = %q, want cand-a", res.CandidateID)
	}
}

func TestStreamResolveVirtualInputExpiredStoredURLRefreshes(t *testing.T) {
	const pinned = "virtual://movie/tt-serve-expired?result=cand-a"
	expiredAt := time.Now().Add(-time.Hour)
	newExpiry := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                   82,
		FilePath:             pinned,
		ResolvedURL:          "https://93.184.216.34/stream/token=old",
		ResolvedURLExpiresAt: &expiredAt,
	}
	calls := 0
	h := NewStreamHandler(nil, &storedURLStreamFileResolver{row: row})
	h.VirtualMediaDetailedResolver = countingDetailedResolver(&calls, ResolvedVirtualMedia{
		URL: "https://93.184.216.34/stream/token=new", URI: pinned, CandidateID: "cand-a",
		ExpiresAt: newExpiry,
	})
	saved := 0
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		saved++
		if args.ResolvedURL != "https://93.184.216.34/stream/token=new" {
			t.Errorf("refreshed resolved_url = %q, want the fresh URL", args.ResolvedURL)
		}
		return 1, nil
	}

	res, cleanup, err := h.resolveVirtualInputURIExcluding(context.Background(), row, 1, "profile", false, nil, false)
	if err != nil {
		t.Fatalf("resolveVirtualInputURIExcluding error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 for an expired stored URL", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=new" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
	if saved != 1 {
		t.Fatalf("refresh writes = %d, want 1", saved)
	}
}

// TestResolveVirtualInputStoredURLLookupErrorFallsBack proves a lookup failure
// is treated as a cache miss: the resolve still lists the provider.
func TestResolveVirtualInputStoredURLLookupErrorFallsBack(t *testing.T) {
	const pinned = "virtual://movie/tt-lookup-error?result=cand-a"
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return nil, errors.New("lookup unavailable")
		},
		VirtualMediaDetailedResolver: countingDetailedResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: pinned, CandidateID: "cand-a",
		}),
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("provider listing calls = %d, want 1 after a lookup failure", calls)
	}
	if res.URL != "https://93.184.216.34/stream/token=fresh" {
		t.Fatalf("resolved URL = %q, want the freshly listed URL", res.URL)
	}
}
