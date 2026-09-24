package handlers

import (
	"context"
	"net/url"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// bindSessionVirtualSourceWithTracks must only apply carried session evidence
// when it provably belongs to the candidate being served. When the evidence
// names a different candidate than the bound file, the bound file's own catalog
// tracks are served instead — never the previous release's inventory.
func TestBindVirtualSourceDiscardsForeignCandidateEvidence(t *testing.T) {
	boundFile := &models.MediaFile{
		ID:       77,
		FilePath: "virtual://movie/tt1?result=candidate-b",
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 5, Codec: "subrip", Language: "eng"},
		},
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/subs/b.srt", Language: "eng", Format: "srt"}},
	}
	session := &playback.Session{
		ID:                         "sess",
		VirtualSourceURI:           boundFile.FilePath,
		VirtualSubtitleEvidenceSet: true,
		// Evidence captured for the previous candidate: different layout.
		VirtualSubtitleEvidenceURI: "virtual://movie/tt1?result=candidate-a",
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 1, Codec: "hdmv_pgs_subtitle", Language: "fra"}},
		VirtualExternalSubtitles:   []models.ExternalSubtitle{{Path: "/subs/a.sup", Language: "fra", Format: "sup"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), boundFile, session, nil)
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Language != "eng" {
		t.Fatalf("foreign candidate evidence was applied: %+v", bound.SubtitleTracks)
	}
	if len(bound.ExternalSubtitles) != 1 || bound.ExternalSubtitles[0].Path != "/subs/b.srt" {
		t.Fatalf("foreign external evidence was applied: %+v", bound.ExternalSubtitles)
	}
}

// Evidence whose URI matches the bound candidate is still trusted and applied:
// legitimate carry behavior for a same-file re-plan must keep working.
func TestBindVirtualSourceAppliesMatchingCandidateEvidence(t *testing.T) {
	uri := "virtual://movie/tt1?result=candidate-a"
	boundFile := &models.MediaFile{
		ID:             77,
		FilePath:       uri,
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip"}},
	}
	session := &playback.Session{
		ID:                         "sess",
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 7, Codec: "ass", Language: "jpn"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), boundFile, session, nil)
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Index != 7 || bound.SubtitleTracks[0].Codec != "ass" {
		t.Fatalf("matching evidence was not applied: %+v", bound.SubtitleTracks)
	}
}

// A legacy session without the provenance field still trusts evidence captured
// while the file path matched the session's virtual URI. With no provenance
// field the only binding the evidence had was the session's URI, so the bind
// override makes that the matching candidate.
func TestBindVirtualSourceLegacyEvidenceUsesPlanBinding(t *testing.T) {
	uri := "virtual://movie/tt1?result=candidate-a"
	boundFile := &models.MediaFile{ID: 77, FilePath: uri}
	session := &playback.Session{
		ID:                         "sess",
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 7, Codec: "ass"}},
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), boundFile, session, nil)
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Index != 7 {
		t.Fatalf("legacy matching evidence was not applied: %+v", bound.SubtitleTracks)
	}
}

// Remapping must run for a same-row rotation whose inventory fingerprint moved,
// translating a French ordinal onto the equivalent French track of the new
// release rather than carrying the stale position.
func TestRemapSubtitleSelectionSameRowRotationUsesFingerprint(t *testing.T) {
	// Same catalog row id, different underlying release.
	source := &models.MediaFile{ID: 3459774, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
		{Index: 2, Codec: "subrip", Language: "eng"},
	}}
	target := &models.MediaFile{ID: 3459774, SubtitleTracks: []models.SubtitleTrack{
		{Index: 9, Codec: "subrip", Language: "eng"},
		{Index: 7, Codec: "subrip", Language: "fra"}, // now at ordinal 1
	}}
	index := 0
	request := playback.StartRequestV3{
		SubtitleTrackIndex: &index,
		SubtitleTrackID:    playback.TrackIDV3(source.ID, "subtitle", 0),
	}
	if err := (&PlaybackHandler{}).remapSubtitleSelectionV3(context.Background(), source, target, &request); err != nil {
		t.Fatalf("same-row remap: %v", err)
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("same-row remap index = %v, want 1 (the French track's new position)", request.SubtitleTrackIndex)
	}
	if request.SubtitleTrackID != playback.TrackIDV3(target.ID, "subtitle", 1) {
		t.Fatalf("same-row remap identity = %q, want the target's published ordinal", request.SubtitleTrackID)
	}
}

// A same-row rotation replaces the catalog row's tracks in place (same id). The
// remap source must then be the session's plan-time evidence, so the carried
// ordinal is translated from the inventory it was actually minted against
// rather than the row's new tracks.
func TestReplanRemapSourceUsesPlanTimeEvidence(t *testing.T) {
	uri := "virtual://movie/tt-row?result=candidate-b"
	// The row was re-probed in place: same id, now English at ordinal 0.
	reloadedRow := &models.MediaFile{
		ID: 3459774, FilePath: uri,
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Language: "eng", Channels: 2}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 4, Codec: "subrip", Language: "eng"}},
	}
	session := &playback.Session{
		ID:                         "sess",
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: uri,
		// The plan-time inventory: French audio/subtitle at ordinal 0.
		VirtualAudioTracks:    []models.AudioTrack{{Codec: "aac", Language: "fra", Channels: 2}},
		VirtualSubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
	}
	handler := &PlaybackHandler{}

	source := handler.replanRemapSourceV3(reloadedRow, session)
	if source.ID != reloadedRow.ID {
		t.Fatalf("remap source id = %d, want the same catalog row", source.ID)
	}
	if len(source.AudioTracks) != 1 || source.AudioTracks[0].Language != "fra" {
		t.Fatalf("remap source audio = %+v, want the plan-time French inventory", source.AudioTracks)
	}
	if sameMediaInventoryV3(source, reloadedRow) {
		t.Fatal("remap source reported the rotated row as unchanged; the stale ordinal would be carried")
	}

	// The carried French ordinal 0 must remap to the reloaded row's French track
	// (which the row no longer has) — here the row has only English, so the
	// remap degrades to the row's default rather than a wrong-language track.
	audioIndex := 0
	start := playback.StartRequestV3{AudioTrackIndex: &audioIndex, AudioTrackID: playback.TrackIDV3(3459774, "audio", 0)}
	if err := remapAudioSelectionV3(source, reloadedRow, &start); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if start.AudioTrackIndex == nil || *start.AudioTrackIndex != 0 {
		t.Fatalf("remap index = %v, want 0 (the row's only track)", start.AudioTrackIndex)
	}
	if start.AudioTrackID != playback.TrackIDV3(reloadedRow.ID, "audio", 0) {
		t.Fatalf("remap identity = %q, want the reloaded row's identity", start.AudioTrackID)
	}
}

// Without virtual evidence the remap source is the loaded row itself, so
// ordinary local/edition behavior is unchanged.
func TestReplanRemapSourceFallsBackToLoadedRow(t *testing.T) {
	row := &models.MediaFile{ID: 7, AudioTracks: []models.AudioTrack{{Codec: "aac"}}}
	handler := &PlaybackHandler{}
	if got := handler.replanRemapSourceV3(row, &playback.Session{}); got != row {
		t.Fatalf("remap source = %v, want the loaded row", got)
	}
	if got := handler.replanRemapSourceV3(row, nil); got != row {
		t.Fatalf("remap source with nil session = %v, want the loaded row", got)
	}
}

// Evidence anchored at a different candidate is still the inventory a carried
// ordinal was minted against, so a remap must use it as the source even though
// serving would discard it. This is the key asymmetry: serving needs matching
// provenance, remapping needs the original inventory.
func TestReplanRemapSourceUsesEvidenceAcrossCandidates(t *testing.T) {
	row := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt?result=b",
		AudioTracks: []models.AudioTrack{{Codec: "aac", Language: "eng"}}}
	session := &playback.Session{
		VirtualSourceURI:           row.FilePath,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: "virtual://movie/tt?result=a",
		VirtualAudioTracks:         []models.AudioTrack{{Codec: "aac", Language: "fra"}},
	}
	handler := &PlaybackHandler{}
	source := handler.replanRemapSourceV3(row, session)
	if source == row {
		t.Fatal("a remap must use the plan-time evidence, not the reloaded row")
	}
	if len(source.AudioTracks) != 1 || source.AudioTracks[0].Language != "fra" {
		t.Fatalf("remap source audio = %+v, want the plan-time French inventory", source.AudioTracks)
	}
}

// A non-virtual effective file after a virtual plan is not remapped from the
// session's virtual evidence.
func TestReplanRemapSourceIgnoresEvidenceForLocalFile(t *testing.T) {
	row := &models.MediaFile{ID: 7, FilePath: "/media/local.mkv",
		AudioTracks: []models.AudioTrack{{Codec: "aac", Language: "eng"}}}
	session := &playback.Session{
		VirtualSourceURI:           "virtual://movie/tt?result=a",
		VirtualSubtitleEvidenceSet: true,
		VirtualAudioTracks:         []models.AudioTrack{{Codec: "aac", Language: "fra"}},
	}
	handler := &PlaybackHandler{}
	if got := handler.replanRemapSourceV3(row, session); got != row {
		t.Fatalf("local file remapped from virtual evidence: %+v", got.AudioTracks)
	}
}

// A recorded rotation (binding moved, revision cleared until a plan re-probes)
// must force a remap even when the reloaded row's inventory compares equal to
// the plan-time evidence.
func TestVirtualCandidateRotationRecordedDetectsRecordedMove(t *testing.T) {
	uri := "virtual://movie/tt-row?result=candidate-b"
	// The reloaded row's tracks happen to look identical to the plan-time ones.
	tracks := []models.AudioTrack{{Codec: "aac", Language: "eng", Channels: 2}}
	row := &models.MediaFile{ID: 7, FilePath: uri, AudioTracks: tracks}
	session := &playback.Session{
		VirtualSourceURI:           uri,
		VirtualSourceRevision:      "", // a binding move cleared it
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: "virtual://movie/tt-row?result=candidate-a",
		VirtualAudioTracks:         append([]models.AudioTrack(nil), tracks...),
	}
	if !virtualCandidateRotationRecordedV3(session, row) {
		t.Fatal("a recorded candidate move was not detected")
	}

	// Once the planner re-probes, the revision is set and the guard stops firing.
	session.VirtualSourceRevision = "rev-b"
	session.VirtualSubtitleEvidenceURI = uri
	if virtualCandidateRotationRecordedV3(session, row) {
		t.Fatal("a re-probed candidate was still treated as a recorded rotation")
	}

	// A non-virtual effective file has no candidate rotation to record.
	local := &models.MediaFile{ID: 7, FilePath: "/media/local.mkv"}
	if virtualCandidateRotationRecordedV3(session, local) {
		t.Fatal("a local file reported a virtual candidate rotation")
	}
}

// A replan that merely re-plans the same candidate (evidence anchored at the
// bound file, revision set) does not force a remap.
func TestVirtualCandidateRotationRecordedIgnoresSameCandidate(t *testing.T) {
	uri := "virtual://movie/tt-row?result=candidate-a"
	row := &models.MediaFile{ID: 7, FilePath: uri}
	session := &playback.Session{
		VirtualSourceURI:           uri,
		VirtualSourceRevision:      "rev-a",
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: uri,
	}
	if virtualCandidateRotationRecordedV3(session, row) {
		t.Fatal("an unchanged candidate was treated as a recorded rotation")
	}
}

func TestRemapSubtitleSelectionSameRowUnchangedIsNoop(t *testing.T) {
	tracks := []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}}
	source := &models.MediaFile{ID: 7, SubtitleTracks: tracks}
	target := &models.MediaFile{ID: 7, SubtitleTracks: append([]models.SubtitleTrack(nil), tracks...)}
	index := 0
	request := playback.StartRequestV3{SubtitleTrackIndex: &index, SubtitleTrackID: playback.TrackIDV3(7, "subtitle", 0)}
	if err := (&PlaybackHandler{}).remapSubtitleSelectionV3(context.Background(), source, target, &request); err != nil {
		t.Fatalf("unchanged remap: %v", err)
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 0 {
		t.Fatalf("unchanged inventory selection moved: %v", request.SubtitleTrackIndex)
	}
}

// Same-row audio rotation must remap by language family, not carry the ordinal.
func TestRemapAudioSelectionSameRowRotationUsesFingerprint(t *testing.T) {
	source := &models.MediaFile{ID: 3459774, AudioTracks: []models.AudioTrack{
		{Codec: "aac", Language: "fra", Channels: 2},
	}}
	target := &models.MediaFile{ID: 3459774, AudioTracks: []models.AudioTrack{
		{Codec: "aac", Language: "eng", Channels: 2},
		{Codec: "ac3", Language: "fra", Channels: 6},
		{Codec: "eac3", Language: "eng", Channels: 6},
	}}
	index := 0
	request := playback.StartRequestV3{
		AudioTrackIndex: &index,
		AudioTrackID:    playback.TrackIDV3(source.ID, "audio", 0),
	}
	if err := remapAudioSelectionV3(source, target, &request); err != nil {
		t.Fatalf("same-row audio remap: %v", err)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 1 {
		t.Fatalf("same-row audio remap = %v, want 1 (the French track)", request.AudioTrackIndex)
	}
}

// A subtitle request naming the old edition after a switch re-maps by identity
// onto the effective file. A foreign ordinal that names no equivalent track
// degrades instead of serving a wrong release's track.
func TestResolveSubtitleEditionSwitchRemapsForeignEdition(t *testing.T) {
	handler := &StreamHandler{}
	named := &models.MediaFile{ID: 100, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
	}}
	effective := &models.MediaFile{ID: 200, SubtitleTracks: []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
		{Index: 8, Codec: "subrip", Language: "fra"},
	}}
	file, index, err := handler.resolveSubtitleEditionSwitch(context.Background(), named, effective, 0, url.Values{})
	if err != nil {
		t.Fatalf("edition switch remap: %v", err)
	}
	if file != effective || index != 1 {
		t.Fatalf("edition switch resolved file=%v index=%d, want the effective file's French track", file, index)
	}
}

func TestResolveSubtitleEditionSwitchDegradesWhenNoEquivalent(t *testing.T) {
	handler := &StreamHandler{}
	named := &models.MediaFile{ID: 100, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "hdmv_pgs_subtitle", Language: "fra"},
	}}
	effective := &models.MediaFile{ID: 200, SubtitleTracks: []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
	}}
	if _, _, err := handler.resolveSubtitleEditionSwitch(context.Background(), named, effective, 0, url.Values{}); err == nil {
		t.Fatal("a foreign edition with no equivalent track was served instead of degrading")
	}
}

// A pinned identity that resolves against the effective file is the current
// plan's own track and is honored directly.
func TestResolveSubtitleEditionSwitchHonorsPinOnEffectiveFile(t *testing.T) {
	handler := &StreamHandler{}
	named := &models.MediaFile{ID: 100, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "fra"},
	}}
	effective := &models.MediaFile{ID: 200, SubtitleTracks: []models.SubtitleTrack{
		{Index: 4, Codec: "subrip", Language: "eng"},
		{Index: 8, Codec: "subrip", Language: "fra"},
	}}
	query := url.Values{playback.EmbeddedSubtitleStreamIndexParamV3: {"8"}}
	file, index, err := handler.resolveSubtitleEditionSwitch(context.Background(), named, effective, 1, query)
	if err != nil {
		t.Fatalf("pinned effective identity: %v", err)
	}
	if file != effective || index != 1 {
		t.Fatalf("pinned identity resolved file=%v index=%d, want effective index 1", file, index)
	}
}
