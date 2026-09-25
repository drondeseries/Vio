package virtuallibrary_test

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestConfigFromSettingsQualitySortRoundTrip proves profile sort criteria ride
// the existing settings JSON: they survive ConfigFromSettings unchanged, and a
// profile stored without them stays back-compatible (no criteria, default
// ranking).
func TestConfigFromSettingsQualitySortRoundTrip(t *testing.T) {
	cfg := virtuallibrary.ConfigFromSettings(map[string]string{
		"virtual_library.enable_quality_profiles": "true",
		"virtual_library.quality_profiles":        `[{"label":"custom","resolution":"1080p","sort":[{"attribute":"size","direction":"desc"},{"attribute":"hdr"}]}]`,
	})
	profiles := cfg.Quality.Profiles
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(profiles))
	}
	criteria := profiles[0].Sort
	if len(criteria) != 2 {
		t.Fatalf("sort = %+v, want 2 criteria", criteria)
	}
	if criteria[0].Attribute != "size" || criteria[0].Direction != "desc" {
		t.Fatalf("criterion[0] = %+v, want size/desc", criteria[0])
	}
	if criteria[1].Attribute != "hdr" || criteria[1].Direction != "" {
		t.Fatalf("criterion[1] = %+v, want hdr with an absent direction (attribute default)", criteria[1])
	}

	// A profile stored before sort existed decodes without criteria, so the
	// default ranking applies.
	legacy := virtuallibrary.ConfigFromSettings(map[string]string{
		"virtual_library.enable_quality_profiles": "true",
		"virtual_library.quality_profiles":        `[{"label":"old","resolution":"1080p"}]`,
	})
	if len(legacy.Quality.Profiles) != 1 {
		t.Fatalf("legacy profiles = %d, want 1", len(legacy.Quality.Profiles))
	}
	if legacy.Quality.Profiles[0].Sort != nil {
		t.Fatalf("legacy profile decoded sort = %+v, want nil", legacy.Quality.Profiles[0].Sort)
	}
}

// TestConfigFromSettingsIndexerSearchTimeout proves the Prowlarr search timeout
// round-trips as an int with the documented default of 20.
func TestConfigFromSettingsIndexerSearchTimeout(t *testing.T) {
	got := virtuallibrary.ConfigFromSettings(map[string]string{
		"virtual_library.indexer_search_timeout_seconds": "45",
	}).IndexerSearchTimeoutSeconds
	if got != 45 {
		t.Fatalf("IndexerSearchTimeoutSeconds = %d, want 45", got)
	}

	def := virtuallibrary.ConfigFromSettings(map[string]string{}).IndexerSearchTimeoutSeconds
	if def != 20 {
		t.Fatalf("default IndexerSearchTimeoutSeconds = %d, want 20", def)
	}
}
