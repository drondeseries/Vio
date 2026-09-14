package jellycompat

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// DeviceProfilePersister stores a normalized per-device capability profile so
// the virtual candidate ranking can be device-aware outside the in-memory,
// token-scoped DeviceProfileStore. It is an optional dependency: nil disables
// persistence without changing session-time negotiation. The userID is the
// native Silo user the compat session maps to.
type DeviceProfilePersister interface {
	PersistDeviceProfile(ctx context.Context, userID int, profile userstore.DeviceCapabilityProfile)
}

type DeviceProfilePersisterFunc func(ctx context.Context, userID int, profile userstore.DeviceCapabilityProfile)

func (f DeviceProfilePersisterFunc) PersistDeviceProfile(ctx context.Context, userID int, profile userstore.DeviceCapabilityProfile) {
	f(ctx, userID, profile)
}

// persistDeviceProfileFromHandshake bridges a Jellyfin capabilities payload to
// the normalized per-device profile. The Jellyfin client device ID (from the
// MediaBrowser auth header or DeviceId query) is the durable device identity
// here; the compat session carries both the native user ID and profile ID.
func (h *PlaybackHandler) persistDeviceProfileFromHandshake(r *http.Request, session *Session, profile DeviceProfile) {
	if session == nil || h.DeviceProfilePersister == nil {
		return
	}
	deviceID := firstNonEmpty(
		firstMediaBrowserAuthorizationValue(r, "DeviceId"),
		newCaseInsensitiveQuery(r.URL.Query()).Get("DeviceId"),
	)
	normalized, ok := normalizedCapabilityProfile(profile, session.ProfileID, deviceID)
	if !ok {
		return
	}
	h.DeviceProfilePersister.PersistDeviceProfile(r.Context(), session.StreamAppUserID, normalized)
}

// normalizedCapabilityProfile converts a Jellyfin DeviceProfile into the
// normalized server-side capability shape. Only the fields the ranker consumes
// are carried over: supported video/audio codecs and containers from
// DirectPlayProfiles, plus max bitrate. When the device reports nothing usable,
// ok is false and the caller must not persist.
func normalizedCapabilityProfile(profile DeviceProfile, profileID, deviceID string) (userstore.DeviceCapabilityProfile, bool) {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(deviceID) == "" {
		return userstore.DeviceCapabilityProfile{}, false
	}
	out := userstore.DeviceCapabilityProfile{
		ProfileID: profileID,
		DeviceID:  deviceID,
		Source:    "client",
	}
	seenVideo := map[string]struct{}{}
	seenAudio := map[string]struct{}{}
	seenContainer := map[string]struct{}{}
	for _, dp := range profile.DirectPlayProfiles {
		if !matchesVideoType(dp.Type) {
			continue
		}
		for _, c := range splitCompatCSV(dp.Container) {
			if c != "" {
				seenContainer[c] = struct{}{}
			}
		}
		for _, c := range splitCompatCSV(dp.VideoCodec) {
			if c != "" {
				seenVideo[c] = struct{}{}
			}
		}
		for _, c := range splitCompatCSV(dp.AudioCodec) {
			if c != "" {
				seenAudio[c] = struct{}{}
			}
		}
	}
	if profile.MaxStreamingBitrate > 0 {
		switch {
		case profile.MaxStreamingBitrate >= 100_000_000:
			out.MaxResolution = "2160p"
		case profile.MaxStreamingBitrate >= 20_000_000:
			out.MaxResolution = "1080p"
		default:
			out.MaxResolution = "720p"
		}
	}
	if len(seenVideo) == 0 && len(seenAudio) == 0 && len(seenContainer) == 0 && out.MaxResolution == "" {
		return userstore.DeviceCapabilityProfile{}, false
	}
	out.CodecsVideo = sortedKeys(seenVideo)
	out.CodecsAudio = sortedKeys(seenAudio)
	out.Containers = sortedKeys(seenContainer)
	// HEVC support is treated as HDR-capable for ranking, while native Dolby
	// Vision support requires an explicit DOVI/DVHE codec declaration.
	out.DolbyVision = containsFold(out.CodecsVideo, "dovi") || containsFold(out.CodecsVideo, "dvhe")
	out.HDR = containsFold(out.CodecsVideo, "hevc") || out.DolbyVision
	out.Fingerprint = userstore.DeviceCapabilityFingerprint(out)
	return out, true
}

// splitCompatCSV splits the comma-separated codec/container lists Jellyfin
// clients send (e.g. "h264,hevc" or "*"), normalizing each token.
func splitCompatCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}
