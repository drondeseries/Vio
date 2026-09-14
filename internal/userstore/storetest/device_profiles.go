package storetest

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// RunDeviceProfiles runs the device-capability-profile conformance tests
// against a UserStore implementation that also implements
// DeviceProfileRegistry.
func RunDeviceProfiles(t *testing.T, newStore func(t *testing.T) userstore.UserStore) {
	t.Run("DeviceProfiles", func(t *testing.T) {
		testDeviceProfiles(t, newStore)
	})
}

// testDeviceProfiles pins the normalized per-device capability registry:
// upsert, fingerprint-deduplicated re-report, per-profile scoping, and
// forget-on-demand.
func testDeviceProfiles(t *testing.T, newStore func(t *testing.T) userstore.UserStore) {
	ctx := context.Background()
	store := newStore(t)
	registry, ok := store.(userstore.DeviceProfileRegistry)
	if !ok {
		t.Skip("store does not implement DeviceProfileRegistry")
	}

	seedSettingProfiles(t, ctx, store, "p1", "p2")

	// Missing profile is nil, not an error.
	got, err := registry.GetDeviceProfile(ctx, "p1", deviceApple)
	if err != nil {
		t.Fatalf("GetDeviceProfile: %v", err)
	}
	if got != nil {
		t.Fatalf("GetDeviceProfile before put = %v, want nil", got)
	}

	profile := userstore.DeviceCapabilityProfile{
		ProfileID:     "p1",
		DeviceID:      deviceApple,
		CodecsVideo:   []string{"h264", "hevc"},
		CodecsAudio:   []string{"aac", "ac3"},
		Containers:    []string{"mp4", "mkv"},
		MaxResolution: "2160p",
		HDR:           true,
		DolbyVision:   true,
		Source:        "client",
	}
	if err := registry.PutDeviceProfile(ctx, profile); err != nil {
		t.Fatalf("PutDeviceProfile: %v", err)
	}

	got, err = registry.GetDeviceProfile(ctx, "p1", deviceApple)
	if err != nil {
		t.Fatalf("GetDeviceProfile after put: %v", err)
	}
	if got == nil {
		t.Fatal("GetDeviceProfile after put = nil")
	}
	if got.Fingerprint == "" {
		t.Error("expected a computed fingerprint")
	}
	wantFingerprint := userstore.DeviceCapabilityFingerprint(profile)
	if got.Fingerprint != wantFingerprint {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, wantFingerprint)
	}
	if len(got.CodecsVideo) != 2 || got.CodecsVideo[0] != "h264" || got.CodecsVideo[1] != "hevc" {
		t.Errorf("codecs_video = %v, want sorted [h264 hevc]", got.CodecsVideo)
	}
	if !got.HDR || !got.DolbyVision || got.MaxResolution != "2160p" || got.Source != "client" {
		t.Errorf("profile fields mismatch: %+v", got)
	}

	// Re-report with identical capabilities keeps the same fingerprint and
	// must not error.
	if err := registry.PutDeviceProfile(ctx, profile); err != nil {
		t.Fatalf("PutDeviceProfile (re-report): %v", err)
	}

	// Other profiles never see this device's profile.
	other, err := registry.GetDeviceProfile(ctx, "p2", deviceApple)
	if err != nil {
		t.Fatalf("GetDeviceProfile(p2): %v", err)
	}
	if other != nil {
		t.Errorf("GetDeviceProfile(p2) = %v, want nil (profile-scoped)", other)
	}

	// Forget removes it.
	if err := registry.ForgetDeviceProfile(ctx, "p1", deviceApple); err != nil {
		t.Fatalf("ForgetDeviceProfile: %v", err)
	}
	got, err = registry.GetDeviceProfile(ctx, "p1", deviceApple)
	if err != nil {
		t.Fatalf("GetDeviceProfile after forget: %v", err)
	}
	if got != nil {
		t.Errorf("GetDeviceProfile after forget = %v, want nil", got)
	}
}
