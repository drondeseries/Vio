package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestVirtualTransportFreshStartRenumberReselects proves #142: a fresh
// transport start (no session binding yet) declares the resolve
// not-session-bound, so a pinned id absent from a renumbered provider listing
// falls through to a live sibling instead of returning
// ErrSessionBoundCandidateAbsent and failing the start with a permanent error.
//
// The row deliberately carries no durable provider identity, so it cannot
// same-release re-match by identity; the only recovery is the fresh-selection
// fall-through that sessionBound=false authorizes.
func TestVirtualTransportFreshStartRenumberReselects(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-fresh-renumber"
	pinned := neutral + "?result=pinned"
	sibling := neutral + "?result=sibling"
	delivered := time.Now().Add(-time.Minute)
	row := &models.MediaFile{
		ID:                         301,
		ContentID:                  "movie-fresh-renumber",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		UpdatedAt:                  time.Now(),
		LastDeliveredAt:            &delivered,
	}

	var binds []bool
	h := &PlaybackHandler{
		fileResolver:                &fakePinFileResolver{file: row},
		VirtualFileLookup:           storedURLLookup(row),
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		sessionMgr:                  playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			bound := VirtualSessionBinding(ctx)
			binds = append(binds, bound)
			if bound {
				// A session-bound resolve must refuse the renumber; this is the
				// behavior that must not fire for a fresh start.
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling.mp4", URI: sibling, CandidateID: "sibling"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      row.ID,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "fresh-renumber",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("fresh start failed on a provider renumber: %v", err)
	}
	defer func() { _ = session.Close() }()
	if len(binds) == 0 || binds[0] {
		t.Fatalf("fresh-start resolve declared sessionBound; binds=%v, want the first resolve not session-bound", binds)
	}
}
