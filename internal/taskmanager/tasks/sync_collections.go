package tasks

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/taskmanager"
)

// CollectionSyncRunner runs a single pass of the collection sync scheduler.
// onProgress may be nil.
type CollectionSyncRunner interface {
	RunOnce(ctx context.Context, onProgress func(catalog.CollectionSyncProgress)) (json.RawMessage, error)
}

// SyncCollectionsTask finds collections with a sync schedule that are due
// and syncs them. It delegates to CollectionSyncRunner for the actual work.
type SyncCollectionsTask struct {
	scheduler CollectionSyncRunner
}

// NewSyncCollectionsTask creates a new SyncCollectionsTask.
func NewSyncCollectionsTask(scheduler CollectionSyncRunner) *SyncCollectionsTask {
	return &SyncCollectionsTask{scheduler: scheduler}
}

func (t *SyncCollectionsTask) Key() string  { return "sync_collections" }
func (t *SyncCollectionsTask) Name() string { return "Sync Collections" }
func (t *SyncCollectionsTask) Description() string {
	return "Syncs collections that have a scheduled automatic sync"
}
func (t *SyncCollectionsTask) Category() taskmanager.TaskCategory {
	return taskmanager.TaskCategoryLibrary
}
func (t *SyncCollectionsTask) IsHidden() bool { return false }

func (t *SyncCollectionsTask) DefaultTriggers() []taskmanager.TriggerConfig {
	return []taskmanager.TriggerConfig{
		{Type: taskmanager.TriggerTypeInterval, IntervalMs: 5 * 60 * 1000}, // every 5 minutes
	}
}

func (t *SyncCollectionsTask) Execute(ctx context.Context, progress taskmanager.ProgressReporter) error {
	progress.Report(0, "Checking for due collections")

	resultData, err := t.scheduler.RunOnce(ctx, func(update catalog.CollectionSyncProgress) {
		if update.Due <= 0 {
			return
		}
		percent := float64(update.Completed) / float64(update.Due) * 100
		message := fmt.Sprintf("Synced %d of %d collections", update.Completed, update.Due)
		if update.CurrentTitle != "" {
			message = fmt.Sprintf("Synced %d of %d: %s", update.Completed, update.Due, update.CurrentTitle)
		}
		progress.Report(percent, message)
	})
	if err != nil {
		return fmt.Errorf("collection sync scheduler: %w", err)
	}

	if resultData != nil {
		progress.SetResultData(resultData)
	}

	progress.Report(100, "Collection sync complete")
	return nil
}
