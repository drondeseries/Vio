package adminjob

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeVirtualRefreshExecutor records the request it was handed and answers a
// fixed result or error.
type fakeVirtualRefreshExecutor struct {
	calls int
	req   VirtualCandidatesRefreshRequest
	err   error
}

func (f *fakeVirtualRefreshExecutor) Execute(_ context.Context, req VirtualCandidatesRefreshRequest, _ func(int, int, string)) (*VirtualCandidatesRefreshResult, error) {
	f.calls++
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return &VirtualCandidatesRefreshResult{ContentID: req.ContentID, ProviderCandidates: 2}, nil
}

// seedRefreshUser creates the account the job's created_by foreign key needs
// and returns its id.
func seedRefreshUser(t *testing.T, r *Repository) int {
	t.Helper()
	var id int
	if err := r.pool.QueryRow(t.Context(), `
		INSERT INTO users (username, role, enabled)
		VALUES ($1, 'user', true) RETURNING id`, "refresh-runner-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM users WHERE id=$1", id) })
	return id
}

// TestCreateVirtualRefreshOneActivePerTitle proves the partial unique index
// serializes refreshes of one title and returns the active job on a repeat.
func TestCreateVirtualRefreshOneActivePerTitle(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	req := VirtualCandidatesRefreshRequest{ContentID: "movie:async", MediaFolderID: 4}
	first, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, req, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", first.ID) })

	_, err = r.CreateVirtualCandidatesRefresh(t.Context(), userID, req, "queued")
	var conflict *ActiveJobConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second create err = %v, want ActiveJobConflictError", err)
	}
	if conflict.Job == nil || conflict.Job.ID != first.ID {
		t.Fatalf("conflict job = %+v, want the active job", conflict.Job)
	}

	// A different title still starts.
	other, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, VirtualCandidatesRefreshRequest{ContentID: "movie:other", MediaFolderID: 4}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", other.ID) })

	active, err := r.GetActiveVirtualRefreshByContentID(t.Context(), "movie:async")
	if err != nil || active.ID != first.ID {
		t.Fatalf("active lookup = %v %+v", err, active)
	}
}

// TestVirtualRefreshRunnerFailureAndSuccess proves the runner executes the
// injected executor and records its outcome.
func TestVirtualRefreshRunnerFailureAndSuccess(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	queued, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, VirtualCandidatesRefreshRequest{ContentID: "movie:runner", MediaFolderID: 4}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", queued.ID) })

	executor := &fakeVirtualRefreshExecutor{}
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualRefreshExecutor(executor)
	worker.runNext()
	if executor.calls != 1 || executor.req.ContentID != "movie:runner" {
		t.Fatalf("executor = %+v", executor)
	}
	done, err := r.GetByID(t.Context(), queued.ID)
	if err != nil || done.Status != StatusCompleted {
		t.Fatalf("job = %+v %v", done, err)
	}

	// A failing executor records the job failed.
	failing, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, VirtualCandidatesRefreshRequest{ContentID: "movie:runner-fail", MediaFolderID: 4}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", failing.ID) })
	worker = NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualRefreshExecutor(&fakeVirtualRefreshExecutor{err: errors.New("provider offline")})
	worker.runNext()
	failed, err := r.GetByID(t.Context(), failing.ID)
	if err != nil || failed.Status != StatusFailed {
		t.Fatalf("job = %+v %v", failed, err)
	}
	if failed.ErrorMessage == "" {
		t.Fatal("failed job carries no error message")
	}
}

// TestVirtualRefreshRunnerWithoutExecutorFails proves a missing executor fails
// the job instead of hanging.
func TestVirtualRefreshRunnerWithoutExecutorFails(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	queued, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, VirtualCandidatesRefreshRequest{ContentID: "movie:no-exec", MediaFolderID: 4}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", queued.ID) })
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.runNext()
	job, err := r.GetByID(t.Context(), queued.ID)
	if err != nil || job.Status != StatusFailed {
		t.Fatalf("job = %+v %v", job, err)
	}
}

// TestVirtualRefreshCancellationQueuedReleasesLock proves the repository accepts
// a virtual-candidates refresh as cancellable, records the intent, and that a
// queued job's cancellation is acknowledged by the runner without executing the
// executor — releasing the one-active-job lock.
func TestVirtualRefreshCancellationQueuedReleasesLock(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	req := VirtualCandidatesRefreshRequest{ContentID: "movie:cancel-queued", MediaFolderID: 4}
	queued, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, req, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", queued.ID) })

	requested, err := r.RequestCancellation(t.Context(), queued.ID)
	if err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if !requested.CancelRequested {
		t.Fatalf("cancel_requested = false after request: %+v", requested)
	}

	// The runner claims the queued job and acknowledges the cancellation without
	// running the executor.
	executor := &fakeVirtualRefreshExecutor{}
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualRefreshExecutor(executor)
	worker.runNext()
	if executor.calls != 0 {
		t.Fatalf("executor ran %d times for a canceled job, want 0", executor.calls)
	}
	done, err := r.GetByID(t.Context(), queued.ID)
	if err != nil || done.Status != StatusCancelled {
		t.Fatalf("job = %+v %v, want canceled", done, err)
	}
	// The unique active-job lock is released, so a fresh refresh of the title
	// starts.
	if _, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, req, "queued"); err != nil {
		t.Fatalf("refresh after cancellation: %v", err)
	}
}

// TestVirtualRefreshCancelQueuedReleasesLockImmediately proves CancelQueued
// settles a queued refresh at once, so the partial unique index no longer covers
// it and a new refresh of the same title starts without waiting for the runner.
func TestVirtualRefreshCancelQueuedReleasesLockImmediately(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	req := VirtualCandidatesRefreshRequest{ContentID: "movie:cancel-now", MediaFolderID: 4}
	queued, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, req, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", queued.ID) })

	canceled, err := r.CancelQueued(t.Context(), queued.ID, "Virtual candidates refresh canceled", time.Now().UTC().Add(time.Hour))
	if err != nil || canceled.Status != StatusCancelled {
		t.Fatalf("cancel queued = %+v %v", canceled, err)
	}
	if _, err := r.GetActiveVirtualRefreshByContentID(t.Context(), req.ContentID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("active lookup after cancel = %v, want not found", err)
	}
}

// TestVirtualRefreshRunningCancellationCancelJob proves the cancel registry
// bridges an API cancellation to the running job's context and the runner
// commits the canceled terminal state.
func TestVirtualRefreshRunningCancellationCancelJob(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	queued, err := r.CreateVirtualCandidatesRefresh(t.Context(), userID, VirtualCandidatesRefreshRequest{ContentID: "movie:cancel-running", MediaFolderID: 4}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", queued.ID) })

	entered := make(chan struct{})
	executor := &blockingVirtualRefreshExecutor{entered: entered}
	registry := NewCancelRegistry()
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualRefreshExecutor(executor)
	worker.SetCancelRegistry(registry)
	go worker.runNext()

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the refresh executor never started")
	}
	if !registry.Cancel(queued.ID) {
		t.Fatal("the running refresh was not registered for cancellation")
	}
	done := waitForVirtualJobTerminal(t, r, queued.ID, 15*time.Second)
	if done.Status != StatusCancelled {
		t.Fatalf("job status = %q (%s), want canceled", done.Status, done.ErrorMessage)
	}
}

// blockingVirtualRefreshExecutor signals when it starts and returns only once
// its context is canceled.
type blockingVirtualRefreshExecutor struct {
	entered chan struct{}
}

func (f *blockingVirtualRefreshExecutor) Execute(ctx context.Context, req VirtualCandidatesRefreshRequest, _ func(int, int, string)) (*VirtualCandidatesRefreshResult, error) {
	close(f.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func waitForVirtualJobTerminal(t *testing.T, r *Repository, id string, timeout time.Duration) *models.AdminJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		job, err := r.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read job %s: %v", id, err)
		}
		switch job.Status {
		case StatusCompleted, StatusFailed, StatusCancelled:
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach a terminal state within %s; last status %s (%s)", id, timeout, job.Status, job.ErrorMessage)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var _ VirtualCandidatesRefreshExecutor = (*fakeVirtualRefreshExecutor)(nil)
