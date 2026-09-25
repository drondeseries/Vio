package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// timeoutRotationReplanRequest mirrors decodeRotationReplanRequest with the
// generic classification a player reports when the transport never produces:
// the production incident classified an undecodable provider stream as
// startup_timeout, which the old predicate refused to rotate on.
func timeoutRotationReplanRequest(start playback.StartRequestV3, plan *playback.PlanV3, replanID string) playback.ReplanRequestV3 {
	req := decodeRotationReplanRequest(start, plan, replanID)
	req.Failure.Classification = "startup_timeout"
	return req
}

// TestPostCommitTimeoutReplanRotates replays the production incident end to
// end: a committed virtual generation whose decoder rejects the source,
// followed by a generic startup_timeout failure recovery, must exclude the
// session-bound candidate and commit the next release — without demoting the
// HLS delivery and without spawning a software decode.
func TestPostCommitTimeoutReplanRotates(t *testing.T) {
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
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("A") {
		t.Fatalf("start candidate = %q, want A", got)
	}
	f.waitForSourceRejected(t, started.SessionID)

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "timeout-rotation-replan-0001"))
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("timeout replan terminal=%+v", recovered.Terminal)
	}
	if recovered.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("rotated plan delivery = %s, want HLS", recovered.PlaybackPlan.Delivery)
	}
	if !containsExclusion(f.recordedExclusions(), "A") {
		t.Fatalf("timeout replan did not exclude the rejected session candidate; exclusions = %v", f.recordedExclusions())
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("replanned effective virtual uri = %q, want B", got)
	}
	if got := f.softwareSpawnCount(); got != 0 {
		t.Fatalf("timeout rotation spawned %d software-decode transcodes, want 0", got)
	}
	record, err := f.handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	caps := record.NormalizedRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3]
	if !caps.Enabled || caps.FailureReason == demoteDeliveryReasonV3 {
		t.Fatalf("HLS delivery was demoted while rotation was pending: %+v", caps)
	}
}

// TestPostCommitTimeoutWithoutVerdictDoesNotRotate proves the timeout gate is
// corroboration, not permission: a startup_timeout replan on a healthy
// session (no live decoder verdict) must not rotate with decode semantics.
func TestPostCommitTimeoutWithoutVerdictDoesNotRotate(t *testing.T) {
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

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "healthy-timeout-replan-0001"))
	if recovered.Terminal == nil {
		t.Fatalf("healthy single-candidate timeout replan unexpectedly planned a route: %#v", recovered.PlaybackPlan)
	}
	if recovered.Terminal.Reason == sourceDecodeFailedReasonV3 {
		t.Fatalf("healthy session was retagged as a decode rejection: %#v", recovered.Terminal)
	}
	for _, excluded := range f.recordedExclusions() {
		if len(excluded) > 0 {
			t.Fatalf("healthy timeout excluded candidates %v without a verdict", excluded)
		}
	}
}

// TestPostCommitTimeoutExplicitPinNeverRotates proves an explicit version pin
// is never substituted even when the timeout is corroborated: the recovery
// takes the pin-preserving software rebuild instead, the session keeps its
// binding, and nothing is excluded.
func TestPostCommitTimeoutExplicitPinNeverRotates(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
	})
	start := f.request()
	start.FileSelection = playback.FileSelectionExplicitV3

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	f.waitForSourceRejected(t, started.SessionID)

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "explicit-timeout-replan-0001"))
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("explicit pin replan terminalled instead of rebuilding in place: %#v", recovered.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("A") {
		t.Fatalf("explicit pin rebound to %q, want the unchanged candidate A", got)
	}
	for _, excluded := range f.recordedExclusions() {
		if len(excluded) > 0 {
			t.Fatalf("explicit pin excluded candidates %v; an explicit pin is never substituted", excluded)
		}
	}
}

// TestPostCommitTimeoutAllCandidatesRejected proves exhaustion terminates
// instead of cycling: with every sibling carrying an active failed_at stamp
// from prior hops, a corroborated timeout replan finds no eligible release
// and terminals source_decode_failed (never a preempting
// adaptation_exhausted, never a retryable terminal), while the session keeps
// its binding. Rotation honors stamps rather than bypassing them, so each
// hop's verdict narrows the next hop instead of reopening rejected releases.
func TestPostCommitTimeoutAllCandidatesRejected(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{
		candidateIDs:        []string{"A", "B", "C"},
		decodeFailCalls:     1,
		rejectAfterManifest: true,
		stampedCandidates:   map[string]bool{"B": true, "C": true},
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
	f.waitForSourceRejected(t, started.SessionID)

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "timeout-exhaustion-replan-0001"))
	if recovered.Terminal == nil || recovered.Terminal.Reason != sourceDecodeFailedReasonV3 {
		t.Fatalf("exhausted timeout rotation terminal = %#v, want %q", recovered.Terminal, sourceDecodeFailedReasonV3)
	}
	if recovered.Terminal.Retryable {
		t.Fatalf("exhausted decode terminal is retryable: %#v", recovered.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("A") {
		t.Fatalf("exhausted rotation rebound the session to %q, want the unchanged candidate A", got)
	}
	if !containsExclusion(f.recordedExclusions(), "A") {
		t.Fatalf("rotation did not record the rejected exclusion: %v", f.recordedExclusions())
	}
}

// TestPostCommitTimeoutStampLandsBeforeResolve proves the async failure
// stamp completes before the next resolve runs: after the verdict, the
// marker must land on the rejected candidate (observable via the fixture's
// marked tracking) so a subsequent replan resolves against landed stamps,
// not just the explicit exclusion list. Without this, cross-replan
// exhaustion would depend on winning a race between the 3s marker budget
// and the client's replan round-trip.
func TestPostCommitTimeoutStampLandsBeforeResolve(t *testing.T) {
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
	f.waitForSourceRejected(t, started.SessionID)
	f.waitForMarked(t)

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if len(marked) == 0 {
		t.Fatal("verdict recorded but the candidate stamp never landed")
	}

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "timeout-stamp-replan-0001"))
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("timeout replan terminalled instead of rotating: %#v", recovered.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("replanned effective virtual uri = %q, want B", got)
	}
}

// timeout replan_request_id returns the first rotation response without
// rotating again.
func TestPostCommitTimeoutDuplicateReplanIdempotent(t *testing.T) {
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
	f.waitForSourceRejected(t, started.SessionID)

	recovery := timeoutRotationReplanRequest(start, started.PlaybackPlan, "idempotent-timeout-replan-0001")
	first := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	if first.PlaybackPlan == nil {
		t.Fatalf("first timeout rotation replan returned a terminal: %#v", first.Terminal)
	}
	firstResolves := len(f.recordedExclusions())
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	second := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("idempotent replay diverged:\n first=%s\nsecond=%s", firstJSON, secondJSON)
	}
	if got := len(f.recordedExclusions()); got != firstResolves {
		t.Fatalf("idempotent replay re-ran rotation: resolves %d -> %d", firstResolves, got)
	}
}

// TestPostCommitStaleTimeoutReplanConflicts proves a timeout replan naming a
// foreign attempt never rotates: the session keeps candidate B from the
// completed rotation and the caller gets a stale-plan conflict.
func TestPostCommitStaleTimeoutReplanConflicts(t *testing.T) {
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
	f.waitForSourceRejected(t, started.SessionID)

	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, timeoutRotationReplanRequest(start, started.PlaybackPlan, "timeout-rotation-replan-0001"))
	if recovered.PlaybackPlan == nil {
		t.Fatalf("rotation replan terminalled: %#v", recovered.Terminal)
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("rotated uri = %q, want B", got)
	}

	stale := timeoutRotationReplanRequest(start, started.PlaybackPlan, "stale-timeout-replan-0002")
	stale.PlaybackAttemptID = "00000000-0000-0000-0000-000000000000"
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+started.SessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", started.SessionID)
	rr := httptest.NewRecorder()
	f.handler.HandleReplanPlaybackV3(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale replan status = %d, want %d; body = %s", rr.Code, http.StatusConflict, rr.Body.String())
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != decodeRotationCandidateURI("B") {
		t.Fatalf("stale replan moved the session to %q, want the committed candidate B", got)
	}
}
