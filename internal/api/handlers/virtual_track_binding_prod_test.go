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

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// prodSubtitleProbeScript emits an ffprobe subtitle layout for candidate B: two
// text tracks, the French one at container index 7.
const prodSubtitleProbeJSON = `#!/bin/sh
cat <<'JSON'
{"streams":[
  {"id":"4","index":4,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}},
  {"id":"7","index":7,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"fra"}}
]}
JSON
`

// Replays the live prod sequence: a virtual session plans against candidate A
// (French subtitle at combined ordinal 0), then a serve-layer rotation moves the
// binding to candidate B while the carried subtitle evidence still describes A.
// The request must serve B's inventory — never A's carried ordinals. With the
// live layout verified against B, the plan ordinal is interpreted against B.
func TestVirtualProdSequenceServesRotatedCandidateInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"), prodSubtitleProbeJSON)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("provider-media"))
	}))
	t.Cleanup(upstream.Close)

	// The catalog row now describes candidate B (re-probed after rotation).
	candidateB := "virtual://movie/tt-prod?result=candidate-b"
	file := &models.MediaFile{
		ID: 3459774, ContentID: "movie-prod", FilePath: candidateB,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
		},
		VirtualOwnerInstallationID: 5,
	}

	manager := playback.NewSessionManager(0, 0)
	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	t.Cleanup(handler.waitForBackgroundSubtitleWarms)
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mkv"}, nil
	})

	// The session was bound to candidate A and carries A's evidence (French at
	// combined ordinal 0, container index 1). The rotation moved the binding to
	// B but left the evidence anchored at A.
	session := &playback.Session{
		ID: "sess-prod", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI:           candidateB,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: "virtual://movie/tt-prod?result=candidate-a",
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
	}
	manager.RegisterReconstructed(session)

	rec := httptest.NewRecorder()
	handler.HandleSubtitle(rec, playbackTestRequest(http.MethodGet,
		"/stream/"+session.ID+"/subtitles/0.vtt", nil,
		map[string]string{"session_id": session.ID, "track": "0.vtt"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read ffmpeg args: %v", err)
	}
	// The carried ordinal 0 named the French track in candidate A. Candidate B
	// carries French at ordinal 1 (container index 7), so the request must be
	// remapped to that track — never served at the stale ordinal 0, which in B
	// is a different language (English at container index 4).
	if !strings.Contains(string(args), "-map 0:s:1") {
		t.Fatalf("extraction did not remap the French selection onto the rotated candidate: %s", args)
	}
	if strings.Contains(string(args), "-map 0:s:0") {
		t.Fatalf("extraction served the stale ordinal against the rotated candidate: %s", args)
	}
}

// When the live layout cannot be established after a rotation, the request must
// fail closed with a retryable error rather than serve the carried ordinal.
func TestVirtualProdSequenceFailsClosedWhenLayoutUnverifiable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"), "#!/bin/sh\nexit 1\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("provider-media"))
	}))
	t.Cleanup(upstream.Close)

	candidateB := "virtual://movie/tt-prod?result=candidate-b"
	file := &models.MediaFile{
		ID: 3459774, ContentID: "movie-prod", FilePath: candidateB,
		SubtitleTracks:             []models.SubtitleTrack{{Index: 7, Codec: "subrip", Language: "fra"}},
		VirtualOwnerInstallationID: 5,
	}
	manager := playback.NewSessionManager(0, 0)
	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	t.Cleanup(handler.waitForBackgroundSubtitleWarms)
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mkv"}, nil
	})

	session := &playback.Session{
		ID: "sess-prod-unverifiable", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI:           candidateB,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: "virtual://movie/tt-prod?result=candidate-a",
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
	}
	manager.RegisterReconstructed(session)

	rec := httptest.NewRecorder()
	handler.HandleSubtitle(rec, playbackTestRequest(http.MethodGet,
		"/stream/"+session.ID+"/subtitles/0.vtt", nil,
		map[string]string{"session_id": session.ID, "track": "0.vtt"}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverifiable rotated layout = %d %q, want retryable 503", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("ffmpeg spawned without a verified layout: %v", err)
	}
}
