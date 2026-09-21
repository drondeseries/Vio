package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
)

// fakeVirtualCandidatesRefresh is the refresh seam: it records the resolved
// identity and answers a fixed version list or a fixed error.
type fakeVirtualCandidatesRefresh struct {
	calls     int
	userID    int
	profileID string
	contentID string
	versions  []catalogpkg.FileVersion
	err       error
}

func (f *fakeVirtualCandidatesRefresh) RefreshVirtualCandidates(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) ([]catalogpkg.FileVersion, error) {
	f.calls++
	f.userID = userID
	f.profileID = profileID
	f.contentID = contentID
	if f.err != nil {
		return nil, f.err
	}
	return f.versions, nil
}

func mediaCandidatesDeps(refresh *fakeVirtualCandidatesRefresh) Dependencies {
	deps := pilotDeps(nil, nil)
	deps.Watch = &fakeWatch{}
	deps.VirtualCandidatesRefresh = refresh
	return deps
}

func TestRefreshVirtualCandidates(t *testing.T) {
	refresh := &fakeVirtualCandidatesRefresh{versions: []catalogpkg.FileVersion{{
		FileID: 42, Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Versions []map[string]any `json:"versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Versions) != 1 || body.Versions[0]["file_id"] != "42" || body.Versions[0]["resolution"] != "2160p" {
		t.Fatalf("versions = %v", body.Versions)
	}
	if refresh.calls != 1 || refresh.contentID != "movie:heat-1995" || refresh.userID != 1 || refresh.profileID != "p-owner" {
		t.Fatalf("refresh call = %+v", refresh)
	}
}

func TestRefreshVirtualCandidatesRejects(t *testing.T) {
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	// Authentication is required before the service is reached.
	refresh := &fakeVirtualCandidatesRefresh{}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", nil), TypeAuthenticationRequired)
	if refresh.calls != 0 {
		t.Fatalf("unauthenticated request reached the service %d times", refresh.calls)
	}

	// A missing service is a retryable wiring gap, not a 404.
	off := newTestHandler(t, pilotDeps(nil, nil))
	requireProblem(t, do(t, off, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner), TypeDependencyUnavailable)

	// Unknown media is a problem 404.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:missing/virtual-candidates:refresh", "", owner), TypeNotFound)

	// Non-virtual content is a problem 4xx.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "The item has no virtual candidates to refresh."}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:local/virtual-candidates:refresh", "", owner), TypeValidationFailed)

	// A provider failure is a retryable dependency problem.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The provider could not be reached; try again."}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner), TypeDependencyUnavailable)
}

// TestRefreshVirtualCandidatesIsProfileOptional mirrors GET /watch/{id}: an
// account-scoped caller may refresh without a profile header.
func TestRefreshVirtualCandidatesIsProfileOptional(t *testing.T) {
	refresh := &fakeVirtualCandidatesRefresh{versions: []catalogpkg.FileVersion{{FileID: 1, Resolution: "1080p", AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))

	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", bearer(memberToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if refresh.profileID != "" {
		t.Fatalf("profile id = %q, want empty", refresh.profileID)
	}
}
