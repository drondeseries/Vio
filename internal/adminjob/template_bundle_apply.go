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
	// VirtualPlayback is tri-state across the job boundary: nil means the
	// client omitted it and the apply default (on) is used; a non-nil pointer
	// carries the client's explicit choice, including false. A *bool is needed
	// because the payload round-trips through JSON, where a plain bool cannot
	// tell "omitted" from "sent false".
	VirtualPlayback *bool                               `json:"virtual_playback,omitempty"`
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
	if req.BundleID == "" {
		return req, fmt.Errorf("bundle_id is required")
	}
	if len(req.LibraryIDs) == 0 {
		return req, fmt.Errorf("library_ids is required")
	}
	return req, nil
}
