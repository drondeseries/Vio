package handlers

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// These tests drive the real production entry point (resolveSubtitleSourceRequest)
// rather than the remapper directly, and assert the provenance of the returned
// file, not just an index. The virtual rotation keeps the catalog row id stable
// (same row, different release), so an assertion on id + language alone would
// also pass if the remap never ran and the OLD evidence file and index were
// returned. The binding URI is the discriminator: a correctly remapped result
// is served from the bound candidate, a short-circuited one is not.

// rotatedSession is a virtual session whose plan-time evidence names candidate
// A while the session is bound to candidate B, with the same catalog row id.
func rotatedSession(oldTracks, newTracks []models.SubtitleTrack, oldURI, newURI string) (*playback.Session, *models.MediaFile) {
	row := &models.MediaFile{
		ID: 700, FilePath: newURI,
		SubtitleTracks: newTracks,
	}
	session := &playback.Session{
		ID:                         "sess-rot",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleTracks:      oldTracks,
	}
	return session, row
}

// Same-row rotation with reordered embedded tracks: the selection must follow
// its language onto the bound release, and the result must be served from the
// bound candidate rather than the old evidence. The bound release reverses the
// evidence order, so an ordinal assertion proves the remap ran: serving the
// stale ordinal would select the other language.
func TestResolveSubtitleSourceRequestSameRowRotationReorderedEmbedded(t *testing.T) {
	oldURI := "virtual://movie/tt-rot?result=cand-a"
	newURI := "virtual://movie/tt-rot?result=cand-b"
	session, row := rotatedSession(
		[]models.SubtitleTrack{
			{Index: 2, Codec: "subrip", Language: "eng"},
			{Index: 3, Codec: "subrip", Language: "fra"},
		},
		[]models.SubtitleTrack{
			{Index: 7, Codec: "subrip", Language: "fra"},
			{Index: 9, Codec: "subrip", Language: "eng"},
		}, oldURI, newURI)

	// Ordinal 1 named the French track against the plan-time evidence.
	resolvedFile, resolvedIndex, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 1, url.Values{})
	if err != nil {
		t.Fatalf("same-row rotation: %v", err)
	}
	if resolvedFile.FilePath != newURI {
		t.Fatalf("served from %q, want the bound candidate %q (the remap did not run)", resolvedFile.FilePath, newURI)
	}
	if resolvedIndex < 0 || resolvedIndex >= len(resolvedFile.SubtitleTracks) {
		t.Fatalf("resolved index %d outside the bound release's %d tracks", resolvedIndex, len(resolvedFile.SubtitleTracks))
	}
	if got := resolvedFile.SubtitleTracks[resolvedIndex].Language; got != "fra" {
		t.Fatalf("selected %q, want the French track carried across the rotation", got)
	}
	if resolvedIndex != 0 {
		t.Fatalf("resolved ordinal %d, want 0 (French leads the bound release; the stale ordinal 1 would name English)", resolvedIndex)
	}
}

// A container index reused for a different language must not be selected: the
// client's language choice outranks the ordinal.
func TestResolveSubtitleSourceRequestReusedIndexOtherLanguageNotSelected(t *testing.T) {
	oldURI := "virtual://movie/tt-reuse?result=cand-a"
	newURI := "virtual://movie/tt-reuse?result=cand-b"
	session, row := rotatedSession(
		[]models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "eng"}},
		// The replacement publishes German where the client used English.
		[]models.SubtitleTrack{{Index: 5, Codec: "subrip", Language: "deu"}},
		oldURI, newURI)

	_, _, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, url.Values{})
	if err == nil {
		t.Fatal("served a different language's track at the reused ordinal")
	}
}

// A missing equivalent must report unavailable rather than an unrelated track.
func TestResolveSubtitleSourceRequestMissingEquivalentUnavailable(t *testing.T) {
	oldURI := "virtual://movie/tt-missing?result=cand-a"
	newURI := "virtual://movie/tt-missing?result=cand-b"
	session, row := rotatedSession(
		[]models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
		[]models.SubtitleTrack{{Index: 8, Codec: "subrip", Language: "eng"}},
		oldURI, newURI)

	if _, _, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, url.Values{}); err == nil {
		t.Fatal("resolved a track that does not exist in the replacement")
	}
}

// External subtitle reorder across a same-row rotation keeps language+format,
// served from the bound candidate.
func TestResolveSubtitleSourceRequestExternalReorder(t *testing.T) {
	oldURI := "virtual://movie/tt-ext?result=cand-a"
	newURI := "virtual://movie/tt-ext?result=cand-b"
	row := &models.MediaFile{ID: 701, FilePath: newURI, ExternalSubtitles: []models.ExternalSubtitle{
		{Language: "spa", Format: "subrip"},
		{Language: "eng", Format: "subrip"},
	}}
	session := &playback.Session{
		ID:                         "sess-ext",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
		VirtualExternalSubtitles: []models.ExternalSubtitle{
			{Language: "eng", Format: "subrip"},
			{Language: "spa", Format: "subrip"},
		},
	}

	resolvedFile, resolvedIndex, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 1, url.Values{})
	if err != nil {
		t.Fatalf("external rotation: %v", err)
	}
	if resolvedFile.FilePath != newURI {
		t.Fatalf("served from %q, want the bound candidate %q", resolvedFile.FilePath, newURI)
	}
	if got := resolvedFile.ExternalSubtitles[resolvedIndex].Language; got != "spa" {
		t.Fatalf("selected %q, want Spanish after the reorder", got)
	}
}

// Captured-empty evidence on the mismatched-provenance path must report
// unavailable, never reinterpret the ordinal against the replacement.
func TestResolveSubtitleSourceRequestCapturedEmptyMismatchedUnavailable(t *testing.T) {
	oldURI := "virtual://movie/tt-empty?result=cand-a"
	newURI := "virtual://movie/tt-empty?result=cand-b"
	row := &models.MediaFile{ID: 702, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-empty",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
		// Captured empty: the plan-time probe found no tracks.
	}

	if _, _, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, url.Values{}); err == nil {
		t.Fatal("captured-empty evidence served a track the client never selected")
	}
}

// Captured-empty evidence with MATCHING provenance is the case the serving
// binder used to swallow: the binder judged evidence presence by slice length,
// so an explicitly captured empty inventory became the populated catalog
// inventory and the ordinal was served against the replacement.
func TestResolveSubtitleSourceRequestCapturedEmptyMatchingUnavailable(t *testing.T) {
	uri := "virtual://movie/tt-same?result=cand-a"
	row := &models.MediaFile{ID: 703, FilePath: uri, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-same",
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleEvidenceSet: true,
	}

	if _, _, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, url.Values{}); err == nil {
		t.Fatal("captured-empty evidence with matching provenance served the row's later tracks")
	}
}

// Absent evidence is the one case where the live row may speak: a reconstructed
// session carrying no snapshot must still resolve against the row's inventory.
func TestResolveSubtitleSourceRequestAbsentEvidenceUsesLiveRow(t *testing.T) {
	uri := "virtual://movie/tt-absent?result=cand-a"
	row := &models.MediaFile{ID: 704, FilePath: uri, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "eng"},
		{Index: 2, Codec: "subrip", Language: "fra"},
	}}
	session := &playback.Session{ID: "sess-absent", VirtualSourceURI: uri}

	resolvedFile, resolvedIndex, err := (&StreamHandler{}).resolveSubtitleSourceRequest(
		context.Background(), row, session, 1, url.Values{})
	if err != nil {
		t.Fatalf("absent evidence: %v", err)
	}
	if resolvedFile.FilePath != uri {
		t.Fatalf("served from %q, want the bound row %q", resolvedFile.FilePath, uri)
	}
	if got := resolvedFile.SubtitleTracks[resolvedIndex].Language; got != "fra" {
		t.Fatalf("selected %q, want French from the live row", got)
	}
}

// A downloaded-subtitle request is file-bound rather than plan-bound, so
// captured-empty plan evidence must not make it unsatisfiable.
func TestResolveSubtitleSourceRequestDownloadedPinSurvivesCapturedEmpty(t *testing.T) {
	uri := "virtual://movie/tt-dl?result=cand-a"
	row := &models.MediaFile{ID: 705, FilePath: uri}
	session := &playback.Session{
		ID:                         "sess-dl",
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleEvidenceSet: true,
	}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"42"}}
	subRepo := downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		705: {{ID: 42, MediaFileID: 705, Language: "eng", Format: subtitles.FormatSRT}},
	}}

	handler := &StreamHandler{SubtitleRepo: subRepo}
	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, query); err != nil {
		t.Fatalf("downloaded pin refused under captured-empty plan evidence: %v", err)
	}
}

// Captured-empty evidence must still bound the file to the session's candidate,
// and must not silently hand back the row's own tracks from the binder.
func TestBindSessionVirtualSourceWithTracksHonorsCapturedEmpty(t *testing.T) {
	uri := "virtual://movie/tt-bind?result=cand-a"
	row := &models.MediaFile{ID: 706, FilePath: uri, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleEvidenceSet: true,
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if bound == nil {
		t.Fatal("binder returned nil")
	}
	if len(bound.SubtitleTracks) != 0 {
		t.Fatalf("binder kept the row's %d tracks, want the captured-empty snapshot applied", len(bound.SubtitleTracks))
	}
}

// Absent evidence leaves the live row's own tracks in place.
func TestBindSessionVirtualSourceWithTracksAbsentEvidenceKeepsRow(t *testing.T) {
	uri := "virtual://movie/tt-bind2?result=cand-a"
	row := &models.MediaFile{ID: 707, FilePath: uri, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{VirtualSourceURI: uri}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Language != "eng" {
		t.Fatalf("absent evidence did not leave the row's tracks in place: %+v", bound.SubtitleTracks)
	}
}

// zzResolver resolves media files by id for the edition-switch branch.
type zzResolver struct{ byID map[int]*models.MediaFile }

func (r zzResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if f, ok := r.byID[id]; ok {
		return f, nil
	}
	return nil, nil
}

// A validated downloaded-subtitle pin is file-bound rather than plan-bound, so
// captured-empty plan evidence must not make it unsatisfiable: the row id keeps
// identifying the same artifact across a rotation that replaced the plan-time
// inventory. Without the downloaded exception this returned
// errSubtitleIdentityUnavailable.
func TestResolveSubtitleSourceRequestCapturedEmptyDownloadedPinServed(t *testing.T) {
	oldURI := "virtual://movie/tt-dl2?result=cand-a"
	newURI := "virtual://movie/tt-dl2?result=cand-b"
	row := &models.MediaFile{ID: 920, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-dl2",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		920: {{ID: 77, MediaFileID: 920, Language: "eng", Format: subtitles.FormatSRT}},
	}}}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"77"}}

	resolvedFile, resolvedIndex, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, query)
	if err != nil {
		t.Fatalf("captured-empty evidence refused a valid downloaded pin: %v", err)
	}
	if resolvedFile.FilePath != newURI {
		t.Fatalf("served from %q, want the bound candidate %q", resolvedFile.FilePath, newURI)
	}
	if want := len(playback.BuildSubtitleInventoryV3(row, nil)); resolvedIndex != want {
		t.Fatalf("resolved index %d, want %d (the downloaded row's published ordinal)", resolvedIndex, want)
	}
}

// The same captured-empty evidence with NO downloaded pin must still be refused:
// the exception is scoped to a validated downloaded identity, not to emptiness.
func TestResolveSubtitleSourceRequestCapturedEmptyWithoutPinStillRefused(t *testing.T) {
	oldURI := "virtual://movie/tt-dl3?result=cand-a"
	newURI := "virtual://movie/tt-dl3?result=cand-b"
	row := &models.MediaFile{ID: 921, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-dl3",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		921: {{ID: 78, MediaFileID: 921, Language: "eng", Format: subtitles.FormatSRT}},
	}}}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, url.Values{}); err == nil {
		t.Fatal("captured-empty evidence without a downloaded pin must be refused")
	}
}

// An edition switch (different row ids) with mismatching provenance and
// captured-empty evidence must also serve a valid downloaded pin: the edition
// branch returns before the rotation guard, so it needs the same exception.
func TestResolveSubtitleSourceRequestEditionSwitchCapturedEmptyDownloadedPin(t *testing.T) {
	oldURI := "virtual://movie/tt-ed?result=cand-a"
	newURI := "virtual://movie/tt-ed?result=cand-b"
	named := &models.MediaFile{ID: 910, FilePath: oldURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "eng"},
	}}
	effective := &models.MediaFile{ID: 911, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 7, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-ed",
		MediaFileID:                911,
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{
		fileResolver: zzResolver{byID: map[int]*models.MediaFile{911: effective}},
		SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			910: {{ID: 55, MediaFileID: 910, Language: "eng", Format: subtitles.FormatSRT}},
			911: {{ID: 55, MediaFileID: 911, Language: "eng", Format: subtitles.FormatSRT}},
		}},
	}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"55"}}

	resolvedFile, resolvedIndex, err := handler.resolveSubtitleSourceRequest(
		context.Background(), named, session, 0, query)
	if err != nil {
		t.Fatalf("edition switch refused a valid downloaded pin under captured-empty evidence: %v", err)
	}
	if resolvedFile.FilePath != newURI {
		t.Fatalf("served from %q, want the effective candidate %q", resolvedFile.FilePath, newURI)
	}
	// The effective file has one embedded track, so the downloaded row's
	// published ordinal is 1: the historical ordinal (0) must not survive.
	if want := len(playback.BuildSubtitleInventoryV3(effective, nil)); resolvedIndex != want {
		t.Fatalf("resolved index %d, want %d (the downloaded row's published ordinal on the effective file)", resolvedIndex, want)
	}
}

// An edition switch must resolve a downloaded pin against the effective file's
// own downloaded list: a row the effective file does not own is unavailable,
// never the historical ordinal passed through.
func TestResolveSubtitleSourceRequestEditionSwitchDownloadedPinMissingOnEffective(t *testing.T) {
	oldURI := "virtual://movie/tt-edmiss?result=cand-a"
	newURI := "virtual://movie/tt-edmiss?result=cand-b"
	named := &models.MediaFile{ID: 912, FilePath: oldURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "eng"},
	}}
	effective := &models.MediaFile{ID: 913, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 7, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-edmiss",
		MediaFileID:                913,
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{
		fileResolver: zzResolver{byID: map[int]*models.MediaFile{913: effective}},
		SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			912: {{ID: 56, MediaFileID: 912, Language: "eng", Format: subtitles.FormatSRT}},
		}},
	}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"56"}}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), named, session, 0, query); !errors.Is(err, errSubtitleIdentityUnavailable) {
		t.Fatalf("downloaded pin missing from the effective file: err = %v, want errSubtitleIdentityUnavailable", err)
	}
}

// An edition switch must reject mixed identity pins before any remap: a
// downloaded pin combined with an embedded pin can never name a single track,
// even when both editions publish equivalent embedded tracks at the ordinal.
func TestResolveSubtitleSourceRequestEditionSwitchMixedPinsRejected(t *testing.T) {
	named := &models.MediaFile{ID: 914, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "eng"},
	}}
	effective := &models.MediaFile{ID: 915, SubtitleTracks: []models.SubtitleTrack{
		{Index: 7, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{ID: "sess-edmix", MediaFileID: 915}
	handler := &StreamHandler{
		fileResolver: zzResolver{byID: map[int]*models.MediaFile{915: effective}},
		SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			915: {{ID: 57, MediaFileID: 915, Language: "eng", Format: subtitles.FormatSRT}},
		}},
	}
	query := url.Values{
		playback.DownloadedSubtitleIDParamV3:        []string{"57"},
		playback.EmbeddedSubtitleStreamIndexParamV3: []string{"7"},
	}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), named, session, 0, query); !errors.Is(err, errSubtitleIdentityInvalid) {
		t.Fatalf("mixed pins on edition switch: err = %v, want errSubtitleIdentityInvalid", err)
	}
}

// An edition switch must reject a repeated downloaded pin before any remap,
// even when the effective file owns that row.
func TestResolveSubtitleSourceRequestEditionSwitchDuplicatePinRejected(t *testing.T) {
	named := &models.MediaFile{ID: 916, SubtitleTracks: []models.SubtitleTrack{
		{Index: 1, Codec: "subrip", Language: "eng"},
	}}
	effective := &models.MediaFile{ID: 917, SubtitleTracks: []models.SubtitleTrack{
		{Index: 7, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{ID: "sess-eddup", MediaFileID: 917}
	handler := &StreamHandler{
		fileResolver: zzResolver{byID: map[int]*models.MediaFile{917: effective}},
		SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
			917: {{ID: 58, MediaFileID: 917, Language: "eng", Format: subtitles.FormatSRT}},
		}},
	}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"58", "58"}}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), named, session, 0, query); !errors.Is(err, errSubtitleIdentityInvalid) {
		t.Fatalf("duplicate pin on edition switch: err = %v, want errSubtitleIdentityInvalid", err)
	}
}

// A pinned downloaded row must be resolved against the bound file's own
// downloaded list even when the evidence is nonempty: removing an earlier
// downloaded row changes the list offset, but the pinned row still belongs to
// this file. The remapped ordinal must name the pinned row's current position,
// not the evidence-time offset.
func TestResolveSubtitleSourceRequestDownloadedPinSurvivesNonemptyEvidenceRotation(t *testing.T) {
	oldURI := "virtual://movie/tt-dlrot?result=cand-a"
	newURI := "virtual://movie/tt-dlrot?result=cand-b"
	row := &models.MediaFile{ID: 930, FilePath: newURI, SubtitleTracks: []models.SubtitleTrack{
		{Index: 3, Codec: "subrip", Language: "eng"},
	}}
	session := &playback.Session{
		ID:                         "sess-dlrot",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleTracks: []models.SubtitleTrack{
			{Index: 1, Codec: "subrip", Language: "eng"},
		},
	}
	// The pinned row survived the rotation, but an earlier downloaded row was
	// removed, so its list offset moved from 1 to 0. The stale ordinal 1 would
	// name a different row (or nothing at all); only the row id is stable.
	handler := &StreamHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		930: {{ID: 92, MediaFileID: 930, Language: "eng", Format: subtitles.FormatSRT}},
	}}}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"92"}}

	resolvedFile, resolvedIndex, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 2, query)
	if err != nil {
		t.Fatalf("nonempty-evidence rotation refused a surviving downloaded pin: %v", err)
	}
	if resolvedFile.FilePath != newURI {
		t.Fatalf("served from %q, want the bound candidate %q", resolvedFile.FilePath, newURI)
	}
	if want := len(playback.BuildSubtitleInventoryV3(row, nil)); resolvedIndex != want {
		t.Fatalf("resolved index %d, want %d (the pinned row's current published ordinal)", resolvedIndex, want)
	}
}

// Captured-empty evidence with a downloaded pin plus an embedded pin must be
// rejected: different identity-pin types can never be combined, and the
// captured-empty exception must not bypass that validation.
func TestResolveSubtitleSourceRequestCapturedEmptyMixedPinsRejected(t *testing.T) {
	oldURI := "virtual://movie/tt-dlmix?result=cand-a"
	newURI := "virtual://movie/tt-dlmix?result=cand-b"
	row := &models.MediaFile{ID: 931, FilePath: newURI}
	session := &playback.Session{
		ID:                         "sess-dlmix",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		931: {{ID: 93, MediaFileID: 931, Language: "eng", Format: subtitles.FormatSRT}},
	}}}
	query := url.Values{
		playback.DownloadedSubtitleIDParamV3:        []string{"93"},
		playback.EmbeddedSubtitleStreamIndexParamV3: []string{"3"},
	}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, query); !errors.Is(err, errSubtitleIdentityInvalid) {
		t.Fatalf("mixed pins under captured-empty evidence: err = %v, want errSubtitleIdentityInvalid", err)
	}
}

// Captured-empty evidence with a repeated downloaded pin must be rejected like
// the normal path: the exception resolves the pin directly, but the pin-shape
// validation still runs first.
func TestResolveSubtitleSourceRequestCapturedEmptyDuplicatePinRejected(t *testing.T) {
	oldURI := "virtual://movie/tt-dldup?result=cand-a"
	newURI := "virtual://movie/tt-dldup?result=cand-b"
	row := &models.MediaFile{ID: 932, FilePath: newURI}
	session := &playback.Session{
		ID:                         "sess-dldup",
		VirtualSourceURI:           newURI,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleEvidenceSet: true,
	}
	handler := &StreamHandler{SubtitleRepo: downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		932: {{ID: 94, MediaFileID: 932, Language: "eng", Format: subtitles.FormatSRT}},
	}}}
	query := url.Values{playback.DownloadedSubtitleIDParamV3: []string{"94", "94"}}

	if _, _, err := handler.resolveSubtitleSourceRequest(
		context.Background(), row, session, 0, query); !errors.Is(err, errSubtitleIdentityInvalid) {
		t.Fatalf("duplicate pin under captured-empty evidence: err = %v, want errSubtitleIdentityInvalid", err)
	}
}
