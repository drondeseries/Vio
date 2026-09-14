package remuxdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedParentRecords(t *testing.T, pool *pgxpool.Pool, folderID int, contentID string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO media_folders (id, type, name)
		VALUES ($1, 'movies', 'Movies')
		ON CONFLICT (id) DO NOTHING`, folderID)
	if err != nil {
		t.Fatalf("seed media_folder: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title)
		VALUES ($1, 'movie', 'Test Movie')
		ON CONFLICT (content_id) DO NOTHING`, contentID)
	if err != nil {
		t.Fatalf("seed media_item: %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	seedParentRecords(t, pool, 7, "movie-1")

	variant := videoVariant(28979107000, "h264", 1080, "", 0,
		ProbeSource{Kind: "nzb", Filename: "release.mkv"})
	variant.Container = "mkv"
	variant.Duration = 6583.0
	variant.Bitrate = 35213589
	ev := EvidenceFromVariant("movie-1", "", 7, "virtual://movie/tt1", MatchSizeTags, &variant)

	if err := store.Record(ctx, ev); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, ok, err := store.Get(ctx, "movie-1", "", 7, "virtual://movie/tt1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("stored evidence not found")
	}
	if string(got.MatchMethod) != string(MatchSizeTags) || got.MatchedSize != 28979107000 {
		t.Fatalf("method/size = %q/%d, want size_tags/28979107000", got.MatchMethod, got.MatchedSize)
	}
	if got.CodecVideo != "h264" || got.Resolution != "1080p" || got.Container != "mkv" {
		t.Fatalf("evidence = %+v, want h264/1080p/mkv", got)
	}
	if len(got.VideoTracks) != 1 || len(got.AudioTracks) != 1 {
		t.Fatalf("tracks = %d video %d audio, want 1/1", len(got.VideoTracks), len(got.AudioTracks))
	}
	if !got.HDRKnown || got.HDR {
		t.Fatalf("hdr = %v known %v, want false/true", got.HDR, got.HDRKnown)
	}

	if _, ok, err := store.Get(ctx, "movie-1", "", 7, "virtual://movie/other"); err != nil || ok {
		t.Fatalf("unknown candidate get = %v %v, want miss", ok, err)
	}

	ev.MatchMethod = MatchInfoHash
	if err := store.Record(ctx, ev); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	got, ok, err = store.Get(ctx, "movie-1", "", 7, "virtual://movie/tt1")
	if err != nil || !ok || string(got.MatchMethod) != string(MatchInfoHash) {
		t.Fatalf("updated evidence = %+v %v %v, want info_hash", got, ok, err)
	}

	// Test TTL expiration: simulate expired evidence
	if _, err := pool.Exec(ctx, `UPDATE remuxdb_match_evidence SET expires_at = now() - interval '1 hour' WHERE content_id=$1`, "movie-1"); err != nil {
		t.Fatalf("expire evidence: %v", err)
	}
	if _, ok, err := store.Get(ctx, "movie-1", "", 7, "virtual://movie/tt1"); err != nil || ok {
		t.Fatalf("expired evidence returned ok=%v err=%v, want miss", ok, err)
	}

	// Test PruneExpired
	pruned, err := store.PruneExpired(ctx)
	if err != nil {
		t.Fatalf("prune expired: %v", err)
	}
	if pruned < 1 {
		t.Fatalf("pruned = %d, want >= 1", pruned)
	}

	// Test foreign key cascade deletion: deleting parent media_item cascades
	seedParentRecords(t, pool, 8, "movie-cascade")
	evCascade := EvidenceFromVariant("movie-cascade", "", 8, "virtual://movie/ttcascade", MatchSize, &variant)
	if err := store.Record(ctx, evCascade); err != nil {
		t.Fatalf("record cascade: %v", err)
	}
	if _, ok, err := store.Get(ctx, "movie-cascade", "", 8, "virtual://movie/ttcascade"); err != nil || !ok {
		t.Fatalf("cascade evidence not stored: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, "movie-cascade"); err != nil {
		t.Fatalf("delete parent media_item: %v", err)
	}
	if _, ok, err := store.Get(ctx, "movie-cascade", "", 8, "virtual://movie/ttcascade"); err != nil || ok {
		t.Fatalf("cascade evidence survived parent item deletion: ok=%v err=%v", ok, err)
	}
}

func TestStoreNilPoolIsNoop(t *testing.T) {
	var store *Store
	if _, ok, err := store.Get(context.Background(), "a", "", 0, "u"); err != nil || ok {
		t.Fatalf("nil get = %v %v", ok, err)
	}
	if err := store.Record(context.Background(), Evidence{}); err != nil {
		t.Fatalf("nil record: %v", err)
	}
	if err := NewStore(nil).Record(context.Background(), Evidence{}); err != nil {
		t.Fatalf("nil pool record: %v", err)
	}
	if n, err := store.PruneExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("nil PruneExpired = %d %v", n, err)
	}
	if n, err := NewStore(nil).PruneExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("nil pool PruneExpired = %d %v", n, err)
	}
}

func TestDecodeTrackListPropagatesCorruptJSON(t *testing.T) {
	var tracks []TrackDetail
	err := decodeTrackList([]byte(`{"not":"an array"}`), &tracks, "video tracks")
	if err == nil {
		t.Fatal("corrupt track JSON should return an error")
	}
	if !strings.Contains(err.Error(), "video tracks") {
		t.Fatalf("error = %v, want label video tracks", err)
	}
	if err := decodeTrackList(nil, &tracks, "video tracks"); err != nil {
		t.Fatalf("empty payload should be tolerated: %v", err)
	}
	if err := decodeTrackList([]byte(`[{"kind":"audio","idx":1}]`), &tracks, "audio tracks"); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Index != 1 {
		t.Fatalf("tracks = %+v, want one index 1", tracks)
	}
}
