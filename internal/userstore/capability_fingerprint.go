package userstore

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// NormalizeCapabilityValues lower-cases and trims a list, removing empties and
// sorting the result so order on the wire never changes the fingerprint.
func NormalizeCapabilityValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// DeviceCapabilityFingerprint derives a stable fingerprint for a normalized
// capability profile so redundant reports (and equal cache keys) can be
// detected without comparing slices field by field.
func DeviceCapabilityFingerprint(profile DeviceCapabilityProfile) string {
	video := NormalizeCapabilityValues(profile.CodecsVideo)
	audio := NormalizeCapabilityValues(profile.CodecsAudio)
	containers := NormalizeCapabilityValues(profile.Containers)
	h := sha256.New()
	h.Write([]byte(strings.Join(video, ",")))
	h.Write([]byte{0x00})
	h.Write([]byte(strings.Join(audio, ",")))
	h.Write([]byte{0x00})
	h.Write([]byte(strings.Join(containers, ",")))
	h.Write([]byte{0x00})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(profile.MaxResolution))))
	h.Write([]byte{0x00})
	if profile.HDR {
		h.Write([]byte("1"))
	} else {
		h.Write([]byte("0"))
	}
	h.Write([]byte{0x00})
	if profile.DolbyVision {
		h.Write([]byte("1"))
	} else {
		h.Write([]byte("0"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
