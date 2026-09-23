package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

func refreshRow(id int, candidate string) *models.MediaFile {
	expires := time.Now().Add(30 * time.Minute)
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "movie-refresh",
		FilePath:                   "virtual://movie/tt-refresh?result=" + candidate,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ResolvedURL:                "https://93.184.216.34/stream/token=old-" + candidate,
		ResolvedURLExpiresAt:       &expires,
		ProviderVideoHash:          "hash-" + candidate,
	}
}

// TestVirtualCandidateRefresherWindowDisabledIsInert proves the pass never
// touches the database or the provider when the trust window is 0.
func TestVirtualCandidateRefresherWindowDisabledIsInert(t *testing.T) {
	listCalls := 0
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			listCalls++
			return nil, errors.New("list must not run with the window disabled")
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			t.Fatal("resolver must not run with the window disabled")
			return ResolvedVirtualMedia{}, nil
		}),
		Window: func() time.Duration { return 0 },
	}
	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("RunOnce = %d, want 0", got)
	}
	if listCalls != 0 {
		t.Fatalf("list calls = %d, want 0", listCalls)
	}
}

// TestVirtualCandidateRefresherRefreshesExpiringRowsViaCAS proves the pass
// resolves each selected row's exact candidate and records the fresh URL and
// expiry through the CAS-fenced same-candidate saver, with the trust/session
// context set so a dropped candidate is refused, not substituted.
func TestVirtualCandidateRefresherRefreshesExpiringRowsViaCAS(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(901, "cand-a"), refreshRow(902, "cand-b")}

	var gotWindow, gotLead time.Duration
	gotLimit := 0
	list := func(_ context.Context, window, lead time.Duration, limit int) ([]*models.MediaFile, error) {
		gotWindow, gotLead, gotLimit = window, lead, limit
		return rows, nil
	}
	newExpiry := time.Now().Add(6 * time.Hour)
	var resolvedPaths []string
	var sawTrust, sawSessionBound, sawForceRefresh []bool
	resolver := VirtualMediaDetailedResolverFunc(func(ctx context.Context, path string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolvedPaths = append(resolvedPaths, path)
		sawTrust = append(sawTrust, virtuallibrary.PersistedCandidateTrusted(ctx))
		sawSessionBound = append(sawSessionBound, VirtualSessionBinding(ctx))
		sawForceRefresh = append(sawForceRefresh, forceRefresh)
		id := virtualResultCandidateID(path)
		return ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=new-" + id, URI: path, CandidateID: id,
			ExpiresAt: newExpiry,
		}, nil
	})
	var saved []models.VirtualFilePersistArgs
	r := &VirtualCandidateRefresher{
		List:     list,
		Resolver: resolver,
		MetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			saved = append(saved, args)
			return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	refreshed := r.RunOnce(context.Background())
	if refreshed != 2 {
		t.Fatalf("refreshed = %d, want 2", refreshed)
	}
	if gotWindow != 720*time.Hour || gotLead != virtualCandidateRefreshLead || gotLimit != virtualCandidateRefreshBatch {
		t.Fatalf("list bounds = window=%v lead=%v limit=%d", gotWindow, gotLead, gotLimit)
	}
	if len(saved) != 2 {
		t.Fatalf("CAS writes = %d, want 2", len(saved))
	}
	for i, args := range saved {
		row := rows[i]
		if args.FileID != row.ID || args.ExpectedFilePath != row.FilePath {
			t.Fatalf("write %d targeted file %d %q, want %d %q", i, args.FileID, args.ExpectedFilePath, row.ID, row.FilePath)
		}
		if args.ResolvedURL != "https://93.184.216.34/stream/token=new-"+virtualResultCandidateID(row.FilePath) {
			t.Fatalf("write %d resolved_url = %q", i, args.ResolvedURL)
		}
		if args.ResolvedURLExpiresAt == nil || !args.ResolvedURLExpiresAt.Equal(newExpiry) {
			t.Fatalf("write %d expiry = %v, want %v", i, args.ResolvedURLExpiresAt, newExpiry)
		}
	}
	for i := range rows {
		if !sawTrust[i] || !sawSessionBound[i] || !sawForceRefresh[i] {
			t.Fatalf("resolve %d context: trust=%v sessionBound=%v forceRefresh=%v, want all true", i, sawTrust[i], sawSessionBound[i], sawForceRefresh[i])
		}
	}
}

// TestVirtualCandidateRefresherFailureIsRateLimited proves a failing provider
// is warn-and-continue and a failed row is backed off instead of retried every
// pass, while healthy rows still refresh.
func TestVirtualCandidateRefresherFailureIsRateLimited(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(911, "cand-a"), refreshRow(912, "cand-b")}
	attempts := map[int]int{}
	resolver := VirtualMediaDetailedResolverFunc(func(_ context.Context, path string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		id := virtualResultCandidateID(path)
		if id == "cand-a" {
			attempts[911]++
			return ResolvedVirtualMedia{}, errors.New("provider unavailable")
		}
		attempts[912]++
		return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=new", URI: path, CandidateID: id}, nil
	})
	writes := 0
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: resolver,
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			writes++
			return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	if got := r.RunOnce(context.Background()); got != 1 {
		t.Fatalf("first pass refreshed = %d, want 1 (the failing row does not count)", got)
	}
	if got := r.RunOnce(context.Background()); got != 1 {
		t.Fatalf("second pass refreshed = %d, want 1", got)
	}
	if attempts[911] != 1 {
		t.Fatalf("failing row attempts = %d, want 1 after the failure cooldown", attempts[911])
	}
	if attempts[912] != 2 {
		t.Fatalf("healthy row attempts = %d, want 2", attempts[912])
	}
	if writes != 2 {
		t.Fatalf("CAS writes = %d, want 2 (one per successful healthy pass)", writes)
	}
}

// TestVirtualCandidateRefresherBatchCap proves one pass is bounded even when
// the source returns more rows than the batch size.
func TestVirtualCandidateRefresherBatchCap(t *testing.T) {
	rows := []*models.MediaFile{
		refreshRow(921, "cand-1"), refreshRow(922, "cand-2"),
		refreshRow(923, "cand-3"), refreshRow(924, "cand-4"),
	}
	resolved := 0
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, path string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolved++
			id := virtualResultCandidateID(path)
			return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=new", URI: path, CandidateID: id}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
		batch:  2,
	}
	if got := r.RunOnce(context.Background()); got != 2 {
		t.Fatalf("refreshed = %d, want the batch cap 2", got)
	}
	if resolved != 2 {
		t.Fatalf("resolver calls = %d, want 2", resolved)
	}
}

// TestVirtualCandidateRefresherSkipsRenumberedCandidate proves a provider
// identity rematch is not written by the background pass: adoption is the
// on-demand path's job, so the row is left untouched.
func TestVirtualCandidateRefresherSkipsRenumberedCandidate(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(931, "cand-a")}
	writes := 0
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=renumbered", URI: "virtual://movie/tt-refresh?result=cand-a-new",
				CandidateID: "cand-a-new", IdentityRematched: true,
			}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			writes++
			return VirtualFileMetadataUpdateResult{}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}
	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("refreshed = %d, want 0 (a renumbered candidate is not written)", got)
	}
	if writes != 0 {
		t.Fatalf("CAS writes = %d, want 0 for a renumbered candidate", writes)
	}
}

// TestVirtualCandidateRefresherSaverErrorCoolsDown proves a persistence failure
// is reported as a refresh error (not a silent success): the row trips the
// failure cooldown and the pass count stays 0.
func TestVirtualCandidateRefresherSaverErrorCoolsDown(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(941, "cand-a")}
	saverErr := errors.New("cassandra is having a day")
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, path string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			id := virtualResultCandidateID(path)
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=new-" + id, URI: path, CandidateID: id,
				ExpiresAt: time.Now().Add(6 * time.Hour),
			}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{}, saverErr
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("refreshed = %d, want 0 when persistence fails", got)
	}
	if !r.rateLimited(941, time.Now()) {
		t.Fatal("failed row is not cooling down after a saver error")
	}
}

// TestVirtualCandidateRefresherSiblingSkipIsNotAFailure proves a resolution
// that does not belong to the row (sibling) is skipped without tripping the
// failure cooldown: nothing was attempted, so the row must not be backed off.
func TestVirtualCandidateRefresherSiblingSkipIsNotAFailure(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(951, "cand-a")}
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=sibling", URI: "virtual://movie/tt-refresh?result=cand-b", CandidateID: "cand-b",
			}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			t.Fatal("saver must not run for a sibling resolution")
			return VirtualFileMetadataUpdateResult{}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("refreshed = %d, want 0 for a skipped sibling", got)
	}
	if r.rateLimited(951, time.Now()) {
		t.Fatal("skipped row is cooling down, want no backoff when nothing was attempted")
	}
}

// TestVirtualCandidateRefresherLostCASRaceIsNeither proves a write the CAS
// fence rejected (RowsAffected 0 — a concurrent serve-path refresh, rematch
// adoption, or re-list won) is neither counted nor cooled down: the bytes are
// already fresh, so there is nothing to retry.
func TestVirtualCandidateRefresherLostCASRaceIsNeither(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(961, "cand-a")}
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, path string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			id := virtualResultCandidateID(path)
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=new-" + id, URI: path, CandidateID: id,
				ExpiresAt: time.Now().Add(6 * time.Hour),
			}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{RowsAffected: 0}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("refreshed = %d, want 0 for a lost CAS race", got)
	}
	if r.rateLimited(961, time.Now()) {
		t.Fatal("row is cooling down after a lost CAS race, want no backoff when the bytes are already fresh")
	}
}

// TestVirtualCandidateRefresherEmptyURLSkipsWithoutCooldown proves an empty
// provider URL is a skip, not a row failure: the next pass retries it without
// waiting out the cooldown.
func TestVirtualCandidateRefresherEmptyURLSkipsWithoutCooldown(t *testing.T) {
	rows := []*models.MediaFile{refreshRow(971, "cand-a")}
	r := &VirtualCandidateRefresher{
		List: func(context.Context, time.Duration, time.Duration, int) ([]*models.MediaFile, error) {
			return rows, nil
		},
		Resolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, path string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URI: path, CandidateID: virtualResultCandidateID(path)}, nil
		}),
		MetadataSaver: func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			t.Fatal("saver must not run for an empty URL")
			return VirtualFileMetadataUpdateResult{}, nil
		},
		Window: func() time.Duration { return 720 * time.Hour },
	}

	if got := r.RunOnce(context.Background()); got != 0 {
		t.Fatalf("refreshed = %d, want 0 for an empty URL", got)
	}
	if r.rateLimited(971, time.Now()) {
		t.Fatal("row is cooling down after an empty URL, want no backoff for a provider hiccup")
	}
}
