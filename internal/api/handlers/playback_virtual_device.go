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

// DeviceCapabilityProfileSource resolves the persisted device capabilities used
// for candidate ranking. It is an interface so tests can inject a deterministic
// profile; production wires providerDeviceCapabilitySource.
type DeviceCapabilityProfileSource interface {
	DeviceCapabilitiesFor(ctx context.Context, profileID, deviceID string) (plugins.DeviceCapabilities, bool)
}

func (h *PlaybackHandler) requestDeviceCapabilities(r *http.Request) (plugins.DeviceCapabilities, bool) {
	if h == nil || h.DeviceCapabilitySource == nil || r == nil {
		return plugins.DeviceCapabilities{}, false
	}
	return h.DeviceCapabilitySource.DeviceCapabilitiesFor(r.Context(), apimw.GetProfileID(r.Context()), requestDeviceID(r))
}

// finalizeVirtualCandidateOrder applies the handler-side candidate ordering:
// device rank, then the explicit quality-rung reorder, then a stable
// rejected-last partition. The partition runs last so a custom-format-rejected
// candidate can never sit at index 0 even when device fit would promote it
// (plugins.ScoreCandidate clamps QualityScore, so reject cannot be expressed as
// a low score alone). With a single candidate the input is returned unchanged:
// a lone rejected stream is a last-resort choice, not an ordering decision.
func (h *PlaybackHandler) finalizeVirtualCandidateOrder(r *http.Request, streams []VirtualPlaybackStream, qualityPreference string, bandwidthCapKbps int) []VirtualPlaybackStream {
	if len(streams) <= 1 {
		return streams
	}
	ranked, _ := h.rankVirtualCandidatesForDevice(r, streams)
	ranked = reorderVirtualCandidatesForQuality(ranked, qualityPreference, bandwidthCapKbps)
	return partitionVirtualStreamsAcceptedFirst(ranked)
}

// partitionVirtualStreamsAcceptedFirst stably moves rejected streams behind
// accepted ones, preserving the ranked order within each group. Accepted
// always sort before rejected; a rejected stream stays selectable as a
// last resort.
func partitionVirtualStreamsAcceptedFirst(streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	rejected := 0
	for _, stream := range streams {
		if stream.Rejected {
			rejected++
		}
	}
	if rejected == 0 || rejected == len(streams) {
		return streams
	}
	ordered := make([]VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		if !stream.Rejected {
			ordered = append(ordered, stream)
		}
	}
	for _, stream := range streams {
		if stream.Rejected {
			ordered = append(ordered, stream)
		}
	}
	return ordered
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
// every request (X-Vio-Device-Id, legacy X-Silo-Device-Id accepted). Empty
// means "no device identity": ranking then falls back to provider order.
func requestDeviceID(r *http.Request) string {
	return clampHeaderValue(headerValue(r.Header, deviceIDHeader, legacyDeviceIDHeader), 128)
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
