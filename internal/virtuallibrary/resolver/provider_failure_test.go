package resolver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGetCandidatesFreshProviderHTTPError proves a provider error is surfaced,
// not flattened into an empty successful answer. GetCandidatesFresh on a cold
// cache fetches synchronously with the caller's context.
func TestGetCandidatesFreshProviderHTTPError(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	r := New(testConfig(p))

	candidates, _, _, err := r.GetCandidatesFresh(context.Background(), "virtual://movie/tt1")
	if err == nil {
		t.Fatalf("GetCandidatesFresh = %+v, nil; want an error", candidates)
	}
	if candidates != nil {
		t.Fatalf("candidates = %+v, want nil on provider error", candidates)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want it to name the provider status", err)
	}
}

// TestGetCandidatesFreshMalformedPayload proves an undecodable body is an
// error, so callers cannot mistake a broken provider for "no sources".
func TestGetCandidatesFreshMalformedPayload(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"streams":`)
	})
	r := New(testConfig(p))

	candidates, _, _, err := r.GetCandidatesFresh(context.Background(), "virtual://movie/tt1")
	if err == nil {
		t.Fatalf("GetCandidatesFresh = %+v, nil; want an error", candidates)
	}
	if !strings.Contains(err.Error(), "decode streaming provider response") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
}

// TestGetCandidatesFreshTimeout proves a caller deadline bounds the synchronous
// fetch instead of hanging or succeeding with partial data. The provider waits
// for its request context to be canceled, so the test does not linger.
func TestGetCandidatesFreshTimeout(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	r := New(testConfig(p))

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.GetCandidatesFresh(ctx, "virtual://movie/tt1")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("GetCandidatesFresh = nil error, want a deadline error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetCandidatesFresh ignored the context deadline")
	}
}

// TestGetCandidatesFreshEmptyListIsEmptySuccess documents the genuine empty
// answer: it is not an error, and it is cached as a negative so it is not
// re-fetched immediately (covered further in the cache tests).
func TestGetCandidatesFreshEmptyListIsEmptySuccess(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeStreams(w, []StreamCandidate{})
	})
	r := New(testConfig(p))

	candidates, _, _, err := r.GetCandidatesFresh(context.Background(), "virtual://movie/tt1")
	if err != nil {
		t.Fatalf("GetCandidatesFresh = %v, want nil for an empty provider list", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want an empty list", candidates)
	}
}
