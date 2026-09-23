package remotestream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
		t.Fatal("cancelled waiter never returned")
	}
	if got, want := b.used(), remoteBodyChunkSize; got != want {
		t.Fatalf("used after cancelled waiter = %d, want only the holder's %d", got, want)
	}

	// The holder's release drains the pool exactly; the cancelled waiter left
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

	chunksCh := pumpRemoteBody(context.Background(), body, budget)
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

	chunksCh := pumpRemoteBody(context.Background(), body, budget)
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
		// several times.
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
