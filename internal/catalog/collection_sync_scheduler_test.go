package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

type fakeCollectionSyncRepo struct {
	due []*models.LibraryCollection
	err error

	mu        sync.Mutex
	advanced  map[string]time.Time
	advanceEr error
}

func (f *fakeCollectionSyncRepo) ListDueForSync(context.Context) ([]*models.LibraryCollection, error) {
	return f.due, f.err
}

func (f *fakeCollectionSyncRepo) UpdateNextSyncAt(_ context.Context, id string, next *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.advanced == nil {
		f.advanced = map[string]time.Time{}
	}
	if next != nil {
		f.advanced[id] = *next
	}
	return f.advanceEr
}

func (f *fakeCollectionSyncRepo) advancedAt(id string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.advanced[id]
	return at, ok
}

type fakeCollectionSyncer struct {
	fn func(ctx context.Context, collectionID string) (*models.LibraryCollectionSyncRun, error)
}

func (f *fakeCollectionSyncer) SyncCollection(ctx context.Context, collectionID string) (*models.LibraryCollectionSyncRun, error) {
	return f.fn(ctx, collectionID)
}

func scheduledCollection(id, title string) *models.LibraryCollection {
	schedule := "0 * * * *"
	return &models.LibraryCollection{ID: id, Title: title, SyncSchedule: &schedule}
}

func newTestScheduler(repo collectionSyncDueLister, syncer collectionSyncer) *CollectionSyncScheduler {
	return &CollectionSyncScheduler{
		repo:        repo,
		service:     syncer,
		logger:      slog.New(slog.DiscardHandler),
		syncTimeout: time.Minute,
	}
}

func TestCollectionSyncSchedulerRunOnceNoDue(t *testing.T) {
	repo := &fakeCollectionSyncRepo{}
	syncer := &fakeCollectionSyncer{fn: func(context.Context, string) (*models.LibraryCollectionSyncRun, error) {
		t.Fatal("sync must not run when nothing is due")
		return nil, nil
	}}
	s := newTestScheduler(repo, syncer)

	var progress []CollectionSyncProgress
	data, err := s.RunOnce(context.Background(), func(p CollectionSyncProgress) { progress = append(progress, p) })
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var result CollectionSyncResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Due != 0 {
		t.Fatalf("result = %+v, want zero", result)
	}
	if len(progress) != 0 {
		t.Fatalf("progress = %+v, want no callback for an empty due set", progress)
	}
}

func TestCollectionSyncSchedulerDeadlineCountsFailedAndAdvances(t *testing.T) {
	repo := &fakeCollectionSyncRepo{due: []*models.LibraryCollection{scheduledCollection("stalled", "Stalled")}}
	syncer := &fakeCollectionSyncer{fn: func(ctx context.Context, _ string) (*models.LibraryCollectionSyncRun, error) {
		if _, ok := ctx.Deadline(); !ok {
			// The scheduler must bound each collection with a deadline; without
			// one a stalled provider would hang the pass forever.
			return &models.LibraryCollectionSyncRun{}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := newTestScheduler(repo, syncer)
	s.syncTimeout = 30 * time.Millisecond

	data, err := s.RunOnce(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var result CollectionSyncResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Due != 1 || result.Failed != 1 || result.Synced != 0 {
		t.Fatalf("result = %+v, want due=1 failed=1 synced=0", result)
	}
	if _, ok := repo.advancedAt("stalled"); !ok {
		t.Fatal("next_sync_at was not advanced after a deadline failure")
	}
}

func TestCollectionSyncSchedulerSyncErrorCountsFailedAndAdvances(t *testing.T) {
	repo := &fakeCollectionSyncRepo{due: []*models.LibraryCollection{scheduledCollection("broken", "Broken")}}
	syncer := &fakeCollectionSyncer{fn: func(context.Context, string) (*models.LibraryCollectionSyncRun, error) {
		return nil, errors.New("provider down")
	}}
	s := newTestScheduler(repo, syncer)

	data, err := s.RunOnce(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var result CollectionSyncResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want failed=1", result)
	}
	if _, ok := repo.advancedAt("broken"); !ok {
		t.Fatal("next_sync_at was not advanced after a plain failure")
	}
}

func TestCollectionSyncSchedulerProgressSequence(t *testing.T) {
	repo := &fakeCollectionSyncRepo{due: []*models.LibraryCollection{
		scheduledCollection("a", "Alpha"),
		scheduledCollection("b", "Bravo"),
		scheduledCollection("c", "Charlie"),
	}}
	syncer := &fakeCollectionSyncer{fn: func(context.Context, string) (*models.LibraryCollectionSyncRun, error) {
		return &models.LibraryCollectionSyncRun{}, nil
	}}
	s := newTestScheduler(repo, syncer)

	// Callbacks are delivered outside the accounting mutex (F6), so a slow
	// observer cannot serialize counters; delivery order across concurrently
	// finishing collections is therefore not guaranteed. The callback is still
	// invoked from distinct goroutines, so guard the collector with its own
	// mutex.
	var (
		snapMu    sync.Mutex
		snapshots []CollectionSyncProgress
	)
	data, err := s.RunOnce(context.Background(), func(p CollectionSyncProgress) {
		snapMu.Lock()
		snapshots = append(snapshots, p)
		snapMu.Unlock()
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var result CollectionSyncResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Synced != 3 {
		t.Fatalf("result = %+v, want synced=3", result)
	}

	snapMu.Lock()
	defer snapMu.Unlock()
	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %d, want initial + one per collection", len(snapshots))
	}
	// Exactly one initial snapshot.
	initial := 0
	for _, snap := range snapshots {
		if snap.Completed == 0 {
			initial++
			if snap.Due != 3 {
				t.Fatalf("initial snapshot = %+v, want due=3 completed=0", snap)
			}
		}
	}
	if initial != 1 {
		t.Fatalf("initial snapshots = %d, want exactly 1", initial)
	}
	// Every finished collection appears exactly once, and the completed counts
	// cover 1..3 exactly.
	seen := map[string]bool{}
	counts := map[int]int{}
	for _, snap := range snapshots {
		if snap.Completed == 0 {
			continue
		}
		if snap.CurrentTitle == "" || snap.CurrentID == "" {
			t.Fatalf("snapshot missing collection identity: %+v", snap)
		}
		if seen[snap.CurrentID] {
			t.Fatalf("collection %q reported twice", snap.CurrentID)
		}
		seen[snap.CurrentID] = true
		counts[snap.Completed]++
	}
	for n := 1; n <= 3; n++ {
		if counts[n] != 1 {
			t.Fatalf("completed=%d observed %d times, want exactly once", n, counts[n])
		}
	}
	for _, want := range []string{"Alpha", "Bravo", "Charlie"} {
		if !containsTitle(snapshots, want) {
			t.Fatalf("missing progress title %q in %+v", want, snapshots)
		}
	}
}

func containsTitle(snapshots []CollectionSyncProgress, title string) bool {
	for _, snap := range snapshots {
		if snap.CurrentTitle == title {
			return true
		}
	}
	return false
}

func TestCollectionSyncSchedulerProgressCallbackOutsideLock(t *testing.T) {
	// F6: the callback must run after accounting releases the result mutex, so
	// a callback that takes the same mutex cannot deadlock.
	repo := &fakeCollectionSyncRepo{due: []*models.LibraryCollection{scheduledCollection("a", "Alpha")}}
	syncer := &fakeCollectionSyncer{fn: func(context.Context, string) (*models.LibraryCollectionSyncRun, error) {
		return &models.LibraryCollectionSyncRun{}, nil
	}}
	s := newTestScheduler(repo, syncer)

	mu := &sync.Mutex{}
	result := &CollectionSyncResult{Due: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.syncOne(context.Background(), repo.due[0], mu, result, func(CollectionSyncProgress) {
			// If the callback ran under mu, this would deadlock.
			mu.Lock()
			mu.Unlock()
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncOne did not finish; the callback likely ran while holding the mutex")
	}
	if result.Synced != 1 {
		t.Fatalf("result = %+v, want synced=1", result)
	}
}

func TestCollectionSyncSchedulerNilProgressCallbackIsSafe(t *testing.T) {
	repo := &fakeCollectionSyncRepo{due: []*models.LibraryCollection{scheduledCollection("a", "Alpha")}}
	syncer := &fakeCollectionSyncer{fn: func(context.Context, string) (*models.LibraryCollectionSyncRun, error) {
		return &models.LibraryCollectionSyncRun{}, nil
	}}
	s := newTestScheduler(repo, syncer)

	if _, err := s.RunOnce(context.Background(), nil); err != nil {
		t.Fatalf("RunOnce with nil callback: %v", err)
	}
}
