package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// evidenceTaskForArgs builds a task from explicit persist args so a test can
// vary exactly one key input (for example RequireAdopt) and keep the rest.
func evidenceTaskForArgs(args models.VirtualFilePersistArgs) *virtualEvidenceTask {
	return &virtualEvidenceTask{
		key:       virtualEvidenceKey(args),
		seq:       1,
		updatedAt: args.UpdatedAt,
		probeAt:   args.ProbeUpdatedAt,
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

// TestEvidenceShutdownStopsAdmissionAndDrainsAcceptedBurst drives the real
// worker pool while admission races shutdown. Every admitted task must be
// persisted (nothing accepted is lost or silently left behind), the buffer must
// end empty with no in-flight work, and admission after shutdown must be an
// explicit rejection.
func TestEvidenceShutdownStopsAdmissionAndDrainsAcceptedBurst(t *testing.T) {
	var persisted sync.Map
	h := &PlaybackHandler{
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			persisted.Store(args.FileID, struct{}{})
			return 1, nil
		},
	}
	buf := h.evidenceBuffer() // starts the real worker pool

	const producers = 4
	accepted := make(map[int]struct{})
	var acceptedMu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fileID := base*1_000_000 + i + 1
				switch h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: fileID}) {
				case virtualEvidenceAccepted:
					acceptedMu.Lock()
					accepted[fileID] = struct{}{}
					acceptedMu.Unlock()
				}
			}
		}(p)
	}
	time.Sleep(20 * time.Millisecond) // let admission race the closure
	close(stop)
	wg.Wait()
	h.stopVirtualEvidence()

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

// TestEvidenceShutdownFinishesDequeuedWork drives the real workers through a
// deterministic shutdown: two tasks are already dequeued and blocked inside the
// saver, a third is still queued, and shutdown must await the workers before
// draining the remainder. All three must persist exactly once.
func TestEvidenceShutdownFinishesDequeuedWork(t *testing.T) {
	started := make(chan int, 8)
	release := make(chan struct{})
	var persistedMu sync.Mutex
	persisted := map[int]int{}
	h := &PlaybackHandler{
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			started <- args.FileID
			<-release
			persistedMu.Lock()
			persisted[args.FileID]++
			persistedMu.Unlock()
			return 1, nil
		},
	}
	h.evidenceBuffer()

	base := time.Now()
	for _, fileID := range []int{1, 2} {
		if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: fileID, UpdatedAt: base}); got != virtualEvidenceAccepted {
			t.Fatalf("enqueue file %d = %v, want accepted", fileID, got)
		}
	}
	// Both workers are now parked inside the saver with a dequeued task.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not dequeue the first two tasks")
		}
	}
	// A third task is accepted but has no free worker, so it stays queued.
	if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 3, UpdatedAt: base}); got != virtualEvidenceAccepted {
		t.Fatalf("enqueue file 3 = %v, want accepted", got)
	}

	stopped := make(chan struct{})
	go func() {
		h.stopVirtualEvidence()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("shutdown returned while dequeued work was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after the in-flight work was released")
	}

	persistedMu.Lock()
	defer persistedMu.Unlock()
	for _, fileID := range []int{1, 2, 3} {
		if persisted[fileID] != 1 {
			t.Fatalf("file %d persisted %d times, want exactly 1", fileID, persisted[fileID])
		}
	}
}

// TestEvidenceShutdownRetriesAfterServiceCancel pins that a retry survives
// service-context cancellation. The first attempt fails transiently, shutdown
// cancels the service context, and the second attempt must still run under an
// independent context and commit. This fails if drain retries observe the
// canceled service context.
func TestEvidenceShutdownRetriesAfterServiceCancel(t *testing.T) {
	serviceCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstAttempt := make(chan struct{})
	var calls int64
	var retryCtxErr atomic.Value
	retryCtxErr.Store("not-run")

	h := &PlaybackHandler{
		ServiceContext: serviceCtx,
		VirtualFileSaver: func(ctx context.Context, _ models.VirtualFilePersistArgs) (int64, error) {
			switch atomic.AddInt64(&calls, 1) {
			case 1:
				close(firstAttempt)
				return 0, errors.New("connection reset")
			default:
				if err := ctx.Err(); err != nil {
					retryCtxErr.Store(err.Error())
				} else {
					retryCtxErr.Store("")
				}
				return 1, nil
			}
		},
	}
	h.evidenceBuffer()
	if got := h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: 7}); got != virtualEvidenceAccepted {
		t.Fatalf("enqueue = %v, want accepted", got)
	}
	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("first attempt never ran")
	}
	cancel() // shutdown cancels the service context mid-retry

	// Single-flight shutdown: this blocks until the watcher's or its own
	// closure, worker await, and drain have all finished.
	h.stopVirtualEvidence()

	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("attempts = %d, want 2 (the retry must survive service cancellation)", got)
	}
	if msg, _ := retryCtxErr.Load().(string); msg != "" {
		t.Fatalf("retry ran under a canceled context: %s", msg)
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

// TestEvidenceRetryClassification pins the pure retry policy. A deterministic
// adoption refusal is terminal (it would fail identically on every attempt); a
// cancellation is terminal; SQLSTATE data/integrity/syntax classes are
// permanent; a timeout, connection or other transport fault is transient and
// retried. Serialization failures (40001) are transient by design, so they keep
// their bounded retry.
func TestEvidenceRetryClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"adoption refusal", errVirtualAdoptIdentityNotPersisted, false},
		{"wrapped adoption refusal", fmt.Errorf("write: %w", errVirtualAdoptIdentityNotPersisted), false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"pg integrity 23", &pgconn.PgError{Code: "23505"}, false},
		{"pg data 22", &pgconn.PgError{Code: "22001"}, false},
		{"pg syntax 42", &pgconn.PgError{Code: "42601"}, false},
		{"pg serialization 40001", &pgconn.PgError{Code: "40001"}, true},
		{"pg connection 08006", &pgconn.PgError{Code: "08006"}, true},
		{"transport", errors.New("connection reset"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualEvidenceRetryable(tc.err); got != tc.want {
				t.Fatalf("virtualEvidenceRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEvidenceAdoptionRefusalIsTerminalAndNotRetried is the follow-up's core
// assertion: a deterministic adoption refusal is attempted exactly once and the
// single terminal log carries the file id, the candidate identity and the
// reason. Both saver wirings are covered because the router prefers the
// metadata saver while the legacy row-count saver is still a fallback.
func TestEvidenceAdoptionRefusalIsTerminalAndNotRetried(t *testing.T) {
	const (
		fileID   = 11
		adoptURI = "virtual://movie/tt-evidence-refused?result=sibling"
	)
	refusal := fmt.Errorf("%w: candidate %s collided with an existing path owner", errVirtualAdoptIdentityNotPersisted, adoptURI)

	saverCases := []struct {
		name string
		wire func(h *PlaybackHandler, calls *int64)
	}{
		{
			name: "legacy row-count saver",
			wire: func(h *PlaybackHandler, calls *int64) {
				h.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
					atomic.AddInt64(calls, 1)
					return 0, refusal
				}
			},
		},
		{
			name: "preferred metadata saver",
			wire: func(h *PlaybackHandler, calls *int64) {
				h.VirtualFileMetadataSaver = func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
					atomic.AddInt64(calls, 1)
					return VirtualFileMetadataUpdateResult{}, refusal
				}
			},
		},
	}
	for _, tc := range saverCases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			var calls int64
			h := &PlaybackHandler{}
			tc.wire(h, &calls)

			err := h.persistVirtualEvidenceTask(evidenceTask(fileID, adoptURI, time.Now()), time.Time{})
			if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
				t.Fatalf("terminal error = %v, want the adoption-refusal sentinel", err)
			}
			if got := atomic.LoadInt64(&calls); got != 1 {
				t.Fatalf("saver calls = %d, want exactly 1: a deterministic adoption refusal must not retry", got)
			}
			logText := logs.String()
			if !strings.Contains(logText, `"file_id":`+fmt.Sprint(fileID)) {
				t.Fatalf("terminal log is missing the file id %d:\n%s", fileID, logText)
			}
			if !strings.Contains(logText, adoptURI) {
				t.Fatalf("terminal log is missing the candidate identity %q:\n%s", adoptURI, logText)
			}
			if !strings.Contains(logText, `"attempts":1`) {
				t.Fatalf("terminal log must report the single actual attempt:\n%s", logText)
			}
			if !strings.Contains(logText, "virtual candidate identity was not adopted") {
				t.Fatalf("terminal log is missing the refusal reason:\n%s", logText)
			}
		})
	}
}

// TestEvidenceCoalescingKeySeparatesFence pins the coalescing-key follow-up: a
// fenced write (RequireAdopt) and a metadata-only write that share the same row,
// expected path, adoption target and stamp do not share an evidence key, so one
// can never coalesce into or over the other.
func TestEvidenceCoalescingKeySeparatesFence(t *testing.T) {
	base := models.VirtualFilePersistArgs{
		FileID:           1,
		OwnerID:          5,
		ExpectedFilePath: "virtual://movie/tt-key?result=anchor",
		AdoptPath:        "virtual://movie/tt-key?result=sibling",
		StampProbe:       true,
	}
	fenced := base
	fenced.RequireAdopt = true
	metadataOnly := base
	metadataOnly.RequireAdopt = false

	if virtualEvidenceKey(fenced) == virtualEvidenceKey(metadataOnly) {
		t.Fatal("a fenced write and a metadata-only write with identical row, path, target and stamp share an evidence key")
	}

	// A collection row has no adoption target: the fenced key must still differ
	// from one that carries a concrete target.
	noTarget := fenced
	noTarget.AdoptPath = ""
	if virtualEvidenceKey(noTarget) == virtualEvidenceKey(fenced) {
		t.Fatal("a fenced write with no adoption target shares a key with one that has a target")
	}
}

// TestEvidenceFencedWriteDoesNotCoalesceWithMetadataOnly drives the buffer: a
// metadata-only write for the same row, path, adoption target and stamp is a
// distinct pending task, so a fenced cross-release write can neither absorb it
// nor be absorbed by it.
func TestEvidenceFencedWriteDoesNotCoalesceWithMetadataOnly(t *testing.T) {
	base := time.Now()
	fencedArgs := models.VirtualFilePersistArgs{
		FileID:           1,
		OwnerID:          5,
		ExpectedFilePath: "virtual://movie/tt-fence?result=anchor",
		AdoptPath:        "virtual://movie/tt-fence?result=sibling",
		StampProbe:       true,
		RequireAdopt:     true,
		UpdatedAt:        base,
	}
	metadataArgs := fencedArgs
	metadataArgs.RequireAdopt = false
	metadataArgs.UpdatedAt = base.Add(time.Second) // newer, so a shared key would coalesce onto it

	buf := newVirtualEvidenceBuffer(4)
	if got := buf.admit(evidenceTaskForArgs(fencedArgs)); got != virtualEvidenceAccepted {
		t.Fatalf("fenced admit = %v, want accepted", got)
	}
	if got := buf.admit(evidenceTaskForArgs(metadataArgs)); got != virtualEvidenceAccepted {
		t.Fatalf("metadata-only admit = %v, want accepted as a distinct task", got)
	}
	if got := buf.len(); got != 2 {
		t.Fatalf("pending = %d, want 2: the fence must not coalesce with metadata-only", got)
	}
	fenced := buf.pop()
	if fenced == nil || !fenced.args.RequireAdopt || fenced.args.AdoptPath != fencedArgs.AdoptPath {
		t.Fatalf("popped task lost the fence: %#v", fenced)
	}
	metadata := buf.pop()
	if metadata == nil || metadata.args.RequireAdopt {
		t.Fatalf("popped metadata-only task = %#v, want the unfenced write", metadata)
	}
}

// TestEvidenceFencedSameKeyCoalescesAndRetainsFence pins the other half: two
// fenced writes that share the key coalesce per the existing newer-snapshot
// rule, and the coalesced task keeps the adoption requirement and target. A
// stale fenced write is rejected and never replaces the newer fence.
func TestEvidenceFencedSameKeyCoalescesAndRetainsFence(t *testing.T) {
	base := time.Now()
	older := models.VirtualFilePersistArgs{
		FileID:           1,
		OwnerID:          5,
		ExpectedFilePath: "virtual://movie/tt-fence?result=anchor",
		AdoptPath:        "virtual://movie/tt-fence?result=sibling",
		StampProbe:       true,
		RequireAdopt:     true,
		UpdatedAt:        base,
	}
	newer := older
	newer.UpdatedAt = base.Add(time.Second)

	buf := newVirtualEvidenceBuffer(4)
	if got := buf.admit(evidenceTaskForArgs(older)); got != virtualEvidenceAccepted {
		t.Fatalf("older admit = %v, want accepted", got)
	}
	if got := buf.admit(evidenceTaskForArgs(newer)); got != virtualEvidenceCoalesced {
		t.Fatalf("newer admit = %v, want coalesced", got)
	}
	if got := buf.len(); got != 1 {
		t.Fatalf("pending = %d, want 1 after coalescing", got)
	}
	// A stale fenced write for the same key is rejected, not allowed to
	// overwrite the newer evidence or drop the fence.
	if got := buf.admit(evidenceTaskForArgs(older)); got != virtualEvidenceRejected {
		t.Fatalf("stale fenced admit = %v, want rejected", got)
	}
	coalesced := buf.pop()
	if coalesced == nil {
		t.Fatal("pending task disappeared after the stale rejection")
	}
	if !coalesced.args.RequireAdopt || coalesced.args.AdoptPath != older.AdoptPath {
		t.Fatalf("coalesced task lost the fence: %#v", coalesced.args)
	}
	if !coalesced.updatedAt.Equal(newer.UpdatedAt) {
		t.Fatalf("coalesced updated_at = %v, want the newest %v", coalesced.updatedAt, newer.UpdatedAt)
	}
}
