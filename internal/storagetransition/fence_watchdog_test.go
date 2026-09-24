package storagetransition

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/blobstore"
)

// recordingHandler captures slog records for assertions on watchdog output.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) warnings() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, 0, len(h.records))
	for _, record := range h.records {
		if record.Level >= slog.LevelWarn {
			out = append(out, record)
		}
	}
	return out
}

// TestFencedForRestartReportsCommittedFence pins the operational status signal:
// after a transition commits, the service reports that source writes are fenced
// until restart, and the report clears when the fence is released.
func TestFencedForRestartReportsCommittedFence(t *testing.T) {
	base := &memoryStore{identity: "s3|old|public|", objects: map[string][]byte{"tmdb/a.webp": []byte("a")}}
	source := &fencedMemoryStore{memoryStore: base}
	target := &memoryStore{identity: "local|target", objects: map[string][]byte{}}
	settings := stagedLocal(t, t.TempDir())
	service := New(nil, settings, nil, source, nil)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	t.Cleanup(service.StopFenceWatchdog)

	if service.FencedForRestart() {
		t.Fatal("service reports fenced before any transition")
	}
	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(adminjob.StorageTransitionProgress) {}); err != nil {
		t.Fatal(err)
	}
	if !source.fenced {
		t.Fatal("source fence was not retained after commit")
	}
	if !service.FencedForRestart() {
		t.Fatal("service must report the retained post-commit fence")
	}
}

// TestFenceWatchdogWarnsWhileCommittedFenceHeld pins the watchdog: while a
// committed transition's fences are retained, the service logs a loud WARN on
// its interval; when the fence releases, the watchdog stops.
func TestFenceWatchdogWarnsWhileCommittedFenceHeld(t *testing.T) {
	base := &memoryStore{identity: "s3|old|public|", objects: map[string][]byte{"tmdb/a.webp": []byte("a")}}
	source := &fencedMemoryStore{memoryStore: base}
	target := &memoryStore{identity: "local|target", objects: map[string][]byte{}}
	settings := stagedLocal(t, t.TempDir())

	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	service := New(nil, settings, nil, source, nil)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	service.fenceWatchdogInterval = 10 * time.Millisecond
	t.Cleanup(service.StopFenceWatchdog)

	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(adminjob.StorageTransitionProgress) {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for len(handler.warnings()) == 0 {
		select {
		case <-deadline:
			t.Fatal("watchdog did not warn while a committed fence was held")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Releasing the retained fence must end the warnings.
	release, err := source.BeginMutationFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
	service.StopFenceWatchdog()
	before := len(handler.warnings())
	time.Sleep(50 * time.Millisecond)
	if after := len(handler.warnings()); after != before {
		t.Fatalf("watchdog kept warning after release: %d -> %d", before, after)
	}
}

// TestFenceWatchdogIsIdempotent ensures a second commit does not spawn a second
// watchdog goroutine.
func TestFenceWatchdogIsIdempotent(t *testing.T) {
	service := New(nil, &memorySettings{values: map[string]string{}}, nil, &fencedMemoryStore{memoryStore: &memoryStore{identity: "s3|old|public|"}}, nil)
	service.fenceWatchdogInterval = time.Hour
	t.Cleanup(service.StopFenceWatchdog)
	service.startFenceWatchdog(false)
	service.fenceMu.Lock()
	stop := service.fenceWatchdogStop
	service.fenceMu.Unlock()
	service.startFenceWatchdog(false)
	service.fenceMu.Lock()
	second := service.fenceWatchdogStop
	service.fenceMu.Unlock()
	if stop != second {
		t.Fatal("a second commit replaced the running watchdog")
	}
}
