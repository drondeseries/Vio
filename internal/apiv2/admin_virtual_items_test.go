package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
)

type fakeAdminVirtualItems struct {
	limit int
	view  handlers.AdminVirtualItemsView
	err   error
}

func (f *fakeAdminVirtualItems) ListAdminVirtualItems(_ context.Context, limit int) (handlers.AdminVirtualItemsView, error) {
	f.limit = limit
	return f.view, f.err
}

func TestAdminVirtualItemsRead(t *testing.T) {
	delivered := fixedTime()
	seen := fixedTime().Add(-1)
	f := &fakeAdminVirtualItems{view: handlers.AdminVirtualItemsView{Items: []handlers.AdminVirtualItemView{{
		ContentID:       "movie-tmdb-1",
		Title:           "One",
		ItemType:        "movie",
		LibraryID:       7,
		LibraryName:     "Movies",
		InstallationID:  11,
		CandidateCount:  2,
		FailedCount:     1,
		LastDeliveredAt: &delivered,
		LastSeenAt:      &seen,
		ReleaseNames:    []string{"alpha", "beta"},
	}}}}
	deps, _ := libraryDeps(t)
	deps.AdminVirtualItems = f
	h := newTestHandler(t, deps)

	requireProblem(t, do(t, h, http.MethodGet, Prefix+"/admin/virtual-items", "", nil), TypeAuthenticationRequired)
	if f.limit != 0 {
		t.Fatal("unauthenticated request reached service")
	}

	rec := do(t, h, http.MethodGet, Prefix+"/admin/virtual-items?limit=250", "", bearer(adminToken))
	if rec.Code != 200 {
		t.Fatalf("read %d %s", rec.Code, rec.Body)
	}
	if f.limit != 250 {
		t.Fatalf("limit = %d, want 250", f.limit)
	}
	var body AdminVirtualItemCollection
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	got := body.Items[0]
	if got.ID != "movie-tmdb-1" || got.LibraryID != "7" || got.InstallationID != "11" ||
		got.Title != "One" || got.Type != "movie" || got.LibraryName != "Movies" ||
		got.CandidateCount != 2 || got.FailedCount != 1 ||
		got.LastDeliveredAt == nil || got.LastSeenAt == nil ||
		len(got.ReleaseNames) != 2 {
		t.Fatalf("unexpected item %+v", got)
	}
}

func TestAdminVirtualItemsUnavailableWithoutService(t *testing.T) {
	deps, _ := libraryDeps(t)
	deps.AdminVirtualItems = nil
	h := newTestHandler(t, deps)
	requireProblem(t, do(t, h, http.MethodGet, Prefix+"/admin/virtual-items", "", bearer(adminToken)), TypeDependencyUnavailable)
}
