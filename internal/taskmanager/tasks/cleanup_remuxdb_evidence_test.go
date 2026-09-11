package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeRemuxDBEvidencePruner struct {
	deleted int64
	err     error
	called  bool
}

func (p *fakeRemuxDBEvidencePruner) PruneExpired(ctx context.Context) (int64, error) {
	p.called = true
	return p.deleted, p.err
}

type remuxDBProgress struct{ message string }

func (p *remuxDBProgress) Report(_ float64, message string) { p.message = message }
func (*remuxDBProgress) SetResultData(json.RawMessage)      {}

func TestCleanupRemuxDBEvidenceTask(t *testing.T) {
	pruner := &fakeRemuxDBEvidencePruner{deleted: 15}
	task := NewCleanupRemuxDBEvidenceTask(pruner)
	progress := &remuxDBProgress{}

	if task.Key() != "cleanup_remuxdb_evidence" {
		t.Fatalf("unexpected key: %q", task.Key())
	}
	if task.Name() != "Cleanup RemuxDB Evidence" {
		t.Fatalf("unexpected name: %q", task.Name())
	}
	if len(task.DefaultTriggers()) == 0 {
		t.Fatal("expected default triggers")
	}

	if err := task.Execute(context.Background(), progress); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !pruner.called {
		t.Fatal("pruner was not called")
	}
	if !strings.Contains(progress.message, "15") {
		t.Fatalf("expected progress message to mention 15 deleted rows: %q", progress.message)
	}

	// Test error propagation
	prunerErr := &fakeRemuxDBEvidencePruner{err: errors.New("db error")}
	taskErr := NewCleanupRemuxDBEvidenceTask(prunerErr)
	progressErr := &remuxDBProgress{}
	if err := taskErr.Execute(context.Background(), progressErr); err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(progressErr.message, "failed") {
		t.Fatalf("expected failure message, got: %q", progressErr.message)
	}

	// Test nil pruner
	taskNil := NewCleanupRemuxDBEvidenceTask(nil)
	progressNil := &remuxDBProgress{}
	if err := taskNil.Execute(context.Background(), progressNil); err != nil {
		t.Fatalf("expected nil pruner to succeed, got: %v", err)
	}
}
