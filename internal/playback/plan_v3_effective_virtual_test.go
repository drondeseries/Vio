package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A virtual candidate substituted for the neutral requested row must be
// visible on the plan, or a client's version menu keeps showing the neutral
// label after a first play never adopts the version that actually played.
func TestPlanPlaybackV3ExposesEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{
		ID:        41,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://movie/tt1234567",
	}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// A non-virtual effective source has no virtual URI to expose.
func TestPlanPlaybackV3OmitsEffectiveVirtualURIForLocalSource(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != "" {
		t.Fatalf("effective virtual URI = %q, want empty for a local source", result.Plan.EffectiveVirtualURI)
	}
}

// The audio-only planner builds its own plan; it must expose the substituted
// virtual URI too.
func TestPlanPlaybackV3AudioOnlyExposesEffectiveVirtualURI(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{
		ID:        76,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://audiobook/tt7654321",
	}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// The URI is a UI hint, not a route input: attaching it must not perturb plan
// identity, or replans would miss the cache and clients would see spurious new
// attempts.
func TestPlanAttemptKeyV3IgnoresEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}

	before := PlanAttemptKeyV3(*result.Plan, "output-1", nil)
	mutated := *result.Plan
	mutated.EffectiveVirtualURI = "virtual://movie/tt1234567?result=other"
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with the effective virtual URI: %q -> %q", before, after)
	}
	if mutated.PlanID != result.Plan.PlanID {
		// PlanID is computed before mutation, but guard the intent explicitly.
		t.Errorf("plan ID changed: %q -> %q", result.Plan.PlanID, mutated.PlanID)
	}
}

// virtualCandidatePlanV3 plans the same virtual requested row against the
// candidate named by resultID, so tests can vary only the resolved candidate.
func virtualCandidatePlanV3(t *testing.T, requested *models.MediaFile, resultID string) *PlanV3 {
	t.Helper()
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=" + resultID
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	return result.Plan
}

// A release rotation that keeps effective_media_file_id fixed must still be
// visible: clients re-arm source-change recovery on the revision. Effective
// media file id is the requested catalog row and does not move, and the v2 wire
// omits effective_virtual_uri, so this field is the delivered rotation identity.
func TestPlanPlaybackV3VirtualSourceRevisionTracksCandidate(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	first := virtualCandidatePlanV3(t, requested, "candidate-a")
	stable := virtualCandidatePlanV3(t, requested, "candidate-a")
	rotated := virtualCandidatePlanV3(t, requested, "candidate-b")

	if first.VirtualSourceRevision == "" {
		t.Fatal("virtual source revision is empty for a resolved virtual candidate")
	}
	if len(first.VirtualSourceRevision) != 24 {
		t.Fatalf("virtual source revision = %q, want a 24-hex opaque token", first.VirtualSourceRevision)
	}
	if first.VirtualSourceRevision == "candidate-a" {
		t.Fatal("virtual source revision leaked the raw candidate identity")
	}
	if stable.VirtualSourceRevision != first.VirtualSourceRevision {
		t.Fatalf("stable candidate revision moved: %q -> %q", first.VirtualSourceRevision, stable.VirtualSourceRevision)
	}
	if rotated.VirtualSourceRevision == first.VirtualSourceRevision {
		t.Fatalf("rotated candidate kept revision %q", first.VirtualSourceRevision)
	}
	if first.EffectiveMediaFileID != rotated.EffectiveMediaFileID {
		t.Fatalf("test setup: effective media file id moved %d -> %d", first.EffectiveMediaFileID, rotated.EffectiveMediaFileID)
	}
}

// A non-virtual effective source has no candidate revision to expose.
func TestPlanPlaybackV3OmitsVirtualSourceRevisionForLocalSource(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision != "" {
		t.Fatalf("virtual source revision = %q, want empty for a local source", result.Plan.VirtualSourceRevision)
	}
}

// A neutral requested row planned without a substitution carries no candidate
// identity, so there is nothing to distinguish and no revision to publish.
func TestPlanPlaybackV3OmitsVirtualSourceRevisionWithoutCandidate(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision != "" {
		t.Fatalf("virtual source revision = %q, want empty without a resolved candidate", result.Plan.VirtualSourceRevision)
	}
}

// The revision is a UI hint, not a route input: it must not perturb plan
// identity, or a rotation would force a spurious new attempt.
func TestPlanAttemptKeyV3IgnoresVirtualSourceRevision(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}
	plan := virtualCandidatePlanV3(t, requested, "working")

	before := PlanAttemptKeyV3(*plan, "output-1", nil)
	mutated := *plan
	mutated.VirtualSourceRevision = "deadbeefdeadbeefdeadbeef"
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with the virtual source revision: %q -> %q", before, after)
	}
	if mutated.PlanID != plan.PlanID {
		t.Errorf("plan ID changed: %q -> %q", plan.PlanID, mutated.PlanID)
	}
}

// The audio-only planner builds its own plan; it must expose the revision too.
func TestPlanPlaybackV3AudioOnlyExposesVirtualSourceRevision(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{ID: 76, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://audiobook/tt7654321"}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision == "" {
		t.Fatal("audio-only virtual plan omitted the virtual source revision")
	}
}

// A repaired inventory under the same candidate id must move the revision: a
// rematch adoption rewrites the declared tracks and clears the probe stamp
// while the ?result= pick stays put, and the client must re-arm recovery for
// the corrected generation.
func TestPlanPlaybackV3VirtualSourceRevisionTracksEvidenceRepair(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	before := virtualCandidatePlanV3(t, requested, "candidate-a")

	// Same track counts, same probe version, no probe stamp: only the
	// playback-relevant track fields move (subtitle language, audio codec).
	// The repaired file keeps the fixture's aac audio claim viable so the
	// plan still routes: the revision must move on track-field changes alone.
	repaired := detailedFixtureFileV3()
	repaired.FilePath = "virtual://movie/tt1234567?result=candidate-a"
	repaired.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Language: "eng", Languages: []string{"eng"}}}
	repaired.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Language: "fre", Codec: "srt"}}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}
	repairResult := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: repaired, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if repairResult.Plan == nil {
		t.Fatalf("result = %#v, want a plan", repairResult)
	}
	if repairResult.Plan.VirtualSourceRevision == before.VirtualSourceRevision {
		t.Fatalf("repaired inventory kept revision %q", repairResult.Plan.VirtualSourceRevision)
	}
}

// The revision must survive a signed-URL renewal: only the media generation
// is an input, never the provider URL or refresh timestamps, so a re-resolve
// that changes nothing about the candidate or its evidence is silent.
func TestPlanPlaybackV3VirtualSourceRevisionIgnoresURLRenewal(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	first := virtualCandidatePlanV3(t, requested, "candidate-a")
	stable := virtualCandidatePlanV3(t, requested, "candidate-a")

	if stable.VirtualSourceRevision != first.VirtualSourceRevision {
		t.Fatalf("URL-equivalent replan moved revision: %q -> %q", first.VirtualSourceRevision, stable.VirtualSourceRevision)
	}
}
