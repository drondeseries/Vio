package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type episodeReleaseDateStub struct {
	calls int
	err   error
}

func (s *episodeReleaseDateStub) EpisodeReleaseDates(context.Context, int, int) (map[int]time.Time, error) {
	s.calls++
	return map[int]time.Time{1: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, s.err
}

func TestEpisodeReleaseFallbackPreservesKnownDates(t *testing.T) {
	provider := &episodeReleaseDateStub{}
	reg := &VirtualMediaRegistrar{EpisodeReleaseDates: provider}
	future := time.Now().Add(time.Hour)
	in := VirtualMedia{MediaType: "series", TMDBID: "42", Episodes: []VirtualEpisode{
		{SeasonNumber: 1, EpisodeNumber: 1},
		{SeasonNumber: 1, EpisodeNumber: 2},
		{SeasonNumber: 1, EpisodeNumber: 3, AirDate: future},
	}}
	out := reg.fillEpisodeReleaseDates(context.Background(), in)
	if provider.calls != 1 || out.Episodes[0].AirDate.IsZero() || !out.Episodes[1].AirDate.IsZero() || !out.Episodes[2].AirDate.Equal(future) {
		t.Fatalf("calls=%d episodes=%+v", provider.calls, out.Episodes)
	}
	provider.err = errors.New("unavailable")
	in.Episodes[0].AirDate = time.Time{}
	out = reg.fillEpisodeReleaseDates(context.Background(), in)
	if !out.Episodes[0].AirDate.IsZero() {
		t.Fatal("failed provider response supplied release evidence")
	}
}

type memoryReleaseOverrides map[ReleaseIdentity]ReleaseOverride

func (m memoryReleaseOverrides) Lookup(_ context.Context, id ReleaseIdentity) (ReleaseOverride, error) {
	return m[id], nil
}

func TestReleaseOverrideIdentityAndDates(t *testing.T) {
	id := ReleaseIdentity{MediaType: "episode", Provider: "tmdb", ProviderID: "42", SeasonNumber: 1, EpisodeNumber: 2}
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ReleaseIdentity{
		{MediaType: "episode", Provider: "tmdb", ProviderID: "042", SeasonNumber: 1, EpisodeNumber: 2},
		{MediaType: "episode", Provider: "tmdb", ProviderID: "42", SeasonNumber: 0, EpisodeNumber: 2},
		{MediaType: "movie", Provider: "tvdb", ProviderID: "42"},
		{MediaType: "movie", Provider: "imdb", ProviderID: "42"},
	} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	for _, input := range []string{"2020-01-01", "2020-01-01T01:00:00+01:00"} {
		date, err := (ReleaseOverrideMutation{ReleaseIdentity: id, ReleaseAt: input, EvidenceNote: "verified"}).validate(false)
		if err != nil || date.Format(time.RFC3339) != "2020-01-01T00:00:00Z" {
			t.Fatalf("date %v %v", date, err)
		}
	}
	for _, note := range []string{"", " ", "bad\x00note"} {
		if _, err := (ReleaseOverrideMutation{ReleaseIdentity: id, ReleaseAt: "2020-01-01", EvidenceNote: note}).validate(false); err == nil {
			t.Fatal("accepted invalid note")
		}
	}
}

func TestReleaseOverrideFutureAndClear(t *testing.T) {
	ctx := context.Background()
	id := ReleaseIdentity{MediaType: "movie", Provider: "tmdb", ProviderID: "42"}
	future := time.Now().UTC().Add(time.Hour)
	lookup := memoryReleaseOverrides{id: {ReleaseAt: &future, Revision: 1}}
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{42: true}}
	gate := newTheatricalReleaseGate(checker, lookup)
	if allowed, err := gate.lookup(ctx, 42); err != nil || allowed {
		t.Fatalf("future override ignored: %v %v", allowed, err)
	}
	lookup[id] = ReleaseOverride{Action: "clear", Revision: 2}
	if allowed, err := gate.lookup(ctx, 42); err != nil || !allowed {
		t.Fatalf("clear did not restore provider: %v %v", allowed, err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	lookup[id] = ReleaseOverride{ReleaseAt: &past, Revision: 3}
	if newTheatricalReleaseGate(nil, lookup).skipTheatricalMovie(ctx, 42, "", "Past Override", 9999, "") {
		t.Fatal("verified past date should override provider future year")
	}
	other := ReleaseIdentity{MediaType: "episode", Provider: "tmdb", ProviderID: "42", SeasonNumber: 1, EpisodeNumber: 1}
	if _, active, err := releaseOverrideDecision(ctx, lookup, []ReleaseIdentity{other}); err != nil || active {
		t.Fatal("movie override leaked into episode")
	}
}

type errAliasQuerier struct{ err error }

type errAliasRow struct{ err error }

func (r errAliasRow) Scan(...any) error { return r.err }

func (q errAliasQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return errAliasRow(q)
}

func (q errAliasQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, q.err
}

func TestReleaseAliasDiscoveryPropagatesReadFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("connection reset")
	if _, err := releaseIdentitiesForContent(ctx, errAliasQuerier{boom}, "movie", "movie-tmdb-1", "movie", "1", "", "", 0, 0); !errors.Is(err, boom) {
		t.Fatalf("scalar read failure = %v, want %v", err, boom)
	}
	if _, err := releaseIdentitiesForContent(ctx, nil, "movie", "movie-tmdb-1", "movie", "1", "", "", 0, 0); err != nil {
		t.Fatalf("nil querier must degrade to incoming-only, got %v", err)
	}
}

func TestReleaseOverrideRequiresActor(t *testing.T) {
	repo := NewReleaseOverrideRepository(nil)
	if _, err := repo.Mutate(context.Background(), 0, ReleaseOverrideMutation{}, false); !errors.Is(err, ErrReleaseOverrideForbidden) {
		t.Fatal(err)
	}
	if _, err := repo.Read(context.Background(), 0, ReleaseIdentity{}, 0, 1); !errors.Is(err, ErrReleaseOverrideForbidden) {
		t.Fatal(err)
	}
}

func TestReleaseOverrideAuditAndConcurrency(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	var actor int
	if err := pool.QueryRow(ctx, `INSERT INTO users(username,role,enabled) VALUES($1,'admin',true) RETURNING id`, fmt.Sprintf("override-%d", time.Now().UnixNano())).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, actor) })
	repo := NewReleaseOverrideRepository(pool)
	id := ReleaseIdentity{MediaType: "movie", Provider: "tmdb", ProviderID: strconv.FormatInt(time.Now().UnixNano(), 10)}
	m := ReleaseOverrideMutation{ReleaseIdentity: id, ReleaseAt: "2099-01-01", EvidenceNote: "verified publication"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := repo.Mutate(ctx, actor, m, false); results <- err })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrReleaseOverrideConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	m.ExpectedRevision, m.ReleaseAt, m.EvidenceNote = 1, "", "restore provider policy"
	if _, err := repo.Mutate(ctx, actor, m, true); err != nil {
		t.Fatal(err)
	}
	history, err := repo.Read(ctx, actor, id, 0, 100)
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	if history[0].Action != "clear" || history[0].ReleaseAt != nil || history[0].Revision != 2 || history[1].ActorAccountID != actor || history[1].EvidenceNote != "verified publication" {
		t.Fatalf("history=%+v", history)
	}
	if _, err := pool.Exec(ctx, `UPDATE verified_release_override_history SET evidence_note='tampered' WHERE provider_id=$1`, id.ProviderID); err == nil {
		t.Fatal("audit history was mutable")
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='user' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read(ctx, actor, id, 0, 1); !errors.Is(err, ErrReleaseOverrideForbidden) {
		t.Fatalf("demoted read: %v", err)
	}
	m.ExpectedRevision, m.ReleaseAt = 2, "2020-01-01"
	if _, err := repo.Mutate(ctx, actor, m, false); !errors.Is(err, ErrReleaseOverrideForbidden) {
		t.Fatalf("demoted write: %v", err)
	}
}

func TestReleaseOverrideConflictingAliasesFailClosed(t *testing.T) {
	ids := releaseIdentities("episode", "42", "99", "tt123", 1, 2)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	lookup := memoryReleaseOverrides{ids[0]: {ReleaseAt: &past}, ids[1]: {ReleaseAt: &future}}
	if allowed, active, err := releaseOverrideDecision(context.Background(), lookup, ids); err != nil || allowed || !active {
		t.Fatalf("%v %v %v", allowed, active, err)
	}
}
