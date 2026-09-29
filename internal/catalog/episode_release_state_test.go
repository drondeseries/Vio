package catalog

import (
	"testing"
	"time"
)

// TestReleaseStateForAirDate pins the single shared release-timing decision:
// a strict calendar-date compare against today UTC. Unknown and malformed
// dates fail open ("") so missing metadata never bans playback.
func TestReleaseStateForAirDate(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")

	for _, tc := range []struct {
		name    string
		airDate string
		want    string
	}{
		{"future is upcoming", tomorrow, EpisodeReleaseUpcoming},
		{"distant future is upcoming", "2099-10-03", EpisodeReleaseUpcoming},
		{"today is released", today, EpisodeReleaseReleased},
		{"past is released", yesterday, EpisodeReleaseReleased},
		{"distant past is released", "2022-02-18", EpisodeReleaseReleased},
		{"empty fails open", "", ""},
		{"whitespace fails open", "   ", ""},
		{"malformed fails open", "not-a-date", ""},
		{"impossible month fails open", "2099-99-99", ""},
		{"impossible day fails open", "2026-02-30", ""},
		{"datetime fails open", "2026-10-03T00:00:00Z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReleaseStateForAirDate(tc.airDate); got != tc.want {
				t.Fatalf("ReleaseStateForAirDate(%q) = %q, want %q", tc.airDate, got, tc.want)
			}
		})
	}
}

// TestEpisodeDetailReleaseSeam pins the classification seam buildEpisodeDetail
// calls: ReleaseState stamps the same formatted air date the detail carries,
// and unknown dates leave it absent. (The full builder needs file fetchers
// and resolvers, so the apiv2 detail-renderer test and the episode fixtures
// cover the wiring instead of here.)
func TestEpisodeDetailReleaseSeam(t *testing.T) {
	air := time.Date(2099, time.October, 3, 0, 0, 0, 0, time.UTC)
	formatted := air.Format("2006-01-02")
	if formatted != "2099-10-03" {
		t.Fatalf("formatted air date = %q, want 2099-10-03", formatted)
	}
	if got := ReleaseStateForAirDate(formatted); got != EpisodeReleaseUpcoming {
		t.Fatalf("ReleaseStateForAirDate(%q) = %q, want upcoming", formatted, got)
	}
	if got := ReleaseStateForAirDate(""); got != "" {
		t.Fatalf("ReleaseStateForAirDate(\"\") = %q, want absent", got)
	}
}
