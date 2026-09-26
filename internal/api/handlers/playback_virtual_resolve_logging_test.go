package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestVirtualStreamResolveFailureLogsRequestIDAndIdentity proves #147's
// transport half: the resolve-failure warn carries a request_id and an explicit
// has_identity flag, so a user-visible failure can be correlated to its resolve
// chain without a manual session-id grep.
func TestVirtualStreamResolveFailureLogsRequestIDAndIdentity(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	pinned := "virtual://movie/tt-log-identity?result=pinned"
	row := &models.MediaFile{
		ID:                         401,
		ContentID:                  "movie-log-identity",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		ProviderVideoHash:          "hash-a",
	}
	h := &PlaybackHandler{
		fileResolver:                &fakePinFileResolver{file: row},
		VirtualFileLookup:           storedURLLookup(row),
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		sessionMgr:                  playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, errors.New("virtual playback provider returned an unsafe stream URL")
		}),
	}
	// forceRefresh skips the stored-URL shortcut so the provider resolve and its
	// failure warn are exercised; a fresh-start context declares not-session-bound.
	ctx := withVirtualSessionBindingV3(context.Background(), false)
	if _, _, err := h.resolveVirtualInputURI(ctx, pinned, 5, 1, "profile", true, nil, ""); err == nil {
		t.Fatal("resolve unexpectedly succeeded; the failure warn was not exercised")
	}
	out := logs.String()
	if !strings.Contains(out, `"request_id"`) {
		t.Fatalf("resolve-failure log missing request_id: %s", out)
	}
	if !strings.Contains(out, `"has_identity":true`) {
		t.Fatalf("resolve-failure log missing has_identity=true: %s", out)
	}
}
