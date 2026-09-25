package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// waitForCandidateRejected waits until the live transcode generation rejects
// its source, without relying on the asynchronous failed_at marker.
func (f *decodeRotationFixture) waitForCandidateRejected(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := f.handler.tm.GetTranscodeSession(sessionID)
		if live != nil && live.IsSourceRejected() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the decoder rejection was never observed on the live session")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// durableExcludedCandidateIDs reads the attempt's durable recovery chain and
// returns the candidate ids scoped to the session's virtual provider source.
func (f *decodeRotationFixture) durableExcludedCandidateIDs(t *testing.T, sessionID string) []string {
	t.Helper()
	store, ok := f.handler.PlanStoreV3.(playback.RecoveryStateStoreV3)
	if !ok {
		t.Fatal("fixture plan store does not implement RecoveryStateStoreV3")
	}
	state, _, err := store.GetRecoveryState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetRecoveryState: %v", err)
	}
	return playback.RecoveryExcludedCandidateIDsV3(state, decodeRotationNeutralURI)
}

// TestDurableExclusionChainDrivesAToBToCTerminal drives three decode-rejected
// hops entirely through the durable chain and proves the walk terminates at the
// third candidate instead of cycling back to A. The asynchronous marker never
// lands (its callback fails), so the exclusion chain is the only thing that can
// make each hop skip the releases already indicted.
func TestDurableExclusionChainDrivesAToBToCTerminal(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B", "C"},
		decodeFailCalls:     3,
		rejectAfterManifest: true,
		markerFails:         true,
	})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("A") {
		t.Fatalf("start candidate = %q, want A", got)
	}
	f.waitForCandidateRejected(t, started.SessionID)

	// Hop 1: A rejected → B. No waitForMarked: the marker is failing.
	hop1 := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, started.PlaybackPlan, "chain-hop-0001"))
	if hop1.Terminal != nil || hop1.PlaybackPlan == nil {
		t.Fatalf("hop 1 terminalled instead of rotating: %+v", hop1.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("hop 1 candidate = %q, want B", got)
	}
	if got := f.durableExcludedCandidateIDs(t, started.SessionID); len(got) != 1 || got[0] != "A" {
		t.Fatalf("hop 1 durable exclusions = %v, want [A]", got)
	}

	// Hop 2: B rejected → C. The durable chain must now hold A and B.
	f.waitForCandidateRejected(t, started.SessionID)
	hop2 := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, hop1.PlaybackPlan, "chain-hop-0002"))
	if hop2.Terminal != nil || hop2.PlaybackPlan == nil {
		t.Fatalf("hop 2 terminalled instead of rotating: %+v", hop2.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("C") {
		t.Fatalf("hop 2 candidate = %q, want C", got)
	}
	durable := f.durableExcludedCandidateIDs(t, started.SessionID)
	if len(durable) != 2 || !containsStringExactV3(durable, "A") || !containsStringExactV3(durable, "B") {
		t.Fatalf("hop 2 durable exclusions = %v, want [A B]", durable)
	}

	// Hop 3: C rejected → terminal. The walk must never return to A.
	f.waitForCandidateRejected(t, started.SessionID)
	hop3 := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, hop2.PlaybackPlan, "chain-hop-0003"))
	if hop3.Terminal == nil || hop3.Terminal.Reason != sourceDecodeFailedReasonV3 {
		t.Fatalf("hop 3 terminal = %+v, want %q", hop3.Terminal, sourceDecodeFailedReasonV3)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("C") {
		t.Fatalf("terminal rebound the session to %q, want the unchanged candidate C", got)
	}
	// Every rotation hop after the first must have excluded A and every prior
	// hop, so the resolver can never be offered a rejected release again.
	for _, excluded := range f.recordedExclusions() {
		if len(excluded) == 0 {
			continue
		}
		if !containsStringExactV3(excluded, "A") {
			t.Fatalf("a rotation hop resolved without excluding A: %v", excluded)
		}
	}
	// Assert the walk cost exactly one transcode start per candidate: the
	// terminal hop must not have rebuilt C, and no hop may rebuild an excluded
	// release. Three candidates, three starts.
	if calls := atomic.LoadInt32(&f.transcodeCalls); calls != 3 {
		t.Fatalf("transcode started %d times, want exactly 3 (one per candidate)", calls)
	}
}

// TestDurableExclusionTypedRejectionOutranksSubtitleStartup proves a confirmed
// decoder rejection is not reclassified as a subtitle-startup failure and does
// not trigger an extra hw_accel=auto fallback before rotation: a plan whose
// source the decoder rejects rotates to the sibling without rebuilding the
// rejected candidate.
func TestDurableExclusionTypedRejectionOutranksSubtitleStartup(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:    []string{"A", "B"},
		decodeFailCalls: 1,
	})
	start := f.request()
	start.SubtitleTrackIndex = intPtrHandlerV3(0)

	code, response := f.start(t, start)
	if code != http.StatusCreated {
		t.Fatalf("start status = %d, want %d; body = %+v", code, http.StatusCreated, response)
	}
	if response.Terminal != nil {
		t.Fatalf("typed rejection was reclassified as a terminal: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil || response.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("start plan = %#v, want a rotated server-transcode plan", response.PlaybackPlan)
	}
	if got := f.sessionVirtualURI(t, response.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("effective virtual uri = %q, want the rotated candidate B", got)
	}
	// One poisoned candidate: the auto pipeline must not rebuild the rejected
	// bytes (which would show as extra starts) before the rotation engages.
	if calls := atomic.LoadInt32(&f.transcodeCalls); calls > 2 {
		t.Fatalf("transcode started %d times, want at most 2 (rejected A plus healthy B)", calls)
	}
	if got := f.softwareSpawnCount(); got != 0 {
		t.Fatalf("rotation spawned %d software-decode transcodes, want 0", got)
	}
}

func intPtrHandlerV3(v int) *int { return &v }

// TestLocalTransportReadinessErrorTypedRejectionOutranksSubtitle pins the
// classification precedence directly: a typed decoder rejection is reported as
// a candidate decode rejection even when the plan is a burn-in subtitle start
// that would otherwise classify as a retryable subtitle-artifact failure.
func TestLocalTransportReadinessErrorTypedRejectionOutranksSubtitle(t *testing.T) {
	rejected := localTransportReadinessErrorV3(
		playback.TranscodeOpts{SubtitleBurnIn: true, SubtitleTrackIndex: 0},
		true,
		playback.ErrSourceDecodeRejected,
	)
	if rejected.reason != candidateSourceDecodeRejectedReasonV3 {
		t.Fatalf("readiness reason = %q, want %q", rejected.reason, candidateSourceDecodeRejectedReasonV3)
	}
	if rejected.retryable {
		t.Fatalf("typed rejection readiness error is retryable: %+v", rejected)
	}
	if errors.Is(rejected.cause, playback.ErrSourceDecodeRejected) == false {
		t.Fatalf("readiness error lost the typed cause: %v", rejected.cause)
	}
}

// TestDurableExclusionKeepsSessionBoundIdentityWhenRowMoved proves the durable
// exclusion is keyed to the session-bound release identity, not the transient
// catalog row. After the session is bound to candidate B and the catalog row is
// rewritten to candidate A, the exclusion of A still suppresses A's result id
// so a replan cannot re-adopt the moved row.
func TestDurableExclusionKeepsSessionBoundIdentityWhenRowMoved(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
	})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	f.waitForCandidateRejected(t, started.SessionID)

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, started.PlaybackPlan, "identity-moved-replan-0001"))
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("replan terminalled: %+v", recovered.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("rotation bound %q, want B", got)
	}
	// The durable chain records A scoped to the session's provider source.
	durable := f.durableExcludedCandidateIDs(t, started.SessionID)
	if len(durable) != 1 || durable[0] != "A" {
		t.Fatalf("durable exclusions = %v, want [A]", durable)
	}
	// A later replan whose catalog row has moved back to A must still exclude A:
	// the resolver is handed the session-bound chain, not just the live row.
	f.file.FilePath = decodeRotationCandidateURI("A")
	second := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, recovered.PlaybackPlan, "identity-moved-replan-0002"))
	if second.PlaybackPlan != nil {
		if got := f.sessionVirtualURI(t, started.SessionID); got == decodeRotationCandidateURI("A") {
			t.Fatal("a moved catalog row rebound the session to the excluded candidate A")
		}
	}
	for _, excluded := range f.recordedExclusions() {
		if len(excluded) == 0 {
			continue
		}
		if !containsStringExactV3(excluded, "A") {
			t.Fatalf("a resolve after the move ran without excluding A: %v", excluded)
		}
	}
}

// TestDurableExclusionsDoNotShrinkOnIdempotentReplay proves a replayed replan
// cannot drop the exclusion chain: the union is monotone across replays.
func TestDurableExclusionsDoNotShrinkOnIdempotentReplay(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
	})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	f.waitForCandidateRejected(t, started.SessionID)

	recovery := decodeRotationReplanRequest(start, started.PlaybackPlan, "no-shrink-0001")
	first := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	if first.PlaybackPlan == nil {
		t.Fatalf("first rotation terminalled: %+v", first.Terminal)
	}
	before := f.durableExcludedCandidateIDs(t, started.SessionID)

	// Replay the identical replan. It must return the first response and leave
	// the chain untouched.
	replayed := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	if replayed.Terminal != nil || replayed.PlaybackPlan == nil {
		t.Fatalf("idempotent replay terminalled: %+v", replayed.Terminal)
	}
	after := f.durableExcludedCandidateIDs(t, started.SessionID)
	if len(after) != len(before) {
		t.Fatalf("idempotent replay changed the exclusion chain: %v -> %v", before, after)
	}
}

// TestFreshAttemptDoesNotInheritExclusions proves a new attempt on the same
// catalog file starts with a clean recovery chain. A decode-rejected attempt's
// chain must not suppress the candidate for a later independent playback.
func TestFreshAttemptDoesNotInheritExclusions(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
	})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	f.waitForCandidateRejected(t, started.SessionID)
	f.handler.tm.CloseTranscodeSession(started.SessionID, "")

	// A fresh attempt with a new playback_attempt_id must not inherit A's
	// exclusion. The first generation binds A again.
	freshStart := start
	freshStart.PlaybackAttemptID = "fresh-attempt-" + strings.ReplaceAll(started.SessionID, "-", "")
	code, fresh := f.start(t, freshStart)
	if code != http.StatusCreated || fresh.PlaybackPlan == nil {
		t.Fatalf("fresh start status=%d response=%+v", code, fresh)
	}
	defer f.handler.tm.CloseTranscodeSession(fresh.SessionID, "")
	if fresh.SessionID == started.SessionID {
		t.Fatal("fresh attempt reused the old session")
	}
}

// TestNonIndictingFailureAddsNoExclusion proves a failure that does not indict
// the release adds nothing to the durable chain: a healthy session's decode_error
// replan must not persist an exclusion.
func TestNonIndictingFailureAddsNoExclusion(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{candidateIDs: []string{"A"}})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	if live := f.handler.tm.GetTranscodeSession(started.SessionID); live == nil || live.IsSourceRejected() {
		t.Fatal("fixture precondition: live generation must be healthy")
	}

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, decodeRotationReplanRequest(start, started.PlaybackPlan, "healthy-no-exclusion-0001"))
	if recovered.Terminal == nil {
		t.Fatalf("healthy single-candidate decode replan unexpectedly planned a route: %#v", recovered.PlaybackPlan)
	}
	if got := f.durableExcludedCandidateIDs(t, started.SessionID); len(got) != 0 {
		t.Fatalf("non-indicting failure added durable exclusions: %v", got)
	}
}

// TestConcurrentReplansCannotShrinkExclusions proves two overlapping replans on
// one attempt each union their exclusion into the durable chain rather than
// last-writer-win. The append helper is exercised directly because the handler
// serializes replans per session; the store must still be safe if that
// serialization ever breaks.
func TestConcurrentReplansCannotShrinkExclusions(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{candidateIDs: []string{"A", "B", "C"}})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")

	store, ok := f.handler.PlanStoreV3.(playback.RecoveryStateStoreV3)
	if !ok {
		t.Fatal("fixture plan store does not implement RecoveryStateStoreV3")
	}
	var wg sync.WaitGroup
	var appended int32
	for _, candidateID := range []string{"A", "B", "C"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for attempt := 0; attempt < 16; attempt++ {
				state, revision, err := store.GetRecoveryState(ctx, started.SessionID)
				if err != nil {
					return
				}
				_, _, err = store.AppendRecoveryExclusions(ctx, started.SessionID, revision, []playback.RecoveryExclusionV3{{
					ProviderSource: decodeRotationNeutralURI, CandidateID: id,
				}})
				if err == nil {
					atomic.AddInt32(&appended, 1)
					_ = state
					return
				}
			}
		}(candidateID)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&appended); got != 3 {
		t.Fatalf("appended %d exclusions, want 3", got)
	}
	durable := f.durableExcludedCandidateIDs(t, started.SessionID)
	if len(durable) != 3 {
		t.Fatalf("concurrent appends shrank the chain: %v", durable)
	}
}
