package altmount

import (
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// historyPayload builds the SABnzbd-compatible history JSON the client parses.
func historyPayload(slots string) string {
	return `{"history":{"slots":[` + slots + `]}}`
}

// TestParseAltmountHistoryRecordsReleaseIdentity proves a completed slot's
// release-scoped key and exact byte count survive parsing, so a candidate can
// later adopt them as durable identity.
func TestParseAltmountHistoryRecordsReleaseIdentity(t *testing.T) {
	payload := historyPayload(`{
		"name": "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		"nzb_name": "My.Movie.2024.1080p.WEB-DL.x264-GRP.nzb",
		"status": "Completed",
		"storage": "/media/movies/My.Movie.2024.1080p.WEB-DL.x264-GRP",
		"bytes": 8500000000,
		"completetime": 1700000000
	}`)
	snapshot, err := parseAltmountHistory(strings.NewReader(payload), time.Now())
	if err != nil {
		t.Fatalf("parseAltmountHistory: %v", err)
	}
	if len(snapshot.Completed) == 0 {
		t.Fatal("completed snapshot is empty")
	}
	var record altmountReleaseRecord
	for _, value := range snapshot.Completed {
		record = value
		break
	}
	if record.Size != 8_500_000_000 {
		t.Fatalf("record size = %d, want AltMount's exact bytes", record.Size)
	}
	if record.Identity == "" {
		t.Fatal("record identity is empty; the release key must survive parsing")
	}
	if !strings.HasPrefix(record.Identity, "mymovie20241080p") {
		t.Fatalf("record identity = %q, want the normalized NZB name", record.Identity)
	}
}

// TestClassifyCandidatesAdoptsAltmountIdentity proves a confirmed candidate
// that carries no hash/GUID and no size adopts AltMount's release key and exact
// size, so its durable identity exists where the Stremio answer provided none.
func TestClassifyCandidatesAdoptsAltmountIdentity(t *testing.T) {
	record := altmountReleaseRecord{
		Size:        8_500_000_000,
		CompletedAt: time.Now().Unix(),
		Identity:    "mymovie20241080pwebdlx264grp",
	}
	client := newAltmountStateClient(nil)
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{
			"mymovie20241080pwebdlx264grp": record,
		},
		Failed: map[string]altmountReleaseRecord{},
	}

	candidates := []stream.StreamCandidate{{
		Name:  "AltMount 1080p",
		Title: "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		URL:   "https://provider.example/my.movie.mkv",
	}}
	client.ClassifyCandidates(candidates)

	got := candidates[0]
	if !got.SourceConfirmed {
		t.Fatal("candidate was not confirmed by the matching release key")
	}
	if got.SourceGUID != "altmount:mymovie20241080pwebdlx264grp" {
		t.Fatalf("SourceGUID = %q, want the namespaced AltMount key", got.SourceGUID)
	}
	if got.FileSize != 8_500_000_000 {
		t.Fatalf("FileSize = %d, want AltMount's exact bytes", got.FileSize)
	}
}

// TestClassifyCandidatesNeverOverwritesDeclaredIdentity proves AltMount is
// corroboration, not an override: a candidate that already carries a hash,
// GUID or size keeps it, so a stronger tier the provider declared is not lost.
func TestClassifyCandidatesNeverOverwritesDeclaredIdentity(t *testing.T) {
	record := altmountReleaseRecord{
		Size:        8_500_000_000,
		CompletedAt: time.Now().Unix(),
		Identity:    "mymovie20241080pwebdlx264grp",
	}
	client := newAltmountStateClient(nil)
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{
			"mymovie20241080pwebdlx264grp": record,
		},
		Failed: map[string]altmountReleaseRecord{},
	}

	candidate := stream.StreamCandidate{
		Name:       "AltMount 1080p",
		Title:      "My.Movie.2024.1080p.WEB-DL.x264-GRP",
		URL:        "https://provider.example/my.movie.mkv",
		FileSize:   8_490_000_000,
		SourceGUID: "prowlarr-guid-1",
	}
	candidate.BehaviorHints.VideoHash = "HASH1"
	candidates := []stream.StreamCandidate{candidate}
	client.ClassifyCandidates(candidates)

	got := candidates[0]
	if got.SourceGUID != "prowlarr-guid-1" {
		t.Fatalf("SourceGUID = %q, want the declared Prowlarr GUID preserved", got.SourceGUID)
	}
	if got.FileSize != 8_490_000_000 {
		t.Fatalf("FileSize = %d, want the declared size preserved", got.FileSize)
	}
	if got.BehaviorHints.VideoHash != "HASH1" {
		t.Fatalf("videoHash = %q, want the declared hash preserved", got.BehaviorHints.VideoHash)
	}
}
