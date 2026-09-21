package remotestream

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// defaultResolutionCacheTTL bounds how long a positive host resolution is
	// reused. Provider media URLs are validated on every play start and again
	// on the serve path; a short TTL removes the per-play DNS round-trip a
	// restart or a seek storm would otherwise repeat while still bounding how
	// long a host that later rebinds to a private address can be served from
	// the cached (public) answer. Three minutes is comfortably longer than the
	// repeated validations inside one playback session and is of the same order
	// as typical provider DNS TTLs.
	defaultResolutionCacheTTL = 3 * time.Minute
	// defaultResolutionCacheMax caps retained hosts. The cache is a latency
	// optimization, not state: an evicted host is simply re-resolved. 1024 is
	// far above the concurrent provider-host set of a household or a modest
	// deployment, so size eviction only fires under pathological churn.
	defaultResolutionCacheMax = 1024
)

// resolutionCacheEntry is one positive, already-public resolution.
type resolutionCacheEntry struct {
	addresses []netip.Addr
	expiresAt time.Time
}

// resolutionCache caches only successful host -> public-address resolutions.
// Negative results are never stored: a transient DNS failure must not keep a
// host unreachable for the TTL. Entries expire by TTL and the map is size-capped
// so a long-lived process cannot accumulate one entry per host ever seen. All
// methods are safe for concurrent use.
type resolutionCache struct {
	mu      sync.Mutex
	entries map[string]resolutionCacheEntry
	ttl     time.Duration
	max     int
	// now is injectable so tests can expire entries deterministically.
	now func() time.Time
}

func newResolutionCache(ttl time.Duration, max int) *resolutionCache {
	if ttl <= 0 {
		ttl = defaultResolutionCacheTTL
	}
	if max <= 0 {
		max = defaultResolutionCacheMax
	}
	return &resolutionCache{
		entries: make(map[string]resolutionCacheEntry),
		ttl:     ttl,
		max:     max,
		now:     time.Now,
	}
}

// defaultResolutionCache is the process-wide cache ValidateURL uses.
var defaultResolutionCache = newResolutionCache(defaultResolutionCacheTTL, defaultResolutionCacheMax)

// lookup returns a copy of the cached addresses for host while the entry is
// fresh. An expired entry is dropped on read.
func (c *resolutionCache) lookup(host string) ([]netip.Addr, bool) {
	if c == nil || host == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[host]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, host)
		return nil, false
	}
	return append([]netip.Addr(nil), entry.addresses...), true
}

// store records a successful public resolution. The caller must only pass
// addresses that already passed the forbidden-address check.
func (c *resolutionCache) store(host string, addresses []netip.Addr) {
	if c == nil || host == "" || len(addresses) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked()
	if len(c.entries) >= c.max {
		c.evictSoonestLocked()
	}
	c.entries[host] = resolutionCacheEntry{
		addresses: append([]netip.Addr(nil), addresses...),
		expiresAt: c.now().Add(c.ttl),
	}
}

// evictExpiredLocked drops every entry whose TTL has lapsed. Called on store so
// the map is swept as it grows rather than requiring a background goroutine.
func (c *resolutionCache) evictExpiredLocked() {
	now := c.now()
	for host, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, host)
		}
	}
}

// evictSoonestLocked makes room by dropping the entry closest to expiry. Called
// only when the map is at capacity after the expired sweep.
func (c *resolutionCache) evictSoonestLocked() {
	var soonestHost string
	var soonest time.Time
	for host, entry := range c.entries {
		if soonestHost == "" || entry.expiresAt.Before(soonest) {
			soonestHost, soonest = host, entry.expiresAt
		}
	}
	if soonestHost != "" {
		delete(c.entries, soonestHost)
	}
}

// len reports the current number of retained entries. Test-only observability.
func (c *resolutionCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
