package api

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestRouterRegistersEvidenceDrainShutdownStep asserts the wiring layer: when
// the router builds a playback handler it registers the probe-evidence drain as
// a named application-shutdown step. That registration is what makes main's
// graceful-shutdown sequence call and await the handler's stop method instead of
// relying on a detached service-context watcher. The drain's own behavior is
// covered in the handlers package; this test covers only the seam.
func TestRouterRegistersEvidenceDrainShutdownStep(t *testing.T) {
	cfg, err := config.LoadFromDB(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	type registration struct {
		name string
		run  func()
	}
	var registered []registration
	newChiRouter(Dependencies{
		Config:     cfg,
		SessionMgr: playback.NewSessionManager(0, 0),
		RegisterShutdownFunc: func(name string, run func()) {
			registered = append(registered, registration{name: name, run: run})
		},
	})

	if len(registered) != 1 {
		t.Fatalf("registered %d application-shutdown step(s), want 1 (virtual-evidence-drain)", len(registered))
	}
	if registered[0].name != "virtual-evidence-drain" {
		t.Fatalf("shutdown step name = %q, want %q", registered[0].name, "virtual-evidence-drain")
	}
	if registered[0].run == nil {
		t.Fatal("evidence drain shutdown step has no run function")
	}
	// Calling it must be safe even when no evidence was ever admitted.
	registered[0].run()
}
