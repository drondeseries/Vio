package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/imagesize"
	"github.com/Silo-Server/silo-server/internal/models"
)

// TestEpisodeReleaseState pins the single release-timing decision the
// episode list, detail, and v2 renderer share: a calendar-date compare
// against today UTC, with unknown and malformed dates failing open (absent)
// so missing metadata never bans playback.
func TestEpisodeReleaseState(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")

	for _, tc := range []struct {
		name    string
		airDate string
		want    string
	}{
		{"future is upcoming", tomorrow, EpisodeReleaseUpcoming},
		{"today is released", today, EpisodeReleaseReleased},
		{"past is released", yesterday, EpisodeReleaseReleased},
		{"distant past is released", "2022-02-18", EpisodeReleaseReleased},
		{"empty fails open", "", ""},
		{"whitespace fails open", "   ", ""},
		{"malformed fails open", "not-a-date", ""},
		{"datetime fails open", "2026-10-03T00:00:00Z", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EpisodeReleaseState(tc.airDate); got != tc.want {
				t.Fatalf("EpisodeReleaseState(%q) = %q, want %q", tc.airDate, got, tc.want)
			}
		})
	}
}

// TestEpisodeResponseShellCarriesReleaseState pins the shell wiring: the
// release state rides the air date it classifies, and a nil AirDate keeps
// the shell fail-open with both members absent. The v1 wire shape is
// covered too: ReleaseState serializes under json:"-", so the frozen
// contract never sees it.
func TestEpisodeResponseShellCarriesReleaseState(t *testing.T) {
	air := time.Date(2099, time.October, 3, 0, 0, 0, 0, time.UTC)
	shell, _ := episodeResponseShell(&models.Episode{AirDate: &air}, episodeImageFallback{}, imagesize.Medium)
	if shell.AirDate != "2099-10-03" {
		t.Fatalf("future shell AirDate = %q, want 2099-10-03", shell.AirDate)
	}
	if shell.ReleaseState != EpisodeReleaseUpcoming {
		t.Fatalf("future shell ReleaseState = %q, want %q", shell.ReleaseState, EpisodeReleaseUpcoming)
	}

	shell, _ = episodeResponseShell(&models.Episode{}, episodeImageFallback{}, imagesize.Medium)
	if shell.AirDate != "" || shell.ReleaseState != "" {
		t.Fatalf("unknown-date shell = air_date %q release_state %q, want both absent", shell.AirDate, shell.ReleaseState)
	}
}

// TestEpisodeResponseV1OmitsReleaseState pins the v1 freeze: the internal
// ReleaseState never reaches the frozen wire shape.
func TestEpisodeResponseV1OmitsReleaseState(t *testing.T) {
	air := time.Date(2099, time.October, 3, 0, 0, 0, 0, time.UTC)
	shell, _ := episodeResponseShell(&models.Episode{AirDate: &air}, episodeImageFallback{}, imagesize.Medium)
	data, err := json.Marshal(shell)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "release_state") {
		t.Fatalf("v1 episode response leaks release_state: %s", data)
	}
	if !strings.Contains(string(data), `"air_date":"2099-10-03"`) {
		t.Fatalf("v1 episode response lost air_date: %s", data)
	}
}
