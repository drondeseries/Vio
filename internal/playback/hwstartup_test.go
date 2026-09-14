package playback

import (
	"errors"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/tonemap"
)

func TestSummarizeToneMapInventoryQuietWhenHardwareValidated(t *testing.T) {
	info := HWAccelInfo{
		Resolved:      "qsv",
		RenderDevices: []string{"/dev/dri/renderD128"},
		DetectedBackends: []DetectedBackend{
			{Backend: "qsv", Verified: true, Device: "/dev/dri/renderD128"},
		},
	}
	caps := tonemap.Capabilities{
		{Mode: tonemap.ModeHardware, Backend: "qsv", Filter: "tonemap_opencl", SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ}},
		{Mode: tonemap.ModeSoftware, Backend: "software", Filter: "tonemapx", SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ}},
	}
	summary := SummarizeToneMapInventory(info, caps, "auto", true, nil, nil)
	if summary.Warn {
		t.Fatalf("Warn = true with a validated hardware executor, attrs = %v", summary.Attrs)
	}
}

func TestSummarizeToneMapInventoryQuietWhenHardwareExplicitlyDisabled(t *testing.T) {
	info := HWAccelInfo{Resolved: "none"}
	summary := SummarizeToneMapInventory(info, nil, "none", true, nil, nil)
	if summary.Warn {
		t.Fatalf("Warn = true with hw_accel=none, attrs = %v", summary.Attrs)
	}
}

func TestSummarizeToneMapInventoryQuietWhenTranscodesDisabled(t *testing.T) {
	info := HWAccelInfo{Resolved: "none"}
	summary := SummarizeToneMapInventory(info, nil, "auto", false, nil, nil)
	if summary.Warn {
		t.Fatalf("Warn = true with transcodes disabled, attrs = %v", summary.Attrs)
	}
}

func TestSummarizeToneMapInventoryWarnsWithProbeReasons(t *testing.T) {
	info := HWAccelInfo{
		Resolved:      "none",
		RenderDevices: nil,
		DetectedBackends: []DetectedBackend{
			{Backend: "qsv", Devices: []string{"/dev/dri/renderD128"}, Reason: "h264_qsv smoke encode failed"},
			{Backend: "vaapi", Reason: "no accessible render devices"},
		},
	}
	caps := tonemap.Capabilities{
		{Mode: tonemap.ModeSoftware, Backend: "software", Filter: "tonemapx", SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ}},
	}
	summary := SummarizeToneMapInventory(info, caps, "auto", true, nil, nil)
	if !summary.Warn {
		t.Fatalf("Warn = false with no hardware executor, attrs = %v", summary.Attrs)
	}
	joined := joinAttrs(summary.Attrs)
	for _, want := range []string{"qsv: h264_qsv smoke encode failed", "vaapi: no accessible render devices", "docker-compose.vaapi.yml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("attrs %v missing %q", summary.Attrs, want)
		}
	}
}

func TestSummarizeToneMapInventoryEmptySettingMeansAuto(t *testing.T) {
	info := HWAccelInfo{Resolved: "none"}
	summary := SummarizeToneMapInventory(info, nil, "", true, nil, nil)
	if !summary.Warn {
		t.Fatalf("Warn = false with empty hw_accel and no hardware executor, attrs = %v", summary.Attrs)
	}
}

func TestSummarizeToneMapInventoryReportsDetectionError(t *testing.T) {
	info := HWAccelInfo{Resolved: "none"}
	summary := SummarizeToneMapInventory(info, nil, "auto", true, ErrHardwareDetectionIncomplete, errors.New("probe timed out"))
	if !summary.Warn {
		t.Fatalf("Warn = false on incomplete detection without hardware, attrs = %v", summary.Attrs)
	}
	joined := joinAttrs(summary.Attrs)
	for _, want := range []string{ErrHardwareDetectionIncomplete.Error(), "probe timed out"} {
		if !strings.Contains(joined, want) {
			t.Errorf("attrs %v missing %q", summary.Attrs, want)
		}
	}
}

func TestDescribeToneMapExecutors(t *testing.T) {
	caps := tonemap.Capabilities{
		{Mode: tonemap.ModeHardware, Backend: "qsv", Filter: "tonemap_opencl", SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ}},
	}
	got := DescribeToneMapExecutors(caps)
	if len(got) != 1 || got[0] != "hardware:qsv/tonemap_opencl(pq)" {
		t.Fatalf("DescribeToneMapExecutors = %v, want [hardware:qsv/tonemap_opencl(pq)]", got)
	}
	if len(DescribeToneMapExecutors(nil)) != 0 {
		t.Fatal("DescribeToneMapExecutors(nil) should be empty")
	}
}

func joinAttrs(attrs []any) string {
	joined := ""
	for _, attr := range attrs {
		if text, ok := attr.(string); ok {
			joined += text + "\x00"
			continue
		}
		if list, ok := attr.([]string); ok {
			for _, text := range list {
				joined += text + "\x00"
			}
		}
	}
	return joined
}
