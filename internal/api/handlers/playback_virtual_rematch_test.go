package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// rematchedIdentityResolver stands in for the detailed resolver after it has
// re-identified the requested release under a new provider result id.
func rematchedIdentityResolver(calls *int, resolved ResolvedVirtualMedia) VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		*calls++
		return resolved, nil
	})
}

// TestResolveVirtualInputAdoptsRematchedIdentity proves that when the resolver
// reports a same-release re-identification, the transport resolve returns the
// matched candidate's URL and adopts the new ?result= plus the resolution's
// identity through the CAS-fenced saver.
func TestResolveVirtualInputAdoptsRematchedIdentity(t *testing.T) {
	const pinned = "virtual://movie/tt-rematch?result=cand-old"
	const rematchedURI = "virtual://movie/tt-rematch?result=cand-new"
	snapshot := time.Now().Add(-time.Minute)
	row := &models.MediaFile{
		ID:                         91,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ProviderVideoHash:          "HASH1",
		UpdatedAt:                  snapshot,
	}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: rematchedIdentityResolver(&calls, ResolvedVirtualMedia{
			URL:               "https://93.184.216.34/stream/rematched",
			URI:               rematchedURI,
			CandidateID:       "cand-new",
			IdentityRematched: true,
			RequestHeaders:    map[string]string{"Referer": "https://provider.example/player"},
			ProviderVideoHash: "HASH1",
		}),
	}
	var saved []models.VirtualFilePersistArgs
	h.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saved = append(saved, args)
		return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true, IdentityAdopted: true}, nil
	}

	res, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if res.URL != "https://93.184.216.34/stream/rematched" {
		t.Fatalf("resolved URL = %q, want the matched candidate's URL, never a refusal", res.URL)
	}
	if res.CandidateID != "cand-new" {
		t.Fatalf("candidate id = %q, want the re-identified candidate", res.CandidateID)
	}
	if got := res.RequestHeaders["Referer"]; got != "https://provider.example/player" {
		t.Fatalf("request headers = %#v, want the matched candidate's headers", res.RequestHeaders)
	}
	if len(saved) != 1 {
		t.Fatalf("adoption writes = %d, want exactly 1", len(saved))
	}
	args := saved[0]
	if args.FileID != row.ID || args.ExpectedFilePath != pinned {
		t.Fatalf("adoption targeted file %d %q, want %d %q", args.FileID, args.ExpectedFilePath, row.ID, pinned)
	}
	if args.AdoptPath != rematchedURI {
		t.Fatalf("adoption path = %q, want the re-identified result id %q", args.AdoptPath, rematchedURI)
	}
	if !args.RequireAdopt {
		t.Fatal("adoption must require the new identity to persist under the fence")
	}
	if args.ProviderVideoHash != "HASH1" {
		t.Fatalf("adopted identity hash = %q, want the new identity", args.ProviderVideoHash)
	}
	if args.ProviderRequestHeaders["Referer"] != "https://provider.example/player" {
		t.Fatalf("adopted request headers = %#v, want them persisted with the identity", args.ProviderRequestHeaders)
	}
}

// TestResolveVirtualInputRefusalIsNotReportedAsRematch proves the handler never
// invents a re-identification: a resolver refusal stays a refusal and nothing is
// adopted.
func TestResolveVirtualInputRefusalIsNotReportedAsRematch(t *testing.T) {
	const pinned = "virtual://movie/tt-refuse?result=cand-old"
	row := &models.MediaFile{
		ID:                         92,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ProviderVideoHash:          "HASH1",
	}
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, errors.New("session-bound virtual candidate \"cand-old\" is no longer listed and candidate rotation was not requested")
		}),
	}
	saves := 0
	h.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saves++
		return VirtualFileMetadataUpdateResult{}, nil
	}

	_, _, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, "")
	if err == nil {
		t.Fatal("expected the dead-pin refusal to propagate")
	}
	if saves != 0 {
		t.Fatalf("adoption writes = %d, want 0 when the resolver refused", saves)
	}
}

// TestResolveVirtualInputLegacyRowDoesNotAdopt proves a row with no durable
// identity never triggers the re-match adoption, even if a resolver happened to
// return a different candidate without flagging a re-match.
func TestResolveVirtualInputLegacyRowDoesNotAdopt(t *testing.T) {
	const pinned = "virtual://movie/tt-legacy?result=cand-old"
	row := &models.MediaFile{ID: 93, FilePath: pinned}
	calls := 0
	h := &PlaybackHandler{
		VirtualFileLookup: storedURLLookup(row),
		VirtualMediaDetailedResolver: rematchedIdentityResolver(&calls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/legacy", URI: pinned, CandidateID: "cand-old",
		}),
	}
	saves := 0
	h.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saves++
		return VirtualFileMetadataUpdateResult{}, nil
	}

	if _, cleanup, err := h.resolveVirtualInputURI(context.Background(), pinned, 5, 1, "profile", false, nil, ""); err != nil {
		t.Fatalf("resolveVirtualInputURI error: %v", err)
	} else if cleanup != nil {
		cleanup()
	}
	if saves != 0 {
		t.Fatalf("adoption writes = %d, want 0 for a legacy row with no identity and no re-match", saves)
	}
}

// TestResolveVirtualInputStoredURLPassesStoredHeaders proves the Phase-2
// stored-URL shortcut hands the stored request headers to the caller, so a
// header-authenticated provider URL keeps working when served from the catalog.
func TestResolveVirtualInputStoredURLPassesStoredHeaders(t *testing.T) {
	const pinned = "virtual://movie/tt-headers?result=cand-a"
	expiresAt := time.Now().Add(2 * time.Hour)
	row := &models.MediaFile{
		ID:                         94,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       &expiresAt,
		ProviderRequestHeaders: map[string]string{
			"Referer":    "https://provider.example/player",
			"Origin":     "https://provider.example",
			"User-Agent": "silo-test",
		},
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
	if res.RequestHeaders["Referer"] != "https://provider.example/player" ||
		res.RequestHeaders["Origin"] != "https://provider.example" ||
		res.RequestHeaders["User-Agent"] != "silo-test" {
		t.Fatalf("resolved request headers = %#v, want the stored set verbatim", res.RequestHeaders)
	}
}

// TestStreamResolveAdoptsRematchedIdentity proves the serve-layer re-resolve
// performs the same adoption when the resolver reports a re-identification.
func TestStreamResolveAdoptsRematchedIdentity(t *testing.T) {
	const pinned = "virtual://movie/tt-serve-rematch?result=cand-old"
	const rematchedURI = "virtual://movie/tt-serve-rematch?result=cand-new"
	snapshot := time.Now().Add(-time.Minute)
	row := &models.MediaFile{
		ID:                         95,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		ProviderVideoHash:          "HASH1",
		UpdatedAt:                  snapshot,
	}
	calls := 0
	h := NewStreamHandler(nil, &storedURLStreamFileResolver{row: row})
	h.VirtualMediaDetailedResolver = rematchedIdentityResolver(&calls, ResolvedVirtualMedia{
		URL:               "https://93.184.216.34/stream/serve-rematched",
		URI:               rematchedURI,
		CandidateID:       "cand-new",
		IdentityRematched: true,
	})
	var saved []models.VirtualFilePersistArgs
	h.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		saved = append(saved, args)
		return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true, IdentityAdopted: true}, nil
	}

	res, cleanup, err := h.resolveVirtualInputURIExcluding(context.Background(), row, 1, "profile", false, nil, false)
	if err != nil {
		t.Fatalf("resolveVirtualInputURIExcluding error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if res.URL != "https://93.184.216.34/stream/serve-rematched" {
		t.Fatalf("resolved URL = %q, want the matched candidate's URL", res.URL)
	}
	if len(saved) != 1 || saved[0].AdoptPath != rematchedURI || !saved[0].RequireAdopt {
		t.Fatalf("serve-layer adoption = %#v, want the re-identified identity under the fence", saved)
	}
}
