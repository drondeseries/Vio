package playback

import (
	"errors"
	"testing"
)

// TestAutoFallbackNegotiation proves the session Auto flag round-trips and that
// an unset session reports ok=false so the caller keeps its own default.
func TestAutoFallbackNegotiation(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, ok := manager.AutoFallback(session.ID); ok {
		t.Fatal("AutoFallback reported set before any negotiation")
	}
	if err := manager.SetAutoFallback(session.ID, true); err != nil {
		t.Fatalf("SetAutoFallback: %v", err)
	}
	if enabled, ok := manager.AutoFallback(session.ID); !ok || !enabled {
		t.Fatalf("AutoFallback = (%v, %v), want (true, true)", enabled, ok)
	}
	if err := manager.SetAutoFallback(session.ID, false); err != nil {
		t.Fatalf("SetAutoFallback off: %v", err)
	}
	if enabled, ok := manager.AutoFallback(session.ID); !ok || enabled {
		t.Fatalf("AutoFallback = (%v, %v), want (false, true)", enabled, ok)
	}
	if err := manager.SetAutoFallback("missing", true); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetAutoFallback missing err = %v, want ErrSessionNotFound", err)
	}
}
