package adminjob

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTemplateBundleApplyPayloadVirtualPlaybackContract pins the durable-job
// contract for virtual_playback. The producer resolves the API default at
// enqueue and stores an explicit boolean, so every newly written job carries
// the key and its meaning is independent of the worker binary that later
// executes it. A field-less payload predates the field and must decode to
// false, preserving its original non-virtual behavior.
func TestTemplateBundleApplyPayloadVirtualPlaybackContract(t *testing.T) {
	t.Run("explicit true round-trips", func(t *testing.T) {
		req, err := decodeTemplateBundleApplyRequest(json.RawMessage(`{"bundle_id":"core_defaults","library_ids":[1],"virtual_playback":true}`))
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if !req.VirtualPlayback {
			t.Fatalf("decoded virtual_playback = false, want true")
		}
	})

	t.Run("explicit false is serialized and round-trips", func(t *testing.T) {
		req, err := decodeTemplateBundleApplyRequest(json.RawMessage(`{"bundle_id":"core_defaults","library_ids":[1],"virtual_playback":false}`))
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if req.VirtualPlayback {
			t.Fatalf("decoded virtual_playback = true, want false")
		}

		// The producer must always write the key: an omitted false would be
		// indistinguishable from a legacy payload and could be read as on by
		// an older worker, so the field must not carry omitempty.
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if !strings.Contains(string(raw), `"virtual_playback":false`) {
			t.Fatalf("serialized payload dropped explicit false: %s", raw)
		}
	})

	t.Run("legacy field-less payload decodes off", func(t *testing.T) {
		req, err := decodeTemplateBundleApplyRequest(json.RawMessage(`{"bundle_id":"core_defaults","library_ids":[1]}`))
		if err != nil {
			t.Fatalf("decode legacy payload: %v", err)
		}
		if req.VirtualPlayback {
			t.Fatal("legacy field-less payload decoded on; must preserve its original off meaning")
		}
	})
}
