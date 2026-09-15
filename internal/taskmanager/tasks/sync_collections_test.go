package tasks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

type fakeCollectionSyncRunner struct {
	progress []catalog.CollectionSyncProgress
	result   json.RawMessage
	err      error
}

func (f *fakeCollectionSyncRunner) RunOnce(_ context.Context, onProgress func(catalog.CollectionSyncProgress)) (json.RawMessage, error) {
	for _, p := range f.progress {
		if onProgress != nil {
			onProgress(p)
		}
	}
	return f.result, f.err
}

type collectionSyncProgressReporter struct {
	percent float64
	message string
	reports []collectionSyncReport
	result  json.RawMessage
}

type collectionSyncReport struct {
	percent float64
	message string
}

func (p *collectionSyncProgressReporter) Report(percent float64, message string) {
	p.percent = percent
	p.message = message
	p.reports = append(p.reports, collectionSyncReport{percent: percent, message: message})
}

func (p *collectionSyncProgressReporter) SetResultData(data json.RawMessage) {
	p.result = append(p.result[:0], data...)
}

func TestSyncCollectionsTaskForwardsSchedulerProgress(t *testing.T) {
	runner := &fakeCollectionSyncRunner{
		progress: []catalog.CollectionSyncProgress{
			{Due: 4, Completed: 0},
			{Due: 4, Completed: 1, CurrentID: "a", CurrentTitle: "Alpha"},
			{Due: 4, Completed: 2, CurrentID: "b", CurrentTitle: "Bravo"},
		},
		result: json.RawMessage(`{"due":4,"synced":2,"failed":0,"skipped":0}`),
	}
	task := NewSyncCollectionsTask(runner)
	progress := &collectionSyncProgressReporter{}

	if err := task.Execute(context.Background(), progress); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Assert interim observations, not just the terminal 0/100. The initial
	// scheduler snapshot reports (0, 0/4) without a title.
	var sawZeroDue, sawTwentyFive, sawFifty bool
	for _, report := range progress.reports {
		switch report.percent {
		case 0:
			sawZeroDue = true
		case 25:
			if report.message != "Synced 1 of 4: Alpha" {
				t.Fatalf("25%% message = %q, want the Alpha title", report.message)
			}
			sawTwentyFive = true
		case 50:
			if report.message != "Synced 2 of 4: Bravo" {
				t.Fatalf("50%% message = %q, want the Bravo title", report.message)
			}
			sawFifty = true
		}
	}
	if !sawZeroDue || !sawTwentyFive || !sawFifty {
		t.Fatalf("interim progress not forwarded: reports = %+v", progress.reports)
	}
	if progress.percent != 100 || progress.message != "Collection sync complete" {
		t.Fatalf("final report = %v %q, want 100 complete", progress.percent, progress.message)
	}
	if string(progress.result) != string(runner.result) {
		t.Fatalf("result data = %s, want %s", progress.result, runner.result)
	}
}
