// internal/subtitles/ratelimit.go
package subtitles

import (
	"context"
	"sync"
	"time"
)

// RateLimiterConfig holds rate limit parameters for a provider.
type RateLimiterConfig struct {
	MaxRequests int
	Window      time.Duration
}

// RateLimiter implements a sliding window rate limiter.
type RateLimiter struct {
	mu         sync.Mutex
	config     RateLimiterConfig
	timestamps []time.Time
}

// NewRateLimiter creates a rate limiter with the given config.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	return &RateLimiter{
		config:     cfg,
		timestamps: make([]time.Time, 0, cfg.MaxRequests),
	}
}

// Allow checks if a request is allowed right now. Returns true if within limits.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.pruneExpired()
	if len(rl.timestamps) >= rl.config.MaxRequests {
		return false
	}
	rl.timestamps = append(rl.timestamps, time.Now())
	return true
}

// Wait blocks until a request is allowed or context is canceled.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		if rl.Allow() {
			return nil
		}
		rl.mu.Lock()
		var waitUntil time.Time
		if len(rl.timestamps) > 0 {
			waitUntil = rl.timestamps[0].Add(rl.config.Window)
		} else {
			waitUntil = time.Now()
		}
		rl.mu.Unlock()

		delay := time.Until(waitUntil)
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (rl *RateLimiter) pruneExpired() {
	cutoff := time.Now().Add(-rl.config.Window)
	i := 0
	for i < len(rl.timestamps) && rl.timestamps[i].Before(cutoff) {
		i++
	}
	rl.timestamps = rl.timestamps[i:]
}
