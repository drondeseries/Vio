package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestProbeVirtualSourcePrefersHeaderAwareProber(t *testing.T) {
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			t.Error("legacy prober should not be called when header-aware prober is set")
			return nil, context.DeadlineExceeded
		},
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			if headers["Referer"] != "https://new.example/" {
				t.Fatalf("headers = %v, want new Referer", headers)
			}
			return f, nil
		},
	}
	file := &models.MediaFile{ID: 1, FilePath: "virtual://movie/1?result=a"}
	if _, err := h.probeVirtualSource(context.Background(), "http://example.com/v.mp4", file, map[string]string{"Referer": "https://new.example/"}); err != nil {
		t.Fatalf("probeVirtualSource error: %v", err)
	}
}

func TestResolveUsesResolutionTimeHeadersAuthoritatively(t *testing.T) {
	var probedHeaders map[string]string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/list.mp4", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL:            "http://localhost:8080/resolved.mp4",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://new.example/"},
			}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID:             "cand-1",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://old.example/"},
			}}, nil
		}),
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			probedHeaders = headers
			f.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
			f.Container = "mp4"
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "movie-1", FilePath: "virtual://movie/1?result=cand-1"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified", resolved.Provenance)
	}
	if probedHeaders["Referer"] != "https://new.example/" {
		t.Fatalf("probed headers = %v, want new Referer", probedHeaders)
	}
}

func TestResolveClearedHeadersDoNotRetainStaleHeaders(t *testing.T) {
	var probedHeaders map[string]string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/list.mp4", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL: "http://localhost:8080/resolved.mp4",
				URI: "virtual://movie/1?result=cand-1",
			}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID:             "cand-1",
				URI:            "virtual://movie/1?result=cand-1",
				RequestHeaders: map[string]string{"Referer": "https://old.example/"},
			}}, nil
		}),
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, f *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
			probedHeaders = headers
			f.VideoTracks = []models.VideoTrack{{Codec: "h264"}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
			f.Container = "mp4"
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 11, ContentID: "movie-1", FilePath: "virtual://movie/1?result=cand-1"}
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if len(probedHeaders) != 0 {
		t.Fatalf("probed headers = %v, want empty after resolution cleared them", probedHeaders)
	}
}

func TestPersistVirtualMetadataBoundedForwardsExpectedPath(t *testing.T) {
	done := make(chan struct{}, 1)
	var gotID int
	var gotPath string
	h := &PlaybackHandler{
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			gotID = args.FileID
			gotPath = args.ExpectedFilePath
			done <- struct{}{}
			return 1, nil
		},
	}
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/1?result=cand-1"}
	snap := snapshotVirtualRow(file)
	_, err := h.persistVirtualMetadataBounded(context.Background(), snap, file.FilePath, file, true)
	if err != nil {
		t.Fatalf("persistVirtualMetadataBounded failed: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("saver was not called")
	}
	if gotID != 42 || gotPath != "virtual://movie/1?result=cand-1" {
		t.Fatalf("saver got (%d, %q), want (42, cand-1 URI)", gotID, gotPath)
	}
}

// TestResolvedSubstitutedCandidateMetadataMatchesResolved proves residual 2:
// when a fresh-selection fall-through serves a different candidate than the one
// probed, the served file must carry the resolved candidate's metadata, not the
// probed candidate's declared resolution, codecs or tracks. The prober returns
// the resolved bytes and the handler must serve those.
func TestResolvedSubstitutedCandidateMetadataMatchesResolved(t *testing.T) {
	var probedURL string
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "http://localhost:8080/list.mp4", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			// The resolver substitutes a different candidate than the probed one.
			return ResolvedVirtualMedia{
				URL:         "http://localhost:8080/resolved.mp4",
				URI:         "virtual://movie/1?result=cand-b",
				CandidateID: "cand-b",
			}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			// The probed candidate declares 2160p DV HEVC/TrueHD.
			return []VirtualPlaybackStream{{
				ID: "cand-a", URI: "virtual://movie/1?result=cand-a",
				Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "truehd", Container: "mkv", HDR: "dv",
			}}, nil
		}),
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, url string, f *models.MediaFile, _ map[string]string) (*models.MediaFile, error) {
			probedURL = url
			// The resolved bytes are 1080p H.264/AAC SDR.
			f.Resolution = "1080p"
			f.CodecVideo = "h264"
			f.CodecAudio = "aac"
			f.HDR = false
			f.Container = "mp4"
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2}}
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 12, ContentID: "movie-1", FilePath: "virtual://movie/1?result=cand-a"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if probedURL != "http://localhost:8080/resolved.mp4" {
		t.Fatalf("probed URL = %q, want the resolved URL", probedURL)
	}
	if resolved.URI != "virtual://movie/1?result=cand-b" || resolved.File == nil {
		t.Fatalf("resolved URI = %q file=%v, want the resolved cand-b", resolved.URI, resolved.File)
	}
	if resolved.File.Resolution != "1080p" || resolved.File.CodecVideo != "h264" ||
		resolved.File.CodecAudio != "aac" || resolved.File.HDR {
		t.Fatalf("served metadata = resolution=%q video=%q audio=%q hdr=%v, want the resolved 1080p h264/aac SDR",
			resolved.File.Resolution, resolved.File.CodecVideo, resolved.File.CodecAudio, resolved.File.HDR)
	}
	if len(resolved.File.VideoTracks) != 1 || resolved.File.VideoTracks[0].Codec != "h264" {
		t.Fatalf("served video tracks = %+v, want the probed resolved h264", resolved.File.VideoTracks)
	}
}
