package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestResolveVirtualPlaybackSourceServesFreshProbeCacheEntry pins variant (c) of
// the audio-switch latency fix: an audio track change respawns the muxed HLS
// generation (the audio is interleaved into the same segments, so the transport
// cannot be reused), and the respawn's rehydration resolve would otherwise pay a
// full synchronous ffprobe for bytes that have not changed. When the probe cache
// already holds a completed probe for the candidate's canonical identity, the
// synchronous resolve must serve it and never invoke the prober.
func TestResolveVirtualPlaybackSourceServesFreshProbeCacheEntry(t *testing.T) {
	const candidateURI = "virtual://movie/tt-probe-cache?result=cand-a"

	file := &models.MediaFile{
		ID:                         501,
		ContentID:                  "movie-probe-cache",
		FilePath:                   candidateURI,
		VirtualOwnerInstallationID: 5,
		Container:                  "virtual",
	}

	detailedCalls := 0
	proberCalls := 0
	cacheLookups := 0
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			detailedCalls++
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=fresh", URI: candidateURI, CandidateID: "cand-a",
			}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/stream/token=legacy", nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			proberCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2}}
			f.Container = "mkv"
			f.Resolution = "1080p"
			f.CodecVideo = "h264"
			f.CodecAudio = "aac"
			return f, nil
		},
		VirtualProbeCacheLookup: func(_ string, probeFile *models.MediaFile) *models.MediaFile {
			cacheLookups++
			return &models.MediaFile{
				FilePath:                   probeFile.FilePath,
				VirtualOwnerInstallationID: probeFile.VirtualOwnerInstallationID,
				VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
				AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2}},
				Resolution:                 "1080p",
				CodecVideo:                 "h264",
				CodecAudio:                 "aac",
				Container:                  "mkv",
				Duration:                   5400,
			}
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)
	// A session-bound rehydration resolve (the audio-switch respawn's evidence
	// refresh) runs with deferProbe=false: it needs the probed inventory to
	// remap the new audio ordinal, so it is exactly the synchronous probe the
	// cache must short-circuit.
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: candidateURI})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified (the cache hit is completed probe evidence)", resolved.Provenance)
	}
	if resolved.File == nil || len(resolved.File.AudioTracks) == 0 || resolved.File.AudioTracks[0].Codec != "aac" {
		t.Fatalf("resolved file = %#v, want the cached probe inventory", resolved.File)
	}
	if proberCalls != 0 {
		t.Fatalf("synchronous prober calls = %d, want 0: a fresh probe-cache entry must skip the ffprobe", proberCalls)
	}
	if cacheLookups == 0 {
		t.Fatal("probe cache was never consulted before the synchronous probe")
	}
	if detailedCalls == 0 {
		t.Fatal("the provider resolve did not run: the cache short-circuits the probe, not the resolve")
	}
}

// TestResolveVirtualPlaybackSourceProbesOnCacheMiss keeps the cache-first path
// honest: with no cached entry the synchronous prober still runs, so a stale or
// absent cache can never fabricate evidence.
func TestResolveVirtualPlaybackSourceProbesOnCacheMiss(t *testing.T) {
	const candidateURI = "virtual://movie/tt-probe-miss?result=cand-a"

	file := &models.MediaFile{
		ID:                         502,
		ContentID:                  "movie-probe-miss",
		FilePath:                   candidateURI,
		VirtualOwnerInstallationID: 5,
		Container:                  "virtual",
	}

	proberCalls := 0
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=fresh", URI: candidateURI, CandidateID: "cand-a"}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/stream/token=legacy", nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			proberCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2}}
			f.Container = "mkv"
			f.Resolution = "1080p"
			f.CodecVideo = "h264"
			f.CodecAudio = "aac"
			return f, nil
		},
		VirtualProbeCacheLookup: func(_ string, _ *models.MediaFile) *models.MediaFile { return nil },
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: candidateURI})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified", resolved.Provenance)
	}
	if proberCalls != 1 {
		t.Fatalf("synchronous prober calls = %d, want 1 on a cache miss", proberCalls)
	}
}
