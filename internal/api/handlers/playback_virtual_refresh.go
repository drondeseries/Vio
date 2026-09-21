package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/database/pglock"
	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
)

// VirtualCandidateRefreshSource lists candidate rows whose signed stored URL is
// approaching expiry and inside the candidate store window. It is wired to the
// scanner repository query that applies the selection rules; the refresher
// itself does not touch the database.
type VirtualCandidateRefreshSource func(ctx context.Context, window, lead time.Duration, limit int) ([]*models.MediaFile, error)

const (
	// virtualCandidateRefreshInterval is the cadence of the background pass. A
	// signed provider URL typically lives hours to days; half-hourly is frequent
	// enough to catch a URL inside the lead window without making the pass a
	// meaningful background load.
	virtualCandidateRefreshInterval = 30 * time.Minute
	// virtualCandidateRefreshLead is how close to expiry a URL must be before
	// the pass pre-warms it. Two hours bounds the waste: a multi-day URL is
	// touched only near its end, while a URL shorter than the lead still gets
	// several attempts (and is protected by the failure cooldown).
	virtualCandidateRefreshLead = 2 * time.Hour
	// virtualCandidateRefreshBatch caps one pass. Twenty resolutions every
	// thirty minutes is a small, predictable provider load even on a busy
	// catalog.
	virtualCandidateRefreshBatch = 20
	// virtualCandidateRefreshFailureCooldown is the per-row backoff after a
	// failed refresh: a dead provider or a dropped candidate is retried at most
	// hourly instead of every pass.
	virtualCandidateRefreshFailureCooldown = time.Hour
	// virtualCandidateRefreshSpacing/Jitter spread a pass's provider calls so a
	// batch does not arrive as a burst.
	virtualCandidateRefreshSpacing = 150 * time.Millisecond
	virtualCandidateRefreshJitter  = 150 * time.Millisecond
	// virtualCandidateRefreshResolveTimeout bounds one candidate's provider
	// resolve so a hung provider cannot stall the whole pass (or the ticker).
	virtualCandidateRefreshResolveTimeout = 30 * time.Second
	// virtualCandidateRefreshLock is the cross-replica advisory lock key
	// ("vio_vcref"), so only one replica runs a pass at a time.
	virtualCandidateRefreshLock int64 = 0x76696f5f76637266
)

// VirtualCandidateRefresher pre-warms signed candidate URLs that are close to
// expiry so a later resume serves a live URL instead of paying a provider
// round-trip. It is deliberately bounded: a small batch, spaced provider calls,
// a cross-replica lock, and a per-row failure cooldown. It only ever refreshes
// the exact persisted candidate through the existing CAS-fenced write, so a
// sibling or a rotated release can never be written onto a row. With the trust
// window disabled it is completely inert.
type VirtualCandidateRefresher struct {
	List          VirtualCandidateRefreshSource
	Resolver      VirtualMediaDetailedResolver
	MetadataSaver VirtualFileMetadataSaver
	Saver         VirtualFileSaver
	Window        func() time.Duration
	LockPool      *pgxpool.Pool
	Logger        *slog.Logger

	mu     sync.Mutex
	failed map[int]time.Time

	// Overridable timing for tests; New sets the production defaults.
	interval time.Duration
	lead     time.Duration
	batch    int
	spacing  time.Duration
	jitter   time.Duration
}

// NewVirtualCandidateRefresher builds the pass with production bounds. A nil
// logger selects slog.Default().
func NewVirtualCandidateRefresher(
	list VirtualCandidateRefreshSource,
	resolver VirtualMediaDetailedResolver,
	metadataSaver VirtualFileMetadataSaver,
	saver VirtualFileSaver,
	window func() time.Duration,
	lockPool *pgxpool.Pool,
	logger *slog.Logger,
) *VirtualCandidateRefresher {
	if logger == nil {
		logger = slog.Default()
	}
	return &VirtualCandidateRefresher{
		List:          list,
		Resolver:      resolver,
		MetadataSaver: metadataSaver,
		Saver:         saver,
		Window:        window,
		LockPool:      lockPool,
		Logger:        logger,
		failed:        map[int]time.Time{},
		interval:      virtualCandidateRefreshInterval,
		lead:          virtualCandidateRefreshLead,
		batch:         virtualCandidateRefreshBatch,
		spacing:       virtualCandidateRefreshSpacing,
		jitter:        virtualCandidateRefreshJitter,
	}
}

func (r *VirtualCandidateRefresher) logger() *slog.Logger {
	if r == nil || r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

func (r *VirtualCandidateRefresher) refreshInterval() time.Duration {
	if r == nil || r.interval <= 0 {
		return virtualCandidateRefreshInterval
	}
	return r.interval
}

func (r *VirtualCandidateRefresher) refreshLead() time.Duration {
	if r == nil || r.lead <= 0 {
		return virtualCandidateRefreshLead
	}
	return r.lead
}

func (r *VirtualCandidateRefresher) refreshBatch() int {
	if r == nil || r.batch <= 0 {
		return virtualCandidateRefreshBatch
	}
	return r.batch
}

// Run drives the pass until ctx is canceled. The first run and every tick are
// jittered so multiple replicas (and a restart) do not align their passes.
func (r *VirtualCandidateRefresher) Run(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	interval := r.refreshInterval()
	// First pass somewhere in the second half of the interval.
	delay := interval/2 + time.Duration(rand.Int64N(int64(interval/2)+1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.RunOnce(ctx)
			// ±1/6 interval of jitter per tick.
			jitter := interval/6 + 1
			next := interval - jitter + time.Duration(rand.Int64N(int64(2*jitter)))
			if next < time.Minute {
				next = time.Minute
			}
			timer.Reset(next)
		}
	}
}

// RunOnce performs at most one bounded pass and returns how many rows were
// refreshed. It is inert (returns 0) when the window is disabled, the source or
// resolver is unwired, or another replica holds the pass lock.
func (r *VirtualCandidateRefresher) RunOnce(ctx context.Context) int {
	if r == nil || r.List == nil || r.Resolver == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	window := time.Duration(0)
	if r.Window != nil {
		window = r.Window()
	}
	if window <= 0 {
		// The window is the trust horizon; with it disabled the pass is inert.
		return 0
	}
	if r.LockPool != nil {
		lock, acquired, err := pglock.TryAcquire(ctx, r.LockPool, virtualCandidateRefreshLock)
		if err != nil {
			r.logger().WarnContext(ctx, "virtual candidate refresh: pass lock failed", "component", "api", "error", err)
			return 0
		}
		if !acquired {
			return 0
		}
		defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	}
	rows, err := r.List(ctx, window, r.refreshLead(), r.refreshBatch())
	if err != nil {
		r.logger().WarnContext(ctx, "virtual candidate refresh: list failed", "component", "api", "error", err)
		return 0
	}
	if len(rows) > r.refreshBatch() {
		rows = rows[:r.refreshBatch()]
	}
	refreshed := 0
	now := time.Now()
	for i, row := range rows {
		if ctx.Err() != nil {
			break
		}
		if row == nil {
			continue
		}
		if r.rateLimited(row.ID, now) {
			continue
		}
		refreshedNow, err := r.refreshOne(ctx, row, window)
		if err != nil {
			r.markFailed(row.ID, now)
			r.logger().WarnContext(ctx, "virtual candidate refresh failed",
				"component", "api", "file_id", row.ID, "candidate_uri", row.FilePath,
				"error", logredact.SanitizeURLError(err))
		} else {
			r.clearFailed(row.ID)
			if refreshedNow {
				refreshed++
			}
		}
		if i < len(rows)-1 {
			time.Sleep(r.spacingFor())
		}
	}
	return refreshed
}

// spacingFor returns the inter-candidate delay with per-call jitter.
func (r *VirtualCandidateRefresher) spacingFor() time.Duration {
	if r == nil || r.spacing <= 0 && r.jitter <= 0 {
		return 0
	}
	spacing := r.spacing
	if r.jitter > 0 {
		spacing += time.Duration(rand.Int64N(int64(r.jitter) + 1))
	}
	return spacing
}

// refreshOne resolves the row's exact candidate and records the fresh URL and
// expiry through the existing CAS-fenced Phase-1 write. It never adopts a
// sibling or a renumbered identity: the refresher only refreshes what the row
// already names. It reports whether a write happened, so the pass count reflects
// actual refreshes and a handled-but-skipped row is not retried in a tight loop.
func (r *VirtualCandidateRefresher) refreshOne(ctx context.Context, row *models.MediaFile, window time.Duration) (bool, error) {
	candidateID := virtualResultCandidateID(row.FilePath)
	if candidateID == "" {
		return false, nil
	}
	// Session-bound so the resolver refuses a release swap, and trusted so an
	// in-window dropped candidate is refused rather than substituted.
	resolveCtx := withVirtualSessionBindingV3(ctx, true)
	resolveCtx = virtualResolveContextWithPersistedTrust(resolveCtx, row, time.Now(), window)
	resolveCtx, cancel := context.WithTimeout(resolveCtx, virtualCandidateRefreshResolveTimeout)
	defer cancel()
	res, err := r.Resolver.ResolveVirtualMediaDetailed(
		resolveCtx, row.FilePath, row.VirtualOwnerInstallationID, 0, "", true, nil, "",
	)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(res.URL) == "" {
		return false, fmt.Errorf("resolver returned an empty URL")
	}
	if res.IdentityRematched {
		// The provider renumbered the same release. Adopting the new path is
		// the on-demand rematch's job; the background pass only refreshes the
		// exact candidate id, so leave the row untouched.
		return false, nil
	}
	refreshStoredVirtualResolution(ctx, row, res, r.MetadataSaver, r.Saver)
	r.logger().InfoContext(ctx, "virtual candidate URL refreshed",
		"component", "api", "file_id", row.ID, "candidate_uri", row.FilePath,
		"candidate_id", candidateID, "status", "refreshed")
	return true, nil
}

func (r *VirtualCandidateRefresher) rateLimited(fileID int, now time.Time) bool {
	if r == nil || fileID <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.failed[fileID]
	return ok && now.Sub(last) < virtualCandidateRefreshFailureCooldown
}

func (r *VirtualCandidateRefresher) markFailed(fileID int, now time.Time) {
	if r == nil || fileID <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = map[int]time.Time{}
	}
	r.failed[fileID] = now
}

func (r *VirtualCandidateRefresher) clearFailed(fileID int) {
	if r == nil || fileID <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failed, fileID)
}
