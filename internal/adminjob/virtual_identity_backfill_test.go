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

// TestVirtualIdentityBackfillRunnerPartialFailureStaysRetriable proves a pass
// where a provider group failed does not complete the job: the one-shot token
// is not consumed and the retriable watermark is preserved in the result
// payload for the next run.
func TestVirtualIdentityBackfillRunnerPartialFailureStaysRetriable(t *testing.T) {
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

	executor := &fakeIdentityBackfillExecutor{result: &VirtualIdentityBackfillResult{
		Cursor: 40, GroupsScanned: 3, GroupsFailed: 1, RowsTotal: 12, RowsScanned: 12, RowsMatched: 7, RowsUnmatched: 5,
	}}
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualIdentityBackfillExecutor(executor)
	worker.runNext()

	got, err := r.GetByID(t.Context(), job.ID)
	if err != nil || got.Status != StatusFailed {
		t.Fatalf("job = %+v %v, want failed after a partial pass", got, err)
	}
	done, err := r.HasFinishedJobOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil {
		t.Fatalf("has finished: %v", err)
	}
	if done {
		t.Fatal("a partially failed backfill consumed the one-shot token")
	}
	var resume VirtualIdentityBackfillResume
	if err := json.Unmarshal(got.ResultPayload, &resume); err != nil {
		t.Fatalf("decode retriable watermark: %v", err)
	}
	if resume.Cursor != 40 {
		t.Fatalf("watermark cursor = %d, want 40", resume.Cursor)
	}
}

// TestVirtualIdentityBackfillRunnerResumesAfterPartialFailure proves a failed
// partial pass is revived with its watermark and the next claim resumes there
// instead of restarting from zero.
func TestVirtualIdentityBackfillRunnerResumesAfterPartialFailure(t *testing.T) {
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

	first := &fakeIdentityBackfillExecutor{result: &VirtualIdentityBackfillResult{
		Cursor: 40, GroupsScanned: 3, GroupsFailed: 1, RowsTotal: 12, RowsScanned: 12, RowsMatched: 7, RowsUnmatched: 5,
	}}
	worker := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker.SetVirtualIdentityBackfillExecutor(first)
	worker.runNext()

	revived, err := r.RequeueLatestFailedOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil || !revived {
		t.Fatalf("requeue failed job = %v %v, want true", revived, err)
	}
	requeued, err := r.GetByID(t.Context(), job.ID)
	if err != nil || requeued.Status != StatusQueued {
		t.Fatalf("job = %+v %v, want queued for retry", requeued, err)
	}

	second := &fakeIdentityBackfillExecutor{}
	worker2 := NewRunner(NewRepository(r.pool), nil, nil, nil, nil, nil, nil, nil, nil)
	worker2.SetVirtualIdentityBackfillExecutor(second)
	worker2.runNext()

	if second.calls != 1 || second.resume.Cursor != 40 {
		t.Fatalf("second executor = %+v, want one call resumed at 40", second)
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
	if _, err := r.pool.Exec(t.Context(), `UPDATE admin_jobs SET status='cancelled' WHERE id=$1`, job.ID); err != nil { //nolint:misspell // Persisted DB enum value, see StatusCancelled.
		t.Fatalf("mark canceled: %v", err)
	}
	done, err = r.HasFinishedJobOfType(t.Context(), JobTypeVirtualIdentityBackfill)
	if err != nil || !done {
		t.Fatalf("has finished = %v %v, want true for canceled", done, err)
	}
}

var _ VirtualIdentityBackfillExecutor = (*fakeIdentityBackfillExecutor)(nil)
