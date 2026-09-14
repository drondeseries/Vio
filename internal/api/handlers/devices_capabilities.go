package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// deviceCapabilitiesRequest is the additive capability-report payload native
// clients send once per (profile, device). The shape mirrors the Jellyfin
// DirectPlayProfile fields so clients that already know their codec support
// can report it without a second vocabulary.
type deviceCapabilitiesRequest struct {
	CodecsVideo   []string `json:"codecs_video"`
	CodecsAudio   []string `json:"codecs_audio"`
	Containers    []string `json:"containers"`
	MaxResolution string   `json:"max_resolution"`
	HDR           bool     `json:"hdr"`
	DolbyVision   bool     `json:"dolby_vision"`
}

type deviceCapabilitiesResponse struct {
	CapabilityFingerprint string `json:"capability_fingerprint"`
}

// HandlePutDeviceCapabilities stores the requesting profile's capability
// profile for one device. It is additive and self-scoped: a device can only
// write its own profile, so no household guard is needed. The response carries
// the stored fingerprint so clients can detect when a re-report is required.
func (h *DeviceHandler) HandlePutDeviceCapabilities(w http.ResponseWriter, r *http.Request) {
	store, ok := h.storeFor(w, r)
	if !ok {
		return
	}
	registry, ok := store.(userstore.DeviceProfileRegistry)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Device capability profiles are not supported by this backend")
		return
	}
	profileID := strings.TrimSpace(apimw.GetProfileID(r.Context()))
	if profileID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "X-Profile-Id header is required")
		return
	}
	deviceID := strings.TrimSpace(chi.URLParam(r, "device_id"))
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "A device id is required")
		return
	}
	var req deviceCapabilitiesRequest
	// Bound the payload like the sibling v3 endpoints: a small fixed cap is
	// generous for capability lists and prevents unbounded allocation on an
	// authenticated route.
	const maxDeviceCapabilitiesBody = 64 << 10
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeviceCapabilitiesBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid capabilities payload")
		return
	}
	profile := userstore.DeviceCapabilityProfile{
		ProfileID:     profileID,
		DeviceID:      deviceID,
		CodecsVideo:   req.CodecsVideo,
		CodecsAudio:   req.CodecsAudio,
		Containers:    req.Containers,
		MaxResolution: req.MaxResolution,
		HDR:           req.HDR,
		DolbyVision:   req.DolbyVision,
		Source:        "client",
	}
	if err := registry.PutDeviceProfile(r.Context(), profile); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to store device capabilities")
		return
	}
	writeJSON(w, http.StatusOK, deviceCapabilitiesResponse{
		CapabilityFingerprint: userstore.DeviceCapabilityFingerprint(profile),
	})
}
