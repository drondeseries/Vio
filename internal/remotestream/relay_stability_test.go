package remotestream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fetchRelayLive drives a registration through the loopback listener that
// Register* started, mirroring a real client. fetchRelay, by contrast, calls
// handle directly with a path relative to the relay base.
func fetchRelayLive(t *testing.T, rawURL, byteRange string) relayFetch {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", rawURL, err)
	}
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	return relayFetch{status: response.StatusCode, header: response.Header.Clone(), body: string(body)}
}

// TestRelayEvictsLeastRecentlyUsedNotOldest proves capacity eviction keeps an
// entry that is older by creation time but recently used, and drops the growing
// set of idle registrations instead.
func TestRelayEvictsLeastRecentlyUsedNotOldest(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	source, _ := url.Parse("https://1.1.1.1/evict")
	base := time.Now().Add(-time.Hour)
	relay.mu.Lock()
	for i := 0; i < relayMaxEntries; i++ {
		entry := &relayEntry{
			source: source, baseName: "stream", createdAt: base.Add(time.Duration(i) * time.Second),
		}
		if i == 0 {
			// old-0 is the oldest by creation but the only one in active use.
			entry.lastAccess = time.Now()
		}
		relay.entries["lru-old-"+strconv.Itoa(i)] = entry
	}
	relay.mu.Unlock()

	if _, release, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/lru-new"); err != nil {
		t.Fatalf("RegisterInsecure at capacity: %v", err)
	} else {
		defer release()
	}

	relay.mu.Lock()
	size := len(relay.entries)
	_, oldestUsedPresent := relay.entries["lru-old-0"]
	_, idleEvictedPresent := relay.entries["lru-old-1"]
	relay.mu.Unlock()
	if size != relayMaxEntries {
		t.Fatalf("entries = %d, want %d", size, relayMaxEntries)
	}
	if !oldestUsedPresent {
		t.Fatal("the recently used oldest entry was evicted before idle ones")
	}
	if idleEvictedPresent {
		t.Fatal("an idle entry survived while a recently used one should be kept")
	}
}

// TestRelayEntryLifetimeSlidesOnPresentation proves a live presentation extends
// the entry's sliding lifetime, so an actively played stream is not dropped just
// because it outlived its registration-time bound.
func TestRelayEntryLifetimeSlidesOnPresentation(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	relayURL, release, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/media.mkv")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer release()
	token, ok := relayTokenFromURL(relayURL)
	if !ok {
		t.Fatalf("relay URL %q has no token", relayURL)
	}

	base := time.Unix(1_700_000_000, 0)
	relay.mu.Lock()
	relay.entries[token].expiresAt = base
	relay.mu.Unlock()

	presented, ok := relay.entryForRequestLocked(token, base.Add(-time.Second))
	if !ok {
		t.Fatal("entry just before expiry was rejected")
	}
	want := base.Add(-time.Second).Add(relayEntryLifetime)
	if !presented.expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v (slid forward)", presented.expiresAt, want)
	}
	if _, ok := relay.entryForRequestLocked(token, base.Add(time.Second)); !ok {
		t.Fatal("entry expired at its original bound despite a live presentation")
	}
}

// TestRelayReusesLiveEntryForSameContent proves a re-registration of the same
// content shares the live entry and its token instead of minting a parallel
// upstream, and that the shared entry survives one holder's release.
func TestRelayReusesLiveEntryForSameContent(t *testing.T) {
	var calls int32
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return relayResponse(request, http.StatusOK, "video/mp4", "ok"), nil
	})}

	urlFirst, releaseFirst, err := relay.Register(context.Background(), "https://1.1.1.1/movie.mp4")
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	urlSecond, releaseSecond, err := relay.Register(context.Background(), "https://1.1.1.1/movie.mp4")
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if urlFirst != urlSecond {
		t.Fatalf("same content registered two handles: %q and %q", urlFirst, urlSecond)
	}

	relay.mu.Lock()
	size := len(relay.entries)
	token, _ := relayTokenFromURL(urlFirst)
	refs := relay.entries[token].refs
	relay.mu.Unlock()
	if size != 1 || refs != 2 {
		t.Fatalf("shared registration = %d entries, refs %d; want 1 entry, refs 2", size, refs)
	}

	releaseFirst()
	releaseFirst() // idempotent
	relay.mu.Lock()
	_, stillLive := relay.entries[token]
	relay.mu.Unlock()
	if !stillLive {
		t.Fatal("one holder's release removed the shared entry")
	}
	if got := fetchRelayLive(t, urlSecond, ""); got.status != http.StatusOK {
		t.Fatalf("shared entry status after first release = %d", got.status)
	}

	releaseSecond()
	relay.mu.Lock()
	_, gone := relay.entries[token]
	relay.mu.Unlock()
	if gone {
		t.Fatal("shared entry survived the last release")
	}
	if got := fetchRelayLive(t, urlSecond, ""); got.status != http.StatusNotFound {
		t.Fatalf("shared entry status after last release = %d, want 404", got.status)
	}
}

// TestRelayKeepsDistinctContentSeparate proves content addressing does not merge
// registrations that differ in the provider URL or in a forwarded header that
// can change the response.
func TestRelayKeepsDistinctContentSeparate(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	urlA, releaseA, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/movie.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer releaseA()
	urlB, releaseB, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/other.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer releaseB()
	urlC, releaseC, err := relay.RegisterInsecureWithHeaders(context.Background(), "https://1.1.1.1/movie.mp4", map[string]string{"Referer": "https://provider.example/"})
	if err != nil {
		t.Fatalf("RegisterInsecureWithHeaders: %v", err)
	}
	defer releaseC()

	if urlA == urlB || urlA == urlC || urlB == urlC {
		t.Fatalf("distinct content shared a handle: %q %q %q", urlA, urlB, urlC)
	}
}

// TestRelayRangeCacheSurvivesTokenRotation proves a byte range cached for a
// source is reused after the registration token rotates, because the cache is
// scoped by the credential-free content key rather than the token.
func TestRelayRangeCacheSurvivesTokenRotation(t *testing.T) {
	var calls int32
	body := strings.Repeat("r", 32)
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.rangeCache.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-31/1000")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		response.Header.Set("Cache-Control", "max-age=60")
		return response, nil
	})}

	firstURL, releaseFirst, err := relay.Register(context.Background(), "https://1.1.1.1/media.mkv")
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if got := fetchRelayLive(t, firstURL, "bytes=0-31"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("first fetch = status %d body %q", got.status, got.body)
	}
	releaseFirst()

	secondURL, releaseSecond, err := relay.Register(context.Background(), "https://1.1.1.1/media.mkv")
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	defer releaseSecond()
	if firstURL == secondURL {
		t.Fatal("re-registration after release reused the token; rotation not exercised")
	}
	if got := fetchRelayLive(t, secondURL, "bytes=0-31"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("post-rotation fetch = status %d body %q", got.status, got.body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the cached range must survive token rotation)", got)
	}
}

// TestRelaySealedReferenceOutlivesItsSealTimeTTLWhileParentLives proves a child
// resource reference stays usable after the TTL it was sealed with, as long as
// the parent session still holds a live registration, and is refused once the
// parent goes.
func TestRelaySealedReferenceOutlivesItsSealTimeTTLWhileParentLives(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	now := time.Unix(1_700_000_000, 0)
	const source = "https://1.1.1.1/segment.ts?token=provider-secret"
	parent := &relayEntry{
		source:     &url.URL{Scheme: "https", Host: "1.1.1.1", Path: "/master.m3u8"},
		baseName:   "stream.m3u8",
		token:      "parent",
		createdAt:  now,
		lastAccess: now,
		expiresAt:  now.Add(2 * relayEntryLifetime), // slid forward by active playback
		refs:       1,
	}
	relay.mu.Lock()
	relay.entries["parent"] = parent
	relay.mu.Unlock()

	opaque, err := relay.sealReference("parent", source, now)
	if err != nil {
		t.Fatal(err)
	}

	// The seal-time TTL has passed, but the parent session is live.
	later := now.Add(relayEntryLifetime + time.Minute)
	opened, err := relay.openReference("parent", opaque, later)
	if err != nil || opened != source {
		t.Fatalf("openReference while parent live = %q, %v; want the child resolved", opened, err)
	}
	relay.mu.Lock()
	slid := parent.expiresAt
	relay.mu.Unlock()
	if want := later.Add(relayEntryLifetime); !slid.Equal(want) {
		t.Fatalf("parent expiry = %v, want %v (extended by the child fetch)", slid, want)
	}

	relay.mu.Lock()
	delete(relay.entries, "parent")
	delete(relay.content, parent.contentKey)
	relay.mu.Unlock()
	if _, err := relay.openReference("parent", opaque, later.Add(time.Minute)); err == nil {
		t.Fatal("expired child reference opened after the parent was released")
	}
}
