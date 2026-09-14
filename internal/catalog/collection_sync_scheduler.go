package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Silo-Server/silo-server/internal/models"
)

// defaultCollectionSyncTimeout bounds a single collection's sync so one
// stalled collection cannot hold the scheduler (and its in-flight guard) open
// indefinitely. Overridable in tests via the scheduler field.
const defaultCollectionSyncTimeout = 15 * time.Minute

// CollectionSyncProgress is a per-collection progress snapshot emitted by
// RunOnce. Due is the number of collections due in this pass; Completed counts
// the collections that have finished (synced, failed, or skipped).
// CurrentID/CurrentTitle identify the collection that just finished.
type CollectionSyncProgress struct {
	Due          int
	Completed    int
	CurrentID    string
	CurrentTitle string
}

// collectionSyncDueLister is the repository surface the scheduler needs. The
// concrete *LibraryCollectionRepository satisfies it; the interface exists so
// scheduler tests can exercise scheduling and progress without a database.
type collectionSyncDueLister interface {
	ListDueForSync(ctx context.Context) ([]*models.LibraryCollection, error)
	UpdateNextSyncAt(ctx context.Context, id string, next *time.Time) error
}

// collectionSyncer syncs a single collection. The concrete
// *LibraryCollectionService satisfies it.
type collectionSyncer interface {
	SyncCollection(ctx context.Context, collectionID string) (*models.LibraryCollectionSyncRun, error)
}

// CollectionSyncScheduler finds collections due for automatic sync and
// processes them with bounded concurrency. It is driven by a TaskManager
// task on a short interval (e.g., every 5 minutes).
type CollectionSyncScheduler struct {
	repo    collectionSyncDueLister
	service collectionSyncer
	logger  *slog.Logger

	// syncTimeout bounds a single collection's sync. Defaults to
	// defaultCollectionSyncTimeout.
	syncTimeout time.Duration

	// inFlight tracks collection IDs currently being synced to prevent
	// concurrent syncs of the same collection (manual vs scheduled).
	inFlight sync.Map
}

// CollectionSyncResult is the JSON summary attached to the task execution.
type CollectionSyncResult struct {
	Due     int `json:"due"`
	Synced  int `json:"synced"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// NewCollectionSyncScheduler creates a new scheduler.
func NewCollectionSyncScheduler(
	repo *LibraryCollectionRepository,
	service *LibraryCollectionService,
	logger *slog.Logger,
) *CollectionSyncScheduler {
	return &CollectionSyncScheduler{
		repo:        repo,
		service:     service,
		logger:      logger,
		syncTimeout: defaultCollectionSyncTimeout,
	}
}

// RunOnce queries for due collections and syncs them with bounded concurrency.
// It returns a JSON summary suitable for task result data. onProgress, when
// non-nil, receives one snapshot when the due set is known and one after each
// collection finishes; it is never called simultaneously.
func (s *CollectionSyncScheduler) RunOnce(ctx context.Context, onProgress func(CollectionSyncProgress)) (json.RawMessage, error) {
	due, err := s.repo.ListDueForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing due collections: %w", err)
	}

	if len(due) == 0 {
		return marshalResult(CollectionSyncResult{}), nil
	}

	s.logger.InfoContext(ctx, "collection sync scheduler: starting",
		"due", len(due),
	)

	if onProgress != nil {
		onProgress(CollectionSyncProgress{Due: len(due)})
	}

	var (
		mu      sync.Mutex
		result  = CollectionSyncResult{Due: len(due)}
		g, gctx = errgroup.WithContext(ctx)
	)
	g.SetLimit(3)

	for _, collection := range due {
		collection := collection

		g.Go(func() error {
			s.syncOne(gctx, collection, &mu, &result, onProgress)
			return nil // never propagate; failures are per-collection
		})
	}

	_ = g.Wait()

	s.logger.InfoContext(ctx, "collection sync scheduler: complete",
		"due", result.Due,
		"synced", result.Synced,
		"failed", result.Failed,
		"skipped", result.Skipped,
	)

	return marshalResult(result), nil
}

// syncOne syncs a single collection and advances its next_sync_at.
func (s *CollectionSyncScheduler) syncOne(ctx context.Context, collection *models.LibraryCollection, mu *sync.Mutex, result *CollectionSyncResult, onProgress func(CollectionSyncProgress)) {
	// Guard against concurrent sync of the same collection (e.g., manual trigger).
	if _, loaded := s.inFlight.LoadOrStore(collection.ID, struct{}{}); loaded {
		s.logger.InfoContext(ctx, "collection sync scheduler: skipping (already in flight)",
			"collection_id", collection.ID,
			"title", collection.Title,
		)
		mu.Lock()
		result.Skipped++
		s.reportProgressLocked(result, collection, onProgress)
		mu.Unlock()
		return
	}
	defer s.inFlight.Delete(collection.ID)

	startedAt := time.Now()

	timeout := s.syncTimeout
	if timeout <= 0 {
		timeout = defaultCollectionSyncTimeout
	}
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	_, syncErr := s.service.SyncCollection(syncCtx, collection.ID)
	cancel()

	completedAt := time.Now()

	// Always advance next_sync_at, even on failure, so we retry on the
	// natural cron schedule rather than every poll interval.
	if collection.SyncSchedule != nil {
		next := ComputeNextSyncAtFrom(*collection.SyncSchedule, completedAt)
		if err := s.repo.UpdateNextSyncAt(ctx, collection.ID, next); err != nil {
			s.logger.ErrorContext(ctx, "collection sync scheduler: failed to advance schedule",
				"collection_id", collection.ID,
				"error", err,
			)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	switch {
	case syncErr != nil && errors.Is(syncErr, context.DeadlineExceeded):
		result.Failed++
		s.logger.ErrorContext(ctx, "collection sync scheduler: sync deadline exceeded",
			"collection_id", collection.ID,
			"title", collection.Title,
			"timeout", timeout,
			"duration", completedAt.Sub(startedAt).Round(time.Millisecond),
			"error", syncErr,
		)
	case syncErr != nil:
		result.Failed++
		s.logger.ErrorContext(ctx, "collection sync scheduler: sync failed",
			"collection_id", collection.ID,
			"title", collection.Title,
			"duration", completedAt.Sub(startedAt).Round(time.Millisecond),
			"error", syncErr,
		)
	default:
		result.Synced++
		s.logger.InfoContext(ctx, "collection sync scheduler: synced",
			"collection_id", collection.ID,
			"title", collection.Title,
			"duration", completedAt.Sub(startedAt).Round(time.Millisecond),
		)
	}

	s.reportProgressLocked(result, collection, onProgress)
}

// reportProgressLocked emits a progress snapshot. Callers must hold the
// result mutex so completed counts and callback delivery stay serialized.
func (s *CollectionSyncScheduler) reportProgressLocked(result *CollectionSyncResult, collection *models.LibraryCollection, onProgress func(CollectionSyncProgress)) {
	if onProgress == nil {
		return
	}
	onProgress(CollectionSyncProgress{
		Due:          result.Due,
		Completed:    result.Synced + result.Failed + result.Skipped,
		CurrentID:    collection.ID,
		CurrentTitle: collection.Title,
	})
}

// IsInFlight returns true if the given collection is currently being synced
// by the scheduler. Used by the manual sync handler to avoid overlap.
func (s *CollectionSyncScheduler) IsInFlight(collectionID string) bool {
	_, ok := s.inFlight.Load(collectionID)
	return ok
}

func marshalResult(r CollectionSyncResult) json.RawMessage {
	data, _ := json.Marshal(r)
	return data
}
