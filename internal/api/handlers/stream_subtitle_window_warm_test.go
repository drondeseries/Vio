package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// newVirtualSubtitleWindowFixture builds a StreamHandler rooted at dir that
// resolves a virtual source through a live relay and extracts text subtitles
// through a fake ffmpeg at dir/ffmpeg. The caller writes the ffmpeg script and
// gets back the handler, the bound session, the virtual file, and the pinned
// source URI.
func newVirtualSubtitleWindowFixture(t *testing.T, dir, ffmpegScript string) (*StreamHandler, *playback.Session, *models.MediaFile, string) {
	t.Helper()
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), ffmpegScript)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("provider-media"))
	}))
	t.Cleanup(upstream.Close)

	handler := NewStreamHandler(nil, nil)
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowInsecureVirtual = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mkv"}, nil
	})

	virtualURI := "virtual://movie/tt-window?result=cand-window"
	file := &models.MediaFile{
		ID: 91, ContentID: "movie-window", FilePath: virtualURI,
		SubtitleTracks:             []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
		VirtualOwnerInstallationID: 5,
	}
	session := &playback.Session{
		ID: "sess-window", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI: virtualURI, VirtualSubtitleTracks: file.SubtitleTracks,
	}
	return handler, session, file, virtualURI
}

// warmArgsLogScript logs every invocation and distinguishes the full-track
// warm (no -ss) from the windowed extract. The warm blocks on gate so the
// coalescing test can hold it in flight.
func warmArgsLogScript(argsLog, gate string) string {
	return "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsLog + "'\n" +
		"case \"$*\" in\n" +
		"  *-ss*)\n    cat <<'VTT'\n" + warmSubtitleVTT + "VTT\n    ;;\n" +
		"  *)\n    while [ ! -f '" + gate + "' ]; do sleep 0.01; done\n    cat <<'VTT'\n" + warmSubtitleVTT + "VTT\n    ;;\n" +
		"esac\n"
}

func ffmpegLogLines(t *testing.T, argsLog string) (window, warm int) {
	t.Helper()
	data, err := os.ReadFile(argsLog)
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "-ss") {
			window++
		} else {
			warm++
		}
	}
	return window, warm
}

func waitForWarmInvocation(t *testing.T, argsLog string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, warm := ffmpegLogLines(t, argsLog); warm > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("detached full-track warm never started")
}

func waitForCommittedTextEntry(t *testing.T, c *playback.SubtitleCache, identity string, trackIndex int, codec string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.HasCommittedTextEntry("unused", identity, trackIndex, codec, "") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("warm never committed a %s entry under identity %q", codec, identity)
}

// A windowed virtual text miss must stream its own window and then start
// exactly one detached full-track warm under the serve identity. A second
// window miss while that warm is in flight must not start a second fill.
func TestVirtualTextWindowMissWarmsOnceWithServeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	gate := filepath.Join(dir, "warm.gate")
	// Always release the warm, even if an assertion fails, so the blocked fake
	// ffmpeg cannot outlive the test.
	t.Cleanup(func() { _ = os.WriteFile(gate, []byte("go"), 0o644) })
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir, warmArgsLogScript(argsLog, gate))

	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil)
		rec := httptest.NewRecorder()
		handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
		return rec
	}

	if rec := serve(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("first window: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// The first window request started its detached warm; wait until it holds
	// the in-flight fill before issuing the second miss.
	waitForWarmInvocation(t, argsLog)

	if rec := serve(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("second window: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Release the warm and wait for the entry it commits.
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, 0)
	waitForCommittedTextEntry(t, handler.SubtitleCache, identity, 0, "subrip")

	window, warm := ffmpegLogLines(t, argsLog)
	if window != 2 {
		t.Fatalf("windowed extracts = %d, want 2", window)
	}
	if warm != 1 {
		t.Fatalf("full-track warms = %d, want exactly 1 (coalesced)", warm)
	}
}

// A committed full-track artifact makes the row-vs-evidence drift probe
// unnecessary: the windowed request must skip the probe and serve its window
// from the cached artifact.
func TestVirtualTextWindowCommittedEntrySkipsDriftProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	probeLog := filepath.Join(dir, "ffprobe.log")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	// The drift probe derives ffprobe from the ffmpeg path; install one that
	// records any probe invocation. It reports an empty live layout, so if the
	// probe ran at all the test would still see its log line.
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"),
		"#!/bin/sh\necho probe >> '"+probeLog+"'\nprintf '{\"streams\":[]}'\n")

	identity := playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, 0)
	done := handler.SubtitleCache.WarmTrackInBackground(playback.StreamExtractOpts{
		InputPath:     "unused",
		CacheIdentity: identity,
		TrackIndex:    0,
		SourceCodec:   "subrip",
		FFmpegPath:    filepath.Join(dir, "ffmpeg"),
	}, playback.StreamExtractSubtitle)
	<-done
	if !handler.SubtitleCache.HasCommittedTextEntry("unused", identity, 0, "subrip", "") {
		t.Fatal("pre-warm did not commit the serve identity")
	}
	if err := os.Remove(argsLog); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil)
	rec := httptest.NewRecorder()
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, true, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("window with committed entry: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("drift probe ran %d times despite a committed artifact, want 0", got)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.TrimSpace(string(data))
	if !strings.Contains(args, "subtitle-cache") {
		t.Fatalf("windowed extract did not read the cached artifact: %s", args)
	}
	if !strings.Contains(args, "-ss") {
		t.Fatalf("windowed extract lost its window: %s", args)
	}
}

// The window-miss warm keys on the exact ordinal the serve path hands it, so
// after a drift remap the post-remap identity is what gets populated and the
// remapped serve path finds it. The start-path warm still keys on the plan
// ordinal; the serve-path warm is authoritative for the remap case, so an
// orphaned plan-ordinal entry self-heals on the next window.
func TestVirtualTextWindowWarmKeysOnServeOrdinalAfterRemap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")

	const remappedOrdinal = 1
	// This is the identity the remapped serve path computes; the warm must
	// commit under it.
	serveIdentity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, remappedOrdinal)
	handler.warmVirtualSubtitleAfterWindowMiss(context.Background(), file, session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   serveIdentity,
		TrackIndex:      remappedOrdinal,
		SourceCodec:     "subrip",
		SeekSeconds:     600,
		DurationSeconds: 600,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, true)
	waitForCommittedTextEntry(t, handler.SubtitleCache, serveIdentity, remappedOrdinal, "subrip")

	planIdentity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	if handler.SubtitleCache.HasCommittedTextEntry("unused", planIdentity, 0, "subrip", "") {
		t.Fatal("warm must not commit under the orphaned plan ordinal")
	}
}

// Non-virtual requests and PGS windows keep their existing path: no
// handler-owned text warm is started for either.
func TestVirtualTextWindowWarmSkipsPGSAndNonVirtual(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\n")
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)

	// PGS returns before any goroutine or cache work.
	handler.warmVirtualSubtitleAfterWindowMiss(context.Background(), file, session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   identity,
		TrackIndex:      0,
		SourceCodec:     "hdmv_pgs_subtitle",
		SeekSeconds:     100,
		DurationSeconds: 600,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, true)
	// A non-virtual handler (virtualActive false) is a no-op.
	handler.warmVirtualSubtitleAfterWindowMiss(context.Background(), file, session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   identity,
		TrackIndex:      0,
		SourceCodec:     "subrip",
		SeekSeconds:     100,
		DurationSeconds: 600,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, false)

	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("no warm should run for PGS or non-virtual requests: %v", err)
	}
}
