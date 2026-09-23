package remotestream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// patternByte is the deterministic source byte at a stream offset. 251 is prime,
// so a misaligned or dropped byte is detected immediately rather than after a
// period.
func patternByte(offset int64) byte { return byte(offset % 251) }

// patternReader is a deterministic finite source. Each response starts at
// offset 0, so every consumer in the load test verifies the same byte sequence.
type patternReader struct {
	remaining int64
	offset    int64
}

func newPatternReader(total int64) *patternReader { return &patternReader{remaining: total} }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = patternByte(r.offset + int64(i))
	}
	r.offset += int64(n)
	r.remaining -= int64(n)
	return n, nil
}

// slowPatternWriter is a downstream consumer: it verifies every byte against
// the source pattern and pauses per write as a slow client would. The pause is
// what keeps the relay's read-ahead pool filled and contended.
type slowPatternWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	delay  time.Duration
	offset int64
	err    error
}

func (w *slowPatternWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *slowPatternWriter) WriteHeader(status int) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}

func (w *slowPatternWriter) Flush() {}

func (w *slowPatternWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	for i, b := range p {
		if b != patternByte(w.offset+int64(i)) {
			w.err = fmt.Errorf("corrupt byte at stream offset %d", w.offset+int64(i))
			return 0, w.err
		}
	}
	w.offset += int64(len(p))
	// The pause simulates a slow client. It is the load, not a wait for a
	// condition: completion is observed through the WaitGroup below.
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	return len(p), nil
}

func (w *slowPatternWriter) progress() (offset int64, status int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offset, w.status, w.err
}

func TestReadAheadBudgetAcquireRelease(t *testing.T) {
	b := newReadAheadBudget(8 * remoteBodyChunkSize)
	if got, want := b.capacity(), 8*remoteBodyChunkSize; got != want {
		t.Fatalf("capacity = %d, want %d", got, want)
	}
	ctx := context.Background()

	if err := b.acquire(ctx, remoteBodyChunkSize); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := b.acquire(ctx, remoteBodyChunkSize); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got, want := b.used(), 2*remoteBodyChunkSize; got != want {
		t.Fatalf("used = %d, want %d", got, want)
	}
	if got, want := b.peak(), 2*remoteBodyChunkSize; got != want {
		t.Fatalf("peak = %d, want %d", got, want)
	}

	b.release(remoteBodyChunkSize)
	if got, want := b.used(), remoteBodyChunkSize; got != want {
		t.Fatalf("used after one release = %d, want %d", got, want)
	}
	b.release(remoteBodyChunkSize)
	if got := b.used(); got != 0 {
		t.Fatalf("used after full release = %d, want 0", got)
	}
	// The high-water mark survives the release.
	if got, want := b.peak(), 2*remoteBodyChunkSize; got != want {
		t.Fatalf("peak after release = %d, want %d", got, want)
	}
}

func TestReadAheadBudgetExhaustionBlocksUntilRelease(t *testing.T) {
	b := newReadAheadBudget(2 * remoteBodyChunkSize)
	ctx := context.Background()
	if err := b.acquire(ctx, 2*remoteBodyChunkSize); err != nil {
		t.Fatalf("exhausting acquire: %v", err)
	}

	started := make(chan struct{})
	acquired := make(chan error, 1)
	go func() {
		close(started)
		acquired <- b.acquire(ctx, remoteBodyChunkSize)
	}()
	<-started

	// The pool has no free bytes, so the waiter cannot have acquired yet.
	select {
	case err := <-acquired:
		t.Fatalf("acquire returned while the pool was exhausted: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got, want := b.used(), 2*remoteBodyChunkSize; got != want {
		t.Fatalf("used while blocked = %d, want %d", got, want)
	}

	b.release(remoteBodyChunkSize)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("waiter after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired after a release")
	}
	if got, want := b.used(), 2*remoteBodyChunkSize; got != want {
		t.Fatalf("used after waiter acquired = %d, want %d", got, want)
	}
	b.release(2 * remoteBodyChunkSize)
}

func TestReadAheadBudgetCancellationUnblocksWaiterWithoutLeak(t *testing.T) {
	b := newReadAheadBudget(remoteBodyChunkSize)
	if err := b.acquire(context.Background(), remoteBodyChunkSize); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- b.acquire(waiterCtx, remoteBodyChunkSize) }()

	// The pool is exhausted, so the waiter is blocked. Give it a moment to
	// park, then cancel; cancellation must return ctx.Err without reserving.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiter never returned")
	}
	if got, want := b.used(), remoteBodyChunkSize; got != want {
		t.Fatalf("used after canceled waiter = %d, want only the holder's %d", got, want)
	}

	// The holder's release drains the pool exactly; the canceled waiter left
	// no reservation behind.
	b.release(remoteBodyChunkSize)
	if got := b.used(); got != 0 {
		t.Fatalf("used after holder release = %d, want 0 (no leak)", got)
	}
}

// TestReadAheadBudgetSingleStreamWithTightPool runs one stream through a pool
// sized to a single chunk, so every refill blocks on the consumer's release.
// It completes without deadlock and drains the pool.
func TestReadAheadBudgetSingleStreamWithTightPool(t *testing.T) {
	const chunks = 5
	budget := newReadAheadBudget(remoteBodyChunkSize)
	body := newPatternReader(int64(remoteBodyChunkSize) * chunks)

	// The pump's exit drain waits for its ctx to be canceled before releasing
	// leftovers, so callers always cancel (the relay does this per request).
	pumpCtx, cancelPump := context.WithCancel(context.Background())
	defer cancelPump()
	chunksCh := pumpRemoteBody(pumpCtx, body, budget)
	var received int64
	var got int
	for {
		chunk, err := nextRemoteBodyChunk(context.Background(), chunksCh, 5*time.Second)
		if err != nil {
			t.Fatalf("next chunk: %v", err)
		}
		if len(chunk.data) == 0 {
			if !errors.Is(chunk.err, io.EOF) {
				t.Fatalf("terminal chunk error = %v, want EOF", chunk.err)
			}
			break
		}
		for i, by := range chunk.data {
			if by != patternByte(received+int64(i)) {
				t.Fatalf("corrupt byte at %d", received+int64(i))
			}
		}
		received += int64(len(chunk.data))
		got++
	}
	if want := int64(remoteBodyChunkSize) * chunks; received != want {
		t.Fatalf("received %d bytes, want %d", received, want)
	}
	if got != chunks {
		t.Fatalf("received %d chunks, want %d", got, chunks)
	}
	if used := budget.used(); used != 0 {
		t.Fatalf("budget used after drain = %d, want 0", used)
	}
}

// TestPumpRemoteBodyZeroByteReadReturnsReservation proves an (0, nil) read
// returns its reservation. The pool holds exactly one chunk, so if the
// reservation leaked the next acquire would block forever and the test would
// time out instead of delivering the data.
func TestPumpRemoteBodyZeroByteReadReturnsReservation(t *testing.T) {
	budget := newReadAheadBudget(remoteBodyChunkSize)
	body := &zeroThenDataReader{data: []byte("hello remote stream")}

	pumpCtx, cancelPump := context.WithCancel(context.Background())
	defer cancelPump()
	chunksCh := pumpRemoteBody(pumpCtx, body, budget)
	done := make(chan struct{})
	var received []byte
	go func() {
		defer close(done)
		for {
			chunk, err := nextRemoteBodyChunk(context.Background(), chunksCh, 5*time.Second)
			if err != nil {
				return
			}
			if len(chunk.data) == 0 {
				return
			}
			received = append(received, chunk.data...)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pump deadlocked after a (0, nil) read")
	}
	if string(received) != "hello remote stream" {
		t.Fatalf("received %q", received)
	}
	if used := budget.used(); used != 0 {
		t.Fatalf("budget used after drain = %d, want 0", used)
	}
}

// zeroThenDataReader returns (0, nil) once, then its data, then EOF.
type zeroThenDataReader struct {
	data   []byte
	zeroed bool
}

func (r *zeroThenDataReader) Read(p []byte) (int, error) {
	if !r.zeroed {
		r.zeroed = true
		return 0, nil
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// progressReader wraps a source and records how many bytes it has served. The
// budget tests poll it to wait on observable pump progress instead of sleeping.
type progressReader struct {
	r     io.Reader
	bytes atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.bytes.Add(int64(n))
	return n, err
}

func (p *progressReader) read() int64 { return p.bytes.Load() }

// waitForReaderBytes blocks until the pump has read at least want bytes from the
// source, failing the test if that never happens.
func waitForReaderBytes(t *testing.T, r *progressReader, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := r.read(); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pump read %d bytes, want at least %d", r.read(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForBudgetUsed blocks until the pool has at least want bytes reserved.
func waitForBudgetUsed(t *testing.T, budget *readAheadBudget, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if budget.used() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("budget used %d never reached %d within %s", budget.used(), want, timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForBudgetZero blocks until every reservation has been returned. It is the
// observable drain signal for an abandoned stream.
func waitForBudgetZero(t *testing.T, budget *readAheadBudget, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if budget.used() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("budget used %d never returned to 0 within %s (stranded reservation)", budget.used(), timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestReadAheadBudgetReleasedWhenConsumerAbandons reproduces the prod leak: a
// pump fills its buffered channel and parks on the next send, then the stream is
// abandoned (client abort / first-byte timeout / HLS-branch return / upstream
// error). Queued chunks hold their reservations, so an abandoned stream must be
// cleared by the pump's own exit drain once its context is canceled; otherwise
// the reservations strand. Neither scenario drains the channel itself — only the
// pump can retire the leftovers — so both fail without the drain.
func TestReadAheadBudgetReleasedWhenConsumerAbandons(t *testing.T) {
	// Capacity exceeds the per-stream buffer, so the channel genuinely fills.
	capacity := (remoteBodyBufferChunks + 4) * remoteBodyChunkSize

	// Scenario A: a consumer receives the first chunk, then abandons. The
	// consumer release is exercised by that one receive; the pump's drain must
	// release every chunk still queued.
	budget := newReadAheadBudget(capacity)
	body := &progressReader{r: newPatternReader(int64(remoteBodyChunkSize) * (remoteBodyBufferChunks + 8))}
	ctx, cancel := context.WithCancel(context.Background())
	chunks := pumpRemoteBody(ctx, body, budget)
	waitForReaderBytes(t, body, int64(remoteBodyChunkSize)*(remoteBodyBufferChunks+1))
	if _, err := nextRemoteBodyChunk(context.Background(), chunks, 5*time.Second); err != nil {
		t.Fatalf("consumer receive: %v", err)
	}
	cancel()
	waitForBudgetZero(t, budget, 5*time.Second)
	t.Logf("scenario A after abandonment: capacity=%d used=%d peak=%d",
		capacity, budget.used(), budget.peak())
	acquireBudgetCapacity(t, budget, capacity)

	// Scenario B: nobody receives at all. The pump's own exit drain must clear
	// the pool after cancel; no consumer cooperation is involved.
	budget = newReadAheadBudget(capacity)
	body = &progressReader{r: newPatternReader(int64(remoteBodyChunkSize) * (remoteBodyBufferChunks + 8))}
	ctx, cancel = context.WithCancel(context.Background())
	// The returned channel is intentionally dropped: nobody receives in this
	// scenario, so the pump's own exit drain has to clear the pool.
	pumpRemoteBody(ctx, body, budget)
	waitForReaderBytes(t, body, int64(remoteBodyChunkSize)*(remoteBodyBufferChunks+1))
	cancel()
	waitForBudgetZero(t, budget, 5*time.Second)
	if got := budget.used(); got != 0 {
		t.Fatalf("budget used after nobody received = %d, want 0 (stranded reservation)", got)
	}
	t.Logf("scenario B after abandonment with no receiver: capacity=%d used=%d peak=%d",
		capacity, budget.used(), budget.peak())
	acquireBudgetCapacity(t, budget, capacity)
}

// acquireBudgetCapacity reserves the whole pool to prove it is free, then
// returns it.
func acquireBudgetCapacity(t *testing.T, budget *readAheadBudget, capacity int) {
	t.Helper()
	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acquireCancel()
	if err := budget.acquire(acquireCtx, capacity); err != nil {
		t.Fatalf("fresh acquire of full capacity after abandonment failed: %v", err)
	}
	budget.release(capacity)
	if got := budget.used(); got != 0 {
		t.Fatalf("budget used after releasing the fresh acquire = %d, want 0", got)
	}
}

// TestReadAheadBudgetSurvivesRepeatedAbortedStreams runs many abandon-and-cancel
// lifecycles against one shared pool. This is the prod failure inverted: dozens
// of aborted starts used to strand their reservations until the 64 MiB pool was
// exhausted and every acquire blocked forever. After every lifecycle the pool
// must read zero used and still hand out its full capacity.
func TestReadAheadBudgetSurvivesRepeatedAbortedStreams(t *testing.T) {
	capacity := (remoteBodyBufferChunks + 4) * remoteBodyChunkSize
	budget := newReadAheadBudget(capacity)
	const lifecycles = 50

	for i := 0; i < lifecycles; i++ {
		body := &progressReader{r: newPatternReader(int64(remoteBodyChunkSize) * (remoteBodyBufferChunks + 8))}
		ctx, cancel := context.WithCancel(context.Background())
		pumpRemoteBody(ctx, body, budget)

		waitForReaderBytes(t, body, int64(remoteBodyChunkSize)*(remoteBodyBufferChunks+1))
		// Abandon without receiving: only the pump's drain can return the
		// queued reservations, which is exactly the prod failure mode.
		cancel()

		waitForBudgetZero(t, budget, 5*time.Second)
		if got := budget.used(); got != 0 {
			t.Fatalf("lifecycle %d: budget used = %d, want 0 (stranded reservation)", i, got)
		}
		if got := budget.peak(); got > budget.capacity() {
			t.Fatalf("lifecycle %d: peak %d exceeded capacity %d", i, got, budget.capacity())
		}
	}
	t.Logf("after %d aborted streams: capacity=%d used=%d peak=%d", lifecycles, budget.capacity(), budget.used(), budget.peak())

	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acquireCancel()
	if err := budget.acquire(acquireCtx, capacity); err != nil {
		t.Fatalf("pool exhausted after %d aborted streams: %v", lifecycles, err)
	}
	budget.release(capacity)
	if got := budget.used(); got != 0 {
		t.Fatalf("budget used after final release = %d, want 0", got)
	}
}

// TestReadAheadBudgetBoundsQueuedReadAhead is the accounting assertion the
// release-at-send design silently broke. A chunk queued in a stream's buffer
// must hold its reservation, so the aggregate pool bounds the sum of every
// stream's queued read-ahead rather than only each stream's momentarily
// in-flight read. With no consumers and a small pool, the pumps must be stopped
// from reading more than the pool holds: if a queued chunk dropped its
// reservation, each stalled stream could fill its 16-chunk buffer with nothing
// counted, and the total read would run to streams x buffer instead of the cap.
func TestReadAheadBudgetBoundsQueuedReadAhead(t *testing.T) {
	const streams = 4
	capacity := 8 * remoteBodyChunkSize
	budget := newReadAheadBudget(capacity)

	bodies := make([]*progressReader, streams)
	cancels := make([]context.CancelFunc, streams)
	// Each source is far larger than the buffer so only the pool can stop the
	// pumps, not end-of-stream.
	for i := range bodies {
		bodies[i] = &progressReader{r: newPatternReader(int64(remoteBodyChunkSize) * 4 * remoteBodyBufferChunks)}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		// The channel is intentionally dropped: no consumer receives, so the
		// pumps block on send once the pool is exhausted.
		_ = pumpRemoteBody(ctx, bodies[i], budget)
	}

	// The pool must actually fill: with queued chunks holding reservations, a
	// pool smaller than streams x buffer is exhausted. Under release-at-send
	// only in-flight reads are counted (at most one chunk per stream), so used
	// would top out at `streams` chunks and never reach the 8-chunk cap.
	waitForBudgetUsed(t, budget, capacity, 5*time.Second)

	var totalRead int64
	for _, body := range bodies {
		totalRead += body.read()
	}
	if used := budget.used(); used > capacity {
		t.Fatalf("used %d exceeded capacity %d", used, capacity)
	}
	// The reservation for a read exists before the read runs, so total bytes
	// read can never exceed what the pool ever held. Allow one in-flight chunk
	// per stream of slack for reads that completed against a reservation about
	// to be counted; the point is the order of magnitude: the broken design
	// reaches streams x full buffer (~64 chunks) here, the pool holds 8.
	upperBound := int64(capacity + streams*remoteBodyChunkSize)
	if totalRead > upperBound {
		t.Fatalf("aggregate read-ahead read %d bytes, want <= %d (capacity %d); the pool is not bounding queued read-ahead",
			totalRead, upperBound, capacity)
	}
	t.Logf("bounds: streams=%d capacity=%d used=%d total_read=%d upper_bound=%d",
		streams, capacity, budget.used(), totalRead, upperBound)

	for _, cancel := range cancels {
		cancel()
	}
	// Only the pump's exit drain retires the queued chunks now; no consumer
	// receives, so this is the abandonment path clearing the pool.
	waitForBudgetZero(t, budget, 5*time.Second)
	if got := budget.used(); got != 0 {
		t.Fatalf("budget used after cancel+drain = %d, want 0", got)
	}
}

// TestRelayAggregateReadAheadBudgetUnderLoad drives many concurrent slow
// consumers through the relay against a fast in-memory source. It validates the
// aggregate cap: the read-ahead pool's peak never exceeds its capacity, which
// the per-stream buffer alone could not bound. It also validates liveness and
// integrity under contention: every consumer receives the full, byte-exact
// source, so the pool applies backpressure without dropping or corrupting
// stream data.
func TestRelayAggregateReadAheadBudgetUnderLoad(t *testing.T) {
	const (
		streams    = 64
		testBudget = 2 << 20 // 2 MiB, 8 chunks: far below streams*perStreamReadAhead
		// Twelve chunks per response: enough that every stream cycles the pool
		// several times. All pumps also reserve a chunk before their first read,
		// so the aggregate demand exceeds the cap from the start and the pool is
		// driven to its full capacity under contention.
		sourceBytes = int64(remoteBodyChunkSize) * 12
		delay       = 250 * time.Microsecond
	)

	relay := NewRelay()
	relay.readAhead = newReadAheadBudget(testBudget)
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{headerContentType: {"video/mp4"}},
			Body:       io.NopCloser(newPatternReader(sourceBytes)),
			Request:    request,
		}, nil
	})}

	writers := make([]*slowPatternWriter, streams)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		writers[i] = &slowPatternWriter{delay: delay}
		wg.Add(1)
		go func(writer *slowPatternWriter) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "http://relay.invalid/source/token/media.bin", nil)
			if err := relay.proxyWithClient(writer, req, "https://1.1.1.1/media.bin", "", relay.client, nil); err != nil {
				// A consumer that sees a write error returns it through its
				// writer; a proxy-level error is recorded here.
				writer.mu.Lock()
				if writer.err == nil {
					writer.err = err
				}
				writer.mu.Unlock()
			}
		}(writers[i])
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("consumers did not finish: deadlock or starvation (peak=%d used=%d)", relay.readAhead.peak(), relay.readAhead.used())
	}

	var progressed int
	for i, writer := range writers {
		offset, status, err := writer.progress()
		if err != nil {
			t.Fatalf("stream %d error: %v (offset=%d)", i, err, offset)
		}
		if status != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, status)
		}
		if offset != sourceBytes {
			t.Fatalf("stream %d received %d bytes, want %d", i, offset, sourceBytes)
		}
		if offset > 0 {
			progressed++
		}
	}

	peak := relay.readAhead.peak()
	if peak > relay.readAhead.capacity() {
		t.Fatalf("aggregate read-ahead peak %d exceeded capacity %d", peak, relay.readAhead.capacity())
	}
	if peak < testBudget/2 {
		t.Fatalf("aggregate read-ahead peak %d never approached the %d-byte cap; the load did not contend", peak, testBudget)
	}
	if got := relay.readAhead.used(); got != 0 {
		t.Fatalf("budget used after all streams finished = %d, want 0 (no leak)", got)
	}
	t.Logf("aggregate read-ahead: streams=%d source_bytes=%d capacity=%d peak=%d progressed=%d/%d",
		streams, sourceBytes, relay.readAhead.capacity(), peak, progressed, streams)
}
