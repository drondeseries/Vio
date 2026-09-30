package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
)

// TestFallbackRotatesToExistingAlternateVersionRow pins the dead-session
// rotation: when the session-bound candidate is dead and the fallback resolves a
// healthy substitute whose path is already owned by another catalog row (an
// existing alternate version of the same content), the fallback must rotate to
// that row instead of attempting a required adoption the SQL sibling guard
// refuses. Before the fix the adoption was refused and the session stayed
// pinned to the dead release.
func TestFallbackRotatesToExistingAlternateVersionRow(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-alt-version"
		oldURI  = neutral + "?result=old"
		newURI  = neutral + "?result=new"
	)
	sessionRow := &models.MediaFile{
		ID: 100, ContentID: "movie-alt-version", FilePath: oldURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5,
		ProviderVideoHash: "hash-old", ProviderReleaseName: "Movie.Old.2160p",
	}
	siblingRow := &models.MediaFile{
		ID: 200, ContentID: "movie-alt-version", FilePath: newURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
		ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
	}
	streams := []VirtualPlaybackStream{{
		ID: "new", URI: newURI, Resolution: "2160p",
		ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
	}}

	var saved []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		fileResolver: byPathPlaybackFileResolver{
			testPlaybackFileResolver: testPlaybackFileResolver{file: sessionRow},
			byPath:                   map[string]*models.MediaFile{newURI: siblingRow},
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL: "https://cdn.example/new.mp4", URI: uri, CandidateID: "new",
				ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
			}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, base *models.MediaFile) (*models.MediaFile, error) {
			probed := *base
			probed.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10}}
			probed.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			probed.Resolution = "2160p"
			probed.CodecVideo = "hevc"
			probed.CodecAudio = "eac3"
			probed.Container = "mkv"
			return &probed, nil
		},
	}
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		saved = append(saved, args)
		return 1, nil
	}

	result := h.fallbackResolveStaleVirtualSource(context.Background(), sessionRow, 1, "profile-1",
		virtualFallbackEligibility{sessionBound: true, rotationAllowed: true, releaseID: "old"})
	if result == nil {
		t.Fatal("fallback returned nil, want the healthy alternate version")
	}
	if result.URI != newURI {
		t.Fatalf("resolved URI = %q, want the alternate version %q", result.URI, newURI)
	}
	if result.File == nil || result.File.ID != siblingRow.ID {
		t.Fatalf("resolved file = %#v, want the alternate version's row id %d", result.File, siblingRow.ID)
	}
	if len(saved) != 0 {
		t.Fatalf("persist calls = %d, want 0 (an existing row needs no adoption): %#v", len(saved), saved)
	}
}
