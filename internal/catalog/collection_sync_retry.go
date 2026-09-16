package catalog

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Silo-Server/silo-server/internal/models"
)

// collectionSyncMaxAttempts bounds a whole-collection-sync retry. The accept
// transaction rolls back atomically on failure, so retrying the whole sync
// observes the pre-attempt state.
const collectionSyncMaxAttempts = 3

// Backoff parameters for a retryable collection sync failure. Package vars
// (not consts) so tests can shrink them; production code never mutates them.
var (
	collectionSyncRetryBaseBackoff = 50 * time.Millisecond
	collectionSyncRetryMaxBackoff  = 2 * time.Second
)

// retryableCollectionSyncCodes are the SQLSTATEs that can surface from an
// advisory-lock-exhausted accept and are safe to retry: out of shared memory
// (53200), lock not available (55P03), deadlock (40P01), and serialization
// failure (40001).
var retryableCollectionSyncCodes = map[string]struct{}{
	"53200": {},
	"55P03": {},
	"40P01": {},
	"40001": {},
}

// isRetryableCollectionSyncError reports whether a failed collection sync may
// succeed on retry.
func isRetryableCollectionSyncError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	_, ok := retryableCollectionSyncCodes[pgErr.Code]
	return ok
}

// retryCollectionSync runs a whole collection sync up to
// collectionSyncMaxAttempts with exponential backoff and full jitter when the
// failure is a transient Postgres condition. It never retries inside
// AcceptPreparedItems' transaction: that transaction is idempotent through
// rollback, so a fresh whole-sync attempt is the safe retry unit. Context
// cancellation between attempts ends the retry loop.
func retryCollectionSync(ctx context.Context, sync func() (*models.LibraryCollectionSyncRun, error)) (*models.LibraryCollectionSyncRun, error) {
	var (
		run *models.LibraryCollectionSyncRun
		err error
	)
	for attempt := 1; attempt <= collectionSyncMaxAttempts; attempt++ {
		run, err = sync()
		if err == nil || !isRetryableCollectionSyncError(err) {
			return run, err
		}
		if attempt == collectionSyncMaxAttempts || ctx.Err() != nil {
			return run, err
		}
		delay := collectionSyncRetryBackoff(attempt)
		slog.WarnContext(ctx, "collection sync: retrying after retryable database failure",
			"component", "catalog",
			"attempt", attempt,
			"max_attempts", collectionSyncMaxAttempts,
			"delay", delay,
			"error", err,
		)
		select {
		case <-ctx.Done():
			return run, err
		case <-time.After(delay):
		}
	}
	return run, err
}

// collectionSyncRetryBackoff returns the full-jitter delay before attempt+1:
// a uniform draw from [0, min(max, base*2^(attempt-1))].
func collectionSyncRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	ceiling := collectionSyncRetryBaseBackoff
	for i := 1; i < attempt && ceiling < collectionSyncRetryMaxBackoff; i++ {
		ceiling *= 2
	}
	if ceiling > collectionSyncRetryMaxBackoff {
		ceiling = collectionSyncRetryMaxBackoff
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}
