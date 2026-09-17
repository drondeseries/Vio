package monitor_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

type dummyValidator struct{}

func (dummyValidator) ValidateConnection(context.Context) error { return nil }

func TestMonitorRunBasic(t *testing.T) {
	tempDir := t.TempDir()
	queueFile := filepath.Join(tempDir, "test-queue.json")
	indexFile := filepath.Join(tempDir, "test-index.json")

	m := monitor.New(dummyValidator{}, nil, nil)
	if err := m.Configure(monitor.Config{
		File:              queueFile,
		ProwlarrIndexFile: indexFile,
	}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	resp, err := m.Run(context.Background(), &monitor.RunScheduledTaskRequest{TaskKey: "monitor-media"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	checked, ok := resp.Output["media_checked"].(int)
	if !ok || checked != 0 {
		t.Fatalf("media_checked = %v, want 0", resp.Output["media_checked"])
	}
}

func TestMonitorRunUnknownTaskKey(t *testing.T) {
	m := monitor.New(dummyValidator{}, nil, nil)
	_, err := m.Run(context.Background(), &monitor.RunScheduledTaskRequest{TaskKey: "unknown-task"})
	if err == nil {
		t.Fatal("expected error for unknown task key")
	}
}

func TestMonitorRunNilReceiverFails(t *testing.T) {
	var m *monitor.Monitor
	_, err := m.Run(context.Background(), &monitor.RunScheduledTaskRequest{TaskKey: "monitor-media"})
	if err == nil {
		t.Fatal("expected error calling Run on nil Monitor")
	}
}

func TestMonitorFulfillNilReceiverFails(t *testing.T) {
	var m *monitor.Monitor
	_, err := m.Fulfill(context.Background(), &monitor.FulfillRequest{})
	if err == nil {
		t.Fatal("expected error calling Fulfill on nil Monitor")
	}
}
