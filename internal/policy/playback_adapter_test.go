package policy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/policy"
)

// stubActionChecker is a minimal policy.ActionChecker whose result is fixed.
type stubActionChecker struct {
	decision policy.ActionDecision
	err      error
}

func (s stubActionChecker) CheckAction(context.Context, policy.ActionInput) (policy.ActionDecision, policy.Meta, error) {
	return s.decision, policy.Meta{}, s.err
}

func TestPlaybackAdmissionDeciderMapsEvalTimeout(t *testing.T) {
	// The engine wraps ErrPolicyEvalTimeout with %w; the adapter must map it to
	// playback's local sentinel so admission can degrade to the inline caps.
	decider := policy.NewPlaybackAdmissionDecider(stubActionChecker{
		err: fmt.Errorf("%w: after 25ms", policy.ErrPolicyEvalTimeout),
	})
	got, err := decider(context.Background(), playback.AdmissionRequest{UserID: 1, RequestedMethod: playback.PlayDirect})
	if !errors.Is(err, playback.ErrAdmissionDeciderTimeout) {
		t.Fatalf("decider error = %v, want playback.ErrAdmissionDeciderTimeout", err)
	}
	if got != (playback.AdmissionDecision{}) {
		t.Fatalf("decider decision = %+v, want zero value on error", got)
	}

	// Every other error propagates unchanged and must NOT match the sentinel
	// that grants the inline fallback: a generic decider failure still denies.
	plain := errors.New("policy unavailable")
	decider = policy.NewPlaybackAdmissionDecider(stubActionChecker{err: plain})
	if _, err := decider(context.Background(), playback.AdmissionRequest{UserID: 1, RequestedMethod: playback.PlayDirect}); !errors.Is(err, plain) {
		t.Fatalf("plain decider error = %v, want the original error", err)
	} else if errors.Is(err, playback.ErrAdmissionDeciderTimeout) {
		t.Fatalf("plain decider error %v matched playback.ErrAdmissionDeciderTimeout", err)
	}
}
