package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/netaccess"
)

// NOTE (fork, stripped for SDK): provider RPCs need
// network_access_provider.v1 client support in the plugin SDK, which the
// pinned SDK predates. applyNetworkAccess answers unavailable for any
// operation; this pins that contract.
func TestNetworkAccessApplyAnswersUnavailable(t *testing.T) {
	f := newResidentFixture(t, ResidentOptions{})
	ctx := context.Background()
	provider := NetworkAccessProvider{InstallationID: 5, CapabilityID: "stub", Provider: "stub", DisplayName: "Stub Overlay"}

	got := f.service.applyNetworkAccess(ctx, provider)
	if got.InstallationID != 5 || got.Provider != "stub" {
		t.Fatalf("identity = %+v", got)
	}
	if got.State != netaccess.StateUnavailable {
		t.Fatalf("state = %q, want unavailable", got.State)
	}
	if !strings.Contains(got.Error, "plugin SDK") {
		t.Fatalf("error = %q, want the SDK reason", got.Error)
	}
	if _, ok := f.broker.Status.Get(5); ok {
		t.Fatal("unavailable answer populated the status cache")
	}
}
