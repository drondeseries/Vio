package playback

import (
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/tonemap"
)

// targetCodecRegistryV3 builds a registry that advertises the requested video
// transformations so the selection matrix can be exercised without a probe.
func targetCodecRegistryV3(names ...string) *TransformationRegistryV3 {
	specs := make([]TransformationSpecV3, 0, len(names))
	for _, name := range names {
		specs = append(specs, TransformationSpecV3{Name: name, RecipeVersion: "1", Available: true})
	}
	return NewTransformationRegistryV3(specs)
}

func targetCodecInputV3(hardware []string, hwAccel string, deliveryVideo []string) PlannerInputV3 {
	request := StartRequestV3{
		Capabilities:          ClientCodecCapabilitiesV3{CodecsVideoHardware: hardware},
		ClientPlaybackContext: ClientPlaybackContextV3{Deliveries: map[string]DeliveryCapabilityV3{}},
	}
	if deliveryVideo != nil {
		request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3] = DeliveryCapabilityV3{VideoCodecs: deliveryVideo}
	}
	return PlannerInputV3{Request: request, Settings: PlannerSettingsV3{HWAccel: hwAccel}}
}

// TestSelectTargetVideoCodecV3Matrix is the client-capability to encoder
// matrix: AV1 wins over HEVC over the H.264 floor, and every unknown or
// unsupported combination falls back to H.264.
func TestSelectTargetVideoCodecV3Matrix(t *testing.T) {
	tests := []struct {
		name       string
		hardware   []string
		hwAccel    string
		delivery   []string
		registry   []string
		wantCodec  string
		wantTransf string
		wantOK     bool
	}{
		{
			name:     "absent capabilities keep the H.264 floor",
			registry: []string{TransformationVideoToH264V3}, wantCodec: TargetVideoCodecH264V3,
			wantTransf: TransformationVideoToH264V3, wantOK: true,
		},
		{
			name:     "h264 only keeps the floor",
			hardware: []string{"h264"}, registry: []string{TransformationVideoToH264V3},
			wantCodec: TargetVideoCodecH264V3, wantTransf: TransformationVideoToH264V3, wantOK: true,
		},
		{
			name:     "hevc hardware upgrades to hevc",
			hardware: []string{"hevc", "h264"}, registry: []string{TransformationVideoToH264V3, TransformationVideoToHEVCV3},
			wantCodec: TargetVideoCodecHEVCV3, wantTransf: TransformationVideoToHEVCV3, wantOK: true,
		},
		{
			name:     "av1 hardware on qsv upgrades to av1",
			hardware: []string{"av1", "hevc", "h264"}, hwAccel: "qsv",
			registry:  []string{TransformationVideoToH264V3, TransformationVideoToHEVCV3, TransformationVideoToAV1V3},
			wantCodec: TargetVideoCodecAV1V3, wantTransf: TransformationVideoToAV1V3, wantOK: true,
		},
		{
			name:     "av1 hardware without qsv keeps the floor",
			hardware: []string{"av1"}, hwAccel: "none",
			registry:  []string{TransformationVideoToH264V3, TransformationVideoToAV1V3},
			wantCodec: TargetVideoCodecH264V3, wantTransf: TransformationVideoToH264V3, wantOK: true,
		},
		{
			name:     "hevc hardware without an hevc encoder keeps the floor",
			hardware: []string{"hevc"}, registry: []string{TransformationVideoToH264V3},
			wantCodec: TargetVideoCodecH264V3, wantTransf: TransformationVideoToH264V3, wantOK: true,
		},
		{
			name:     "HLS delivery codec list caps the upgrade",
			hardware: []string{"hevc", "h264"}, delivery: []string{"h264"},
			registry:  []string{TransformationVideoToH264V3, TransformationVideoToHEVCV3},
			wantCodec: TargetVideoCodecH264V3, wantTransf: TransformationVideoToH264V3, wantOK: true,
		},
		{
			name:     "empty HLS delivery codec list is not a constraint",
			hardware: []string{"hevc"}, delivery: []string{},
			registry:  []string{TransformationVideoToH264V3, TransformationVideoToHEVCV3},
			wantCodec: TargetVideoCodecHEVCV3, wantTransf: TransformationVideoToHEVCV3, wantOK: true,
		},
		{
			name:     "no video encoder at all is unavailable",
			registry: []string{TransformationAudioToAACV3}, wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := targetCodecInputV3(tt.hardware, tt.hwAccel, tt.delivery)
			got, ok := selectTargetVideoCodecV3(input, targetCodecRegistryV3(tt.registry...))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.codec != tt.wantCodec || got.transformation != tt.wantTransf {
				t.Fatalf("selection = %+v, want codec %q transformation %q", got, tt.wantCodec, tt.wantTransf)
			}
		})
	}
}

// TestBuildFFmpegArgs_TargetCodecSelectsQSVEncoderAndPackaging pins the
// per-codec encoder and HLS packaging: H.264 and HEVC stay in MPEG-TS, AV1
// uses av1_qsv and fragmented MP4 because MPEG-TS has no AV1 stream type.
func TestBuildFFmpegArgs_TargetCodecSelectsQSVEncoderAndPackaging(t *testing.T) {
	tests := []struct {
		codec      string
		wantCase   string
		wantType   string
		wantSuffix string
	}{
		{codec: transcodeCodecH264, wantCase: "-c:v h264_qsv", wantType: "mpegts", wantSuffix: "seg_%05d.ts"},
		{codec: transcodeCodecHEVC, wantCase: "-c:v hevc_qsv", wantType: "mpegts", wantSuffix: "seg_%05d.ts"},
		{codec: transcodeCodecAV1, wantCase: "-c:v av1_qsv", wantType: "fmp4", wantSuffix: "seg_%05d.m4s"},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			args := buildFFmpegArgs(TranscodeOpts{
				InputPath: "/media/sdr.mkv", OutputDir: t.TempDir(),
				TargetCodecVideo: tt.codec, TargetCodecAudio: "aac",
				SourceVideoCodec: "h264", TargetResolution: "1080p", HWAccel: "qsv",
			})
			joined := strings.Join(args, " ")
			for _, token := range []string{tt.wantCase, "-hls_segment_type " + tt.wantType, tt.wantSuffix} {
				if !strings.Contains(joined, token) {
					t.Fatalf("%s args missing %q: %s", tt.codec, token, joined)
				}
			}
		})
	}
}

// TestResolveToneMapFilterV3HonorsVPPOnlyOnQSV covers the playback setting's
// intended combinations at the executor boundary.
func TestResolveToneMapFilterV3HonorsVPPOnlyOnQSV(t *testing.T) {
	tests := []struct {
		name   string
		mode   tonemap.Mode
		hw     string
		vpp    bool
		probed string
		want   string
	}{
		{name: "off keeps the probed filter", mode: tonemap.ModeHardware, hw: "qsv", vpp: false, probed: tonemap.HardwareFilterOpenCL, want: tonemap.HardwareFilterOpenCL},
		{name: "on plus qsv selects VPP", mode: tonemap.ModeHardware, hw: "qsv", vpp: true, probed: tonemap.HardwareFilterOpenCL, want: tonemap.HardwareFilterQSVVPP},
		{name: "on plus vaapi is ignored", mode: tonemap.ModeHardware, hw: "vaapi", vpp: true, probed: tonemap.HardwareFilterVAAPI, want: tonemap.HardwareFilterVAAPI},
		{name: "on plus software is ignored", mode: tonemap.ModeSoftware, hw: "none", vpp: true, probed: tonemap.SoftwareFilterBT2390, want: tonemap.SoftwareFilterBT2390},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveToneMapFilterV3(tt.probed, tt.mode, tt.hw, tt.vpp); got != tt.want {
				t.Fatalf("ResolveToneMapFilterV3() = %q, want %q", got, tt.want)
			}
		})
	}
}
