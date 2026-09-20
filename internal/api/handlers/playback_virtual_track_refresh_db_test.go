package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readTrackRefreshRow reads the inventory and probe stamp of a row.
func readTrackRefreshRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) (audio string, subtitle string, probeSource *string, probeUpdatedAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT audio_tracks::text, subtitle_tracks::text, probe_source, probe_updated_at
		FROM media_files WHERE id = $1`, id).Scan(&audio, &subtitle, &probeSource, &probeUpdatedAt); err != nil {
		t.Fatalf("read track refresh row %d: %v", id, err)
	}
	return audio, subtitle, probeSource, probeUpdatedAt
}

// TestVirtualFileMetadataUpdateClearProbeInvalidatesEvidence proves the
// ClearProbe write drops probe_source and probe_updated_at together, so a row
// whose stored inventory no longer describes the adopted bytes stops looking
// probed and the next start re-probes it. The track inventory written with the
// clear lands as usual.
func TestVirtualFileMetadataUpdateClearProbeInvalidatesEvidence(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994370, 7060
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-clear-probe?result=a"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id,
			probe_source, probe_updated_at)
		VALUES('movie-clear-probe', $1, $2, 'virtual', $3, 'virtual', NOW())
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	if probeUpdatedAt == nil {
		t.Fatal("fixture row was not stamped; the clear test would be vacuous")
	}

	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: candidatePath,
		VideoTracks:    []byte(`[{"codec":"h264"}]`),
		AudioTracks:    []byte(`[{"codec":"eac3","channels":6,"language":"fr"}]`),
		SubtitleTracks: []byte(`[]`),
		Resolution:     "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		ClearProbe: true,
	}); err != nil {
		t.Fatalf("clear-probe write failed: %v", err)
	}

	audio, subtitle, probeSource, probeAfter := readTrackRefreshRow(t, ctx, pool, rowID)
	if probeSource != nil {
		t.Fatalf("probe_source = %q, want NULL after a clear", *probeSource)
	}
	if probeAfter != nil {
		t.Fatalf("probe_updated_at = %v, want NULL after a clear", probeAfter)
	}
	var gotAudio []models.AudioTrack
	if err := json.Unmarshal([]byte(audio), &gotAudio); err != nil {
		t.Fatalf("unmarshal audio_tracks %s: %v", audio, err)
	}
	if len(gotAudio) != 1 || gotAudio[0].Language != "fr" {
		t.Fatalf("audio_tracks = %#v, want the cleared row's declared inventory written", gotAudio)
	}
	if subtitle != `[]` {
		t.Fatalf("subtitle_tracks = %s, want the old inventory dropped", subtitle)
	}
}

// TestVirtualFileMetadataUpdateClearProbePreservesCollectionStamp proves the
// clear never invalidates a collection-owned row: the collection materializer
// relies on probe_source = 'virtual_collection', and a real probe still stamps
// it. The clear branch must mirror the stamp branch's collection guard.
func TestVirtualFileMetadataUpdateClearProbePreservesCollectionStamp(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994371, 7061
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-clear-probe-collection?result=a"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id,
			probe_source, probe_updated_at)
		VALUES('movie-clear-probe-collection', $1, $2, 'virtual', $3, 'virtual_collection', NOW())
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert collection row: %v", err)
	}

	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: candidatePath,
		VideoTracks: []byte(`[{"codec":"h264"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		ClearProbe: true,
	}); err != nil {
		t.Fatalf("clear-probe write failed: %v", err)
	}

	_, _, probeSource, probeAfter := readTrackRefreshRow(t, ctx, pool, rowID)
	if probeSource == nil || *probeSource != "virtual_collection" {
		t.Fatalf("probe_source = %v, want virtual_collection preserved", probeSource)
	}
	if probeAfter == nil {
		t.Fatal("probe_updated_at was cleared on a collection-owned row")
	}
}

// TestVirtualFileMetadataUpdateProbeOverwritesDeclaredInventory proves the
// probe is the authority: a later probe write overwrites a row's declared
// placeholder inventory with the real probed tracks and stamps the row, so the
// next start serves the real streams rather than the placeholder labels.
func TestVirtualFileMetadataUpdateProbeOverwritesDeclaredInventory(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994372, 7062
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-probe-overwrite?result=a"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id,
			probe_source, audio_tracks, subtitle_tracks)
		VALUES('movie-probe-overwrite', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"aac","channels":2,"language":"fr"}]'::jsonb, '[]'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert declared row: %v", err)
	}

	probedAudio := `[{"codec":"eac3","index":1,"channels":6,"language":"en","default":true}]`
	probedSubtitle := `[{"codec":"subrip","index":5,"language":"en","default":true}]`
	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: candidatePath,
		VideoTracks:    []byte(`[{"codec":"h264"}]`),
		AudioTracks:    []byte(probedAudio),
		SubtitleTracks: []byte(probedSubtitle),
		Resolution:     "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
	}); err != nil {
		t.Fatalf("probe overwrite failed: %v", err)
	}

	audio, subtitle, probeSource, probeAfter := readTrackRefreshRow(t, ctx, pool, rowID)
	var gotAudio []models.AudioTrack
	if err := json.Unmarshal([]byte(audio), &gotAudio); err != nil {
		t.Fatalf("unmarshal audio_tracks %s: %v", audio, err)
	}
	if len(gotAudio) != 1 || gotAudio[0].Codec != "eac3" || gotAudio[0].Index != 1 ||
		gotAudio[0].Language != "en" || gotAudio[0].Channels != 6 {
		t.Fatalf("audio_tracks = %#v, want the probed inventory on top of the declared one", gotAudio)
	}
	var gotSubtitle []models.SubtitleTrack
	if err := json.Unmarshal([]byte(subtitle), &gotSubtitle); err != nil {
		t.Fatalf("unmarshal subtitle_tracks %s: %v", subtitle, err)
	}
	if len(gotSubtitle) != 1 || gotSubtitle[0].Codec != "subrip" || gotSubtitle[0].Index != 5 || gotSubtitle[0].Language != "en" {
		t.Fatalf("subtitle_tracks = %#v, want the probed inventory on top of the declared one", gotSubtitle)
	}
	if probeSource == nil || *probeSource != "virtual" || probeAfter == nil {
		t.Fatalf("probe stamp = (%v, %v), want the row stamped", probeSource, probeAfter)
	}
}
