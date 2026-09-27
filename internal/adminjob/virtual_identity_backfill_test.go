package adminjob

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeIdentityBackfillExecutor records the resume cursor the runner hands it and
// reports progress so the runner's cursor persistence can be asserted.
type fakeIdentityBackfillExecutor struct {
	calls  int
	resume VirtualIdentityBackfillResume
	result *VirtualIdentityBackfillResult
	err    error
}

func (f *fakeIdentityBackfillExecutor) Execute(_ context.Context, _ VirtualIdentityBackfillRequest, resume VirtualIdentityBackfillResume, progress func(current, total, cursor int, message string)) (*VirtualIdentityBackfillResult, error) {
	f.calls++
	f.resume = resume
	if progress != nil {
		progress(10, 10, 42, "scanning")
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &VirtualIdentityBackfillResult{Cursor: 42, RowsTotal: 10, RowsScanned: 10, RowsMatched: 3}, nil
}

// TestVirtualIdentityBackfillRunnerFailsWithoutExecutor proves a missing
// executor fails the job instead of hanging.
func TestVirtualIdentityBackfillRunnerFailsWithoutExecutor(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	job, err := r.Create(t.Context(), CreateJobInput{
		JobType:         JobTypeVirtualIdentityBackfill,
		CreatedByUserID: userID,
		Message:         "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", job.ID) })

	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.runNext()
	got, err := r.GetByID(t.Context(), job.ID)
	if err != nil || got.Status != StatusFailed {
		t.Fatalf("job = %+v %v, want failed", got, err)
	}
}

// TestVirtualIdentityBackfillRunnerResumesAndCompletes proves the runner reads
// the durable cursor from a prior claim's result payload, passes it to the
// executor, and records the executor's result on completion.
func TestVirtualIdentityBackfillRunnerResumesAndCompletes(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	job, err := r.Create(t.Context(), CreateJobInput{
		JobType:         JobTypeVirtualIdentityBackfill,
		CreatedByUserID: userID,
		Message:         "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", job.ID) })

	// Simulate a prior interrupted claim: the cursor was persisted before the
	// process died.
	if _, err := r.pool.Exec(t.Context(), `UPDATE admin_jobs SET result_payload = '{"cursor":123}'::jsonb WHERE id=$1`, job.ID); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	executor := &fakeIdentityBackfillExecutor{}
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualIdentityBackfillExecutor(executor)
	worker.runNext()

	if executor.calls != 1 || executor.resume.Cursor != 123 {
		t.Fatalf("executor = %+v, want one call resumed at 123", executor)
	}
	got, err := r.GetByID(t.Context(), job.ID)
	if err != nil || got.Status != StatusCompleted {
		t.Fatalf("job = %+v %v, want completed", got, err)
	}
	var result VirtualIdentityBackfillResult
	if err := json.Unmarshal(got.ResultPayload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Cursor != 42 || result.RowsMatched != 3 || result.RowsTotal != 10 {
		t.Fatalf("result = %+v, want the executor's summary", result)
	}
}

// TestHasFinishedJobOfTypeGatesOneShot proves the one-shot marker: a completed
// or canceled job is reported, while a queued or absent one is not (so a
// failed run is retried on the next boot).
func TestHasFinishedJobOfTypeGatesOneShot(t *testing.T) {
	r := lifecycleRepo(t)
	userID := seedRefreshUser(t, r)
	job, err := r.Create(t.Context(), CreateJobInput{
		JobType:         JobTypeVirtualIdentityBackfill,
		CreatedByUserID: userID,
		Message:         "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), "DELETE FROM admin_jobs WHERE id=$1", job.ID) })

	done, err := r.HasFinishedJobOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil {
		t.Fatalf("has finished: %v", err)
	}
	if done {
		t.Fatal("a queued job read as finished")
	}
	if _, err := r.pool.Exec(t.Context(), `UPDATE admin_jobs SET status='completed' WHERE id=$1`, job.ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	done, err = r.HasFinishedJobOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil || !done {
		t.Fatalf("has finished = %v %v, want true for completed", done, err)
	}
	if _, err := r.pool.Exec(t.Context(), `UPDATE admin_jobs SET status='cancelled' WHERE id=$1`, job.ID); err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}
	done, err = r.HasFinishedJobOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil || !done {
		t.Fatalf("has finished = %v %v, want true for cancelled", done, err)
	}
}

var _ VirtualIdentityBackfillExecutor = (*fakeIdentityBackfillExecutor)(nil)
