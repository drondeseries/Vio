package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const warmSubtitleVTT = "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhello\n"

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

	handler.warmVirtualSubtitlesV3(context.Background(), stale, file)

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
