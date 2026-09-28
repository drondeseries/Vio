package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A replan for a session whose in-memory entry has been reaped must return the
// session_not_found 404 immediately, without queueing behind the replan slot
// budget. Production saw an ~11s slot wait before that verdict because the
// cheap lookups ran after the slot, per-session mutex, and advisory lock.
func TestHandleReplanPlaybackV3DeadSessionFailsFastWithoutReplanSlot(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: v3HandlerFixtureFile(t)})

	// The attempt row outlives the in-memory session (a reaped/stopped session
	// on this replica), so the store lookup succeeds and only the live-session
	// check can produce the 404.
	const sessionID = "11111111-1111-1111-1111-111111111111"
	const attemptID = "attempt-dead-session-0001"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:         sessionID,
		PlaybackAttemptID: attemptID,
		UserID:            1,
		ProfileID:         "profile-1",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	// Saturate the replan slot budget. A request that reaches the slot blocks
	// until the request context expires and reports capacity exhaustion; the
	// dead session must be rejected before that.
	handler.v3ReplanSlotsOnce.Do(func() { handler.v3ReplanSlots = make(chan struct{}, 1) })
	handler.v3ReplanSlots <- struct{}{}

	base := v3HandlerStartRequest()
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-dead-session-0001",
		FailedPlanID:          "plan-failed-0001",
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(newAuthorizedPlaybackContext(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(ctx)
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()

	started := time.Now()
	handler.HandleReplanPlaybackV3(rr, req)
	elapsed := time.Since(started)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("dead-session replan took %v; it queued for a replan slot instead of failing fast", elapsed)
	}
	// The dead session must not have consumed the saturated slot: a second
	// token still does not fit.
	select {
	case handler.v3ReplanSlots <- struct{}{}:
		t.Fatal("dead-session replan consumed the saturated replan slot")
	default:
	}
}
