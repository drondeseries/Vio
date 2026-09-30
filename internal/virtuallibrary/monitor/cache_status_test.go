package monitor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

func TestReleaseCachedDerivesFromPersistedSnapshotAndConfig(t *testing.T) {
	tempDir := t.TempDir()
	indexFile := filepath.Join(tempDir, "altmount-state.json")

	// Persist an AltMount snapshot with one completed release.
	stateJSON := []byte(`{
		"completed": {
			"movie20241080pwebdl": {
				"release_key": "movie20241080pwebdl",
				"release_name": "Movie.2024.1080p.WEB-DL"
			}
		},
		"failed": {}
	}`)
	if err := os.WriteFile(indexFile, stateJSON, 0o600); err != nil {
		t.Fatalf("write index file: %v", err)
	}

	m := monitor.New(dummyValidator{}, nil, nil)

	// 1. Unconfigured provider returns known=false.
	if cached, known := m.ReleaseCached("Movie.2024.1080p.WEB-DL"); known || cached {
		t.Fatalf("unconfigured: cached=%v known=%v, want false, false", cached, known)
	}

	// 2. Configure AltMount with the persisted index file (simulating restart).
	if err := m.ConfigureAltmount("http://127.0.0.1:8080", "test-key", 15, indexFile); err != nil {
		t.Fatalf("ConfigureAltmount: %v", err)
	}

	// 3. Persisted release must immediately report cached=true without waiting for classification.
	if cached, known := m.ReleaseCached("Movie.2024.1080p.WEB-DL"); !known || !cached {
		t.Fatalf("persisted release: cached=%v known=%v, want true, true", cached, known)
	}

	// Uncached release reports cached=false, known=true.
	if cached, known := m.ReleaseCached("Uncached.2024"); !known || cached {
		t.Fatalf("uncached release: cached=%v known=%v, want false, true", cached, known)
	}

	// 4. Changing provider configuration invalidates the snapshot.
	if err := m.ConfigureAltmount("http://127.0.0.1:9090", "different-key", 15, filepath.Join(tempDir, "empty-state.json")); err != nil {
		t.Fatalf("ConfigureAltmount new provider: %v", err)
	}

	// The old provider's release must no longer be cached on the new provider instance.
	if cached, known := m.ReleaseCached("Movie.2024.1080p.WEB-DL"); !known || cached {
		t.Fatalf("after provider reconfigure: cached=%v known=%v, want false, true", cached, known)
	}
}
