package jellycompat

import (
	"testing"
)

func TestNormalizedCapabilityProfileExtractsDirectPlayFields(t *testing.T) {
	profile := DeviceProfile{
		MaxStreamingBitrate: 80_000_000,
		DirectPlayProfiles: []DirectPlayProfile{
			{Type: "Video", Container: "mp4,mkv", VideoCodec: "h264,hevc", AudioCodec: "aac,ac3"},
		},
	}
	got, ok := normalizedCapabilityProfile(profile, "p1", "dev-1")
	if !ok {
		t.Fatal("expected a usable profile")
	}
	if !containsFold(got.CodecsVideo, "h264") || !containsFold(got.CodecsVideo, "hevc") {
		t.Errorf("video codecs = %v, want h264+hevc", got.CodecsVideo)
	}
	if !containsFold(got.CodecsAudio, "ac3") {
		t.Errorf("audio codecs = %v, want ac3", got.CodecsAudio)
	}
	if !containsFold(got.Containers, "mkv") {
		t.Errorf("containers = %v, want mkv", got.Containers)
	}
	if got.MaxResolution != "1080p" {
		t.Errorf("max resolution = %q, want 1080p (80Mbps)", got.MaxResolution)
	}
	if !got.HDR {
		t.Error("expected HDR true when HEVC is supported")
	}
	if got.DolbyVision {
		t.Error("generic HEVC support must not imply native Dolby Vision")
	}
	if got.ProfileID != "p1" || got.DeviceID != "dev-1" || got.Source != "client" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Fingerprint == "" {
		t.Error("expected a fingerprint")
	}
}

func TestNormalizedCapabilityProfileDetectsExplicitDolbyVision(t *testing.T) {
	profile := DeviceProfile{
		DirectPlayProfiles: []DirectPlayProfile{{Type: "Video", VideoCodec: "hevc,dovi"}},
	}
	got, ok := normalizedCapabilityProfile(profile, "p1", "dev-1")
	if !ok || !got.DolbyVision {
		t.Fatalf("expected explicit DOVI support, got ok=%v profile=%+v", ok, got)
	}
}

func TestNormalizedCapabilityProfileEmptyIsRejected(t *testing.T) {
	got, ok := normalizedCapabilityProfile(DeviceProfile{}, "p1", "dev-1")
	if ok {
		t.Fatalf("expected empty profile to be rejected, got %+v", got)
	}
}

func TestNormalizedCapabilityProfileRequiresIdentity(t *testing.T) {
	profile := DeviceProfile{
		DirectPlayProfiles: []DirectPlayProfile{{Type: "Video", Container: "mp4", VideoCodec: "h264"}},
	}
	if _, ok := normalizedCapabilityProfile(profile, "", "dev-1"); ok {
		t.Error("empty profileID must be rejected")
	}
	if _, ok := normalizedCapabilityProfile(profile, "p1", ""); ok {
		t.Error("empty deviceID must be rejected")
	}
}
