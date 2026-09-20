package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestResolveVirtualResumeFromDatabaseRow is the end-to-end restart
// equivalence: a fresh handler with cold caches reads the same migrated
// database row a previous process wrote (Phase 1's persisted URL), takes the
// deferred fast path with zero listing, and the transport then serves the
// row's own stored URL without a provider round-trip.
func TestResolveVirtualResumeFromDatabaseRow(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994431, 7101
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-resume-db?result=cand-db"
	expiresAt := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	candidateID, _, _ := seedResolvedCandidateRow(t, pool, folderID, ownerID, "movie-resume-db", candidatePath, models.MediaFile{
		ResolvedURL:          "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt: &expiresAt,
	})
	// seedResolvedCandidateRow leaves file_size NULL; the candidate rows the
	// scanner writes always carry it, so give the seeded row the shape the
	// catalog reader expects.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET file_size = 0 WHERE id = $1`, candidateID); err != nil {
		t.Fatalf("normalize seeded row size: %v", err)
	}

	repo := scanner.NewFileRepository(pool)
	row, err := repo.GetByPath(ctx, candidatePath)
	if err != nil || row == nil {
		t.Fatalf("load seeded candidate row: %v", err)
	}
	if row.ResolvedURL != "https://93.184.216.34/stream/token=stored" {
		t.Fatalf("seeded row lost its persisted URL: %q", row.ResolvedURL)
	}

	listerCalls, detailedCalls := 0, 0
	h := virtualResumeHandler(&listerCalls, &detailedCalls, nil)
	h.VirtualFileLookup = func(ctx context.Context, path string) (*models.MediaFile, error) {
		loaded, lookupErr := repo.GetByPath(ctx, path)
		if lookupErr != nil {
			return nil, ErrVirtualCandidateNotFound
		}
		return loaded, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, row, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls != 0 || detailedCalls != 0 {
		t.Fatalf("resume paid a provider call: lister=%d detailed=%d, want 0/0", listerCalls, detailedCalls)
	}
	if resolved.URI != candidatePath {
		t.Fatalf("resolved URI = %q, want persisted %q", resolved.URI, candidatePath)
	}

	// The serve-layer stored-URL shortcut then hands the persisted URL to the
	// transport, still without listing or resolving the provider.
	served, cleanup, err := h.resolveVirtualInputURI(ctx, resolved.URI, ownerID, 1, "profile-1", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if served.URL != row.ResolvedURL {
		t.Fatalf("served URL = %q, want the persisted %q", served.URL, row.ResolvedURL)
	}
	if listerCalls != 0 || detailedCalls != 0 {
		t.Fatalf("transport stored-URL serve paid a provider call: lister=%d detailed=%d", listerCalls, detailedCalls)
	}
}
