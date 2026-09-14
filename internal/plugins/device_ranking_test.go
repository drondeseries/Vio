package plugins

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

func TestRankVirtualStreamsForDevicePrefersDirectPlay(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=a", Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "truehd", Container: "mkv", HDR: "dv"},
		{URI: "virtual://movie/1?result=b", Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mp4"},
		{URI: "virtual://movie/1?result=c", Resolution: "720p", CodecVideo: "h264", CodecAudio: "aac", Container: "mp4"},
	}
	device := DeviceCapabilities{
		CodecsVideo: []string{"h264"},
		CodecsAudio: []string{"aac"},
		Containers:  []string{"mp4"},
	}
	ranked := RankVirtualStreamsForDevice(candidates, device)
	if ranked[0].URI != candidates[1].URI {
		t.Fatalf("expected 1080p h264 direct-play first, got %s", ranked[0].URI)
	}
	if ranked[1].URI != candidates[2].URI {
		t.Fatalf("expected 720p h264 second, got %s", ranked[1].URI)
	}
	if ranked[2].URI != candidates[0].URI {
		t.Fatalf("expected 2160p hevc last (no direct play), got %s", ranked[2].URI)
	}
}

func TestRankVirtualStreamsForDeviceEmptyProfileKeepsOrder(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=a", Resolution: "720p"},
		{URI: "virtual://movie/1?result=b", Resolution: "2160p"},
	}
	got := RankVirtualStreamsForDevice(candidates, DeviceCapabilities{})
	if len(got) != 2 || got[0].URI != candidates[0].URI || got[1].URI != candidates[1].URI {
		t.Fatalf("empty profile must preserve provider order, got %+v", got)
	}
}

func TestRankVirtualStreamsForDeviceResolutionTieBreak(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=a", Resolution: "720p", CodecVideo: "h264"},
		{URI: "virtual://movie/1?result=b", Resolution: "2160p", CodecVideo: "h264"},
	}
	device := DeviceCapabilities{CodecsVideo: []string{"h264"}, MaxResolution: "1080p"}
	ranked := RankVirtualStreamsForDevice(candidates, device)
	// 720p is below the 1080p cap and direct-plays; 2160p exceeds the cap.
	if ranked[0].URI != candidates[0].URI {
		t.Fatalf("expected 720p (within device cap) first, got %s", ranked[0].URI)
	}
}

func TestDeviceCapabilitiesFromProfileNormalizes(t *testing.T) {
	profile := userstore.DeviceCapabilityProfile{
		CodecsVideo:   []string{"HEVC", "h264"},
		CodecsAudio:   []string{"aac"},
		Containers:    []string{"mp4"},
		MaxResolution: "2160p",
		HDR:           true,
		DolbyVision:   true,
	}
	got := DeviceCapabilitiesFromProfile(profile)
	if len(got.CodecsVideo) != 2 || got.CodecsVideo[0] != "HEVC" {
		t.Fatalf("codec list must be preserved verbatim for the scorer, got %v", got.CodecsVideo)
	}
	if !got.HDR || !got.DolbyVision || got.MaxResolution != "2160p" {
		t.Fatalf("hdr/max resolution lost: %+v", got)
	}
}

func TestRankVirtualStreamsForDeviceDoesNotTreatGenericHDRAsDolbyVision(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=hdr", CodecVideo: "h264", HDR: "true"},
		{URI: "virtual://movie/1?result=sdr", CodecVideo: "h264"},
	}
	got := RankVirtualStreamsForDevice(candidates, DeviceCapabilities{CodecsVideo: []string{"h264"}})
	if got[0].URI != candidates[1].URI {
		t.Fatalf("generic HDR candidate should be treated as HDR rather than DV, got %s first", got[0].URI)
	}
}

func TestRankVirtualStreamsForDevicePenalizesExplicitDVWithoutNativeSupport(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=dv", CodecVideo: "h264", HDR: "Dolby Vision Profile 5"},
		{URI: "virtual://movie/1?result=hdr", CodecVideo: "h264", HDR: "HDR10"},
	}
	got := RankVirtualStreamsForDevice(candidates, DeviceCapabilities{CodecsVideo: []string{"h264"}, HDR: true})
	if got[0].URI != candidates[1].URI {
		t.Fatalf("explicit DV should be penalized without native DV support, got %s first", got[0].URI)
	}
}

func TestRankVirtualStreamsForDeviceDoesNotTreatDVDAsDolbyVision(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=dvd", CodecVideo: "h264", HDR: "DVD"},
		{URI: "virtual://movie/1?result=sdr", CodecVideo: "h264", HDR: ""},
	}
	got := RankVirtualStreamsForDevice(candidates, DeviceCapabilities{CodecsVideo: []string{"h264"}})
	if len(got) != 2 || got[0].URI != candidates[0].URI {
		t.Fatalf("DVD should not receive DV penalty, got %+v", got)
	}
}

func TestRankVirtualStreamsForDeviceAtmosBonusRequiresAudioEvidence(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=plain", CodecVideo: "h264", CodecAudio: "aac", HasAtmos: false},
		{URI: "virtual://movie/1?result=atmos", CodecVideo: "h264", CodecAudio: "eac3", HasAtmos: true},
	}

	// 1. Device with explicit EAC3 audio capability receives Atmos bonus and ranks Atmos first
	eac3Device := DeviceCapabilities{
		CodecsVideo: []string{"h264"},
		CodecsAudio: []string{"aac", "eac3"},
	}
	rankedEAC3 := RankVirtualStreamsForDevice(candidates, eac3Device)
	if rankedEAC3[0].URI != candidates[1].URI {
		t.Fatalf("expected Atmos stream first for EAC3 device, got %s", rankedEAC3[0].URI)
	}

	// 2. Device with video capability but NO audio codec capability does NOT receive Atmos bonus
	noAudioDevice := DeviceCapabilities{
		CodecsVideo: []string{"h264"},
		CodecsAudio: nil,
	}
	rankedNoAudio := RankVirtualStreamsForDevice(candidates, noAudioDevice)
	if rankedNoAudio[0].URI != candidates[0].URI {
		t.Fatalf("expected provider order preserved when audio capability unknown, got %s", rankedNoAudio[0].URI)
	}
}

func TestRankVirtualStreamsForDeviceQualityScoreTieBreak(t *testing.T) {
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/1?result=low-quality", Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", QualityScore: -10},
		{URI: "virtual://movie/1?result=high-quality", Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", QualityScore: 40},
	}
	device := DeviceCapabilities{
		CodecsVideo: []string{"h264"},
		CodecsAudio: []string{"aac"},
	}
	ranked := RankVirtualStreamsForDevice(candidates, device)
	if ranked[0].URI != candidates[1].URI {
		t.Fatalf("expected higher quality score stream first on equal codec match, got %s", ranked[0].URI)
	}
}
