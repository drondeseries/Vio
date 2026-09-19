package handlers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A burst of detached workers must never run more than the gate's capacity
// concurrently, and tryAcquire must never block. This is the admission floor
// every spawn site relies on.
func TestVirtualDetachedGateBurstStaysWithinCap(t *testing.T) {
	const capacity = 4
	gate := newVirtualDetachedGate(capacity)
	release := make(chan struct{})
	var active, maxActive int64
	var wg sync.WaitGroup

	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !gate.tryAcquire() {
				return // shed, never queued or blocked
			}
			defer gate.release()
			cur := atomic.AddInt64(&active, 1)
			for {
				old := atomic.LoadInt64(&maxActive)
				if cur <= old || atomic.CompareAndSwapInt64(&maxActive, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt64(&active, -1)
		}()
	}

	// Wait until the gate is saturated; the excess goroutines have returned.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&active) < int64(capacity) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&active); got != int64(capacity) {
		t.Fatalf("gate admitted %d workers, want exactly the cap %d", got, capacity)
	}

	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&maxActive); got != int64(capacity) {
		t.Fatalf("peak concurrent detached workers = %d, want %d", got, int64(capacity))
	}
}

// The handler owns one bounded, lazy gate: literal handlers must still get a
// bound, and two handlers must not share it.
func TestDetachedGateIsHandlerScopedAndBounded(t *testing.T) {
	h := &PlaybackHandler{}
	gate := h.detachedGate()
	if gate == nil {
		t.Fatal("detachedGate returned nil")
	}
	if got := gate.capacity(); got != virtualDetachedWorkerCap {
		t.Fatalf("gate capacity = %d, want %d", got, virtualDetachedWorkerCap)
	}
	if h.detachedGate() != gate {
		t.Fatal("detachedGate did not reuse the handler's gate")
	}
	other := &PlaybackHandler{}
	if other.detachedGate() == gate {
		t.Fatal("gate leaked across handlers")
	}
}

// A saturated gate must shed the subtitle search on the request path: no
// blocking, no spawned goroutine, and no leftover dedupe key that would
// suppress the retry once a slot frees.
func TestSaturatedDetachedGateShedsSubtitleSearchWithoutBlocking(t *testing.T) {
	h := &PlaybackHandler{SubtitleSearchInFlight: &sync.Map{}}
	gate := h.detachedGate()
	for i := 0; i < gate.capacity(); i++ {
		if !gate.tryAcquire() {
			t.Fatalf("acquire slot %d failed while gate was below its cap", i)
		}
	}

	searched := make(chan struct{}, 1)
	h.VirtualSubtitleSearcher = func(context.Context, string, string, string, int, int, int, int, []string) {
		searched <- struct{}{}
	}
	file := &models.MediaFile{ID: 42, ContentID: "movie"}
	cand := VirtualPlaybackStream{URI: "virtual://movie/1?result=x"}

	returned := make(chan struct{})
	go func() {
		h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("maybeTriggerSubtitleSearch blocked on a saturated gate")
	}
	select {
	case <-searched:
		t.Fatal("subtitle search spawned while the gate was saturated")
	case <-time.After(50 * time.Millisecond):
	}
	if _, loaded := h.SubtitleSearchInFlight.Load(file.ID); loaded {
		t.Fatal("a shed search left its dedupe key behind")
	}

	// Free one slot: the same file must be admitted and spawned.
	gate.release()
	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-searched:
	case <-time.After(2 * time.Second):
		t.Fatal("subtitle search did not run after a slot was released")
	}
}

// Canceling the service context must stop a detached search, release its gate
// slot, and drop its in-flight dedupe key so shutdown leaks neither a goroutine
// nor admission state.
func TestServiceContextCancellationStopsDetachedSubtitleSearch(t *testing.T) {
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	h := &PlaybackHandler{
		ServiceContext:         serviceCtx,
		SubtitleSearchInFlight: &sync.Map{},
		VirtualSubtitleSearcher: func(ctx context.Context, _ string, _ string, _ string, _, _, _, _ int, _ []string) {
			close(started)
			<-ctx.Done()
			close(stopped)
		},
	}
	file := &models.MediaFile{ID: 7, ContentID: "movie"}
	cand := VirtualPlaybackStream{URI: "virtual://movie/1?result=x"}

	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("detached subtitle search did not start")
	}

	serviceCancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("detached subtitle search did not stop when the service context was canceled")
	}

	gate := h.detachedGate()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, loaded := h.SubtitleSearchInFlight.Load(file.ID)
		if !loaded && len(gate.slots) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, loaded := h.SubtitleSearchInFlight.Load(file.ID); loaded {
		t.Fatal("shutdown leaked the subtitle search dedupe key")
	}
	if got := len(gate.slots); got != 0 {
		t.Fatalf("shutdown leaked %d gate slot(s)", got)
	}
}

// The detached context must both follow the service lifecycle and carry its own
// timeout, so shutdown cancels it and a hung worker cannot run forever.
func TestVirtualDetachedContextFollowsServiceLifecycle(t *testing.T) {
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	h := &PlaybackHandler{ServiceContext: serviceCtx}

	ctx, cancel := h.virtualDetachedContext(context.Background(), time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("detached context canceled before service shutdown")
	default:
	}

	serviceCancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("detached context did not follow service shutdown")
	}
}

// Under churn of distinct candidates the probe-failure map must stay capped,
// while a still-live key keeps returning its cached failure.
func TestProbeFailureCacheStaysBoundedUnderChurn(t *testing.T) {
	base := time.Now()
	c := &virtualProbeFailureCache{marks: make(map[string]virtualProbeFailureMark)}

	churn := virtualProbeFailureMaxEntries + 64
	for i := 0; i < churn; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		c.now = func() time.Time { return at }
		c.mark(fmt.Sprintf("candidate-%d", i))
	}
	if got := len(c.marks); got > virtualProbeFailureMaxEntries {
		t.Fatalf("probe failure marks = %d, want <= %d", got, virtualProbeFailureMaxEntries)
	}

	last := fmt.Sprintf("candidate-%d", churn-1)
	c.now = func() time.Time { return base.Add(time.Duration(churn-1) * time.Millisecond) }
	if !c.recent(last) {
		t.Fatal("live failure marker was evicted by churn")
	}
	if got := c.count(last); got == 0 {
		t.Fatal("live failure marker no longer reports its failure count")
	}
}

// Evidence persistence is queued, not gated: a burst must not block the
// request path, and an overflow must be dropped with a loud reason rather than
// silently. Accepted writes must still drain.
func TestEvidencePersistQueueOverloadIsLoudAndNonBlocking(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var saved int64
	h := &PlaybackHandler{
		ServiceContext: serviceCtx,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			<-release
			return atomic.AddInt64(&saved, 1), nil
		},
	}

	for i := 0; i < virtualEvidenceQueueSize+virtualEvidenceWorkers+16; i++ {
		h.enqueueVirtualProbeEvidence(context.Background(), models.VirtualFilePersistArgs{FileID: i})
	}
	if !strings.Contains(logs.String(), "detached evidence queue full") {
		t.Fatalf("queue-full drop was not logged; logs:\n%s", logs.String())
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&saved) < int64(virtualEvidenceQueueSize) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&saved); got < int64(virtualEvidenceQueueSize) {
		t.Fatalf("drained %d queued evidence writes, want at least %d", got, virtualEvidenceQueueSize)
	}
	serviceCancel()
}
