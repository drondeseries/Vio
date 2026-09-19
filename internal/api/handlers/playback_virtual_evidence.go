package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgconn"
)

// Probe-evidence persistence is a bounded, coalescing, retrying pipeline
// rather than a fire-and-forget queue.
//
// Admission: an evidence write is keyed by row identity plus the target source
// identity and stamping semantics (see virtualEvidenceKey). A key already
// pending is coalesced only when the new snapshot is at least as new as the
// queued one; a stale writer is rejected outright so it can never overwrite
// newer evidence, selection or failure state. A genuinely distinct key is
// queued until the pending bound is reached, then rejected. Both outcomes are
// returned to the caller, so overload is observable rather than a silent drop.
//
// Execution: a fixed worker pool (virtualEvidenceWorkers) drains the buffer.
// Each write carries its original CAS snapshot (updated_at, probe_updated_at,
// owner, library) so the SQL fence rejects a write whose row has moved on. A
// transient failure is retried with bounded exponential backoff; a permanent
// database error, a CAS miss (rows affected zero), or exhausted retries is a
// terminal failure that is logged and dropped. A CAS miss is never retried: a
// newer writer already committed the row.
//
// Shutdown: pending work is drained for virtualEvidenceDrainGrace after the
// service context ends. Work still pending when the grace expires is abandoned
// with a warning. Nothing is durable across process death: accepted work lives
// only in this in-memory buffer, so a crash loses every queued or in-flight
// write. That is safe because the loss is evidence, not data — the next
// playback start re-probes and re-admits, and the catalog keeps its last
// committed snapshot until then.

const (
	// virtualEvidenceQueueSize bounds queued (not yet executing) evidence
	// writes. Active tasks are removed from the pending buffer before they
	// execute, so active + pending is virtualEvidenceQueueSize +
	// virtualEvidenceWorkers.
	virtualEvidenceQueueSize = 256
	// virtualEvidenceWorkers is the number of workers that persist queued
	// evidence. Two keeps a burst moving without competing with probe workers
	// for the aggregate gate.
	virtualEvidenceWorkers = 2
	// virtualEvidencePersistBudget bounds one evidence write attempt.
	virtualEvidencePersistBudget = 5 * time.Second
	// virtualEvidenceMaxAttempts bounds retries for one accepted write.
	virtualEvidenceMaxAttempts = 3
	// virtualEvidenceRetryBaseDelay is the first backoff step; it doubles per
	// attempt up to virtualEvidenceRetryMaxDelay.
	virtualEvidenceRetryBaseDelay = 100 * time.Millisecond
	virtualEvidenceRetryMaxDelay  = time.Second
	// virtualEvidenceDrainGrace bounds the shutdown drain of accepted work.
	virtualEvidenceDrainGrace = 5 * time.Second
)

// errVirtualEvidenceStale marks a CAS-fenced write that matched no row because
// a newer writer (or a candidate rotation) already committed. It is terminal:
// retrying cannot help.
var errVirtualEvidenceStale = errors.New("virtual evidence write superseded by a newer snapshot")

// virtualEvidenceAdmission is the explicit result of admitting one evidence
// write.
type virtualEvidenceAdmission int

const (
	// virtualEvidenceAccepted means the write was queued for execution.
	virtualEvidenceAccepted virtualEvidenceAdmission = iota
	// virtualEvidenceCoalesced means the write was folded onto an equivalent
	// pending write (or was rejected as an older snapshot of one).
	virtualEvidenceCoalesced
	// virtualEvidenceRejected means the write could not be admitted because the
	// pending buffer was full or the snapshot was stale relative to a pending
	// equivalent.
	virtualEvidenceRejected
)

// String forms of virtualEvidenceAdmission. They are the stable textual result
// of an admission, distinct from the autoscan delivery status of the same name.
const (
	virtualEvidenceResultAccepted  = "accepted"
	virtualEvidenceResultCoalesced = "coalesced"
	virtualEvidenceResultRejected  = "rejected"
	virtualEvidenceResultUnknown   = "unknown"
)

func (a virtualEvidenceAdmission) String() string {
	switch a {
	case virtualEvidenceAccepted:
		return virtualEvidenceResultAccepted
	case virtualEvidenceCoalesced:
		return virtualEvidenceResultCoalesced
	case virtualEvidenceRejected:
		return virtualEvidenceResultRejected
	default:
		return virtualEvidenceResultUnknown
	}
}

// virtualEvidenceTask is one buffered evidence write plus the ordering
// metadata coalescing uses to decide whether a newer request supersedes it.
type virtualEvidenceTask struct {
	key       string
	seq       uint64
	updatedAt time.Time
	probeAt   *time.Time
	args      models.VirtualFilePersistArgs
}

// virtualEvidenceKey identifies an evidence target. Same row, same expected
// path, same adopted source identity and same stamping semantics may coalesce;
// a different adoption target (a different release) or a different stamping
// mode is a distinct task so coalescing cannot drop a probe stamp or write one
// source's evidence under another's identity.
func virtualEvidenceKey(args models.VirtualFilePersistArgs) string {
	return fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%t",
		args.FileID, args.OwnerID, args.ExpectedFilePath, args.AdoptPath, args.StampProbe)
}

func virtualEvidenceProbeTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// virtualEvidenceSnapshotNewer reports whether a's snapshot supersedes b's. It
// is the stale-writer guard: coalescing never lets an older snapshot replace a
// newer queued one.
func virtualEvidenceSnapshotNewer(a, b *virtualEvidenceTask) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.updatedAt.After(b.updatedAt) {
		return true
	}
	if a.updatedAt.Before(b.updatedAt) {
		return false
	}
	pa, pb := virtualEvidenceProbeTime(a.probeAt), virtualEvidenceProbeTime(b.probeAt)
	if pa.After(pb) {
		return true
	}
	if pa.Before(pb) {
		return false
	}
	return a.seq >= b.seq
}

// virtualEvidenceBuffer is a bounded FIFO of pending evidence tasks with a
// coalescing index. It is deliberately not a channel: coalescing must replace
// a pending task in place, which a channel cannot express. All methods are safe
// for concurrent use.
type virtualEvidenceBuffer struct {
	mu       sync.Mutex
	pending  []*virtualEvidenceTask
	index    map[string]*virtualEvidenceTask
	capacity int
	seq      uint64
	signalCh chan struct{}
}

func newVirtualEvidenceBuffer(capacity int) *virtualEvidenceBuffer {
	if capacity <= 0 {
		capacity = virtualEvidenceQueueSize
	}
	return &virtualEvidenceBuffer{
		index:    make(map[string]*virtualEvidenceTask),
		capacity: capacity,
		signalCh: make(chan struct{}, 1),
	}
}

func (b *virtualEvidenceBuffer) signal() {
	if b == nil || b.signalCh == nil {
		return
	}
	select {
	case b.signalCh <- struct{}{}:
	default:
	}
}

func (b *virtualEvidenceBuffer) nextSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
}

// admit attempts to enqueue one task, returning the explicit admission result.
func (b *virtualEvidenceBuffer) admit(t *virtualEvidenceTask) virtualEvidenceAdmission {
	if b == nil || t == nil {
		return virtualEvidenceRejected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.index[t.key]; ok {
		if !virtualEvidenceSnapshotNewer(t, existing) {
			// A stale writer for an equivalent target: reject rather than let
			// it replace newer evidence.
			return virtualEvidenceRejected
		}
		existing.args = t.args
		existing.updatedAt = t.updatedAt
		existing.probeAt = t.probeAt
		existing.seq = t.seq
		b.signal()
		return virtualEvidenceCoalesced
	}
	if len(b.pending) >= b.capacity {
		return virtualEvidenceRejected
	}
	b.index[t.key] = t
	b.pending = append(b.pending, t)
	b.signal()
	return virtualEvidenceAccepted
}

// pop removes and returns the oldest pending task, or nil when none is queued.
// The key leaves the coalescing index here, so a duplicate arriving while this
// task executes becomes a distinct, sequentially-later write.
func (b *virtualEvidenceBuffer) pop() *virtualEvidenceTask {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return nil
	}
	t := b.pending[0]
	b.pending = b.pending[1:]
	delete(b.index, t.key)
	return t
}

func (b *virtualEvidenceBuffer) len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func (b *virtualEvidenceBuffer) signalChannel() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.signalCh
}

// evidenceBuffer returns the handler's evidence buffer, constructing it and
// starting the bounded worker pool on first use.
func (h *PlaybackHandler) evidenceBuffer() *virtualEvidenceBuffer {
	if h == nil {
		return nil
	}
	h.virtualEvidenceOnce.Do(func() {
		if h.virtualEvidenceBuffer == nil {
			h.virtualEvidenceBuffer = newVirtualEvidenceBuffer(virtualEvidenceQueueSize)
		}
		buf := h.virtualEvidenceBuffer
		for range virtualEvidenceWorkers {
			go h.runVirtualEvidenceWorker(buf)
		}
	})
	return h.virtualEvidenceBuffer
}

// enqueueVirtualProbeEvidence admits one catalog write into the evidence
// buffer. It never blocks: a full buffer rejects instead.
func (h *PlaybackHandler) enqueueVirtualProbeEvidence(_ context.Context, args models.VirtualFilePersistArgs) virtualEvidenceAdmission {
	if h == nil || h.VirtualFileSaver == nil {
		return virtualEvidenceRejected
	}
	if args.FileID <= 0 {
		return virtualEvidenceRejected
	}
	buf := h.evidenceBuffer()
	task := &virtualEvidenceTask{
		key:       virtualEvidenceKey(args),
		seq:       buf.nextSeq(),
		updatedAt: args.UpdatedAt,
		probeAt:   args.ProbeUpdatedAt,
		args:      args,
	}
	return buf.admit(task)
}

// runVirtualEvidenceWorker drains accepted work until the service context ends,
// then performs a bounded drain of what is left before exiting.
func (h *PlaybackHandler) runVirtualEvidenceWorker(buf *virtualEvidenceBuffer) {
	var serviceDone <-chan struct{}
	if h.ServiceContext != nil {
		serviceDone = h.ServiceContext.Done()
	}
	for {
		if task := buf.pop(); task != nil {
			// persistVirtualEvidenceTask logs terminal failures itself, so the
			// worker reports and drops them there and moves to the next task.
			_ = h.persistVirtualEvidenceTask(task, time.Time{})
			continue
		}
		select {
		case <-buf.signalChannel():
		case <-serviceDone:
			h.drainVirtualEvidence(buf)
			return
		}
	}
}

// drainVirtualEvidence persists accepted work for at most
// virtualEvidenceDrainGrace after shutdown began, then abandons the remainder
// with a warning. Drain writes use a context independent of the canceled
// service context so they can still complete.
func (h *PlaybackHandler) drainVirtualEvidence(buf *virtualEvidenceBuffer) {
	deadline := time.Now().Add(virtualEvidenceDrainGrace)
	for time.Now().Before(deadline) {
		task := buf.pop()
		if task == nil {
			return
		}
		// persistVirtualEvidenceTask logs terminal failures itself.
		_ = h.persistVirtualEvidenceTask(task, deadline)
	}
	if n := buf.len(); n > 0 {
		slog.Warn("virtual probe evidence abandoned at shutdown",
			"component", "api", "queued", n)
	}
}

// persistVirtualEvidenceTask executes one accepted write with bounded retry.
// drainDeadline, when non-zero, bounds the whole task (including backoff) for
// shutdown draining. The return value is the write error, or
// errVirtualEvidenceStale on a CAS miss; both are logged as terminal.
func (h *PlaybackHandler) persistVirtualEvidenceTask(task *virtualEvidenceTask, drainDeadline time.Time) error {
	if task == nil || h.VirtualFileSaver == nil {
		return nil
	}
	saver := h.VirtualFileSaver
	var lastErr error
	for attempt := 1; attempt <= virtualEvidenceMaxAttempts; attempt++ {
		if attempt > 1 {
			delay := virtualEvidenceBackoff(attempt)
			if !drainDeadline.IsZero() {
				remaining := time.Until(drainDeadline)
				if remaining <= 0 {
					lastErr = context.DeadlineExceeded
					break
				}
				if delay > remaining {
					delay = remaining
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-h.serviceDoneChannel():
				timer.Stop()
				lastErr = context.Canceled
				goto terminal
			}
		}
		writeCtx, cancel := h.evidenceWriteContext(drainDeadline)
		rows, err := saver(writeCtx, task.args)
		cancel()
		if err == nil {
			if rows == 0 {
				lastErr = errVirtualEvidenceStale
				goto terminal
			}
			return nil
		}
		lastErr = err
		if !virtualEvidenceRetryable(err) {
			goto terminal
		}
	}
terminal:
	slog.Error("virtual probe evidence persist terminal failure",
		"component", "api", "file_id", task.args.FileID, "attempts", virtualEvidenceMaxAttempts,
		"error", lastErr)
	return lastErr
}

func (h *PlaybackHandler) serviceDoneChannel() <-chan struct{} {
	if h == nil || h.ServiceContext == nil {
		return nil
	}
	return h.ServiceContext.Done()
}

// evidenceWriteContext builds one write attempt's context. Normal writes are
// bound to the service lifecycle with the per-attempt budget; drain writes use
// a context independent of the canceled service so shutdown can still flush.
func (h *PlaybackHandler) evidenceWriteContext(drainDeadline time.Time) (context.Context, context.CancelFunc) {
	if drainDeadline.IsZero() {
		return h.virtualDetachedContext(h.ServiceContext, virtualEvidencePersistBudget)
	}
	timeout := virtualEvidencePersistBudget
	if remaining := time.Until(drainDeadline); remaining > 0 && remaining < timeout {
		timeout = remaining
	}
	return context.WithTimeout(context.Background(), timeout)
}

func virtualEvidenceBackoff(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	delay := virtualEvidenceRetryBaseDelay
	for i := 1; i < attempt-1; i++ {
		delay *= 2
		if delay >= virtualEvidenceRetryMaxDelay {
			return virtualEvidenceRetryMaxDelay
		}
	}
	if delay > virtualEvidenceRetryMaxDelay {
		return virtualEvidenceRetryMaxDelay
	}
	return delay
}

// virtualEvidenceRetryable classifies a write error. Cancellation stops the
// task; integrity and data/syntax-class database errors are permanent; a
// timeout, a connection error or any other transport fault is transient and
// worth a bounded retry.
func virtualEvidenceRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		for _, prefix := range []string{"22", "23", "42"} {
			if strings.HasPrefix(pgErr.Code, prefix) {
				return false
			}
		}
	}
	return true
}
