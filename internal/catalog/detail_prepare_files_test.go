package catalog

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// recordingProbeEnsurer records which half of the ensurer contract each
// prepare path asks for.
type recordingProbeEnsurer struct {
	probeCalls  []int
	cachedCalls []int
}

func (e *recordingProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	e.probeCalls = append(e.probeCalls, file.ID)
	return file, nil
}

func (e *recordingProbeEnsurer) EnsureCopySafetyCached(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	e.cachedCalls = append(e.cachedCalls, file.ID)
	return file, nil
}

type recordingCopySafetyRacer struct {
	raced []int
}

func (r *recordingCopySafetyRacer) RaceScan(fileID int) {
	r.raced = append(r.raced, fileID)
}

func h264File(id int, multiplePPS *bool) *models.MediaFile {
	return &models.MediaFile{
		ID:         id,
		CodecVideo: "h264",
		VideoTracks: []models.VideoTrack{{
			Codec:       "h264",
			MultiplePPS: multiplePPS,
		}},
	}
}

// Browse detail must never trigger the H.264 copy-safety scan: the verdict is
// not serialized into those responses, so the scan is pure warm-up and its
// read is what made first-time browsing slow on remote storage.
func TestPrepareBrowseFilesSkipsCopySafety(t *testing.T) {
	ensurer := &recordingProbeEnsurer{}
	racer := &recordingCopySafetyRacer{}
	svc := &DetailService{probeEnsurer: ensurer, copySafetyRacer: racer}
	files := []*models.MediaFile{h264File(1, nil), h264File(2, nil)}

	prepared := svc.prepareBrowseFiles(context.Background(), files)

	if len(prepared) != 2 {
		t.Fatalf("prepareBrowseFiles() returned %d files, want 2", len(prepared))
	}
	if len(ensurer.cachedCalls) != 0 {
		t.Fatalf("browse path resolved copy safety for %v, want probe repair only", ensurer.cachedCalls)
	}
	if len(ensurer.probeCalls) != 2 {
		t.Fatalf("browse path called EnsureProbeOnly %d times, want 2 — probe repair must still run", len(ensurer.probeCalls))
	}
	if len(racer.raced) != 0 {
		t.Fatalf("browse path raced scans for %v, want none", racer.raced)
	}
}

// The watch surfaces prepare a play, but must not block on the bitstream scan:
// they take the cached-only ensure and start the scan in the background.
func TestPreparePlaybackFilesUsesCachedEnsureAndRacesScan(t *testing.T) {
	ensurer := &recordingProbeEnsurer{}
	racer := &recordingCopySafetyRacer{}
	svc := &DetailService{probeEnsurer: ensurer, copySafetyRacer: racer}
	files := []*models.MediaFile{h264File(1, nil), h264File(2, nil)}

	prepared := svc.preparePlaybackFiles(context.Background(), files)

	if len(prepared) != 2 {
		t.Fatalf("preparePlaybackFiles() returned %d files, want 2", len(prepared))
	}
	if len(ensurer.cachedCalls) != 2 {
		t.Fatalf("watch path called EnsureCopySafetyCached %d times, want 2", len(ensurer.cachedCalls))
	}
	if len(racer.raced) != 2 || racer.raced[0] != 1 || racer.raced[1] != 2 {
		t.Fatalf("watch path raced %v, want scans for files 1 and 2", racer.raced)
	}
}

// A file whose verdict is already known, or that is not H.264, has nothing to
// resolve: no background scan may be started for it.
func TestPreparePlaybackFilesSkipsRaceWhenNothingToScan(t *testing.T) {
	known := false
	ensurer := &recordingProbeEnsurer{}
	racer := &recordingCopySafetyRacer{}
	svc := &DetailService{probeEnsurer: ensurer, copySafetyRacer: racer}
	files := []*models.MediaFile{
		h264File(1, &known),
		{ID: 2, CodecVideo: "hevc", VideoTracks: []models.VideoTrack{{Codec: "hevc"}}},
		{ID: 3},
	}

	svc.preparePlaybackFiles(context.Background(), files)

	if len(racer.raced) != 0 {
		t.Fatalf("watch path raced %v, want no scans for known or non-H.264 files", racer.raced)
	}
}

func TestPrepareFilesWithoutEnsurerPassesFilesThrough(t *testing.T) {
	svc := &DetailService{}
	files := []*models.MediaFile{{ID: 1}, nil, {ID: 2}}

	if got := len(svc.prepareBrowseFiles(context.Background(), files)); got != 2 {
		t.Fatalf("prepareBrowseFiles() returned %d files, want 2 (nil entries dropped)", got)
	}
	if got := len(svc.preparePlaybackFiles(context.Background(), files)); got != 2 {
		t.Fatalf("preparePlaybackFiles() returned %d files, want 2 (nil entries dropped)", got)
	}
}

// blockingProbeEnsurer blocks the copy-safety call until it is released or its
// context is done, standing in for the multi-second remote reads a cold probe
// performs on a remote library.
type blockingProbeEnsurer struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newBlockingProbeEnsurer() *blockingProbeEnsurer {
	return &blockingProbeEnsurer{started: make(chan struct{}, 4), release: make(chan struct{})}
}

func (e *blockingProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	return file, nil
}

func (e *blockingProbeEnsurer) EnsureCopySafetyCached(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	e.calls.Add(1)
	select {
	case e.started <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		return file, nil
	case <-ctx.Done():
		return file, ctx.Err()
	}
}

// A watch fetch must return from the metadata already on the row without
// waiting for the probe, however long the probe takes. The repair still runs
// detached in the background.
func TestPrepareWatchFilesDoesNotBlockOnSlowProbe(t *testing.T) {
	ensurer := newBlockingProbeEnsurer()
	svc := &DetailService{probeEnsurer: ensurer}
	files := []*models.MediaFile{h264File(1, nil), h264File(2, nil)}

	start := time.Now()
	prepared := svc.prepareWatchFiles(context.Background(), "movie-slow", "movie", files)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("prepareWatchFiles blocked %s on a slow probe, want an immediate return from known metadata", elapsed)
	}
	if len(prepared) != 2 {
		t.Fatalf("prepareWatchFiles() returned %d files, want 2", len(prepared))
	}

	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	svc.watchRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 2 {
		t.Fatalf("background probe called %d times, want 2", got)
	}
}

// Preparation is claimed before the background refresh starts, so a second
// fetch of the same unchanged file set does not launch a second refresh.
func TestPrepareWatchFilesSchedulesOneRefreshPerFileSet(t *testing.T) {
	ensurer := newBlockingProbeEnsurer()
	svc := &DetailService{probeEnsurer: ensurer}
	files := []*models.MediaFile{h264File(1, nil), h264File(2, nil)}

	svc.prepareWatchFiles(context.Background(), "movie-once", "movie", files)
	svc.prepareWatchFiles(context.Background(), "movie-once", "movie", files)

	// Only the first refresh has started; the second fetch saw the claim.
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	svc.watchRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 2 {
		t.Fatalf("background probe called %d times for two fetches of one file set, want 2 (one refresh)", got)
	}
}

// The response is built from the row's existing metadata even when the
// ensurer would return a repaired row: the repair is not on the response path.
func TestPrepareWatchFilesServesKnownMetadata(t *testing.T) {
	svc := &DetailService{probeEnsurer: &repairedProbeEnsurer{}}
	files := []*models.MediaFile{{ID: 1, Duration: 10}}

	prepared := svc.prepareWatchFiles(context.Background(), "movie-known", "movie", files)
	if len(prepared) != 1 || prepared[0].Duration != 10 {
		t.Fatalf("prepareWatchFiles() = %+v, want the row's known metadata (duration 10)", prepared)
	}
	svc.watchRefreshWG.Wait()
}

type repairedProbeEnsurer struct{}

func (e *repairedProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	return file, nil
}

func (e *repairedProbeEnsurer) EnsureCopySafetyCached(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	repaired := *file
	repaired.Duration = 999
	return &repaired, nil
}

// A probe failure must fail open: the watch fetch still succeeds from known
// metadata, and the background refresh surfaces the error only as a log.
func TestPrepareWatchFilesFailsOpenOnProbeError(t *testing.T) {
	ensurer := &failingProbeEnsurer{}
	svc := &DetailService{probeEnsurer: ensurer}
	files := []*models.MediaFile{{ID: 1}}

	prepared := svc.prepareWatchFiles(context.Background(), "movie-fail", "movie", files)
	if len(prepared) != 1 {
		t.Fatalf("prepareWatchFiles() returned %d files, want 1", len(prepared))
	}
	svc.watchRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("background probe called %d times, want 1", got)
	}
}

type failingProbeEnsurer struct{ calls atomic.Int32 }

func (e *failingProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	return file, errors.New("probe failed")
}

func (e *failingProbeEnsurer) EnsureCopySafetyCached(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	e.calls.Add(1)
	return file, errors.New("probe failed")
}

// A saturated watch-refresh semaphore must drop the memo claim and launch no
// goroutine, so a later fetch can retry instead of piling up behind the probe
// slots. Draining a slot lets the retry through.
func TestPrepareWatchFilesDropsClaimWhenRefreshSaturated(t *testing.T) {
	ensurer := newBlockingProbeEnsurer()
	svc := &DetailService{probeEnsurer: ensurer}
	sem := svc.watchRefreshSemaphore()
	for i := 0; i < watchRefreshConcurrency; i++ {
		sem <- struct{}{}
	}
	defer func() {
		for {
			select {
			case <-sem:
			default:
				return
			}
		}
	}()

	files := []*models.MediaFile{{ID: 1}}
	prepared := svc.prepareWatchFiles(context.Background(), "movie-saturated", "movie", files)
	if len(prepared) != 1 {
		t.Fatalf("prepareWatchFiles() returned %d files, want 1", len(prepared))
	}
	svc.watchRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 0 {
		t.Fatalf("saturated refresh ran %d probes, want 0", got)
	}
	svc.watchPrepareMu.Lock()
	_, claimed := svc.watchPrepared["movie-saturated"]
	svc.watchPrepareMu.Unlock()
	if claimed {
		t.Fatal("saturated refresh left its memo claim; a later fetch would not retry")
	}

	// Draining a slot lets a later fetch retry and claim again.
	<-sem
	prepared = svc.prepareWatchFiles(context.Background(), "movie-saturated", "movie", files)
	if len(prepared) != 1 {
		t.Fatalf("retry prepareWatchFiles() returned %d files, want 1", len(prepared))
	}
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("retry after draining did not start a refresh")
	}
	close(ensurer.release)
	svc.watchRefreshWG.Wait()
}

// Chapter-thumbnail queueing moves with the rest of the preparation: the first
// fetch for a file set enqueues once, in the background, and a repeated fetch
// of the same unchanged set does not enqueue again.
func TestPrepareWatchFilesQueuesChapterThumbsOncePerFileSet(t *testing.T) {
	queuer := &recordingChapterQueuer{}
	svc := &DetailService{probeEnsurer: &recordingProbeEnsurer{}, chapterThumbs: queuer}
	files := []*models.MediaFile{{ID: 1}, {ID: 2}}

	svc.prepareWatchFiles(context.Background(), "movie-queue", "movie", files)
	svc.watchRefreshWG.Wait()
	if len(queuer.calls) != 1 {
		t.Fatalf("first fetch queued %d chapter-thumb batches, want 1", len(queuer.calls))
	}
	if got := queuer.calls[0]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("queued file IDs %v, want [1 2]", got)
	}

	svc.prepareWatchFiles(context.Background(), "movie-queue", "movie", files)
	svc.watchRefreshWG.Wait()
	if len(queuer.calls) != 1 {
		t.Fatalf("repeated fetch re-queued chapter thumbs: %d batches, want 1", len(queuer.calls))
	}
}
