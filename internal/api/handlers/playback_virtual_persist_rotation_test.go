package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// persistRotationResolver resolves rows both by id (the inventory build) and by
// exact path (the identity guard's ownership lookup), so one resolver serves the
// rotation write and the publish that follows it.
type persistRotationResolver struct {
	files  map[int]*models.MediaFile
	byPath map[string]*models.MediaFile
}

func (r persistRotationResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	return r.files[id], nil
}

func (r persistRotationResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	return r.byPath[path], nil
}

// persistRotationEvidenceSaver records the catalog writes and the file ids the
// evidence pipeline published inventory for, so a rotation can be observed
// end to end: the write targets the owner row and the publish names it too.
type persistRotationEvidenceSaver struct {
	mu     sync.Mutex
	args   []models.VirtualFilePersistArgs
	result VirtualFileMetadataUpdateResult
}

func (s *persistRotationEvidenceSaver) save(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.args = append(s.args, args)
	return s.result, nil
}

func (s *persistRotationEvidenceSaver) recorded() []models.VirtualFilePersistArgs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.VirtualFilePersistArgs(nil), s.args...)
}

// probedDualAudioFile is the freshly probed inventory the fix must not lose:
// two audio tracks and one subtitle, unlike the pinned row's declared 1xAAC
// snapshot.
func probedDualAudioFile() *models.MediaFile {
	return &models.MediaFile{
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
		VideoTracks:    []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
		AudioTracks:    []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}, {Codec: "aac", Channels: 2, Language: "deu"}},
		SubtitleTracks: []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
}

// TestPersistProbeEvidenceRotatesToSiblingOwnerRow pins the persist-path
// rotation: verified evidence for a URI a sibling row verifiably owns is written
// to that owner row, not attempted as a CAS adoption the SQL sibling fence would
// refuse. The owner's catalog identity is adopted while the freshly probed
// tracks are retained, the write is metadata-only against the owner's own path
// (no adoption fence needed), and the owner row's snapshot supplies the write
// generation.
func TestPersistProbeEvidenceRotatesToSiblingOwnerRow(t *testing.T) {
	const (
		neutral    = "virtual://movie/tt-persist-rotate"
		pinnedURI  = neutral + "?result=pinned"
		ownerURI   = neutral + "?result=owner"
		content    = "movie-persist-rotate"
		folderID   = 9
		ownerID    = 5
		pinnedFile = 601
		ownerFile  = 602
	)
	ownerUpdated := time.Now().Add(-time.Minute).UTC()
	pinnedRow := &models.MediaFile{
		ID: pinnedFile, ContentID: content, FilePath: pinnedURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-pinned",
		UpdatedAt:         time.Now().UTC(),
	}
	ownerRow := &models.MediaFile{
		ID: ownerFile, ContentID: content, FilePath: ownerURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-owner", UpdatedAt: ownerUpdated,
	}

	saver := &persistRotationEvidenceSaver{result: VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
		testPlaybackFileResolver: testPlaybackFileResolver{file: pinnedRow},
		byPath:                   map[string]*models.MediaFile{ownerURI: ownerRow},
	})
	h.VirtualFileMetadataSaver = saver.save
	h.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }

	h.persistVirtualProbeEvidence(context.Background(), pinnedRow, ownerURI, probedDualAudioFile(), true, true)
	h.stopVirtualEvidence()

	calls := saver.recorded()
	if len(calls) != 1 {
		t.Fatalf("persist writes = %d, want exactly 1 onto the owner row", len(calls))
	}
	got := calls[0]
	if got.FileID != ownerFile {
		t.Fatalf("write file_id = %d, want the owner row %d", got.FileID, ownerFile)
	}
	if got.ExpectedFilePath != ownerURI {
		t.Fatalf("write expected path = %q, want the owner row's own path %q", got.ExpectedFilePath, ownerURI)
	}
	if got.AdoptPath != "" || got.RequireAdopt {
		t.Fatalf("rotated write nominated an adoption fence: %+v", got)
	}
	if got.UpdatedAt != ownerUpdated {
		t.Fatalf("write generation = %v, want the owner row's snapshot %v", got.UpdatedAt, ownerUpdated)
	}
	if got.OwnerID != ownerID || got.LibraryID != folderID {
		t.Fatalf("write identity = (owner %d, library %d), want (%d, %d)", got.OwnerID, got.LibraryID, ownerID, folderID)
	}
	if got.ExpectedProviderVideoHash != "hash-owner" {
		t.Fatalf("write provider identity = %q, want the owner row's %q", got.ExpectedProviderVideoHash, "hash-owner")
	}
	// The freshly probed tracks must survive the rotation.
	var audio []models.AudioTrack
	if err := json.Unmarshal(got.AudioTracks, &audio); err != nil {
		t.Fatalf("unmarshal audio tracks: %v", err)
	}
	if len(audio) != 2 {
		t.Fatalf("audio tracks = %#v, want the two probed tracks retained", audio)
	}
	var subs []models.SubtitleTrack
	if err := json.Unmarshal(got.SubtitleTracks, &subs); err != nil {
		t.Fatalf("unmarshal subtitle tracks: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subtitle tracks = %#v, want the probed subtitle retained", subs)
	}
}

// TestPersistProbeEvidenceRotationPublishesVerifiedInventory is the publish
// half: a session bound to the rotated owner row receives the inventory_updated
// event carrying the probed tracks the moment the rotated write commits, so the
// menus stop showing the stale declared snapshot.
func TestPersistProbeEvidenceRotationPublishesVerifiedInventory(t *testing.T) {
	const (
		neutral   = "virtual://movie/tt-persist-publish"
		pinnedURI = neutral + "?result=pinned"
		ownerURI  = neutral + "?result=owner"
		content   = "movie-persist-publish"
		ownerID   = 5
	)
	probedAt := time.Now().UTC()
	ownerRow := &models.MediaFile{
		ID: 702, ContentID: content, FilePath: ownerURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProbeUpdatedAt: &probedAt,
		AudioTracks:    probedDualAudioFile().AudioTracks,
		SubtitleTracks: probedDualAudioFile().SubtitleTracks,
	}
	pinnedRow := &models.MediaFile{
		ID: 701, ContentID: content, FilePath: pinnedURI,
		MediaFolderID: 9, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
	}

	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", pinnedRow.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// The serve layer binds a rotated session to the row that owns the candidate
	// URI; model that here so the publish target is the owner row.
	if err := sessionMgr.SetEffectiveMediaFileID(session.ID, ownerRow.ID); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}
	saver := &persistRotationEvidenceSaver{result: VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}}
	h := NewPlaybackHandler(sessionMgr, persistRotationResolver{
		files:  map[int]*models.MediaFile{pinnedRow.ID: pinnedRow, ownerRow.ID: ownerRow},
		byPath: map[string]*models.MediaFile{ownerURI: ownerRow},
	})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	h.VirtualFileMetadataSaver = saver.save
	h.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }

	h.persistVirtualProbeEvidence(context.Background(), pinnedRow, ownerURI, probedDualAudioFile(), true, false)
	h.stopVirtualEvidence()

	// The write landed on the owner row.
	calls := saver.recorded()
	if len(calls) != 1 || calls[0].FileID != ownerRow.ID {
		t.Fatalf("persist writes = %+v, want exactly one onto owner row %d", calls, ownerRow.ID)
	}
	// And the publish named the owner row, reaching the bound session.
	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1 inventory_updated", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	if event.Name != playback.RealtimeEventInventoryUpdated {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventInventoryUpdated)
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.EffectiveMediaFileID != ownerRow.ID {
		t.Fatalf("effective media file id = %d, want the owner row %d", payload.EffectiveMediaFileID, ownerRow.ID)
	}
	if len(payload.AudioTracks) != 2 {
		t.Fatalf("payload audio tracks = %#v, want the probed dual audio inventory", payload.AudioTracks)
	}
	if len(payload.SubtitleInventory) == 0 {
		t.Fatalf("payload subtitle inventory = %#v, want the probed subtitle carried to the menu", payload.SubtitleInventory)
	}
}

// TestPersistProbeEvidenceRotationGuardRefusalKeepsOldBehavior pins the
// fail-closed half: when the identity guard cannot answer who owns the candidate
// path, the write is refused with a concrete owner_lookup_failed reason rather
// than adopting bytes whose owner is unknown.
func TestPersistProbeEvidenceRotationGuardRefusalKeepsOldBehavior(t *testing.T) {
	logs := captureHandlerLogs(t)
	const (
		neutral = "virtual://movie/tt-persist-guard"
		uri     = neutral + "?result=owner"
	)
	pinnedRow := &models.MediaFile{
		ID: 801, ContentID: "movie-persist-guard", FilePath: neutral + "?result=pinned",
		MediaFolderID: 9, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	saver := &persistRotationEvidenceSaver{result: VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}}
	h := &PlaybackHandler{
		fileResolver:             erroringPathFileResolver{err: errors.New("catalog unavailable")},
		VirtualFileMetadataSaver: saver.save,
		VirtualFileSaver:         func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil },
	}

	h.persistVirtualProbeEvidence(context.Background(), pinnedRow, uri, probedDualAudioFile(), true, true)
	h.stopVirtualEvidence()

	if calls := saver.recorded(); len(calls) != 0 {
		t.Fatalf("guard refusal still wrote %+v, want no write", calls)
	}
	logText := logs.String()
	if !strings.Contains(logText, `"reason":"`+virtualProbeRefusalOwnerLookupFailed+`"`) {
		t.Fatalf("guard refusal did not log the concrete owner_lookup_failed reason:\n%s", logText)
	}
	if !strings.Contains(logText, uri) {
		t.Fatalf("guard refusal did not log the candidate identity %q:\n%s", uri, logText)
	}
}

// TestVirtualProbeEvidenceRefusalReasonSplit pins the refusal taxonomy against
// the SQL fence's own precedence: a sibling path owner, a live failed verdict,
// and the remaining stale-snapshot case are named apart, so the next diagnosis
// does not have to read a lumped bucket.
func TestVirtualProbeEvidenceRefusalReasonSplit(t *testing.T) {
	const (
		neutral  = "virtual://movie/tt-refusal-split"
		uri      = neutral + "?result=owner"
		content  = "movie-refusal-split"
		folderID = 9
		ownerID  = 5
	)
	pinnedRow := &models.MediaFile{
		ID: 900, ContentID: content, FilePath: neutral + "?result=pinned",
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
	}
	// Another content's row owns the candidate path: the identity guard will not
	// rotate to it, so the fence refusal is a sibling owner.
	unrelatedOwner := &models.MediaFile{
		ID: 901, ContentID: "different-content", FilePath: uri,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID,
	}
	sibling := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{uri: unrelatedOwner}},
	}
	if got := sibling.virtualProbeEvidenceRefusalReason(context.Background(), pinnedRow, uri); got != virtualProbeRefusalSiblingOwner {
		t.Fatalf("sibling-owner reason = %q, want %q", got, virtualProbeRefusalSiblingOwner)
	}

	// No path owner, but the candidate identity carries a live failed verdict.
	failedAt := time.Now().UTC()
	failedRow := &models.MediaFile{
		ID: 902, ContentID: content, FilePath: uri,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, FailedAt: &failedAt,
	}
	verdict := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{}},
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return failedRow, nil
		},
	}
	if got := verdict.virtualProbeEvidenceRefusalReason(context.Background(), pinnedRow, uri); got != virtualProbeRefusalFailedVerdict {
		t.Fatalf("failed-verdict reason = %q, want %q", got, virtualProbeRefusalFailedVerdict)
	}

	// No path owner and no live verdict: the only remaining fence cause is a
	// stale CAS snapshot.
	plain := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{}},
	}
	if got := plain.virtualProbeEvidenceRefusalReason(context.Background(), pinnedRow, uri); got != virtualProbeRefusalStaleSnapshot {
		t.Fatalf("stale-snapshot reason = %q, want %q", got, virtualProbeRefusalStaleSnapshot)
	}
}
