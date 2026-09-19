package handlers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgconn"
)

func evidenceTask(fileID int, adoptPath string, updatedAt time.Time) *virtualEvidenceTask {
	args := models.VirtualFilePersistArgs{
		FileID:           fileID,
		OwnerID:          5,
		ExpectedFilePath: "virtual://movie/tt-evidence",
		AdoptPath:        adoptPath,
		StampProbe:       true,
		UpdatedAt:        updatedAt,
	}
	return &virtualEvidenceTask{
		key:       virtualEvidenceKey(args),
		updatedAt: args.UpdatedAt,
		args:      args,
	}
}

// TestEvidenceAdmissionSaturationIsExplicit pins that a full buffer returns
// virtualEvidenceRejected rather than blocking or silently dropping, and that
// capacity frees as work is popped.
func TestEvidenceAdmissionSaturationIsExplicit(t *testing.T) {
	buf := newVirtualEvidenceBuffer(2)
	base := time.Now()
	if got := buf.admit(evidenceTask(1, "path-1", base)); got != virtualEvidenceAccepted {
		t.Fatalf("first admit = %v, want accepted", got)
	}
	if got := buf.admit(evidenceTask(2, "path-2", base)); got != virtualEvidenceAccepted {
		t.Fatalf("second admit = %v, want accepted", got)
	}
	if got := buf.admit(evidenceTask(3, "path-3", base)); got != virtualEvidenceRejected {
		t.Fatalf("overflow admit = %v, want rejected", got)
	}
	if buf.len() != 2 {
		t.Fatalf("pending = %d, want the capacity 2", buf.len())
	}
	buf.pop()
	if got := buf.admit(evidenceTask(3, "path-3", base)); got != virtualEvidenceAccepted {
		t.Fatalf("admit after pop = %v, want accepted", got)
	}
}

// TestEvidenceCoalescesEquivalentNewerWrite pins coalescing: a newer snapshot
// for the same target replaces the pending one in place, so only one write
// remains and it carries the newest evidence.
func TestEvidenceCoalescesEquivalentNewerWrite(t *testing.T) {
	buf := newVirtualEvidenceBuffer(4)
	base := time.Now()
	old := evidenceTask(1, "path", base)
	newer := evidenceTask(1, "path", base.Add(time.Second))
	buf.admit(old)
	if got := buf.admit(newer); got != virtualEvidenceCoalesced {
		t.Fatalf("newer admit = %v, want coalesced", got)
	}
	if buf.len() != 1 {
		t.Fatalf("pending = %d, want 1 after coalescing", buf.len())
	}
	if popped := buf.pop(); !popped.updatedAt.Equal(newer.updatedAt) {
		t.Fatalf("coalesced task updated_at = %v, want newest %v", popped.updatedAt, newer.updatedAt)
	}
}

// TestEvidenceRejectsStaleWriterForEquivalentTarget pins the stale-writer rule:
// an older snapshot for a pending target is rejected, never allowed to
// overwrite newer evidence.
func TestEvidenceRejectsStaleWriterForEquivalentTarget(t *testing.T) {
	buf := newVirtualEvidenceBuffer(4)
	base := time.Now()
	newer := evidenceTask(1, "path", base.Add(time.Second))
	stale := evidenceTask(1, "path", base)
	buf.admit(newer)
	if got := buf.admit(stale); got != virtualEvidenceRejected {
		t.Fatalf("stale admit = %v, want rejected", got)
	}
	if popped := buf.pop(); !popped.updatedAt.Equal(newer.updatedAt) {
		t.Fatalf("pending evidence was overwritten by the stale writer: %v", popped.updatedAt)
	}
}

// TestEvidenceDistinctTargetsDoNotCoalesce pins candidate rotation and
// source-identity separation: the same row adopting two different releases is
// two tasks, not one coalesced write.
func TestEvidenceDistinctTargetsDoNotCoalesce(t *testing.T) {
	buf := newVirtualEvidenceBuffer(4)
	base := time.Now()
	if got := buf.admit(evidenceTask(1, "release-a", base)); got != virtualEvidenceAccepted {
		t.Fatalf("release-a admit = %v, want accepted", got)
	}
	if got := buf.admit(evidenceTask(1, "release-b", base.Add(time.Second))); got != virtualEvidenceAccepted {
		t.Fatalf("release-b admit = %v, want accepted (distinct source identity)", got)
	}
	if buf.len() != 2 {
		t.Fatalf("pending = %d, want 2 distinct releases", buf.len())
	}
}

// TestEvidenceRetriesTransientFailureWithBackoff pins bounded retry: a write
// that fails twice transiently still succeeds on the third attempt.
func TestEvidenceRetriesTransientFailureWithBackoff(t *testing.T) {
	var calls int64
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			if atomic.AddInt64(&calls, 1) < 3 {
				return 0, errors.New("connection reset")
			}
			return 1, nil
		},
	}
	if err := h.persistVirtualEvidenceTask(evidenceTask(1, "path", time.Now()), time.Time{}); err != nil {
		t.Fatalf("transient failure was not recovered: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// TestEvidencePermanentFailureIsTerminal pins that a permanent database error
// is not retried and surfaces as an explicit terminal failure.
func TestEvidencePermanentFailureIsTerminal(t *testing.T) {
	var calls int64
	permanent := &pgconn.PgError{Code: "23514", Message: "check violation"}
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			atomic.AddInt64(&calls, 1)
			return 0, permanent
		},
	}
	if err := h.persistVirtualEvidenceTask(evidenceTask(1, "path", time.Now()), time.Time{}); !errors.Is(err, permanent) {
		t.Fatalf("terminal error = %v, want the permanent database error", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("permanent failure retried %d times, want 1", got)
	}
}

// TestEvidenceCASMissIsTerminalAndNotRetried pins the CAS fence outcome: a
// zero-row update means a newer snapshot (or a rotation) already committed, so
// the stale writer stops immediately.
func TestEvidenceCASMissIsTerminalAndNotRetried(t *testing.T) {
	var calls int64
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			atomic.AddInt64(&calls, 1)
			return 0, nil // CAS fence matched nothing
		},
	}
	if err := h.persistVirtualEvidenceTask(evidenceTask(1, "path", time.Now()), time.Time{}); !errors.Is(err, errVirtualEvidenceStale) {
		t.Fatalf("CAS miss error = %v, want errVirtualEvidenceStale", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("CAS miss retried %d times, want 1", got)
	}
}

// TestEvidenceShutdownDrainsAcceptedWork pins drain semantics: accepted work
// left pending when shutdown begins is flushed during the grace window.
func TestEvidenceShutdownDrainsAcceptedWork(t *testing.T) {
	var saved int64
	h := &PlaybackHandler{
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			return atomic.AddInt64(&saved, 1), nil
		},
	}
	buf := newVirtualEvidenceBuffer(8)
	base := time.Now()
	for i := 1; i <= 3; i++ {
		buf.admit(evidenceTask(i, "path", base.Add(time.Duration(i)*time.Second)))
	}
	h.drainVirtualEvidence(buf)
	if got := atomic.LoadInt64(&saved); got != 3 {
		t.Fatalf("drained %d accepted writes, want 3", got)
	}
	if buf.len() != 0 {
		t.Fatalf("pending after drain = %d, want 0", buf.len())
	}
}

// TestEvidenceConcurrentInstancesAreSafe pins that concurrent admission from
// independent producers is bounded and race-free. Run with -race.
func TestEvidenceConcurrentInstancesAreSafe(t *testing.T) {
	buf := newVirtualEvidenceBuffer(32)
	base := time.Now()
	var accepted, coalesced, rejected int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 64; i++ {
				key := g*64 + i
				switch buf.admit(evidenceTask(key, "path", base.Add(time.Duration(i)*time.Millisecond))) {
				case virtualEvidenceAccepted:
					atomic.AddInt64(&accepted, 1)
				case virtualEvidenceCoalesced:
					atomic.AddInt64(&coalesced, 1)
				case virtualEvidenceRejected:
					atomic.AddInt64(&rejected, 1)
				}
			}
		}(g)
	}
	wg.Wait()
	if got := buf.len(); got > 32 {
		t.Fatalf("pending = %d, want <= capacity 32", got)
	}
	if accepted+coalesced+rejected != 8*64 {
		t.Fatalf("admissions = %d, want %d", accepted+coalesced+rejected, 8*64)
	}
}

// TestEvidenceRestartStartsEmpty pins the documented crash semantics: accepted
// work lives only in memory, so a fresh handler (a restart) has no retained
// evidence and relies on the next playback start to re-admit.
func TestEvidenceRestartStartsEmpty(t *testing.T) {
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	first := &PlaybackHandler{ServiceContext: firstCtx}
	first.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 1})

	// A restarted handler gets a new, empty buffer.
	restartCtx, restartCancel := context.WithCancel(context.Background())
	defer restartCancel()
	restarted := &PlaybackHandler{ServiceContext: restartCtx}
	restarted.evidenceBuffer()
	if n := restarted.evidenceBuffer().len(); n != 0 {
		t.Fatalf("restarted buffer retained %d task(s), want 0", n)
	}
	restartCancel()
}
