package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// errVirtualProviderUnavailable marks a resolve that failed because the
// provider's listing was transiently unavailable (empty answer, request
// failure, or 5XX) for a session-bound candidate the catalog still trusts. It
// is deliberately a retryable dependency condition, not a verdict about the
// release: the serve layer answers 503 provider_unavailable so hls.js keeps
// retrying the same pinned release instead of rotating to a sibling.
var errVirtualProviderUnavailable = errors.New("virtual provider listing temporarily unavailable")

// virtualProviderOutageBackoff is the bounded wait schedule for a transient
// provider-listing failure on a session-bound candidate the catalog still
// trusts. Two retries add at most ~3s of latency, well under the serve and
// transport-startup budgets, and every wait observes request cancellation.
var virtualProviderOutageBackoff = []time.Duration{1 * time.Second, 2 * time.Second}

// virtualProviderListingOutage reports whether a virtual resolve failure is a
// transient provider-listing outage rather than a verdict about the release.
// Only the causes a live provider outage produces are named:
//   - resolver.ErrProviderUnavailable: the provider request failed or answered
//     5XX;
//   - ErrPersistedCandidateTrusted: the trusted persisted same-identity
//     candidate was absent from an empty/partial answer (the resolver refuses
//     to substitute, which is exactly the case that should be retried, not
//     rotated);
//   - the "no streams available from provider" message: an empty answer that
//     carried no pinned-candidate refusal (e.g. a neutral re-list).
//
// A genuine different-release substitution or an untrusted dead pin never
// reaches here because callers gate the retry on the trust predicate below.
func virtualProviderListingOutage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, resolver.ErrProviderUnavailable) {
		return true
	}
	if errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		return true
	}
	return strings.Contains(err.Error(), "no streams available from provider")
}

// virtualCandidateTrustedForOutageRetry reports whether a catalog row is the
// session-bound candidate a transient provider outage should be retried for:
// it carries durable provider identity plus transport evidence (a stored
// resolved URL or a recorded delivery) and is inside the candidate store
// window. A row with no evidence, no identity, or a lapsed window keeps the
// pre-existing behavior: the outage is reported, not retried.
func (h *PlaybackHandler) virtualCandidateTrustedForOutageRetry(row *models.MediaFile) bool {
	if h == nil {
		return false
	}
	return virtualCandidateTrustedForOutageRetry(row, h.virtualCandidateTrustWindow())
}

// virtualCandidateTrustedForOutageRetry is the StreamHandler counterpart of the
// playback handler's trust predicate. The window is read from the handler so
// both share one implementation.
func (h *StreamHandler) virtualCandidateTrustedForOutageRetry(row *models.MediaFile) bool {
	if h == nil {
		return false
	}
	return virtualCandidateTrustedForOutageRetry(row, h.virtualStoredURLTrustWindow())
}

func virtualCandidateTrustedForOutageRetry(row *models.MediaFile, window time.Duration) bool {
	if row == nil || window <= 0 {
		return false
	}
	if _, ok := persistedVirtualIdentity(row); !ok {
		return false
	}
	if strings.TrimSpace(row.ResolvedURL) == "" && row.LastDeliveredAt == nil {
		return false
	}
	return virtualCandidateWithinStoreWindow(row, time.Now(), window)
}

// retryVirtualProviderOutageResolve runs resolve, and while it fails with a
// transient provider-listing outage for a trusted session-bound row, retries up
// to len(virtualProviderOutageBackoff) times with the bounded schedule. A
// canceled request stops immediately. Retry attempts are passed relist=true so
// the resolve closure forces a genuine re-list past the negative cache the
// outage just wrote, and marks the service resolve as an outage re-list. The
// stored-first resolve stays the first attempt, so a healthy persisted URL
// still wins with zero provider calls and a transient outage never swaps the
// viewer's release.
func retryVirtualProviderOutageResolve(
	ctx context.Context,
	trusted bool,
	resolve func(context.Context, bool) (ResolvedVirtualMedia, error),
) (ResolvedVirtualMedia, error) {
	resolved, err := resolve(ctx, false)
	if !trusted {
		return resolved, err
	}
	for attempt := 0; err != nil && virtualProviderListingOutage(err) && attempt < len(virtualProviderOutageBackoff); attempt++ {
		wait := virtualProviderOutageBackoff[attempt]
		if !sleepWithContext(ctx, wait) {
			return resolved, err
		}
		slog.InfoContext(ctx, "virtual provider listing unavailable; retrying the session-bound candidate",
			"component", "api", "attempt", attempt+1, "retry_in_ms", wait.Milliseconds(), "error", err)
		resolved, err = resolve(ctx, true)
	}
	return resolved, err
}

// classifyVirtualProviderOutage wraps a final provider outage so the serve
// layer can answer 503 provider_unavailable instead of a permanent resolve
// error. It is a no-op for a non-outage error or an untrusted row.
func classifyVirtualProviderOutage(err error, trusted bool) error {
	if err == nil || !trusted || !virtualProviderListingOutage(err) {
		return err
	}
	return fmt.Errorf("%w: %w", errVirtualProviderUnavailable, err)
}

// sleepWithContext waits for d or returns false when ctx is canceled first.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
