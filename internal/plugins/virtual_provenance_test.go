package plugins

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func provenanceTestService(configs map[int][]*RuntimeConfig) *Service {
	return &Service{configs: &fakeServiceConfigStore{configsByInstallation: configs}}
}

func TestAllowInsecureForProvenanceCoreIgnoresPluginConfig(t *testing.T) {
	old := CoreInsecureAllowed
	defer func() { CoreInsecureAllowed = old }()

	// Plugin has the opt-in, core does not: core rows must stay strict.
	svc := provenanceTestService(map[int][]*RuntimeConfig{
		7: {{InstallationID: 7, Key: "streaming", Value: map[string]any{"allow_insecure_http": true}}},
	})
	CoreInsecureAllowed = func(context.Context) bool { return false }
	if svc.AllowInsecureForProvenance(context.Background(), models.VirtualProvenanceCore, 7) {
		t.Fatal("core provenance with core opt-out must not inherit the plugin opt-in")
	}

	// Core opts in, plugin does not: core rows use the relay-insecure path.
	svc = provenanceTestService(nil)
	CoreInsecureAllowed = func(context.Context) bool { return true }
	if !svc.AllowInsecureForProvenance(context.Background(), models.VirtualProvenanceCore, 7) {
		t.Fatal("core provenance with core opt-in must allow insecure")
	}
}

func TestAllowInsecureForProvenancePluginPathPreserved(t *testing.T) {
	old := CoreInsecureAllowed
	CoreInsecureAllowed = func(context.Context) bool { return true }
	defer func() { CoreInsecureAllowed = old }()

	// Core opt-in must never rescue a plugin/legacy row: plugin config governs.
	svc := provenanceTestService(nil)
	for _, p := range []models.VirtualProvenance{"", models.VirtualProvenancePlugin, models.VirtualProvenanceLocal, "bogus"} {
		if svc.AllowInsecureForProvenance(context.Background(), p, 7) {
			t.Fatalf("provenance %q with no plugin opt-in must fail closed", p)
		}
	}
	svc = provenanceTestService(map[int][]*RuntimeConfig{
		7: {{InstallationID: 7, Key: "streaming", Value: map[string]any{"allow_insecure_http": true}}},
	})
	if !svc.AllowInsecureForProvenance(context.Background(), models.VirtualProvenancePlugin, 7) {
		t.Fatal("plugin provenance with plugin opt-in must allow insecure")
	}
	// InstallationAllowsInsecure <=0 rejection preserved byte-for-byte.
	if svc.AllowInsecureForProvenance(context.Background(), models.VirtualProvenancePlugin, 0) {
		t.Fatal("plugin provenance with installation 0 must fail closed")
	}
	if (&Service{}).AllowInsecureForProvenance(context.Background(), models.VirtualProvenancePlugin, 7) {
		t.Fatal("nil-config service must fail closed on the plugin path")
	}
}

func TestCoreVirtualInsecureAllowedNilFailsClosed(t *testing.T) {
	old := CoreInsecureAllowed
	CoreInsecureAllowed = nil
	defer func() { CoreInsecureAllowed = old }()
	if CoreVirtualInsecureAllowed(context.Background()) {
		t.Fatal("nil CoreInsecureAllowed must fail closed")
	}
	if CoreVirtualInsecureAllowed(nil) {
		t.Fatal("nil ctx with nil callback must fail closed")
	}
}
