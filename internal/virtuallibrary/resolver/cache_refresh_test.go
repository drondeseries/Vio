package resolver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func cacheEntry(t *testing.T, r *Resolver, key string) candidateCacheEntry {
	t.Helper()
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		t.Fatalf("cache entry %q is missing", key)
	}
	return entry
}

func mutateCacheEntry(t *testing.T, r *Resolver, key string, mutate func(*candidateCacheEntry)) {
	t.Helper()
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		t.Fatalf("cache entry %q is missing", key)
	}
	mutate(&entry)
	r.cache[key] = entry
}

func TestGetCandidatesServesCacheWithoutProviderCall(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	first, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(first) != 1 {
		t.Fatalf("first GetCandidates: count=%d err=%v, want 1", len(first), err)
	}

	// A different provider answer must be invisible to a cache hit.
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})
	second, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(second) != 1 {
		t.Fatalf("second GetCandidates: count=%d err=%v, want 1", len(second), err)
	}
	if second[0].URL != first[0].URL {
		t.Fatalf("second answer URL = %q, want the cached %q", second[0].URL, first[0].URL)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (cache hit)", got)
	}
}

// TestGetCandidatesFreshServesWithinFreshServeFloor pins the budget that keeps
// several resolve rounds inside one playback start on one provider round-trip.
func TestGetCandidatesFreshServesWithinFreshServeFloor(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}

	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})
	got, _, _, err := r.GetCandidatesFresh(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("GetCandidatesFresh: count=%d err=%v, want the cached 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/one.mkv" {
		t.Fatalf("URL = %q, want the cached answer within the fresh-serve floor", got[0].URL)
	}
	if requests := p.requests(); requests != 1 {
		t.Fatalf("provider requests = %d, want 1 (within fresh-serve floor)", requests)
	}
}

func TestGetCandidatesFreshRefetchesAfterFloor(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	// Age the entry past the floor; a forced lookup must now take the full
	// fetch path.
	mutateCacheEntry(t, r, "movie|tt1", func(entry *candidateCacheEntry) {
		entry.fetchedAt = time.Now().Add(-freshServeFloor - time.Second)
	})
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})

	got, _, _, err := r.GetCandidatesFresh(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("GetCandidatesFresh: count=%d err=%v, want 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/two.mkv" {
		t.Fatalf("URL = %q, want the refetched answer", got[0].URL)
	}
	if requests := p.requests(); requests != 2 {
		t.Fatalf("provider requests = %d, want 2", requests)
	}
}

// TestGetCandidatesFreshUnboundedBypassesFloor covers the genuine re-list path
// used to escape dead candidates: it must not be served the entry the caller
// is trying to replace.
func TestGetCandidatesFreshUnboundedBypassesFloor(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})

	got, _, _, err := r.GetCandidatesFreshUnbounded(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("GetCandidatesFreshUnbounded: count=%d err=%v, want 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/two.mkv" {
		t.Fatalf("URL = %q, want the unbounded refetch", got[0].URL)
	}
	if requests := p.requests(); requests != 2 {
		t.Fatalf("provider requests = %d, want 2", requests)
	}
}

func TestNegativeCacheCoversEmptyProviderAnswer(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	first, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(first) != 0 {
		t.Fatalf("first GetCandidates: count=%d err=%v, want empty with no error", len(first), err)
	}
	// A newly added source would be picked up after negativeCacheTTL; within it
	// the empty answer is reused without another provider round-trip.
	answer.set([]StreamCandidate{{URL: "https://cdn.example/late.mkv", Name: "Late.Release.1080p"}})
	second, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(second) != 0 {
		t.Fatalf("second GetCandidates: count=%d err=%v, want the negative-cached empty answer", len(second), err)
	}
	if requests := p.requests(); requests != 1 {
		t.Fatalf("provider requests = %d, want 1 (negative cache)", requests)
	}
}

// TestStalePositiveGraceServesStaleAndRefreshesInBackground covers the
// background-refresh path: an expired-but-in-grace positive answer is served
// immediately while one refresh repopulates it.
func TestStalePositiveGraceServesStaleAndRefreshesInBackground(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	mutateCacheEntry(t, r, "movie|tt1", func(entry *candidateCacheEntry) {
		entry.expiresAt = time.Now().Add(-time.Second)
	})
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})

	got, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("stale-serve GetCandidates: count=%d err=%v, want 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/one.mkv" {
		t.Fatalf("URL = %q, want the stale-but-in-grace answer", got[0].URL)
	}

	waitFor(t, 5*time.Second, "background refresh to replace the stale entry", func() bool {
		r.cacheMu.Lock()
		defer r.cacheMu.Unlock()
		entry, ok := r.cache["movie|tt1"]
		return ok && len(entry.candidates) == 1 && entry.candidates[0].URL == "https://cdn.example/two.mkv"
	})
	if requests := p.requests(); requests != 2 {
		t.Fatalf("provider requests = %d, want 2 (stale serve + one background refresh)", requests)
	}
}

// TestPastStaleGraceFetchesFreshSynchronously covers the foreground blocking
// path used once the stale grace has elapsed.
func TestPastStaleGraceFetchesFreshSynchronously(t *testing.T) {
	answer := &mutableStreams{}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	mutateCacheEntry(t, r, "movie|tt1", func(entry *candidateCacheEntry) {
		entry.expiresAt = time.Now().Add(-candidateStaleGrace - time.Second)
	})
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})

	got, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("past-grace GetCandidates: count=%d err=%v, want 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/two.mkv" {
		t.Fatalf("URL = %q, want the synchronously fetched answer", got[0].URL)
	}
	if requests := p.requests(); requests != 2 {
		t.Fatalf("provider requests = %d, want 2", requests)
	}
}

func TestCandidateCacheEntryCapEvictsOldest(t *testing.T) {
	r := New(Config{})
	base := time.Now()
	candidate := StreamCandidate{URL: "https://cdn.example/x.mkv", Name: "X"}

	total := maxCandidateCacheEntries + 20
	for i := 0; i < total; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		key := fmt.Sprintf("movie|entry-%d", i)
		r.storeCandidateCache(key, []StreamCandidate{candidate}, at.Add(time.Hour), at, 0)
	}

	r.cacheMu.Lock()
	entries := len(r.cache)
	_, oldestPresent := r.cache["movie|entry-0"]
	_, newestPresent := r.cache[fmt.Sprintf("movie|entry-%d", total-1)]
	r.cacheMu.Unlock()

	if entries != maxCandidateCacheEntries {
		t.Fatalf("cache entries = %d, want the cap %d", entries, maxCandidateCacheEntries)
	}
	if oldestPresent {
		t.Fatal("oldest entry was not evicted")
	}
	if !newestPresent {
		t.Fatal("newest entry was evicted")
	}
}

func TestCandidateCacheByteCapEvicts(t *testing.T) {
	r := New(Config{})
	base := time.Now()
	big := StreamCandidate{URL: "https://cdn.example/x.mkv", Name: strings.Repeat("x", 1<<20)}

	const inserts = 20
	for i := 0; i < inserts; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		r.storeCandidateCache(fmt.Sprintf("movie|big-%d", i), []StreamCandidate{big}, at.Add(time.Hour), at, 0)
	}

	r.cacheMu.Lock()
	entries := len(r.cache)
	bytes := r.cacheBytes
	r.cacheMu.Unlock()

	if bytes > maxCandidateCacheBytes {
		t.Fatalf("cache bytes = %d, want <= %d", bytes, maxCandidateCacheBytes)
	}
	if entries >= inserts {
		t.Fatalf("cache entries = %d, want the byte cap to evict below %d", entries, inserts)
	}
}

func TestCandidateCacheRejectsSingleOversizedEntry(t *testing.T) {
	r := New(Config{})
	huge := StreamCandidate{
		URL:  "https://cdn.example/x.mkv",
		Name: strings.Repeat("x", maxCandidateCacheBytes+1),
	}
	now := time.Now()
	r.storeCandidateCache("movie|huge", []StreamCandidate{huge}, now.Add(time.Hour), now, 0)

	r.cacheMu.Lock()
	entries := len(r.cache)
	r.cacheMu.Unlock()
	if entries != 0 {
		t.Fatalf("cache entries = %d, want 0 (entry exceeds the byte budget)", entries)
	}
}

// TestConcurrentRequestsJoinSingleFlight proves two concurrent cold requests
// share one provider round-trip: the provider is held open until both callers
// are parked, then released, and must have seen exactly one request.
func TestConcurrentRequestsJoinSingleFlight(t *testing.T) {
	answer := []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}}
	release := make(chan struct{})
	firstHit := make(chan struct{})
	var once sync.Once
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(firstHit) })
		<-release
		writeStreams(w, answer)
	})
	r := New(testConfig(p))
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([][]StreamCandidate, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, _, errs[i] = r.GetCandidates(ctx, "virtual://movie/tt1")
		}(i)
	}

	select {
	case <-firstHit:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}
	close(release)
	wg.Wait()

	if requests := p.requests(); requests != 1 {
		t.Fatalf("provider requests = %d, want 1 (callers must join the in-flight fetch)", requests)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if len(results[i]) != 1 || results[i][0].URL != answer[0].URL {
			t.Fatalf("caller %d result = %+v, want %+v", i, results[i], answer)
		}
	}
}
