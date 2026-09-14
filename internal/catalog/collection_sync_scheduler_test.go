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

	// The scheduler serializes callbacks (one snapshot per finished collection,
	// delivered while holding the result mutex), so a plain append is safe and
	// -race validates that contract.
	var snapshots []CollectionSyncProgress
	data, err := s.RunOnce(context.Background(), func(p CollectionSyncProgress) {
		snapshots = append(snapshots, p)
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

	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %d, want initial + one per collection", len(snapshots))
	}
	if snapshots[0].Due != 3 || snapshots[0].Completed != 0 {
		t.Fatalf("initial snapshot = %+v, want due=3 completed=0", snapshots[0])
	}
	// Every collection after the initial snapshot must appear exactly once,
	// with a strictly increasing completed count and its own title.
	seen := map[string]bool{}
	for i, snap := range snapshots[1:] {
		if snap.Completed != i+1 {
			t.Fatalf("snapshot %d completed = %d, want %d", i+1, snap.Completed, i+1)
		}
		if snap.CurrentTitle == "" || snap.CurrentID == "" {
			t.Fatalf("snapshot %d missing collection identity: %+v", i+1, snap)
		}
		if seen[snap.CurrentID] {
			t.Fatalf("collection %q reported twice", snap.CurrentID)
		}
		seen[snap.CurrentID] = true
	}
	for _, want := range []string{"Alpha", "Bravo", "Charlie"} {
		if !containsTitle(snapshots[1:], want) {
			t.Fatalf("missing progress title %q in %+v", want, snapshots[1:])
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
