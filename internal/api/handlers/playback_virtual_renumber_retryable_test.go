package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestResolveVirtualInputURIClassifiesTrustedRenumberRetryable proves #146: when
// a renumber cannot be re-selected because the trusted durable-identity pin does
// not re-match, the transport resolve surfaces the retryable provider_unavailable
// classification instead of a bare permanent resolve failure, so the client
// backs off and retries the release it picked rather than reporting a hard error.
func TestResolveVirtualInputURIClassifiesTrustedRenumberRetryable(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	pinned := "virtual://movie/tt-trusted-renumber?result=pinned"
	row := providerOutageRow(501, pinned)
	// No stored URL: force the provider resolve path so the renumber surfaces.
	row.ResolvedURL = ""

	h := &PlaybackHandler{
		fileResolver:                &fakePinFileResolver{file: row},
		VirtualFileLookup:           storedURLLookup(row),
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		sessionMgr:                  playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{}, fmt.Errorf("trusted persisted virtual candidate %q is no longer listed and candidate rotation was not requested: %w", "pinned", virtuallibrary.ErrPersistedCandidateTrusted)
		}),
	}
	// forceRefresh skips the stored-URL shortcut; sessionBound=false is the
	// fresh-start intent, and the row stays trusted via its durable identity.
	ctx := withVirtualSessionBindingV3(context.Background(), false)
	_, _, err := h.resolveVirtualInputURI(ctx, pinned, 5, 1, "profile", true, nil, "")
	if !errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("err = %v, want the retryable provider_unavailable classification", err)
	}
}
