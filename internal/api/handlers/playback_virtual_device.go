package handlers

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func (h *PlaybackHandler) requestDeviceCapabilities(r *http.Request) (plugins.DeviceCapabilities, bool) {
	if h == nil || h.DeviceCapabilitySource == nil || r == nil {
		return plugins.DeviceCapabilities{}, false
	}
	return h.DeviceCapabilitySource.DeviceCapabilitiesFor(r.Context(), apimw.GetProfileID(r.Context()), requestDeviceID(r))
}

// rankVirtualCandidatesForDevice ranks the candidate list for the requesting
// device using its persisted capability profile. When no profile is known the
// provider returns candidates unchanged (device order), preserving the
// pre-feature resolution-label behavior.
func (h *PlaybackHandler) rankVirtualCandidatesForDevice(r *http.Request, streams []VirtualPlaybackStream) ([]VirtualPlaybackStream, plugins.DeviceCapabilities) {
	if len(streams) == 0 {
		return streams, plugins.DeviceCapabilities{}
	}
	capabilities, ok := h.requestDeviceCapabilities(r)
	if !ok {
		return streams, plugins.DeviceCapabilities{}
	}
	meta := make([]plugins.VirtualStreamMetadata, len(streams))
	for i := range streams {
		meta[i] = &streams[i]
	}
	ranked := plugins.RankVirtualStreamsForDevice(meta, capabilities)
	out := make([]VirtualPlaybackStream, len(ranked))
	for i := range ranked {
		stream, ok := ranked[i].(*VirtualPlaybackStream)
		if !ok {
			return streams, capabilities
		}
		out[i] = *stream
	}
	return out, capabilities
}

// requestDeviceID returns the durable device ID the web/app clients send on
// every request (X-Silo-Device-Id). Empty means "no device identity": ranking
// then falls back to provider order.
func requestDeviceID(r *http.Request) string {
	return clampHeaderValue(r.Header.Get(deviceIDHeader), 128)
}

// providerDeviceCapabilitySource implements plugins.DeviceCapabilityProfileSource
// on top of the shared UserStoreProvider, with a short TTL cache so Postgres is
// never on the playback/pre-warm critical path. It reads the native user ID
// from the request context (which the UserStoreProvider is scoped by).
type providerDeviceCapabilitySource struct {
	provider userstore.UserStoreProvider
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]providerCapabilityEntry
}

func (s *providerDeviceCapabilitySource) CapabilitiesForProfile(profileID string) plugins.DeviceCapabilities {
	if s == nil || s.provider == nil || profileID == "" {
		return plugins.DeviceCapabilities{}
	}
	// The playback handler's ranking hook has no request context. Keep the
	// profile-only fallback conservative; request-scoped callers use the full
	// DeviceCapabilitiesFor method below.
	return plugins.DeviceCapabilities{}
}

type providerCapabilityEntry struct {
	device    plugins.DeviceCapabilities
	found     bool
	expiresAt time.Time
}

// newProviderDeviceCapabilitySource creates a source with a 5-minute found TTL
// and 1-minute misses.
func NewProviderDeviceCapabilitySource(provider userstore.UserStoreProvider) *providerDeviceCapabilitySource {
	return &providerDeviceCapabilitySource{
		provider: provider,
		ttl:      5 * time.Minute,
		now:      time.Now,
		entries:  make(map[string]providerCapabilityEntry),
	}
}

// DeviceCapabilitiesFor returns the persisted capability profile for the
// (user, profile, device) encoded in ctx + profileID + deviceID.
func (s *providerDeviceCapabilitySource) DeviceCapabilitiesFor(ctx context.Context, profileID, deviceID string) (plugins.DeviceCapabilities, bool) {
	if s == nil || s.provider == nil || profileID == "" || deviceID == "" {
		return plugins.DeviceCapabilities{}, false
	}
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return plugins.DeviceCapabilities{}, false
	}
	key := userDeviceKey(userID, profileID, deviceID)
	now := s.now()
	s.mu.Lock()
	entry, ok := s.entries[key]
	s.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.device, entry.found
	}

	store, err := s.provider.ForUser(ctx, userID)
	if err != nil || store == nil {
		return plugins.DeviceCapabilities{}, false
	}
	registry, ok := store.(userstore.DeviceProfileRegistry)
	if !ok {
		return plugins.DeviceCapabilities{}, false
	}
	profile, err := registry.GetDeviceProfile(ctx, profileID, deviceID)
	if err != nil || profile == nil {
		return plugins.DeviceCapabilities{}, false
	}
	device := plugins.DeviceCapabilitiesFromProfile(*profile)
	ttl := s.ttl
	s.mu.Lock()
	// Opportunistic hygiene on write: expired entries leave only when their
	// key is re-read, so bound the map here to keep growth finite per process.
	for k, e := range s.entries {
		if now.After(e.expiresAt) && k != key {
			delete(s.entries, k)
		}
	}
	const maxDeviceCapabilityEntries = 8192
	for len(s.entries) >= maxDeviceCapabilityEntries {
		oldestKey := ""
		var oldest time.Time
		for k, e := range s.entries {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.entries, oldestKey)
	}
	s.entries[key] = providerCapabilityEntry{device: device, found: true, expiresAt: now.Add(ttl)}
	s.mu.Unlock()
	return device, true
}

func userDeviceKey(userID int, profileID, deviceID string) string {
	// A single string key is all the map needs; the  separators keep the
	// three components unambiguous.
	return strconv.Itoa(userID) + "\x00" + profileID + "\x00" + deviceID
}
