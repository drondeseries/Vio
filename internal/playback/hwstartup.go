package playback

import (
	"strings"

	"github.com/Silo-Server/silo-server/internal/tonemap"
)

// ToneMapStartupSummary is the operator-facing verdict on which tone-map
// executors validated for this host. Warn is true when transcodes are enabled
// and the operator did not explicitly disable hardware acceleration, yet no
// hardware executor validated — the exact situation in which HDR transcodes
// silently fall back to software encoding (libx264). Attrs carries the
// evidence: the resolved backend, visible render devices, validated
// executors, and per-backend probe reasons.
type ToneMapStartupSummary struct {
	Warn  bool
	Attrs []any
}

// SummarizeToneMapInventory is pure so the warn/quiet boundary is unit
// tested; callers supply the detection report, the probed capabilities, and
// the effective playback settings.
func SummarizeToneMapInventory(info HWAccelInfo, caps tonemap.Capabilities, hwAccelSetting string, transcodeEnabled bool, detectErr, probeErr error) ToneMapStartupSummary {
	setting := strings.TrimSpace(hwAccelSetting)
	if setting == "" {
		setting = hwAccelAuto
	}
	attrs := []any{
		"resolved", info.Resolved,
		"hw_accel", setting,
		"render_devices", info.RenderDevices,
		"tonemap_executors", DescribeToneMapExecutors(caps),
	}
	if detectErr != nil {
		attrs = append(attrs, "detection_error", detectErr.Error())
	}
	if probeErr != nil {
		attrs = append(attrs, "tonemap_probe_error", probeErr.Error())
	}
	if !transcodeEnabled || setting == HWAccelNone || hasHardwareToneMapExecutor(caps) {
		return ToneMapStartupSummary{Attrs: attrs}
	}
	attrs = append(attrs, "unverified_backends", unverifiedBackendReasons(info))
	attrs = append(attrs, "hint", "HDR transcodes need a validated hardware executor: check /dev/dri passthrough (docker-compose.vaapi.yml overlay), playback.hw_device, and the Intel OpenCL runtime required for QSV tone mapping")
	return ToneMapStartupSummary{Warn: true, Attrs: attrs}
}

// DescribeToneMapExecutors renders validated executors compactly, e.g.
// "hardware:qsv/tonemap_opencl(pq)".
func DescribeToneMapExecutors(caps tonemap.Capabilities) []string {
	described := make([]string, 0, len(caps))
	for _, capability := range caps {
		kinds := make([]string, 0, len(capability.SourceKinds))
		for _, kind := range capability.SourceKinds {
			kinds = append(kinds, string(kind))
		}
		described = append(described, string(capability.Mode)+":"+capability.Backend+"/"+capability.Filter+"("+strings.Join(kinds, ",")+")")
	}
	return described
}

func hasHardwareToneMapExecutor(caps tonemap.Capabilities) bool {
	for _, capability := range caps {
		if capability.Mode == tonemap.ModeHardware {
			return true
		}
	}
	return false
}

func unverifiedBackendReasons(info HWAccelInfo) []string {
	var reasons []string
	for _, backend := range info.DetectedBackends {
		if backend.Verified {
			continue
		}
		reason := strings.TrimSpace(backend.Reason)
		if reason == "" {
			reason = "not verified"
		}
		reasons = append(reasons, backend.Backend+": "+reason)
	}
	return reasons
}
