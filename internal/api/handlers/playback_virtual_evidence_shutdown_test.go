package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// These tests drive PlaybackHandler.StopVirtualEvidence, the method the
// application shutdown sequence calls (wired in cmd/silo via
// api.Dependencies.RegisterShutdownFunc). The lifecycle watcher starts the same
// single-flight path, but it is only a safety net; the tests here do not cancel
// the service context, so they prove the application-facing entry point yields
// the documented terminal state on its own.

// TestEvidenceApplicationShutdownDrainsAcceptedWork is the graceful-shutdown
// regression: work accepted before shutdown is persisted when the application
// calls the stop method, the buffer ends empty with no in-flight work, and a
// later admission is rejected explicitly rather than queued for a worker that no
// longer exists.
func TestEvidenceApplicationShutdownDrainsAcceptedWork(t *testing.T) {
	var persisted sync.Map
	h := &PlaybackHandler{
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			persisted.Store(args.FileID, struct{}{})
			return 1, nil
		},
	}
	buf := h.evidenceBuffer() // starts the real worker pool

	const count = 64
	base := time.Now()
	for i := 1; i <= count; i++ {
		if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: i, UpdatedAt: base}); got != virtualEvidenceAccepted {
			t.Fatalf("enqueue file %d = %v, want accepted", i, got)
		}
	}

	// This is the application shutdown path.
	h.StopVirtualEvidence()

	if n := buf.len(); n != 0 {
		t.Fatalf("pending after application shutdown = %d, want 0", n)
	}
	if n := buf.inflightCount(); n != 0 {
		t.Fatalf("in-flight after application shutdown = %d, want 0", n)
	}
	for i := 1; i <= count; i++ {
		if _, ok := persisted.Load(i); !ok {
			t.Fatalf("accepted evidence for file %d was not persisted by application shutdown", i)
		}
	}
	if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 1_000_000}); got != virtualEvidenceRejected {
		t.Fatalf("admission after application shutdown = %v, want rejected", got)
	}
}

// TestEvidenceApplicationShutdownRacesAdmission pins the atomic
// closure/admission rule through the public stop method: producers admit while
// shutdown starts, every admission that was accepted is persisted, and every
// admission that lost the race is reported as rejected.
func TestEvidenceApplicationShutdownRacesAdmission(t *testing.T) {
	var persisted sync.Map
	h := &PlaybackHandler{
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			persisted.Store(args.FileID, struct{}{})
			return 1, nil
		},
	}
	buf := h.evidenceBuffer()

	const producers = 4
	firstAdmitted := make(chan struct{}, producers)
	var acceptedMu sync.Mutex
	accepted := map[int]struct{}{}
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			first := true
			for i := 0; ; i++ {
				fileID := base*1_000_000 + i + 1
				switch h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: fileID, UpdatedAt: time.Now()}) {
				case virtualEvidenceAccepted:
					acceptedMu.Lock()
					accepted[fileID] = struct{}{}
					acceptedMu.Unlock()
					if first {
						first = false
						firstAdmitted <- struct{}{}
					}
				case virtualEvidenceRejected:
					// Shutdown won the admission race for this producer.
					return
				}
			}
		}(p)
	}
	// Wait until every producer has admitted at least once, so the stop below
	// races live admission rather than running before the producers start.
	for i := 0; i < producers; i++ {
		select {
		case <-firstAdmitted:
		case <-time.After(5 * time.Second):
			t.Fatal("producers never admitted; test did not exercise the race")
		}
	}
	h.StopVirtualEvidence()
	wg.Wait()

	if n := buf.len(); n != 0 {
		t.Fatalf("pending after shutdown = %d, want 0", n)
	}
	if n := buf.inflightCount(); n != 0 {
		t.Fatalf("in-flight after shutdown = %d, want 0", n)
	}
	if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 999_999_999}); got != virtualEvidenceRejected {
		t.Fatalf("admission after shutdown = %v, want rejected", got)
	}

	acceptedMu.Lock()
	defer acceptedMu.Unlock()
	if len(accepted) == 0 {
		t.Fatal("no admissions landed before shutdown; test did not exercise the race")
	}
	for fileID := range accepted {
		if _, ok := persisted.Load(fileID); !ok {
			t.Fatalf("accepted evidence for file %d was not persisted by shutdown", fileID)
		}
	}
}

// TestEvidenceApplicationShutdownIsIdempotentAndConcurrent pins that the stop
// method is safe to call repeatedly and from several goroutines during
// shutdown: every accepted write is persisted exactly once and no caller
// deadlocks waiting on a second drain.
func TestEvidenceApplicationShutdownIsIdempotentAndConcurrent(t *testing.T) {
	var saved int64
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			return atomic.AddInt64(&saved, 1), nil
		},
	}
	h.evidenceBuffer()
	base := time.Now()
	for i := 1; i <= 5; i++ {
		if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: i, UpdatedAt: base}); got != virtualEvidenceAccepted {
			t.Fatalf("enqueue file %d = %v, want accepted", i, got)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.StopVirtualEvidence()
		}()
	}
	wg.Wait()
	h.StopVirtualEvidence() // sequential repeat after the pool has drained

	if got := atomic.LoadInt64(&saved); got != 5 {
		t.Fatalf("persisted %d writes, want 5 (each accepted write exactly once)", got)
	}
}

// TestEvidenceApplicationShutdownExpiredBudgetAbandonsAndWarns pins the forced
// termination semantics: when the drain budget is already exhausted, the stop
// method does not hang and does not persist what it cannot reach. It abandons
// the still-queued in-memory work with a warning instead. The work is evidence,
// not data: a later restart re-probes it.
func TestEvidenceApplicationShutdownExpiredBudgetAbandonsAndWarns(t *testing.T) {
	var persisted int64
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			atomic.AddInt64(&persisted, 1)
			return 1, nil
		},
	}
	// Build the buffer without starting workers, so the drain is the only path
	// that could touch the queued work.
	buf := newVirtualEvidenceBuffer(8)
	buf.drainGrace = -time.Second // already-expired drain budget
	h.virtualEvidenceBuffer = buf
	base := time.Now()
	for i := 1; i <= 3; i++ {
		if got := buf.admit(evidenceTask(i, "path", base)); got != virtualEvidenceAccepted {
			t.Fatalf("admit %d = %v, want accepted", i, got)
		}
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.StopVirtualEvidence()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop with an expired drain budget hung instead of abandoning")
	}

	if got := atomic.LoadInt64(&persisted); got != 0 {
		t.Fatalf("expired-budget drain persisted %d writes, want 0", got)
	}
	if n := buf.len(); n != 3 {
		t.Fatalf("abandoned pending = %d, want the 3 still-queued writes", n)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, "virtual probe evidence abandoned at shutdown") {
		t.Fatalf("no abandonment warning was logged; got %q", logOutput)
	}
}

// TestEvidenceRestartReprobeReAdmits pins the documented non-durability: a
// restart builds a fresh, empty buffer and does not inherit the previous
// process's in-memory evidence. Recovery is a new probe that re-admits the same
// file, and that fresh admission persists again — nothing is implied to survive
// process death.
func TestEvidenceRestartReprobeReAdmits(t *testing.T) {
	var firstSaved int64
	first := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			return atomic.AddInt64(&firstSaved, 1), nil
		},
	}
	first.evidenceBuffer()
	if got := first.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 42, UpdatedAt: time.Now()}); got != virtualEvidenceAccepted {
		t.Fatalf("first instance enqueue = %v, want accepted", got)
	}
	first.StopVirtualEvidence()

	// A fresh handler models a restarted process. It must not have the previous
	// in-memory buffer.
	var restartSaved int64
	restarted := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			return atomic.AddInt64(&restartSaved, 1), nil
		},
	}
	restarted.evidenceBuffer()
	if n := restarted.virtualEvidenceBuffer.len(); n != 0 {
		t.Fatalf("restarted buffer retained %d task(s), want 0", n)
	}

	// The next playback start re-probes and re-admits the same file.
	if got := restarted.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 42, UpdatedAt: time.Now()}); got != virtualEvidenceAccepted {
		t.Fatalf("restart re-admission = %v, want accepted", got)
	}
	restarted.StopVirtualEvidence()
	if got := atomic.LoadInt64(&restartSaved); got != 1 {
		t.Fatalf("restart persisted %d write(s), want 1", got)
	}
}
