package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const warmSubtitleVTT = "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhello\n"

// newVirtualSubtitleWarmFixture builds a handler whose virtual subtitle warm
// runs through a fake ffmpeg that records its argv and emits a VTT payload. The
// fake ffmpeg ignores the requested output format (a warm for an ASS track is
// still stored as bytes under the .ass cache key), so tests assert on the argv
// and cache identity rather than the emitted body.
func newVirtualSubtitleWarmFixture(t *testing.T, tracks []models.SubtitleTrack) (*PlaybackHandler, *playback.Session, *models.MediaFile, string, string) {
	t.Helper()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	mediaPath := filepath.Join(dir, "media.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsLog + "'\ncat <<'VTT'\n" + warmSubtitleVTT + "VTT\n"
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), script)

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.VirtualMediaResolver = VirtualMediaResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string) (string, error) {
		return mediaPath, nil
	})
	virtualURI := "virtual://movie/tt-warm?result=cand-warm"
	file := &models.MediaFile{
		ID: 77, ContentID: "movie-warm", FilePath: virtualURI,
		SubtitleTracks: tracks, VirtualOwnerInstallationID: 5,
	}
	session := &playback.Session{
		ID: "sub-warm-selected", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI: virtualURI, VirtualSubtitleTracks: tracks,
	}
	return handler, session, file, argsLog, mediaPath
}

// waitForWarmEntry polls the serve path's own cache lookup until the given
// track's committed entry is visible, so a test never races the detached warm.
func waitForWarmEntry(t *testing.T, handler *PlaybackHandler, session *playback.Session, file *models.MediaFile, trackIndex int, codec string) {
	t.Helper()
	opts := playback.StreamExtractOpts{
		InputPath:     "unused",
		CacheIdentity: playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, trackIndex),
		TrackIndex:    trackIndex,
		SourceCodec:   codec,
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
		if err := handler.SubtitleCache.ServeExtract(rec, req, opts, func(context.Context, playback.StreamExtractOpts) error {
			return errors.New("cache miss: warm identity did not match serve identity")
		}); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("virtual subtitle warm never populated track %d", trackIndex)
}

// Only the selected embedded track is warmed, and it is warmed whole-track:
// the windowed serve branch reads a full-track cache entry (it has no window in
// its key) and re-extracts the client's window from it, so a windowed warm
// artifact would silently truncate later windows.
func TestWarmVirtualSubtitlesWarmsSelectedTrackWholeTrack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, argsLog, _ := newVirtualSubtitleWarmFixture(t, []models.SubtitleTrack{
		{Index: 0, Codec: "subrip"},
		{Index: 1, Codec: "ass"},
	})

	handler.warmVirtualSubtitlesV3(context.Background(), session, file, 1)
	waitForWarmEntry(t, handler, session, file, 1, "ass")

	if got := countLogLines(t, argsLog); got != 1 {
		t.Fatalf("ffmpeg ran %d times for a two-track file, want exactly 1 (selected track only)", got)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(strings.TrimSpace(string(data)))
	if !slices.Contains(args, "0:s:1") {
		t.Fatalf("warm args %v do not map the selected track 0:s:1", args)
	}
	if slices.Contains(args, "-ss") || slices.Contains(args, "-to") {
		t.Fatalf("warm args %v carry a window; the serve path reads a whole-track entry", args)
	}
}

func TestWarmVirtualSubtitlesSkipsWhenNoEmbeddedTrackSelected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, argsLog, _ := newVirtualSubtitleWarmFixture(t, []models.SubtitleTrack{{Index: 0, Codec: "subrip"}})

	handler.warmVirtualSubtitlesV3(context.Background(), session, file, -1)

	if got := countLogLines(t, argsLog); got != 0 {
		t.Fatalf("ffmpeg ran %d times with no selected subtitle, want 0", got)
	}
}

func TestWarmVirtualSubtitlesSkipsFileWithoutSubtitleTracks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, argsLog, _ := newVirtualSubtitleWarmFixture(t, nil)

	handler.warmVirtualSubtitlesV3(context.Background(), session, file, 0)

	if got := countLogLines(t, argsLog); got != 0 {
		t.Fatalf("ffmpeg ran %d times for a file without subtitle tracks, want 0", got)
	}
}

func TestWarmVirtualSubtitlesSkipsNonVirtualFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, argsLog, _ := newVirtualSubtitleWarmFixture(t, []models.SubtitleTrack{{Index: 0, Codec: "ass"}})
	file.FilePath = "/local/movie.mkv"
	session.VirtualSourceURI = ""

	handler.warmVirtualSubtitlesV3(context.Background(), session, file, 0)

	if got := countLogLines(t, argsLog); got != 0 {
		t.Fatalf("ffmpeg ran %d times for a non-virtual file, want 0", got)
	}
}

// The scheduler must hand the warm to a detached goroutine rather than run it
// on the start path. The warm's relay resolution blocks here; the scheduling
// call must still return, then the resolver runs behind it.
func TestScheduleVirtualWarmAfterTransportIsDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, _, mediaPath := newVirtualSubtitleWarmFixture(t, []models.SubtitleTrack{{Index: 0, Codec: "subrip"}})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handler.VirtualMediaResolver = VirtualMediaResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return mediaPath, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	started := time.Now()
	handler.scheduleVirtualWarmAfterTransportV3(context.Background(), session, file, 0)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("scheduleVirtualWarmAfterTransportV3 blocked %v on the warm", elapsed)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("detached warm never reached relay resolution")
	}
	close(release)
	waitForWarmEntry(t, handler, session, file, 0, "subrip")
}

// The start path's session copy predates updateV3SessionState, so the text
// warm must fall back to the effective file's pinned URI. That fallback must
// produce exactly the identity the serve path computes from the session's
// bound URI, track for track.
func TestVirtualSubtitleWarmSourceURIMatchesServeBinding(t *testing.T) {
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt1?result=cand-1"}

	// The stale start-path copy: UpdateStreamState has not run yet.
	stale := &playback.Session{}
	warmURI := virtualSubtitleWarmSourceURI(stale, file)
	if warmURI != file.FilePath {
		t.Fatalf("warm URI with stale session = %q, want effective file URI %q", warmURI, file.FilePath)
	}

	// updateV3SessionState binds the effective file's URI; the serve path reads
	// it back from the session. The two identities must agree for every track.
	bound := &playback.Session{VirtualSourceURI: file.FilePath}
	for track := 0; track < 3; track++ {
		warmID := playback.VirtualSubtitleCacheIdentity(file.ID, warmURI, track)
		serveID := playback.VirtualSubtitleCacheIdentity(file.ID, bound.VirtualSourceURI, track)
		if warmID != serveID {
			t.Fatalf("track %d: warm identity %q != serve identity %q", track, warmID, serveID)
		}
	}

	// A live session URI still wins when present.
	live := &playback.Session{VirtualSourceURI: "virtual://movie/tt1?result=other"}
	if got := virtualSubtitleWarmSourceURI(live, file); got != live.VirtualSourceURI {
		t.Fatalf("warm URI with live session = %q, want %q", got, live.VirtualSourceURI)
	}
}

// With a stale session copy the text warm must actually schedule (not no-op)
// and land its entry under the serve path's cache identity, so the first
// client fetch is a byte-for-byte cache hit with no second demux.
func TestWarmVirtualSubtitlesSchedulesWithStaleSessionCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	ffmpegLog := filepath.Join(dir, "ffmpeg.log")
	mediaPath := filepath.Join(dir, "media.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho run >> '" + ffmpegLog + "'\ncat <<'VTT'\n" + warmSubtitleVTT + "VTT\n"
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), script)

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.VirtualMediaResolver = VirtualMediaResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string) (string, error) {
		return mediaPath, nil
	})
	file := &models.MediaFile{
		ID: 42, ContentID: "movie-1", FilePath: "virtual://movie/tt1?result=cand-1",
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Codec: "subrip"}}, VirtualOwnerInstallationID: 5,
	}
	// The start path's session copy: VirtualSourceURI is still empty.
	stale := &playback.Session{ID: "sub-warm-session", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID}

	handler.warmVirtualSubtitlesV3(context.Background(), stale, file, 0)

	// The serve path reads the post-update session binding, which
	// updateV3SessionState set to the effective file's URI.
	serveOpts := playback.StreamExtractOpts{
		InputPath:     mediaPath,
		CacheIdentity: playback.VirtualSubtitleCacheIdentity(file.ID, file.FilePath, 0),
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
		err := handler.SubtitleCache.ServeExtract(rec, req, serveOpts, func(context.Context, playback.StreamExtractOpts) error {
			return errors.New("cache miss: warm identity did not match serve identity")
		})
		if err == nil {
			if got := rec.Body.String(); got != warmSubtitleVTT {
				t.Fatalf("served body = %q, want warm payload %q", got, warmSubtitleVTT)
			}
			if got := countLogLines(t, ffmpegLog); got != 1 {
				t.Fatalf("ffmpeg ran %d times, want exactly 1", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("virtual subtitle warm never populated the serve-path cache identity")
}
