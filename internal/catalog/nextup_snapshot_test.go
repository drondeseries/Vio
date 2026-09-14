package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// fakeNextUpStateStore is a minimal userstore.UserStore + NextUpStateStore
// double. The embedded UserStore is nil, so only the methods exercised by the
// pure-unit paths below are reachable.
type fakeNextUpStateStore struct {
	userstore.UserStore
	pages []userstore.NextUpStatePage
	err   error
}

func (f *fakeNextUpStateStore) ListNextUpStatePage(ctx context.Context, _ string, _ *userstore.NextUpStateCursor, _ int) (userstore.NextUpStatePage, error) {
	if err := ctx.Err(); err != nil {
		return userstore.NextUpStatePage{}, err
	}
	if f.err != nil {
		return userstore.NextUpStatePage{}, f.err
	}
	if len(f.pages) == 0 {
		return userstore.NextUpStatePage{Exhausted: true}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

// stateProvider returns one fixed store for every user id.
type stateProvider struct {
	store userstore.UserStore
}

func (p stateProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return p.store, nil
}
func (p stateProvider) Close() error { return nil }

// plainUserStore models a store that predates the NextUpStateStore capability.
type plainUserStore struct {
	userstore.UserStore
}

func errorProvider(err error) userstore.UserStoreProvider {
	return errorUserStoreProvider{err: err}
}

type errorUserStoreProvider struct {
	err error
}

func (p errorUserStoreProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return nil, p.err
}
func (p errorUserStoreProvider) Close() error { return nil }

func TestNextUpRepository_RequiresNextUpStateStore(t *testing.T) {
	repo := NewNextUpRepository(nil, stateProvider{store: plainUserStore{}})
	_, err := repo.ListNextUp(context.Background(), NextUpQuery{UserID: 1, ProfileID: "p"})
	if err == nil {
		t.Fatal("expected an error for a store without NextUpStateStore")
	}
	if !strings.Contains(err.Error(), "NextUpStateStore") {
		t.Fatalf("error should name the missing capability, got %v", err)
	}
}

func TestNextUpRepository_NilStoreErrors(t *testing.T) {
	repo := NewNextUpRepository(nil, stateProvider{store: nil})
	_, err := repo.ListNextUp(context.Background(), NextUpQuery{UserID: 1, ProfileID: "p"})
	if err == nil {
		t.Fatal("expected an error for a nil store")
	}
}

func TestNextUpRepository_ProviderErrorPropagates(t *testing.T) {
	want := errors.New("store unavailable")
	repo := NewNextUpRepository(nil, errorProvider(want))
	_, err := repo.ListNextUp(context.Background(), NextUpQuery{UserID: 1, ProfileID: "p"})
	if !errors.Is(err, want) {
		t.Fatalf("provider error = %v, want %v", err, want)
	}
}

func TestNextUpRepository_CanceledContextPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repo := NewNextUpRepository(nil, stateProvider{store: &fakeNextUpStateStore{}})
	_, err := repo.ListNextUp(ctx, NextUpQuery{UserID: 1, ProfileID: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want context.Canceled", err)
	}
}

func TestNextUpRepository_StateStoreErrorPropagates(t *testing.T) {
	want := errors.New("state page failed")
	repo := NewNextUpRepository(nil, stateProvider{store: &fakeNextUpStateStore{err: want}})
	_, err := repo.ListNextUp(context.Background(), NextUpQuery{UserID: 1, ProfileID: "p"})
	if !errors.Is(err, want) {
		t.Fatalf("state store error = %v, want %v", err, want)
	}
}

func TestNextUpRepository_EmptyStateReturnsNothing(t *testing.T) {
	// An exhausted empty page must return no rows without touching the catalog
	// pool (nil here), which also proves the page is not mistaken for a signal
	// to keep paging.
	repo := NewNextUpRepository(nil, stateProvider{store: &fakeNextUpStateStore{}})
	results, err := repo.ListNextUp(context.Background(), NextUpQuery{UserID: 1, ProfileID: "p", Limit: 20})
	if err != nil {
		t.Fatalf("ListNextUp: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results, got %+v", results)
	}
}

func TestNextUpRepository_EmptyProfileReturnsNothing(t *testing.T) {
	repo := NewNextUpRepository(nil, stateProvider{store: &fakeNextUpStateStore{}})
	for _, q := range []NextUpQuery{
		{UserID: 0, ProfileID: "p"},
		{UserID: 1, ProfileID: ""},
	} {
		results, err := repo.ListNextUp(context.Background(), q)
		if err != nil {
			t.Fatalf("ListNextUp(%+v): %v", q, err)
		}
		if len(results) != 0 {
			t.Fatalf("ListNextUp(%+v) = %+v, want empty", q, results)
		}
	}
}

func TestNextUpAnchorNewerThan(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mk := func(at time.Time, season, episode int, contentID string) nextUpAnchor {
		return nextUpAnchor{
			nextUpEpisode: nextUpEpisode{ContentID: contentID, SeasonNumber: season, EpisodeNumber: episode},
			UpdatedAt:     at,
		}
	}

	if !mk(base.Add(time.Minute), 1, 1, "a").newerThan(mk(base, 9, 9, "z")) {
		t.Fatal("newer updated_at must win regardless of episode")
	}
	if mk(base, 1, 1, "a").newerThan(mk(base.Add(time.Minute), 1, 1, "a")) {
		t.Fatal("older updated_at must lose")
	}
	if !mk(base, 2, 1, "a").newerThan(mk(base, 1, 9, "z")) {
		t.Fatal("higher season must win on a timestamp tie")
	}
	if !mk(base, 1, 2, "a").newerThan(mk(base, 1, 1, "z")) {
		t.Fatal("higher episode must win on a season tie")
	}
	if !mk(base, 1, 1, "z").newerThan(mk(base, 1, 1, "a")) {
		t.Fatal("higher content id must win on an episode tie")
	}
	if mk(base, 1, 1, "a").newerThan(mk(base, 1, 1, "a")) {
		t.Fatal("identical anchors must not be newer")
	}
}

func TestNextUpPageBelowThreshold(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	page := userstore.NextUpStatePage{Entries: []userstore.NextUpStateEntry{
		{MediaItemID: "a", UpdatedAt: base},
		{MediaItemID: "b", UpdatedAt: base.Add(-time.Minute)},
	}}
	if !nextUpPageBelowThreshold(page, base) {
		t.Fatal("page whose oldest entry is below the threshold must stop the walk")
	}
	if nextUpPageBelowThreshold(page, base.Add(-time.Minute)) {
		t.Fatal("page whose oldest entry ties the threshold must keep paging for further ties")
	}
	if !nextUpPageBelowThreshold(userstore.NextUpStatePage{}, base) {
		t.Fatal("empty page must be treated as below any threshold")
	}
}

func TestNextUpPageReachedCutoff(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := base.Add(time.Minute)
	q := NextUpQuery{DateCutoff: &cutoff}

	if nextUpPageReachedCutoff(NextUpQuery{}, userstore.NextUpStatePage{}) {
		t.Fatal("a nil cutoff must never report reached")
	}
	above := userstore.NextUpStatePage{Entries: []userstore.NextUpStateEntry{{UpdatedAt: base.Add(time.Hour)}}}
	if nextUpPageReachedCutoff(q, above) {
		t.Fatal("entries at or after the cutoff must not report reached")
	}
	below := userstore.NextUpStatePage{Entries: []userstore.NextUpStateEntry{{UpdatedAt: base}}}
	if !nextUpPageReachedCutoff(q, below) {
		t.Fatal("an entry before the cutoff must report reached")
	}
}

func TestNextUpStateContentIDsSkipsBlankIDs(t *testing.T) {
	got := nextUpStateContentIDs([]userstore.NextUpStateEntry{
		{MediaItemID: "a"},
		{MediaItemID: "   "},
		{MediaItemID: "b"},
	})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("nextUpStateContentIDs = %v, want [a b]", got)
	}
}
