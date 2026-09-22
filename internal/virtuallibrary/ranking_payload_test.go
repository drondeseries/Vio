package virtuallibrary_test

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
)

// TestRankingForPathReportsProfileAndDefault proves the ranking projection is
// config-derived: a configured profile selector reports that profile's label
// and its own sort keys, while an absent or unknown selector reports the
// built-in default order.
func TestRankingForPathReportsProfileAndDefault(t *testing.T) {
	cfg := virtuallibrary.Config{
		Enabled:     true,
		ManifestURL: "https://example.invalid/manifest.json",
		MonitorFile: filepath.Join(t.TempDir(), "monitor.json"),
		Quality: quality.QualityConfig{
			EnableProfiles: true,
			Profiles: []quality.QualityProfile{{
				Label: "4K HDR",
				Sort:  []quality.SortCriterion{{Attribute: "size", Direction: "desc"}},
			}},
		},
	}
	svc := virtuallibrary.New(cfg, nil, nil)
	if svc == nil {
		t.Fatal("service is nil")
	}

	profilePath := "virtual://movie/tt1?profile=" + url.QueryEscape("4K HDR")
	ranking := svc.RankingForPath(profilePath)
	if ranking.Source != virtuallibrary.VirtualRankingSourceProfile || ranking.ProfileLabel != "4K HDR" {
		t.Fatalf("profile ranking = %+v, want the 4K HDR profile", ranking)
	}
	if len(ranking.Criteria) != 1 || ranking.Criteria[0].Attribute != "size" || ranking.Criteria[0].Direction != "desc" {
		t.Fatalf("profile criteria = %+v, want the profile's own sort", ranking.Criteria)
	}

	for _, path := range []string{"virtual://movie/tt1", "virtual://movie/tt1?profile=unknown"} {
		defaultRanking := svc.RankingForPath(path)
		if defaultRanking.Source != virtuallibrary.VirtualRankingSourceDefault || defaultRanking.ProfileLabel != "" {
			t.Fatalf("default ranking for %q = %+v", path, defaultRanking)
		}
		if len(defaultRanking.Criteria) == 0 {
			t.Fatalf("default ranking for %q has no criteria", path)
		}
		if defaultRanking.Criteria[0].Attribute != quality.EffectiveSortCriteria(quality.QualityProfile{})[0].Attribute {
			t.Fatalf("default ranking for %q = %+v, want the built-in order", path, defaultRanking.Criteria)
		}
	}
}
