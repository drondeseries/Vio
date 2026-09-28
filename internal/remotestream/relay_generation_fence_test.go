package remotestream

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A byte range cached for one relay registration must not be replayed to a
// replacement registration of the same source and Range. The registration is
// the relay's generation/incarnation: a restart or provider re-resolve registers
// a fresh token, so a displaced generation's cached range bytes can never be
// read by its replacement.
func TestRelayRangeCacheFencesRegistrations(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	body := strings.Repeat("g", 32)

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.rangeCache.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-31/1000")
		response.Header.Set("Content-Length", "32")
		return response, nil
	})}

	firstURL, firstCleanup := registerRelayForTest(t, relay, "generation-a", "https://1.1.1.1/media.mkv")
	defer firstCleanup()
	if got := fetchRelay(t, relay, firstURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("first registration fetch = status %d, %d bytes", got.status, len(got.body))
	}
	// A repeat within the same registration is a byte-exact cache hit.
	if got := fetchRelay(t, relay, firstURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("within-registration fetch = status %d, %d bytes", got.status, len(got.body))
	}
	mu.Lock()
	firstCalls := calls
	mu.Unlock()
	if firstCalls != 1 {
		t.Fatalf("within-registration upstream calls = %d, want 1", firstCalls)
	}

	// The replacement generation registers the same upstream URL and Range under
	// a new token; the displaced entry must not answer it.
	secondURL, secondCleanup := registerRelayForTest(t, relay, "generation-b", "https://1.1.1.1/media.mkv")
	defer secondCleanup()
	if got := fetchRelay(t, relay, secondURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("replacement registration fetch = status %d, %d bytes", got.status, len(got.body))
	}
	mu.Lock()
	total := calls
	mu.Unlock()
	if total != 2 {
		t.Fatalf("upstream calls = %d, want 2 (a replacement registration must not reuse a displaced generation's cached range)", total)
	}
}

// relayRangeCacheKey refuses a request with no registration, so unregistered
// proxy traffic never populates or reads the cache.
func TestRelayRangeCacheKeyRequiresRegistration(t *testing.T) {
	target, err := url.Parse("https://1.1.1.1/media.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if key := relayRangeCacheKey(target, "bytes=0-31", "identity", ""); key != "" {
		t.Fatalf("unregistered key = %q, want empty", key)
	}
	if key := relayRangeCacheKey(target, "", "identity", "generation-a"); key != "" {
		t.Fatalf("rangeless key = %q, want empty", key)
	}
	if key := relayRangeCacheKey(nil, "bytes=0-31", "identity", "generation-a"); key != "" {
		t.Fatalf("nil target key = %q, want empty", key)
	}
}
