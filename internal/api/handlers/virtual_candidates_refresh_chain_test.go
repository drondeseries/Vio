package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// recordingEventCount counts recorded events whose payload contains every
// needle. Used to prove the completion event fired exactly once.
func recordingEventCount(bus *recordingEventBus, needles ...string) int {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	count := 0
	for _, event := range bus.events {
		matched := true
		for _, needle := range needles {
			if !strings.Contains(event.Payload, needle) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

func recordingEventLen(bus *recordingEventBus) int {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	return len(bus.events)
}

func recordingEventPayloads(bus *recordingEventBus) []string {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	out := make([]string, 0, len(bus.events))
	for _, event := range bus.events {
		out = append(out, event.Payload)
	}
	return out
}

// waitForJobTerminal polls the durable job row until it reaches a terminal
// state (observable state, never a fixed sleep).
func waitForJobTerminal(t *testing.T, repo *adminjob.Repository, id string, timeout time.Duration) *models.AdminJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		job, err := repo.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read job %s: %v", id, err)
		}
		switch job.Status {
		case adminjob.StatusCompleted, adminjob.StatusFailed, adminjob.StatusCancelled:
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach a terminal state within %s; last status %s (%s)", id, timeout, job.Status, job.ErrorMessage)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestVirtualCandidatesRefreshEndToEndCarriesTracksIndexerReleasesAndEvent is
// the whole-chain proof: a Refresh List job runs through the real adminjob
// runner and the real executor pipeline (provider re-list and DB persist →
// indexer search → dedup → indexer-release upsert → enrichment persist →
// completion event) against the real catalog projections. It asserts the watch
// detail's version list carries the probed audio/subtitle tracks, the
// indexer-release projection carries the new release, the versions_updated event
// fired exactly once, and the one-active-job lock is held while running and
// released after completion.
func TestVirtualCandidatesRefreshEndToEndCarriesTracksIndexerReleasesAndEvent(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := t.Context()

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("movie-refresh-chain-%d", suffix)
	sourcePath := fmt.Sprintf("virtual://movie/refresh-chain-%d", suffix)
	candidateURI := sourcePath + "?result=c1"

	// --- Seed a virtual movie with its source row ---
	var userID int
	if err := pool.QueryRow(ctx, `INSERT INTO users(username, role, enabled) VALUES($1,'user',true) RETURNING id`, fmt.Sprintf("refresh-chain-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var folderID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type, name, enabled) VALUES('movies', $1, true) RETURNING id`, contentID).Scan(&folderID); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id, type, title, genres, runtime) VALUES($1,'movie','Refresh Chain','{}',90)`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id, media_folder_id) VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	// The neutral virtual source row the refresh service discovers. Owner 5
	// drives the candidate rows' owner identity.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, file_size, container, probe_source, virtual_owner_installation_id, duration)
		VALUES($1,$2,$3,0,'virtual','virtual',5,5400)`, contentID, folderID, sourcePath); err != nil {
		t.Fatalf("seed virtual source: %v", err)
	}
	t.Cleanup(func() {
		b := context.Background()
		_, _ = pool.Exec(b, `DELETE FROM admin_jobs WHERE created_by_user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM virtual_indexer_releases WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(b, `DELETE FROM users WHERE id=$1`, userID)
	})

	fileRepo := scanner.NewFileRepository(pool)
	store := virtuallibrary.NewIndexerReleaseStore(pool)

	// --- Provider re-list + real DB persistence (mirrors router wiring) ---
	sink := func(sinkCtx context.Context, source *models.MediaFile, streams []VirtualPlaybackStream) error {
		candidates := make([]scanner.VirtualCandidate, 0, len(streams))
		for _, stream := range streams {
			if !strings.HasPrefix(stream.URI, "virtual://") || stream.URI == source.FilePath {
				continue
			}
			candidates = append(candidates, scanner.VirtualCandidate{
				OwnerInstallationID:    stream.OwnerInstallationID,
				URI:                    stream.URI,
				Label:                  stream.Label,
				Resolution:             stream.Resolution,
				CodecVideo:             stream.CodecVideo,
				CodecAudio:             stream.CodecAudio,
				HDR:                    stream.HDR,
				FileSize:               stream.FileSize,
				Bitrate:                stream.Bitrate,
				AudioLanguages:         stream.AudioLanguages,
				SubtitleLanguages:      stream.SubtitleLanguages,
				ResolvedURL:            stream.ProviderURL,
				ResolvedURLExpiresAt:   stream.ProviderExpiresAt,
				ProviderVideoHash:      stream.ProviderVideoHash,
				ProviderGUID:           stream.ProviderGUID,
				ProviderReleaseName:    stream.ProviderReleaseName,
				ProviderReleaseSize:    stream.FileSize,
				ProviderRequestHeaders: stream.RequestHeaders,
			})
		}
		return fileRepo.ReplaceVirtualCandidates(sinkCtx, source, candidates)
	}

	// The provider re-list is gated so the test can observe the job in its
	// running state and prove the one-active-job lock holds there too.
	listEntered := make(chan struct{})
	listRelease := make(chan struct{})
	var listEnteredOnce sync.Once
	refreshSvc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listEnteredOnce.Do(func() {
				close(listEntered)
				<-listRelease
			})
			return []VirtualPlaybackStream{{
				ID: "c1", URI: path + "?result=c1", Label: "Heat 1995 1080p WEB-DL x264-GRP",
				Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3",
				FileSize: 8_000_000_000, ProviderURL: "https://provider.example/stream",
				OwnerInstallationID: 5,
			}}, nil
		}),
		Persist:      sink,
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
	}

	// The prober returns BOTH audio and subtitle tracks, so enrichment has real
	// inventory to persist (the provider declaration carried none).
	probedAudio := models.AudioTrack{Language: "eng", Codec: "eac3", Channels: 6, Default: true}
	probedSubtitle := models.SubtitleTrack{Index: 3, Language: "eng", Codec: "subrip"}
	playback := &PlaybackHandler{
		VirtualFileLookup: func(lookupCtx context.Context, path string) (*models.MediaFile, error) {
			file, err := fileRepo.GetByPath(lookupCtx, path)
			if err != nil && errors.Is(err, scanner.ErrFileNotFound) {
				return nil, ErrVirtualCandidateNotFound
			}
			return file, err
		},
		VirtualCandidateFileLookup: func(lookupCtx context.Context, path, cid, eid string, oid int) (*models.MediaFile, error) {
			file, err := fileRepo.GetVirtualCandidateByNeutralPath(lookupCtx, path, cid, eid, oid)
			if err != nil && errors.Is(err, scanner.ErrFileNotFound) {
				return nil, ErrVirtualCandidateNotFound
			}
			return file, err
		},
		VirtualFileSaver: func(saveCtx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			return ExecVirtualFileMetadataUpdate(saveCtx, pool, args)
		},
		VirtualFileMetadataSaver: func(saveCtx context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return ExecVirtualFileMetadataUpdateResult(saveCtx, pool, args)
		},
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, file *models.MediaFile, _ map[string]string) (*models.MediaFile, error) {
			probed := *file
			probed.Resolution = "2160p"
			probed.CodecVideo = "hevc"
			probed.CodecAudio = "eac3"
			probed.Container = "mkv"
			probed.Duration = 5400
			probed.VideoTracks = []models.VideoTrack{{Codec: "hevc", Profile: "main 10", Width: 3840, Height: 2160, BitDepth: 10, VideoRange: "SDR", VideoRangeType: "SDR"}}
			probed.AudioTracks = []models.AudioTrack{probedAudio}
			probed.SubtitleTracks = []models.SubtitleTrack{probedSubtitle}
			return &probed, nil
		},
	}

	searcher := &fakeIndexerSearcher{releases: []virtuallibrary.SearchItem{
		// Deduped: same release name and size as the provider listing.
		fakeSearchRelease("dup", "Heat.1995.1080p.WEB-DL.x264-GRP", 8_000_000_000),
		// Kept: a different release the provider does not have.
		fakeSearchRelease("new", "Heat.1995.2160p.WEB-DL.x265-NEWGRP", 8_000_000_000),
	}}

	bus := &recordingEventBus{}
	hub := notifications.NewHub("refresh-chain", bus)
	executor := &VirtualCandidatesRefreshExecutor{
		Refresh:  refreshSvc,
		Store:    store,
		Searcher: searcher,
		Enricher: playback,
		Events:   hub,
	}

	// --- Acceptance path: real job service, real repositories ---
	jobs := adminjob.NewRepository(pool)
	jobSvc := &VirtualCandidatesRefreshJobService{
		Detail:       &fakeIndexerReleaseDetail{},
		Items:        catalog.NewItemRepository(pool),
		Episodes:     catalog.NewEpisodeRepository(pool),
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
		Jobs:         jobs,
	}

	job1, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("create refresh job: %v", err)
	}
	if job1.Status != adminjob.StatusQueued {
		t.Fatalf("job1 status = %q, want queued", job1.Status)
	}

	// One active job per title: a second refresh while the first is queued is
	// coalesced onto the same job rather than starting a second provider work.
	coalesced, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("second create while queued: %v", err)
	}
	if coalesced.ID != job1.ID {
		t.Fatalf("second create returned job %s, want the active %s", coalesced.ID, job1.ID)
	}

	// --- Run the real production runner path ---
	runner := adminjob.NewRunner(jobs, nil, nil, nil, nil, nil, nil, nil, nil)
	runner.SetVirtualRefreshExecutor(executor)
	runner.Start()
	t.Cleanup(runner.Stop)

	// Wait until the job is actually running (the provider re-list is in
	// flight), then prove the lock also holds while running.
	select {
	case <-listEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh job never entered the provider re-list")
	}
	running, err := jobs.GetByID(ctx, job1.ID)
	if err != nil {
		t.Fatalf("read running job: %v", err)
	}
	if running.Status != adminjob.StatusRunning {
		t.Fatalf("job status = %q, want running", running.Status)
	}
	coalescedWhileRunning, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("second create while running: %v", err)
	}
	if coalescedWhileRunning.ID != job1.ID {
		t.Fatalf("second create while running returned job %s, want the active %s", coalescedWhileRunning.ID, job1.ID)
	}
	close(listRelease)

	completed := waitForJobTerminal(t, jobs, job1.ID, 30*time.Second)
	if completed.Status != adminjob.StatusCompleted {
		t.Fatalf("job failed: status=%s message=%s error=%s", completed.Status, completed.Message, completed.ErrorMessage)
	}

	// The completion event fires exactly once, after persistence and enrichment,
	// as catalog.item.changed carrying change=versions_updated for the item.
	if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", contentID); n != 1 {
		all := recordingEventPayloads(bus)
		t.Fatalf("catalog.item.changed/versions_updated for %s fired %d times, want 1; payloads=%v", contentID, n, all)
	}

	// --- The watch detail's version list carries the refreshed version WITH
	// the probed track inventory ---
	detailSvc := catalog.NewDetailService(
		catalog.NewItemRepository(pool),
		catalog.NewEpisodeRepository(pool),
		catalog.NewSeasonRepository(pool),
		catalog.NewPersonRepository(pool),
		fileRepo,
	)
	detail, err := detailSvc.GetWatchDetail(ctx, contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("GetWatchDetail: %v", err)
	}
	var candidateVersion *catalog.FileVersion
	for i := range detail.Versions {
		if detail.Versions[i].FilePath == candidateURI {
			candidateVersion = &detail.Versions[i]
			break
		}
	}
	if candidateVersion == nil {
		paths := make([]string, 0, len(detail.Versions))
		for _, version := range detail.Versions {
			paths = append(paths, version.FilePath)
		}
		t.Fatalf("refreshed candidate %q missing from watch versions %v", candidateURI, paths)
	}
	if len(candidateVersion.AudioTracks) == 0 {
		t.Fatalf("watch version %q carries no audio tracks after enrichment", candidateURI)
	}
	if got := candidateVersion.AudioTracks[0]; got.Language != "eng" || got.Codec != "eac3" {
		t.Fatalf("watch audio track = %+v, want language eng codec eac3", got)
	}
	if len(candidateVersion.SubtitleTracks) == 0 {
		t.Fatalf("watch version %q carries no subtitle tracks after enrichment", candidateURI)
	}
	if got := candidateVersion.SubtitleTracks[0]; got.Language != "eng" || got.Codec != "subrip" {
		t.Fatalf("watch subtitle track = %+v, want language eng codec subrip", got)
	}

	// --- The indexer-release projection that feeds apiv2's indexer_releases ---
	releaseSvc := &VirtualIndexerReleaseService{
		Store:        store,
		Detail:       &fakeIndexerReleaseDetail{},
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
	}
	views, err := releaseSvc.IndexerReleasesForWatch(ctx, contentID)
	if err != nil {
		t.Fatalf("IndexerReleasesForWatch: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("indexer releases = %+v, want the one new release", views)
	}
	if views[0].DownloadState != "not_downloaded" {
		t.Fatalf("indexer release state = %q", views[0].DownloadState)
	}
	if !strings.Contains(views[0].Title, "2160p") {
		t.Fatalf("indexer release title = %q, want the new 2160p release", views[0].Title)
	}
	if strings.Contains(views[0].ReleaseID, "http") {
		t.Fatalf("indexer release id leaked a URL: %q", views[0].ReleaseID)
	}

	// --- The active-job lock is released after completion ---
	runner.Stop()
	job3, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("create after completion: %v", err)
	}
	if job3.ID == job1.ID {
		t.Fatalf("a refresh after completion reused the completed job %s; lock was not released", job1.ID)
	}
}
