package plugins

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// DeviceCapabilities holds the client decode capabilities used for candidate ranking.
type DeviceCapabilities struct {
	CodecsVideo   []string
	CodecsAudio   []string
	Containers    []string
	MaxResolution string
	HDR           bool
	DolbyVision   bool
}

func (d DeviceCapabilities) Fingerprint() string {
	return userstore.DeviceCapabilityFingerprint(userstore.DeviceCapabilityProfile{
		CodecsVideo:   d.CodecsVideo,
		CodecsAudio:   d.CodecsAudio,
		Containers:    d.Containers,
		MaxResolution: d.MaxResolution,
		HDR:           d.HDR,
		DolbyVision:   d.DolbyVision,
	})
}

// VirtualStreamMetadata is the minimal candidate surface the device ranker
// needs. Both the plugins.VirtualPlaybackStream (prewarm) and the
// handlers.VirtualPlaybackStream (playback start) shapes implement it, so
// ranking is shared instead of duplicated.
type VirtualStreamMetadata interface {
	GetCodecVideo() string
	GetCodecAudio() string
	GetHDR() string
	GetContainer() string
	GetResolution() string
	GetHasAtmos() bool
	GetQualityScore() int
}

// DeviceCapabilitiesFromProfile converts a persisted, normalized capability
// profile into the DeviceCapabilities shape the prewarm scorer already
// understands, so playback ranking and pre-warming use one scorer.
func DeviceCapabilitiesFromProfile(p userstore.DeviceCapabilityProfile) DeviceCapabilities {
	return DeviceCapabilities{
		CodecsVideo:   append([]string(nil), p.CodecsVideo...),
		CodecsAudio:   append([]string(nil), p.CodecsAudio...),
		Containers:    append([]string(nil), p.Containers...),
		MaxResolution: p.MaxResolution,
		HDR:           p.HDR,
		DolbyVision:   p.DolbyVision,
	}
}

// hasDeviceCapabilities reports whether a DeviceCapabilities carries any
// usable signal. An empty value means "no profile known": ranking must then
// preserve the provider's original order exactly, matching pre-feature
// behavior.
func hasDeviceCapabilities(device DeviceCapabilities) bool {
	return len(device.CodecsVideo) > 0 ||
		len(device.CodecsAudio) > 0 ||
		len(device.Containers) > 0 ||
		strings.TrimSpace(device.MaxResolution) != "" ||
		device.HDR ||
		device.DolbyVision
}

// RankVirtualStreamsForDevice orders candidates best-first for the given
// device: direct-playable (matching video codec, audio codec, container,
// resolution, HDR) before everything else, with the provider's original order
// kept as the deterministic tie-break. Without any capability signal the
// candidates are returned unchanged, preserving the pre-feature selection.
func RankVirtualStreamsForDevice[T VirtualStreamMetadata](candidates []T, device DeviceCapabilities) []T {
	if len(candidates) <= 1 || !hasDeviceCapabilities(device) {
		return candidates
	}
	indexed := make([]virtualRankedStream[T], len(candidates))
	for i, c := range candidates {
		indexed[i] = virtualRankedStream[T]{stream: c, score: ScoreCandidate(c, device), index: i}
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].score != indexed[j].score {
			return indexed[i].score > indexed[j].score
		}
		return indexed[i].index < indexed[j].index
	})
	out := make([]T, len(candidates))
	for i, r := range indexed {
		out[i] = r.stream
	}
	return out
}

func ScoreCandidate[T VirtualStreamMetadata](c T, device DeviceCapabilities) int {
	score := 0

	// Video codec match (biggest factor)
	videoCodec := strings.ToLower(strings.TrimSpace(c.GetCodecVideo()))
	if videoCodec != "" && deviceSupportsCodec(device.CodecsVideo, videoCodec) {
		score += 100
	}

	// Resolution: prefer matching or lower (avoid transcode from 4K to 1080p)
	resScore := resolutionScore(c.GetResolution(), device.MaxResolution)
	score += resScore

	// HDR & Dolby Vision:
	// Dolby Vision (Profile 5 especially) on a non-DV device renders as bright green/purple.
	hdrLower := strings.ToLower(strings.TrimSpace(c.GetHDR()))
	isDV := isDolbyVisionCandidate(hdrLower)
	isHDR := isDV || strings.Contains(hdrLower, "hdr") || hdrLower == "true"
	if isDV && !device.DolbyVision {
		score -= 45 // Strongly penalize Dolby Vision when device lacks native DV support
	} else if device.HDR {
		if isHDR {
			score += 30
		}
	} else if isHDR {
		score -= 20 // HDR content on non-HDR device = tone-map needed
	}

	// Audio codec match (less critical — audio transcode is cheaper)
	audioCodec := strings.ToLower(strings.TrimSpace(c.GetCodecAudio()))
	if audioCodec != "" && deviceSupportsCodec(device.CodecsAudio, audioCodec) {
		score += 50
	}

	// Dolby Atmos bonus when device explicitly supports Atmos or compatible multi-channel passthrough
	if c.GetHasAtmos() && len(device.CodecsAudio) > 0 {
		if deviceSupportsCodec(device.CodecsAudio, "eac3") || deviceSupportsCodec(device.CodecsAudio, "truehd") || deviceSupportsCodec(device.CodecsAudio, "atmos") {
			score += 15
		}
	}

	// Container match
	if c.GetContainer() != "" && deviceSupportsContainer(device.Containers, c.GetContainer()) {
		score += 10
	}

	// Custom format or provider quality score (clamped to prevent overriding hard compatibility)
	if qs := c.GetQualityScore(); qs != 0 {
		if qs > 50 {
			qs = 50
		} else if qs < -50 {
			qs = -50
		}
		score += qs / 5
	}

	return score
}

func deviceSupportsCodec(deviceCodecs []string, codec string) bool {
	if len(deviceCodecs) == 0 {
		return true
	}
	codec = strings.ToLower(codec)
	for _, c := range deviceCodecs {
		if strings.EqualFold(strings.TrimSpace(c), codec) {
			return true
		}
	}
	return false
}

func deviceSupportsContainer(deviceContainers []string, container string) bool {
	if len(deviceContainers) == 0 {
		return true
	}
	container = strings.ToLower(container)
	for _, c := range deviceContainers {
		if strings.EqualFold(strings.TrimSpace(c), container) {
			return true
		}
	}
	return false
}

func resolutionScore(candidateRes, maxRes string) int {
	candP := parseResP(candidateRes)
	maxP := parseResP(maxRes)
	if candP <= 0 || maxP <= 0 {
		return 0
	}
	if candP <= maxP {
		return 20 // within limits
	}
	return -40 // exceeds max = requires downscale transcode
}

func parseResP(res string) int {
	res = strings.ToLower(strings.TrimSpace(res))
	res = strings.TrimSuffix(res, "p")
	p, err := strconv.Atoi(res)
	if err != nil {
		return 0
	}
	return p
}

type virtualRankedStream[T any] struct {
	stream T
	score  int
	index  int
}

// DeviceCapabilityProfileSource reads the persisted, normalized capability
// profile for one (profile, device). Implementations should be fast and
// cached: it sits on the playback and pre-warm paths.
type DeviceCapabilityProfileSource interface {
	DeviceCapabilitiesFor(ctx context.Context, profileID, deviceID string) (DeviceCapabilities, bool)
}

func isDolbyVisionCandidate(hdr string) bool {
	lower := strings.ToLower(strings.TrimSpace(hdr))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "dolby vision") || strings.Contains(lower, "dovi") {
		return true
	}
	if lower == "dv" || strings.HasPrefix(lower, "dv ") || strings.HasSuffix(lower, " dv") || strings.Contains(lower, " dv ") ||
		strings.HasPrefix(lower, "dv-") || strings.HasSuffix(lower, "-dv") || strings.Contains(lower, "-dv-") ||
		strings.HasPrefix(lower, "dv_") || strings.HasSuffix(lower, "_dv") || strings.Contains(lower, "_dv_") ||
		strings.HasPrefix(lower, "dv.") || strings.HasSuffix(lower, ".dv") || strings.Contains(lower, ".dv.") {
		return true
	}
	for _, marker := range []string{"profile 5", "profile 7", "profile 8", "dv5", "dv7", "dv8"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
