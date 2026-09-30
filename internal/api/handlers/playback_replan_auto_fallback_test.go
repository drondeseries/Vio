package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestRenegotiateAutoFallbackV3 pins the mid-session re-arm: a replan that
// carries auto_fallback replaces the session flag so a later dead-source
// recovery rotates for a session that started explicit, while an omitted field
// leaves the negotiated intent untouched.
func TestRenegotiateAutoFallbackV3(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// The session starts explicit, which is the state the bug leaves behind.
	if err := manager.SetAutoFallback(session.ID, false); err != nil {
		t.Fatalf("SetAutoFallback off: %v", err)
	}
	handler := NewPlaybackHandler(manager)

	enabled := true
	handler.renegotiateAutoFallbackV3(context.Background(), session.ID, playback.ReplanRequestV3{AutoFallback: &enabled})
	if got, ok := manager.AutoFallback(session.ID); !ok || !got {
		t.Fatalf("after re-arm AutoFallback = (%v, %v), want (true, true)", got, ok)
	}

	handler.renegotiateAutoFallbackV3(context.Background(), session.ID, playback.ReplanRequestV3{})
	if got, ok := manager.AutoFallback(session.ID); !ok || !got {
		t.Fatalf("omitted auto_fallback changed the intent: AutoFallback = (%v, %v), want (true, true)", got, ok)
	}

	disabled := false
	handler.renegotiateAutoFallbackV3(context.Background(), session.ID, playback.ReplanRequestV3{AutoFallback: &disabled})
	if got, ok := manager.AutoFallback(session.ID); !ok || got {
		t.Fatalf("after disarming AutoFallback = (%v, %v), want (false, true)", got, ok)
	}
}
