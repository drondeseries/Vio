package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Silo-Server/silo-server/internal/taskmanager"
)

type remuxDBEvidencePruner interface {
	PruneExpired(ctx context.Context) (int64, error)
}

// CleanupRemuxDBEvidenceTask deletes expired match evidence rows from remuxdb_match_evidence.
type CleanupRemuxDBEvidenceTask struct {
	pruner remuxDBEvidencePruner
}

// NewCleanupRemuxDBEvidenceTask creates a scheduled task for RemuxDB match evidence retention.
func NewCleanupRemuxDBEvidenceTask(pruner remuxDBEvidencePruner) *CleanupRemuxDBEvidenceTask {
	return &CleanupRemuxDBEvidenceTask{pruner: pruner}
}

func (t *CleanupRemuxDBEvidenceTask) Key() string  { return "cleanup_remuxdb_evidence" }
func (t *CleanupRemuxDBEvidenceTask) Name() string { return "Cleanup RemuxDB Evidence" }
func (t *CleanupRemuxDBEvidenceTask) Description() string {
	return "Prunes expired RemuxDB candidate match evidence"
}
func (t *CleanupRemuxDBEvidenceTask) Category() taskmanager.TaskCategory {
	return taskmanager.TaskCategorySystem
}
func (t *CleanupRemuxDBEvidenceTask) IsHidden() bool { return false }

func (t *CleanupRemuxDBEvidenceTask) DefaultTriggers() []taskmanager.TriggerConfig {
	return []taskmanager.TriggerConfig{
		{Type: taskmanager.TriggerTypeStartup},
		{Type: taskmanager.TriggerTypeInterval, IntervalMs: int64((24 * time.Hour) / time.Millisecond)},
	}
}

func (t *CleanupRemuxDBEvidenceTask) Execute(ctx context.Context, progress taskmanager.ProgressReporter) error {
	progress.Report(0, "Pruning expired RemuxDB match evidence")
	if t.pruner == nil {
		progress.Report(100, "RemuxDB store not configured")
		return nil
	}
	deleted, err := t.pruner.PruneExpired(ctx)
	if err != nil {
		slog.WarnContext(ctx, "remuxdb evidence cleanup failed", "component", "taskmanager", "task", t.Key(), "error", err)
		progress.Report(100, fmt.Sprintf("RemuxDB evidence cleanup failed: %v", err))
		return err
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "remuxdb evidence cleanup completed", "component", "taskmanager", "task", t.Key(), "deleted", deleted)
	}
	progress.Report(100, fmt.Sprintf("Pruned %d expired RemuxDB evidence rows", deleted))
	return nil
}
