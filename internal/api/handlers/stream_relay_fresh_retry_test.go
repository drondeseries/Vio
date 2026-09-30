package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// TestStreamResolveFreshRegistrationMintsNewRelayToken pins the serve-path
// retry contract for issue 1: a plain re-resolve of an unchanged provider URL
// reuses the live relay token, while the retry that sets the fresh-registration
// marker (withVirtualRelayFreshRegistration) mints a new one. Without the
// marker a 502 retry would replay the very registration whose upstream failed.
func TestStreamResolveFreshRegistrationMintsNewRelayToken(t *testing.T) {
	file := &models.MediaFile{
		ID:                         9,
		ContentID:                  "movie-fresh-relay",
		FilePath:                   "virtual://movie/movie-fresh-relay?result=cand-a",
		VirtualOwnerInstallationID: 7,
	}
	relay := remotestream.NewRelay()
	t.Cleanup(func() { _ = relay.Close(context.Background()) })
	h := &StreamHandler{
		RemoteStreamRelay:   relay,
		AllowPrivateStreams: func(int) bool { return true },
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://provider.local/video.mp4", URI: file.FilePath, CandidateID: "cand-a"}, nil
		}),
	}

	first, cleanupFirst, err := h.resolveVirtualInputURI(context.Background(), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if cleanupFirst != nil {
		defer cleanupFirst()
	}
	second, cleanupSecond, err := h.resolveVirtualInputURI(context.Background(), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if cleanupSecond != nil {
		defer cleanupSecond()
	}
	if first.URL != second.URL {
		t.Fatalf("plain re-registration minted a new token: first=%q second=%q", first.URL, second.URL)
	}

	fresh, cleanupFresh, err := h.resolveVirtualInputURI(withVirtualRelayFreshRegistration(context.Background()), file, 1, "profile-1", false)
	if err != nil {
		t.Fatalf("fresh resolve: %v", err)
	}
	if cleanupFresh != nil {
		defer cleanupFresh()
	}
	if fresh.URL == first.URL {
		t.Fatalf("fresh re-registration reused the live relay token %q", first.URL)
	}
}
