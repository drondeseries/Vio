package adminjob

import (
	"encoding/json"
	"fmt"
)

const JobTypeTemplateBundleApply = "template_bundle_apply"

type TemplateBundleApplyRequest struct {
	BundleID       string `json:"bundle_id"`
	LibraryIDs     []int  `json:"library_ids"`
	DeleteExisting bool   `json:"delete_existing"`
	// VirtualPlayback is the effective virtual-playback choice. The producer
	// resolves the API default at enqueue time and always stores an explicit
	// value; the field has no omitempty so a stored false is serialized and
	// never mistaken for an omitted field.
	//
	// Legacy payloads written before this field existed have no
	// virtual_playback key and decode to false, which preserves their original
	// behavior. A field-less payload is therefore always treated as "off";
	// only payloads written by a producer that resolves the default carry an
	// explicit true. Do not reintroduce a pointer/*bool here: at the job
	// boundary nil must not mean "default on", or already-queued legacy jobs
	// would be reinterpreted. See decodeTemplateBundleApplyRequest.
	VirtualPlayback bool                                `json:"virtual_playback"`
	Featured        *TemplateBundleApplyFeaturedRequest `json:"featured,omitempty"`
}

type TemplateBundleApplyFeaturedRequest struct {
	Home      *TemplateBundleApplyFeaturedHome `json:"home,omitempty"`
	Libraries map[int]string                   `json:"libraries,omitempty"`
}

type TemplateBundleApplyFeaturedHome struct {
	LibraryID  int    `json:"library_id"`
	TemplateID string `json:"template_id"`
}

func decodeTemplateBundleApplyRequest(data json.RawMessage) (TemplateBundleApplyRequest, error) {
	var req TemplateBundleApplyRequest
	if len(data) > 0 {
		if err := json.Unmarshal(data, &req); err != nil {
			return req, fmt.Errorf("invalid template bundle apply payload: %w", err)
		}
	}
	// A missing virtual_playback key leaves the bool zero value, false. That
	// is deliberate: payloads enqueued before the field existed must keep
	// their original non-virtual meaning rather than inherit the newer
	// "omitted means on" API default. Producers enqueue an explicit value.
	if req.BundleID == "" {
		return req, fmt.Errorf("bundle_id is required")
	}
	if len(req.LibraryIDs) == 0 {
		return req, fmt.Errorf("library_ids is required")
	}
	return req, nil
}
