package adminjob

import (
	"context"
	"errors"
	"testing"
	"time"
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

var _ VirtualCandidatesRefreshExecutor = (*fakeVirtualRefreshExecutor)(nil)
