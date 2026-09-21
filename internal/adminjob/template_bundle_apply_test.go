package adminjob

import (
	"encoding/json"
	"testing"
)

// TestTemplateBundleApplyPayloadPreservesVirtualPlaybackTriState pins the
// tri-state representation across the queued-job boundary. virtual_playback is
// a *bool so the job payload can distinguish an omitted field (nil, meaning
// the apply default of on) from an explicit false. A non-nil pointer survives
// the omitempty JSON round-trip even when it points at false, so an operator
// who toggled the setting off keeps non-virtual collections after a restart.
func TestTemplateBundleApplyPayloadPreservesVirtualPlaybackTriState(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantNil   bool
		wantValue bool
	}{
		{name: "omitted", payload: `{"bundle_id":"core_defaults","library_ids":[1]}`, wantNil: true},
		{name: "explicit_false", payload: `{"bundle_id":"core_defaults","library_ids":[1],"virtual_playback":false}`, wantValue: false},
		{name: "explicit_true", payload: `{"bundle_id":"core_defaults","library_ids":[1],"virtual_playback":true}`, wantValue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := decodeTemplateBundleApplyRequest(json.RawMessage(tc.payload))
			if err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if tc.wantNil {
				if req.VirtualPlayback != nil {
					t.Fatalf("omitted virtual_playback decoded to %v, want nil", *req.VirtualPlayback)
				}
				return
			}
			if req.VirtualPlayback == nil || *req.VirtualPlayback != tc.wantValue {
				t.Fatalf("decoded virtual_playback = %v, want %v", req.VirtualPlayback, tc.wantValue)
			}

			// Re-marshal the way the job queue persists the payload, then
			// decode again. omitempty must not drop the explicit false.
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			again, err := decodeTemplateBundleApplyRequest(raw)
			if err != nil {
				t.Fatalf("re-decode payload: %v", err)
			}
			if again.VirtualPlayback == nil || *again.VirtualPlayback != tc.wantValue {
				t.Fatalf("round-tripped virtual_playback = %v, want %v (raw %s)", again.VirtualPlayback, tc.wantValue, raw)
			}
		})
	}
}
