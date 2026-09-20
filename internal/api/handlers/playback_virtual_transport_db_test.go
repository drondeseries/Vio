package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// transportRow is the persisted transport surface read back for a row.
type transportRow struct {
	Path        string
	VideoHash   *string
	GUID        *string
	ReleaseName *string
	ReleaseSize *int64
	ResolvedURL *string
	Headers     *string
}

func readTransportRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) transportRow {
	t.Helper()
	var row transportRow
	if err := pool.QueryRow(ctx, `
		SELECT file_path, provider_video_hash, provider_guid, provider_release_name,
		       provider_release_size, resolved_url, provider_request_headers::text
		FROM media_files WHERE id = $1`, id).Scan(
		&row.Path, &row.VideoHash, &row.GUID, &row.ReleaseName,
		&row.ReleaseSize, &row.ResolvedURL, &row.Headers,
	); err != nil {
		t.Fatalf("read transport row %d: %v", id, err)
	}
	return row
}

// TestVirtualFileMetadataUpdateAdoptingDifferentReleaseClearsPriorTransport is
// the release-replacement regression: adopting release B (which supplies only a
// GUID) over release A (which carried a hash, a release name and size, a stored
// URL and request headers) must replace the whole transport set. No field that
// described A may survive attached to B's path, and an omitted tier must clear
// rather than COALESCE the previous release's value.
func TestVirtualFileMetadataUpdateAdoptingDifferentReleaseClearsPriorTransport(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994360, 7050
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	releaseAPath := "virtual://movie/tt-transport-a?result=a"
	releaseBPath := "virtual://movie/tt-transport-b?result=b"

	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			video_tracks, resolution, provider_video_hash, provider_guid, provider_release_name, provider_release_size,
			resolved_url, resolved_url_expires_at, provider_request_headers)
		VALUES('movie-transport',
			$1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p',
			'release-a-hash', 'release-a-guid', 'Release.A.2024', 111111,
			'http://release-a.example/stream', NOW() + INTERVAL '1 hour',
			'{"Referer":"http://release-a.example/"}'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, releaseAPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert release A row: %v", err)
	}

	// Release B supplies only a GUID; every other transport field is omitted.
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: releaseAPath,
		VideoTracks: []byte(`[{"codec":"av1"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: releaseBPath, RequireAdopt: true,
		ProviderGUID: "release-b-guid",
	})
	if err != nil {
		t.Fatalf("adopting release B failed: %v", err)
	}
	if !result.IdentityAdopted || !result.MetadataUpdated {
		t.Fatalf("result = %+v, want a confirmed adoption", result)
	}

	got := readTransportRow(t, ctx, pool, rowID)
	if got.Path != releaseBPath {
		t.Fatalf("file_path = %q, want the adopted release B path %q", got.Path, releaseBPath)
	}
	if got.GUID == nil || *got.GUID != "release-b-guid" {
		t.Fatalf("provider_guid = %v, want release-b-guid", got.GUID)
	}
	if got.VideoHash != nil {
		t.Fatalf("release A hash survived onto release B: %q", *got.VideoHash)
	}
	if got.ReleaseName != nil {
		t.Fatalf("release A name survived onto release B: %q", *got.ReleaseName)
	}
	if got.ReleaseSize != nil {
		t.Fatalf("release A size survived onto release B: %d", *got.ReleaseSize)
	}
	if got.ResolvedURL != nil {
		t.Fatalf("release A URL survived onto release B: %q", *got.ResolvedURL)
	}
	if got.Headers != nil {
		t.Fatalf("release A headers survived onto release B: %s", *got.Headers)
	}
}

// TestVirtualFileMetadataUpdateHeaderlessResolutionClearsHeaders pins the
// nil-vs-empty rule on the handler write: a refreshed URL replaces the complete
// header set, so a headerless resolution must clear the previous URL's headers
// rather than leave them attached to the new URL.
func TestVirtualFileMetadataUpdateHeaderlessResolutionClearsHeaders(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994361, 7051
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-transport-refresh?result=cand"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			resolved_url, resolved_url_expires_at, provider_request_headers, provider_video_hash)
		VALUES('movie-transport-refresh', $1, $2, 'virtual', $3, 'virtual',
			'http://old.example/stream', NOW() + INTERVAL '1 hour',
			'{"Referer":"http://old.example/"}'::jsonb, 'old-hash')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// A URL refresh with no headers: the header set must be cleared, while the
	// same-release omitted identity tier (hash) is preserved.
	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: candidatePath,
		VideoTracks: []byte(`[]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		ResolvedURL: "http://new.example/stream",
		// ProviderRequestHeaders deliberately nil: the new URL carries none.
	}); err != nil {
		t.Fatalf("headerless URL refresh failed: %v", err)
	}

	got := readTransportRow(t, ctx, pool, rowID)
	if got.ResolvedURL == nil || *got.ResolvedURL != "http://new.example/stream" {
		t.Fatalf("resolved_url = %v, want the refreshed URL", got.ResolvedURL)
	}
	if got.Headers != nil {
		t.Fatalf("headers = %s, want them cleared with the refreshed URL", *got.Headers)
	}
	if got.VideoHash == nil || *got.VideoHash != "old-hash" {
		t.Fatalf("hash = %v, want the same-release omitted tier preserved", got.VideoHash)
	}
}

// TestVirtualFileMetadataUpdateMetadataOnlyPreservesTransport pins that a
// metadata-only write (no URL, no identity, no adoption) leaves the stored
// transport fields untouched.
func TestVirtualFileMetadataUpdateMetadataOnlyPreservesTransport(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994362, 7052
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-transport-meta?result=cand"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			resolved_url, resolved_url_expires_at, provider_request_headers, provider_video_hash, provider_guid)
		VALUES('movie-transport-meta', $1, $2, 'virtual', $3, 'virtual',
			'http://kept.example/stream', NOW() + INTERVAL '1 hour',
			'{"Referer":"http://kept.example/"}'::jsonb, 'kept-hash', 'kept-guid')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	before := readTransportRow(t, ctx, pool, rowID)

	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: candidatePath,
		VideoTracks: []byte(`[{"codec":"h264"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
	}); err != nil {
		t.Fatalf("metadata-only update failed: %v", err)
	}

	after := readTransportRow(t, ctx, pool, rowID)
	if after.ResolvedURL == nil || *after.ResolvedURL != "http://kept.example/stream" {
		t.Fatalf("resolved_url = %v, want the stored URL preserved", after.ResolvedURL)
	}
	if after.Headers == nil || *after.Headers != *before.Headers {
		t.Fatalf("headers = %v, want the stored set preserved", after.Headers)
	}
	if after.VideoHash == nil || *after.VideoHash != "kept-hash" || after.GUID == nil || *after.GUID != "kept-guid" {
		t.Fatalf("identity = (%v, %v), want the stored (kept-hash, kept-guid) preserved", after.VideoHash, after.GUID)
	}
}
