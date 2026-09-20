package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- Blocker 1: one fail-closed verdict policy everywhere. ---

// TestVerdictExactLookupErrorSurvivesNeutralSuccess pins that a failed
// exact-path verdict lookup is preserved even when the provider-neutral
// fallback returns a healthy row. The exact identity is the one whose verdict
// matters; a healthy sibling must not mask the failure to read it.
func TestVerdictExactLookupErrorSurvivesNeutralSuccess(t *testing.T) {
	const uri = "virtual://movie/tt-exact?result=anchor"
	exactErr := errors.New("exact lookup unavailable")
	failedAt := time.Now()
	h := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, exactErr
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return &models.MediaFile{ID: 12, FilePath: uri, FailedAt: &failedAt}, nil
		},
	}
	file := &models.MediaFile{ID: 1, ContentID: "movie-tt-exact", FilePath: uri}

	err := h.virtualCandidateVerdictError(context.Background(), uri, file, 5, false)
	if err == nil {
		t.Fatal("an exact lookup failure was masked by a healthy neutral row")
	}
	if !errors.Is(err, exactErr) {
		t.Fatalf("verdict error = %v, want it to wrap the exact lookup failure", err)
	}
	if !errors.Is(err, errVirtualCandidateVerdictUnknown) {
		t.Fatalf("verdict error = %v, want it to carry the unknown-verdict sentinel", err)
	}
	if retryErr := h.virtualCandidateVerdictError(context.Background(), uri, file, 5, true); retryErr != nil {
		t.Fatalf("explicit retry did not bypass the unknown verdict: %v", retryErr)
	}

	// Metadata callers still receive the fallback row, with the error alongside.
	row, found, lookupErr := h.lookupVirtualCandidateRowDetailed(context.Background(), uri, file.ContentID, file.EpisodeID, 5)
	if !found || row == nil || row.ID != 12 {
		t.Fatalf("fallback row = %#v found=%v, want row id 12", row, found)
	}
	if !errors.Is(lookupErr, exactErr) {
		t.Fatalf("detailed lookup error = %v, want the exact lookup failure", lookupErr)
	}
}

// TestVerdictExactMissWithNeutralHitStaysEligible pins the genuine
// exact-miss-with-neutral-hit path: a virtual row stored under the
// provider-neutral key but requested with a concrete ?result= URI misses the
// exact lookup by design. That not-found must not taint the healthy neutral
// fallback row, or the candidate can never resolve. The filter recognizes both
// the native mapping (ErrVirtualCandidateNotFound) and the underlying repo
// sentinel (scanner.ErrFileNotFound). A live failed verdict on the neutral row
// still blocks, so the not-found exception does not weaken enforcement.
func TestVerdictExactMissWithNeutralHitStaysEligible(t *testing.T) {
	const uri = "virtual://movie/tt-neutral-hit?result=anchor"
	file := &models.MediaFile{ID: 3, ContentID: "movie-tt-neutral-hit", FilePath: uri}

	notFoundErrs := map[string]error{
		"ErrVirtualCandidateNotFound": ErrVirtualCandidateNotFound,
		"scanner.ErrFileNotFound":     scanner.ErrFileNotFound,
	}
	for name, notFound := range notFoundErrs {
		t.Run(name, func(t *testing.T) {
			h := &PlaybackHandler{
				VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
					return nil, notFound
				},
				VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
					return &models.MediaFile{ID: 42, FilePath: uri}, nil
				},
			}
			row, found, lookupErr := h.lookupVirtualCandidateRowDetailed(context.Background(), uri, file.ContentID, file.EpisodeID, 5)
			if lookupErr != nil {
				t.Fatalf("genuine exact miss tainted the neutral hit: %v", lookupErr)
			}
			if !found || row == nil || row.ID != 42 {
				t.Fatalf("row = %#v found=%v, want the neutral fallback row id 42", row, found)
			}
			if err := h.virtualCandidateVerdictError(context.Background(), uri, file, 5, false); err != nil {
				t.Fatalf("genuine exact miss left the candidate ineligible: %v", err)
			}

			// A live verdict on the neutral fallback row must still block even
			// though the exact lookup missed.
			failedAt := time.Now()
			failed := &PlaybackHandler{
				VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
					return nil, notFound
				},
				VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
					return &models.MediaFile{ID: 42, FilePath: uri, FailedAt: &failedAt}, nil
				},
			}
			if err := failed.virtualCandidateVerdictError(context.Background(), uri, file, 5, false); err == nil {
				t.Fatal("a live verdict on the neutral fallback row was ignored after a genuine exact miss")
			}
		})
	}
}

// TestResolveVirtualPlaybackSourceVerdictLookupOutageFailsClosed pins the main
// candidate loop's use of the detailed lookup: when the candidate's verdict
// cannot be read after the provider resolve, the candidate is neither served
// nor adopted, and the unknown-verdict sentinel stops the loop.
func TestResolveVirtualPlaybackSourceVerdictLookupOutageFailsClosed(t *testing.T) {
	lookupErr := errors.New("catalog unavailable")
	resolverCalls, saveCalls := 0, 0
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/x.mp4", nil
		}),
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, lookupErr
		},
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			saveCalls++
			return 1, nil
		},
	}
	file := &models.MediaFile{
		ID: 80, ContentID: "movie-tt-loop-outage",
		FilePath:                   "virtual://movie/tt-loop-outage?result=anchor",
		VirtualOwnerInstallationID: 5,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playback/start", nil)

	_, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err == nil {
		t.Fatal("a verdict lookup outage was treated as an eligible candidate")
	}
	if !errors.Is(err, errVirtualCandidateVerdictUnknown) {
		t.Fatalf("error = %v, want the unknown-verdict sentinel", err)
	}
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want it to wrap the lookup failure", err)
	}
	if resolverCalls == 0 {
		t.Fatal("test did not reach the resolve path")
	}
	if saveCalls != 0 {
		t.Fatalf("saver calls = %d, want no persistence while the verdict is unknown", saveCalls)
	}
}

// TestFallbackSiblingSelectionStopsOnVerdictOutage pins that the sibling
// selection loop fails the whole fallback when a candidate's verdict is
// unknown, instead of treating the outage as "this candidate is dead" and
// substituting a later sibling the verdict system never cleared.
func TestFallbackSiblingSelectionStopsOnVerdictOutage(t *testing.T) {
	const (
		anchorURI = "virtual://movie/tt-sib-outage?result=anchor"
		firstURI  = "virtual://movie/tt-sib-outage?result=first"
		secondURI = "virtual://movie/tt-sib-outage?result=second"
	)
	lookupErr := errors.New("catalog unavailable")
	resolverCalls, saveCalls := 0, 0
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{ID: "first", URI: firstURI, Resolution: "1080p"},
				{ID: "second", URI: secondURI, Resolution: "1080p"},
			}, nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			if path == firstURI {
				return nil, lookupErr
			}
			return &models.MediaFile{ID: 60, FilePath: path}, nil
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			resolverCalls++
			return "http://localhost:8080/stream.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			saveCalls++
			return 1, nil
		},
	}
	file := &models.MediaFile{ID: 61, ContentID: "movie-tt-sib-outage", FilePath: anchorURI, VirtualOwnerInstallationID: 5}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: false})
	if got != nil {
		t.Fatalf("fallback returned %#v during a verdict lookup outage, want nil", got)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0: the loop continued past an unknown verdict", resolverCalls)
	}
	if saveCalls != 0 {
		t.Fatalf("saver calls = %d, want 0", saveCalls)
	}
}

// --- Blocker 2: verdict and adoption are fenced together. ---

// TestFallbackVerdictCommittedInAdoptionWindowIsFenced is the deterministic
// barrier test: a failed_at verdict is committed after the fallback's final
// verdict read and before the adoption write. The persistence-backed fence must
// reject the adoption, and the final state must show the row did not adopt the
// substitute.
func TestFallbackVerdictCommittedInAdoptionWindowIsFenced(t *testing.T) {
	const (
		anchorURI  = "virtual://movie/tt-window?result=anchor"
		healthyURI = "virtual://movie/tt-window?result=healthy"
	)
	anchorUpdated := time.Now().Add(-2 * time.Hour).UTC()
	dbPath := anchorURI
	failedIdentity := ""
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "healthy", URI: healthyURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "http://localhost:8080/healthy.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		// The fake emulates the real saver's fence: the verdict is evaluated in
		// the same "statement" as the write, so a verdict committed by the
		// barrier is seen.
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			if failedIdentity != "" && args.AdoptPath != "" &&
				virtualPlaybackNeutralKey(failedIdentity) == virtualPlaybackNeutralKey(args.AdoptPath) {
				if args.RequireAdopt {
					return 0, errVirtualAdoptIdentityNotPersisted
				}
			}
			if args.RequireAdopt && args.AdoptPath != "" {
				dbPath = args.AdoptPath
			}
			return 1, nil
		},
	}
	file := &models.MediaFile{
		ID: 90, ContentID: "movie-tt-window", FilePath: anchorURI,
		VirtualOwnerInstallationID: 5, MediaFolderID: 9, UpdatedAt: anchorUpdated,
	}

	previousBarrier := virtualAdoptionBarrier
	virtualAdoptionBarrier = func() { failedIdentity = healthyURI }
	t.Cleanup(func() { virtualAdoptionBarrier = previousBarrier })

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: false})
	if got != nil {
		t.Fatalf("fallback returned %#v, want nil: a verdict committed before adoption must block it", got)
	}
	if dbPath != anchorURI {
		t.Fatalf("final database file_path = %q, want the un-adopted %q", dbPath, anchorURI)
	}
}

// fakeVerdictRow is a pgx.Row that always fails Scan with err.
type fakeVerdictRow struct{ err error }

func (r fakeVerdictRow) Scan(...any) error { return r.err }

// fakeStringRow is a pgx.Row that scans a single string destination.
type fakeStringRow struct{ value string }

func (r fakeStringRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("fakeStringRow: unexpected destination count")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("fakeStringRow: destination is not *string")
	}
	*p = r.value
	return nil
}

// fakeMetadataDB records the adoptPath and verdict-fence flag of every
// QueryRow call and returns the queued rows in order.
type fakeMetadataDB struct {
	adoptPaths []string
	fences     []bool
	rows       []pgx.Row
}

func (f *fakeMetadataDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	adopt := ""
	if len(args) >= 18 {
		if s, ok := args[17].(string); ok {
			adopt = s
		}
	}
	fence := false
	if len(args) >= 21 {
		if b, ok := args[20].(bool); ok {
			fence = b
		}
	}
	f.adoptPaths = append(f.adoptPaths, adopt)
	f.fences = append(f.fences, fence)
	row := f.rows[0]
	f.rows = f.rows[1:]
	return row
}

// TestExecVirtualFileMetadataUpdateExplicitRetryDisablesVerdictFence pins that
// an explicit retry does not carry the verdict fence: the caller deliberately
// re-adopts a known-bad candidate.
func TestExecVirtualFileMetadataUpdateExplicitRetryDisablesVerdictFence(t *testing.T) {
	const candidatePath = "virtual://movie/tt-retry-ok?result=cand"
	db := &fakeMetadataDB{rows: []pgx.Row{fakeStringRow{value: candidatePath}}}
	args := models.VirtualFilePersistArgs{
		FileID: 8, ExpectedFilePath: candidatePath, AdoptPath: candidatePath,
		RequireAdopt: true, AllowFailedVerdict: true, StampProbe: true,
		UpdatedAt: time.Now(), OwnerID: 5, LibraryID: 9,
	}
	rows, err := ExecVirtualFileMetadataUpdate(context.Background(), db, args)
	if err != nil {
		t.Fatalf("explicit retry adoption error: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1 for a confirmed explicit-retry adoption", rows)
	}
	if len(db.fences) != 1 || db.fences[0] {
		t.Fatalf("verdict fence = %v, want disabled for an explicit retry", db.fences)
	}
}

// TestExecVirtualFileMetadataUpdateUniquenessRetryDoesNotReportAdoption pins
// that a uniqueness collision under RequireAdopt is refused without a
// metadata-only retry: the retry would stamp the substitute's tracks and probe
// evidence on a row that does not own them.
func TestExecVirtualFileMetadataUpdateUniquenessRetryDoesNotReportAdoption(t *testing.T) {
	const candidatePath = "virtual://movie/tt-retry?result=cand"
	db := &fakeMetadataDB{rows: []pgx.Row{
		fakeVerdictRow{err: &pgconn.PgError{Code: "23505"}},
		fakeStringRow{value: candidatePath},
	}}
	args := models.VirtualFilePersistArgs{
		FileID: 7, ExpectedFilePath: candidatePath, AdoptPath: candidatePath,
		RequireAdopt: true, StampProbe: true, UpdatedAt: time.Now(), OwnerID: 5, LibraryID: 9,
	}
	rows, err := ExecVirtualFileMetadataUpdate(context.Background(), db, args)
	if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0: a collision is not an adoption", rows)
	}
	if len(db.adoptPaths) != 1 || db.adoptPaths[0] != candidatePath {
		t.Fatalf("adopt paths = %v, want a single adoption attempt and no metadata-only retry", db.adoptPaths)
	}
}

// --- Blocker 3: metadata persistence is separate from identity adoption. ---

// TestFallbackUniquenessConflictDoesNotReportAdoption pins that when a
// uniqueness conflict prevents the validated identity from being adopted, the
// fallback does not report the substitute as adopted.
func TestFallbackUniquenessConflictDoesNotReportAdoption(t *testing.T) {
	const (
		anchorURI  = "virtual://movie/tt-conflict?result=anchor"
		healthyURI = "virtual://movie/tt-conflict?result=healthy"
	)
	anchorUpdated := time.Now().Add(-2 * time.Hour).UTC()
	dbPath := anchorURI
	h := &PlaybackHandler{
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "healthy", URI: healthyURI, Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "http://localhost:8080/healthy.mp4", nil
		}),
		VirtualPlaybackSourceProber: releaseIdentityProber,
		// A sibling owns the resolved path, so the metadata update lands but
		// the identity is not adopted. The saver reports that distinctly.
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			if args.RequireAdopt && args.AdoptPath != "" {
				return 0, errVirtualAdoptIdentityNotPersisted
			}
			dbPath = args.AdoptPath
			return 1, nil
		},
	}
	file := &models.MediaFile{
		ID: 91, ContentID: "movie-tt-conflict", FilePath: anchorURI,
		VirtualOwnerInstallationID: 5, MediaFolderID: 9, UpdatedAt: anchorUpdated,
	}

	got := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: false})
	if got != nil {
		t.Fatalf("fallback returned %#v despite a metadata-only write, want nil", got)
	}
	if dbPath != anchorURI {
		t.Fatalf("final database file_path = %q, want the un-adopted %q", dbPath, anchorURI)
	}
}

// TestVirtualFileMetadataUpdateVerdictFenceBlocksAdoption is the DB-gated proof
// that the verdict fence is evaluated in the same statement as the adoption: a
// failed_at committed before the write blocks adoption, so the whole write is
// refused and neither the probe metadata nor the stamp lands.
func TestVirtualFileMetadataUpdateVerdictFenceBlocksAdoption(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994320, 7010
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-verdict?result=cand-a"
	neutralPath := virtualPlaybackNeutralKey(adoptPath)
	anchorPath := "virtual://movie/tt-db-verdict?result=anchor"

	var targetID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-verdict', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, anchorPath, ownerID,
	).Scan(&targetID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert target row: %v", err)
	}
	// The failed verdict is stored at the provider-neutral path, so the
	// sibling guard does not block adoption; only the verdict fence can.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, failed_at)
		VALUES('movie-db-verdict', $1, $2, 'virtual', $3, 'virtual', NOW())`, folderID, neutralPath, ownerID); err != nil {
		t.Fatalf("insert failed verdict row: %v", err)
	}

	args := models.VirtualFilePersistArgs{
		FileID: targetID, ExpectedFilePath: anchorPath,
		VideoTracks: []byte(`[]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: adoptPath, RequireAdopt: true,
	}
	rows, err := ExecVirtualFileMetadataUpdate(ctx, pool, args)
	if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0", rows)
	}

	var path string
	var resolution, stampedAt *string
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at::text FROM media_files WHERE id = $1`, targetID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read target row: %v", err)
	}
	if path != anchorPath {
		t.Fatalf("final database file_path = %q, want the un-adopted %q", path, anchorPath)
	}
	if resolution != nil {
		t.Fatalf("resolution = %q, want no probe metadata on a refused adoption", *resolution)
	}
	if stampedAt != nil {
		t.Fatalf("probe_updated_at = %q, want no probe stamp on a refused adoption", *stampedAt)
	}
}

// TestVirtualFileMetadataUpdateUniquenessConflictDoesNotReportAdoption is the
// DB-gated proof that a resolved identity colliding with an existing path owner
// is not reported as an adoption: the whole write is refused, file_path keeps
// the row's own identity, and neither the substitute's tracks nor its probe
// stamp land.
func TestVirtualFileMetadataUpdateUniquenessConflictDoesNotReportAdoption(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994321, 7011
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-conflict"
	candidatePath := adoptPath + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-conflict', $1, $2, 'virtual', $3, 'virtual')`, folderID, adoptPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-conflict', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	args := models.VirtualFilePersistArgs{
		FileID: candidateID, ExpectedFilePath: candidatePath,
		VideoTracks: []byte(`[]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: adoptPath, RequireAdopt: true,
	}
	rows, err := ExecVirtualFileMetadataUpdate(ctx, pool, args)
	if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0", rows)
	}

	var path string
	var resolution, stampedAt *string
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at::text FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	// The persisted identity is the row's own path, so the returned identity
	// (nil source) never claims the un-adopted adoptPath.
	if path != candidatePath {
		t.Fatalf("final database file_path = %q, want the row's own identity %q", path, candidatePath)
	}
	if resolution != nil {
		t.Fatalf("resolution = %q, want no substitute metadata on a refused adoption", *resolution)
	}
	if stampedAt != nil {
		t.Fatalf("probe_updated_at = %q, want no substitute probe stamp on a refused adoption", *stampedAt)
	}
}
