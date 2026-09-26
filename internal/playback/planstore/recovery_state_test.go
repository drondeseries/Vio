package planstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestPostgresRecoveryState covers the durable attempt-scoped exclusion chain
// against a real migrated Postgres. The store is the mechanism item C depends
// on: a confirmed candidate failure must survive a simulated node reload (a
// fresh pool reading the same row), concurrent appends must union rather than
// clobber, and a fresh attempt must inherit nothing.
func TestPostgresRecoveryState(t *testing.T) {
	f := newPlanstoreFixture(t)
	store := NewPostgres(f.pool)
	ctx := context.Background()

	seed := func(t *testing.T, sessionID string) {
		t.Helper()
		record := f.attemptRecord(sessionID, "att-recovery-"+sessionID, "digest-recovery")
		if err := store.SaveAttempt(ctx, record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
	}

	t.Run("FreshAttemptStartsWithNoExclusions", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		state, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		if len(state.Exclusions) != 0 {
			t.Fatalf("fresh attempt inherited exclusions: %+v", state.Exclusions)
		}
		if revision != 0 {
			t.Fatalf("fresh attempt revision = %d, want 0", revision)
		}
	})

	t.Run("AppendPersistsAndSurvivesFreshPoolReload", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		_, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		merged, next, err := store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{{
			ProviderSource: "virtual://movie/tt-recovery",
			CandidateID:    "A",
			ReleaseID:      "guid-a",
			FileID:         f.mediaFileID,
		}})
		if err != nil {
			t.Fatalf("AppendRecoveryExclusions: %v", err)
		}
		if len(merged.Exclusions) != 1 || merged.Exclusions[0].CandidateID != "A" {
			t.Fatalf("append state = %+v, want candidate A", merged.Exclusions)
		}
		if next != revision+1 {
			t.Fatalf("revision = %d, want %d", next, revision+1)
		}

		// Simulate a node reload: a completely fresh pool with no shared state
		// must read the durable chain back from Postgres.
		freshStore := NewPostgres(f.freshPool(t))
		reloaded, reloadedRevision, err := freshStore.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("reloaded GetRecoveryState: %v", err)
		}
		if len(reloaded.Exclusions) != 1 || reloaded.Exclusions[0].CandidateID != "A" {
			t.Fatalf("reloaded chain = %+v, want candidate A", reloaded.Exclusions)
		}
		if reloadedRevision != next {
			t.Fatalf("reloaded revision = %d, want %d", reloadedRevision, next)
		}

		// The attempt record read also carries the chain, so the replan path
		// sees it without a second query.
		record, err := freshStore.GetAttempt(ctx, sessionID)
		if err != nil {
			t.Fatalf("reloaded GetAttempt: %v", err)
		}
		if len(record.RecoveryState.Exclusions) != 1 || record.RecoveryRevision != next {
			t.Fatalf("reloaded record recovery = %+v rev=%d, want A rev=%d", record.RecoveryState.Exclusions, record.RecoveryRevision, next)
		}
	})

	t.Run("AppendIsIdempotent", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		exclusion := playback.RecoveryExclusionV3{ProviderSource: "virtual://movie/tt-idem", CandidateID: "A"}
		_, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		first, _, err := store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{exclusion})
		if err != nil {
			t.Fatalf("first append: %v", err)
		}
		// Replay the same append at the now-current revision.
		second, _, err := store.AppendRecoveryExclusions(ctx, sessionID, revision+1, []playback.RecoveryExclusionV3{exclusion})
		if err != nil {
			t.Fatalf("idempotent append: %v", err)
		}
		if len(first.Exclusions) != 1 || len(second.Exclusions) != 1 {
			t.Fatalf("idempotent append grew the chain: first=%+v second=%+v", first.Exclusions, second.Exclusions)
		}
	})

	t.Run("StaleRevisionIsRejectedNotClobbered", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		_, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		if _, _, err := store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{{ProviderSource: "s", CandidateID: "A"}}); err != nil {
			t.Fatalf("append A: %v", err)
		}
		// A second writer still holding the pre-append revision loses.
		_, _, err = store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{{ProviderSource: "s", CandidateID: "B"}})
		if !errors.Is(err, playback.ErrRecoveryRevisionConflictV3) {
			t.Fatalf("stale append err = %v, want ErrRecoveryRevisionConflictV3", err)
		}
		// The committed chain is untouched: A survived, B never landed.
		state, _, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState after conflict: %v", err)
		}
		if !playback.RecoveryCandidateExcludedV3(state, "s", "A", "") {
			t.Fatalf("conflict dropped candidate A: %+v", state.Exclusions)
		}
		if playback.RecoveryCandidateExcludedV3(state, "s", "B", "") {
			t.Fatalf("stale append landed candidate B: %+v", state.Exclusions)
		}
	})

	t.Run("ConcurrentAppendsNeverShrinkTheChain", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		candidates := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var failures []error
		for _, candidateID := range candidates {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				// Each writer reads then appends, retrying a lost revision
				// compare exactly as the handler does.
				for attempt := 0; attempt < 16; attempt++ {
					state, revision, err := store.GetRecoveryState(ctx, sessionID)
					if err != nil {
						mu.Lock()
						failures = append(failures, err)
						mu.Unlock()
						return
					}
					_, _, err = store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{{ProviderSource: "s", CandidateID: id}})
					if err == nil {
						_ = state
						return
					}
					if !errors.Is(err, playback.ErrRecoveryRevisionConflictV3) {
						mu.Lock()
						failures = append(failures, err)
						mu.Unlock()
						return
					}
				}
				mu.Lock()
				failures = append(failures, errors.New("writer exhausted retries"))
				mu.Unlock()
			}(candidateID)
		}
		wg.Wait()
		if len(failures) > 0 {
			t.Fatalf("concurrent appends failed: %v", failures)
		}
		state, _, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("final GetRecoveryState: %v", err)
		}
		for _, candidateID := range candidates {
			if !playback.RecoveryCandidateExcludedV3(state, "s", candidateID, "") {
				t.Fatalf("concurrent append lost candidate %s: %+v", candidateID, state.Exclusions)
			}
		}
		if len(state.Exclusions) != len(candidates) {
			t.Fatalf("chain has %d exclusions, want %d: %+v", len(state.Exclusions), len(candidates), state.Exclusions)
		}
	})

	t.Run("ScopeIsolatesProviderSources", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		_, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		if _, _, err := store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{
			{ProviderSource: "provider-a", CandidateID: "1"},
			{ProviderSource: "provider-b", CandidateID: "1"},
		}); err != nil {
			t.Fatalf("append scoped: %v", err)
		}
		state, _, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		if !playback.RecoveryCandidateExcludedV3(state, "provider-a", "1", "") ||
			!playback.RecoveryCandidateExcludedV3(state, "provider-b", "1", "") {
			t.Fatalf("scoped exclusions did not both land: %+v", state.Exclusions)
		}
		if got := playback.RecoveryExcludedCandidateIDsV3(state, "provider-a"); len(got) != 1 || got[0] != "1" {
			t.Fatalf("provider-a scope ids = %v, want [1]", got)
		}
		if got := playback.RecoveryExcludedCandidateIDsV3(state, "provider-c"); len(got) != 0 {
			t.Fatalf("unrelated scope leaked ids: %v", got)
		}
	})

	t.Run("ReleaseIdentityMatchesRenumberedResult", func(t *testing.T) {
		sessionID := uuid.NewString()
		seed(t, sessionID)

		_, revision, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		if _, _, err := store.AppendRecoveryExclusions(ctx, sessionID, revision, []playback.RecoveryExclusionV3{{
			ProviderSource: "s", CandidateID: "A", ReleaseID: "guid-a",
		}}); err != nil {
			t.Fatalf("append: %v", err)
		}
		state, _, err := store.GetRecoveryState(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetRecoveryState: %v", err)
		}
		// The provider renumbered the same release: B is excluded by identity.
		if !playback.RecoveryCandidateExcludedV3(state, "s", "B", "guid-a") {
			t.Fatalf("renumbered release not matched by identity: %+v", state.Exclusions)
		}
		if playback.RecoveryCandidateExcludedV3(state, "s", "B", "guid-other") {
			t.Fatalf("different release matched: %+v", state.Exclusions)
		}
	})
}

// freshPool opens a second pool to the same database, so a read proves the
// chain lives in Postgres rather than in the store's process memory.
func (f *planstoreFixture) freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("SILO_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open fresh pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPostgresRecoveryAppendUnknownSession proves an append against a session
// with no live attempt row reports ErrSessionNotFound rather than creating one.
func TestPostgresRecoveryAppendUnknownSession(t *testing.T) {
	f := newPlanstoreFixture(t)
	store := NewPostgres(f.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := store.AppendRecoveryExclusions(ctx, uuid.NewString(), -1, []playback.RecoveryExclusionV3{{ProviderSource: "s", CandidateID: "A"}}); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("append unknown session err = %v, want ErrSessionNotFound", err)
	}
	if _, _, err := store.GetRecoveryState(ctx, uuid.NewString()); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("get unknown session err = %v, want ErrSessionNotFound", err)
	}
}
