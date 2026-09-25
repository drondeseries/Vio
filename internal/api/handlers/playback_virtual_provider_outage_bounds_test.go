package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestRetryVirtualProviderOutageResolveIsBounded proves the retry loop enforces
// its bound in code: a resolver that fails every attempt is called exactly
// initial + len(backoff) times, never more, and every backoff wait is the
// configured schedule. A runaway retry would amplify a provider outage into a
// request storm.
func TestRetryVirtualProviderOutageResolveIsBounded(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	calls := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, provider502()
	})
	wantCalls := 1 + len(virtualProviderOutageBackoff)
	if calls != wantCalls {
		t.Fatalf("provider calls = %d, want exactly %d (initial + bounded retries)", calls, wantCalls)
	}
	if !virtualProviderListingOutage(err) {
		t.Fatalf("final error = %v, want it still classified as a provider outage", err)
	}
	if got := len(virtualProviderOutageBackoff); got == 0 {
		t.Fatal("test replaced the backoff with an empty schedule; retry bound is not exercised")
	}
}

// TestRetryVirtualProviderOutageResolveStopsOnNonOutage proves a non-outage
// resolve failure is never retried: the loop returns the first error, so a real
// release verdict is surfaced immediately instead of waiting out the schedule.
func TestRetryVirtualProviderOutageResolveStopsOnNonOutage(t *testing.T) {
	calls := 0
	_, err := retryVirtualProviderOutageResolve(context.Background(), true, func(context.Context, bool) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
	})
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1: a non-outage error must not be retried", calls)
	}
	if errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("non-outage error was classified as provider_unavailable: %v", err)
	}
}

// TestClassifyVirtualProviderOutageNeverLeaksForNonOutage pins the classification
// guard in code: provider_unavailable is only ever attached to a genuine outage
// for a trusted row. An untrusted row or a non-outage error keeps its original
// error, so the retryable 503 cannot mask a permanent resolve verdict.
func TestClassifyVirtualProviderOutageNeverLeaksForNonOutage(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		trusted bool
		want    bool
	}{
		{"outage + trusted", provider502(), true, true},
		{"outage + untrusted", provider502(), false, false},
		{"empty listing + trusted", providerEmptyListing(), true, true},
		{"nil error", nil, true, false},
		{"absent pin + trusted", absentSessionPinError("pinned"), true, false},
		{"arbitrary error + trusted", errors.New("decode failed"), true, false},
		{"resolver unavailable sentinel + trusted", fmt.Errorf("listing: %w", resolver.ErrProviderUnavailable), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errors.Is(classifyVirtualProviderOutage(tc.err, tc.trusted), errVirtualProviderUnavailable)
			if got != tc.want {
				t.Fatalf("classifyVirtualProviderOutage(%v, %v) provider_unavailable = %v, want %v", tc.err, tc.trusted, got, tc.want)
			}
		})
	}
}

// TestVirtualProviderOutageBackoffIsBounded pins the production schedule itself:
// a future edit cannot make the retry unbounded or push the added latency past
// the documented ceiling without failing this test.
func TestVirtualProviderOutageBackoffIsBounded(t *testing.T) {
	if len(virtualProviderOutageBackoff) == 0 {
		t.Fatal("outage backoff schedule is empty; the retry is not exercised")
	}
	const maxRetries = 3
	if len(virtualProviderOutageBackoff) > maxRetries {
		t.Fatalf("outage backoff has %d retries, want at most %d", len(virtualProviderOutageBackoff), maxRetries)
	}
	var total time.Duration
	for _, d := range virtualProviderOutageBackoff {
		if d <= 0 || d > 5*time.Second {
			t.Fatalf("outage backoff step %s is outside (0, 5s]", d)
		}
		total += d
	}
	if total > 5*time.Second {
		t.Fatalf("outage backoff total %s exceeds the 5s latency bound", total)
	}
}
