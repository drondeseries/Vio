package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeRefreshDetail is the watch-detail seam: it answers a fixed detail or a
// fixed error, and can answer a different detail after the refresh so a test
// can prove the response is the post-refresh list.
type fakeRefreshDetail struct {
	detail *catalog.WatchDetail
	post   *catalog.WatchDetail
	err    error
	calls  int
}

func (f *fakeRefreshDetail) WatchDetail(_ context.Context, _ int, _ string, _ string, _ catalog.AccessFilter) (*catalog.WatchDetail, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls > 1 && f.post != nil {
		return f.post, nil
	}
	return f.detail, nil
}

func refreshTestSource(id int, contentID, path string) *models.MediaFile {
	return &models.MediaFile{
		ID: id, ContentID: contentID, FilePath: path,
		Container: "virtual", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
	}
}

func refreshTestDetail() *catalog.WatchDetail {
	return &catalog.WatchDetail{Versions: []catalog.FileVersion{{
		FileID: 7, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}}
}

// TestVirtualCandidatesRefreshServiceRefreshesAndReturnsList covers the happy
// path: the item's virtual source group is force-listed, persisted through the
// candidate sink with the provider-neutral source row, and the post-refresh
// watch-detail versions are returned.
func TestVirtualCandidatesRefreshServiceRefreshesAndReturnsList(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	candidate := refreshTestSource(8, "movie:x", "virtual://movie/x?result=old")

	var listPaths []string
	var persistedSource *models.MediaFile
	var persistedStreams []VirtualPlaybackStream
	post := &catalog.WatchDetail{Versions: []catalog.FileVersion{{
		FileID: 9, Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}}
	detail := &fakeRefreshDetail{detail: refreshTestDetail(), post: post}
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listPaths = append(listPaths, path)
			return []VirtualPlaybackStream{{
				ID: "new", URI: path + "?result=new", Resolution: "2160p",
			}}, nil
		}),
		Persist: func(_ context.Context, s *models.MediaFile, streams []VirtualPlaybackStream) error {
			persistedSource = s
			persistedStreams = streams
			return nil
		},
		ContentFiles: func(_ context.Context, contentID string) ([]*models.MediaFile, error) {
			if contentID == "movie:x" {
				return []*models.MediaFile{candidate, source}, nil
			}
			return nil, nil
		},
		Detail: detail,
	}

	versions, err := svc.RefreshVirtualCandidates(context.Background(), 1, "p-owner", "movie:x", catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("RefreshVirtualCandidates: %v", err)
	}
	if len(listPaths) != 1 || listPaths[0] != "virtual://movie/x" {
		t.Fatalf("listed paths = %v, want the neutral source", listPaths)
	}
	if persistedSource == nil || persistedSource.FilePath != "virtual://movie/x" || persistedSource.VirtualOwnerInstallationID != 5 {
		t.Fatalf("persisted source = %+v", persistedSource)
	}
	if len(persistedStreams) != 1 || persistedStreams[0].URI != "virtual://movie/x?result=new" {
		t.Fatalf("persisted streams = %+v", persistedStreams)
	}
	if len(versions) != 1 || versions[0].FileID != 9 {
		t.Fatalf("versions = %+v, want the post-refresh list", versions)
	}
	if detail.calls != 2 {
		t.Fatalf("detail reads = %d, want the pre- and post-refresh reads", detail.calls)
	}
}

// TestVirtualCandidatesRefreshServiceEpisodeSource proves an episode content id
// resolves its source through the episode lookup and still lists the neutral
// group.
func TestVirtualCandidatesRefreshServiceEpisodeSource(t *testing.T) {
	episode := refreshTestSource(21, "series:y", "virtual://series/y/1/2")
	var listed string
	persisted := 0
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listed = path
			return []VirtualPlaybackStream{{ID: "a", URI: path + "?result=a"}}, nil
		}),
		Persist: func(_ context.Context, _ *models.MediaFile, _ []VirtualPlaybackStream) error {
			persisted++
			return nil
		},
		EpisodeFiles: func(_ context.Context, episodeID string) ([]*models.MediaFile, error) {
			if episodeID == "episode:y:1:2" {
				return []*models.MediaFile{episode}, nil
			}
			return nil, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	if _, err := svc.RefreshVirtualCandidates(context.Background(), 1, "p", "episode:y:1:2", catalog.AccessFilter{}); err != nil {
		t.Fatalf("RefreshVirtualCandidates: %v", err)
	}
	if listed != "virtual://series/y/1/2" || persisted != 1 {
		t.Fatalf("listed=%q persisted=%d", listed, persisted)
	}
}

// TestVirtualCandidatesRefreshServiceNonVirtualIsClientError proves an item
// with no virtual rows is a 4xx problem, not a provider failure.
func TestVirtualCandidatesRefreshServiceNonVirtualIsClientError(t *testing.T) {
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			t.Fatal("provider listed for a non-virtual item")
			return nil, nil
		}),
		Persist:      func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
		Detail:       &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	_, err := svc.RefreshVirtualCandidates(context.Background(), 1, "p", "movie:local", catalog.AccessFilter{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 APIError", err)
	}
}

// TestVirtualCandidatesRefreshServiceProviderFailureIsRetryable proves a
// provider failure is a retryable 503 and never persists.
func TestVirtualCandidatesRefreshServiceProviderFailureIsRetryable(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	persisted := 0
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return nil, errors.New("provider offline")
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			persisted++
			return nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	_, err := svc.RefreshVirtualCandidates(context.Background(), 1, "p", "movie:x", catalog.AccessFilter{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 APIError", err)
	}
	if persisted != 0 {
		t.Fatalf("persist calls = %d, want 0 on provider failure", persisted)
	}
}

// TestVirtualCandidatesRefreshServiceCoalescesConcurrentCalls proves two
// concurrent refreshes of one title share a single provider re-list instead of
// storming the provider.
func TestVirtualCandidatesRefreshServiceCoalescesConcurrentCalls(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	var listCalls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			if atomic.AddInt32(&listCalls, 1) == 1 {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			return []VirtualPlaybackStream{{ID: "a", URI: path + "?result=a"}}, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.RefreshVirtualCandidates(context.Background(), 1, "p", "movie:x", catalog.AccessFilter{})
		}(i)
	}
	// Wait until the first caller is inside the provider list, then give the
	// second caller a moment to join the in-flight refresh before releasing.
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("provider list calls = %d, want 1 (coalesced)", got)
	}
}
