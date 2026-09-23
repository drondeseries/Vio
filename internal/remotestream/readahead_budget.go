package remotestream

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// relayReadAheadBudgetBytes bounds the total read-ahead the relay queues across
// every active stream on one relay.
//
// remoteBodyBufferChunks already bounds a single stream (~4 MiB), but it cannot
// bound the sum: 1000 stalled streams at the per-stream cap still hold
// gigabytes. 64 MiB is the aggregate ceiling. It lets roughly sixteen streams
// sit at their full per-stream read-ahead before the rest feel backpressure,
// which covers a household or a bounded transcode fan-out, while a thousand
// stalled streams hold 64 MiB of queued payload instead of ~4 GiB. Like the
// other relay knobs this is a compile-time constant rather than a setting:
// changing it is a capacity decision for the whole process, not a per-request
// preference.
const relayReadAheadBudgetBytes = 64 << 20

// readAheadBudget is a byte-weighted semaphore shared by every stream on one
// relay. A producer reserves a chunk's worth before it reads from the upstream
// and holds that reservation for as long as the chunk sits queued for the
// consumer; the queue is the read-ahead, so a thousand stalled streams hold the
// pool's worth of queued payload rather than a buffer's worth each. The
// consumer returns the reservation when it dequeues the chunk, and the producer
// returns it on abort if the chunk was never delivered. When the pool is
// exhausted an acquire blocks, which stops the upstream read and lets TCP
// backpressure reach the provider instead of buffering more locally.
// Cancellation unblocks the waiter and returns its reservation. It never drops
// bytes and never strands them: a reserved chunk is released exactly once, by
// its single receiver (the consumer) or by the producer on abort or on the
// exit drain for an abandoned stream.
type readAheadBudget struct {
	sem           *semaphore.Weighted
	capacityBytes int
	usedBytes     atomic.Int64
	peakBytes     atomic.Int64
}

// newReadAheadBudget returns a pool of capacityBytes, or the relay default when
// capacityBytes is not positive.
func newReadAheadBudget(capacityBytes int) *readAheadBudget {
	if capacityBytes <= 0 {
		capacityBytes = relayReadAheadBudgetBytes
	}
	return &readAheadBudget{
		sem:           semaphore.NewWeighted(int64(capacityBytes)),
		capacityBytes: capacityBytes,
	}
}

// acquire reserves n bytes, blocking until they are free or ctx is done. An
// error means no reservation was made.
func (b *readAheadBudget) acquire(ctx context.Context, n int) error {
	if b == nil || n <= 0 {
		return nil
	}
	if err := b.sem.Acquire(ctx, int64(n)); err != nil {
		return err
	}
	used := b.usedBytes.Add(int64(n))
	for {
		peak := b.peakBytes.Load()
		if used <= peak || b.peakBytes.CompareAndSwap(peak, used) {
			break
		}
	}
	return nil
}

// release returns n bytes to the pool. It must balance an acquire.
func (b *readAheadBudget) release(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.usedBytes.Add(-int64(n))
	b.sem.Release(int64(n))
}

// used reports the bytes currently reserved.
func (b *readAheadBudget) used() int {
	if b == nil {
		return 0
	}
	return int(b.usedBytes.Load())
}

// capacity reports the pool size in bytes.
func (b *readAheadBudget) capacity() int {
	if b == nil {
		return 0
	}
	return b.capacityBytes
}

// peak reports the high-water mark of used since the pool was created. It is
// the observable aggregate read-ahead residency the load test asserts on.
func (b *readAheadBudget) peak() int {
	if b == nil {
		return 0
	}
	return int(b.peakBytes.Load())
}
