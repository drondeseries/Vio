package resolver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// countingProvider is an httptest Stremio provider. It always answers
// /manifest.json with a valid manifest and counts stream-endpoint requests so
// tests can assert provider call counts instead of sleeping.
type countingProvider struct {
	server  *httptest.Server
	handler http.HandlerFunc

	mu    sync.Mutex
	count int
}

func newCountingProvider(t *testing.T, streamHandler http.HandlerFunc) *countingProvider {
	t.Helper()
	p := &countingProvider{handler: streamHandler}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`)
			return
		}
		p.mu.Lock()
		p.count++
		p.mu.Unlock()
		p.handler(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *countingProvider) manifestURL() string { return p.server.URL + "/manifest.json" }

func (p *countingProvider) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// mutableStreams is a concurrency-safe provider answer that a test can swap
// between fetches.
type mutableStreams struct {
	mu      sync.Mutex
	streams []StreamCandidate
}

func (m *mutableStreams) set(streams []StreamCandidate) {
	m.mu.Lock()
	m.streams = append([]StreamCandidate(nil), streams...)
	m.mu.Unlock()
}

func (m *mutableStreams) get() []StreamCandidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StreamCandidate(nil), m.streams...)
}

func writeStreams(w http.ResponseWriter, streams []StreamCandidate) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stremioResponse{Streams: streams})
}

func testConfig(p *countingProvider) Config {
	return Config{
		ManifestURL:   p.manifestURL(),
		AllowInsecure: true,
	}
}

// fakeClassifier implements CandidateClassifier with predicates so a test can
// decide which candidates a source of truth confirms or fails.
type fakeClassifier struct {
	confirm func(StreamCandidate) bool
	fail    func(StreamCandidate) bool
}

func (f fakeClassifier) ClassifyCandidates(candidates []StreamCandidate) {
	for i := range candidates {
		if f.confirm != nil && f.confirm(candidates[i]) {
			candidates[i].SourceConfirmed = true
		}
		if f.fail != nil && f.fail(candidates[i]) {
			candidates[i].SourceFailed = true
		}
	}
}

// waitFor polls an observable condition until it holds or the timeout expires.
// Tests wait on state (request counters, cache contents) rather than fixed
// delays.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
