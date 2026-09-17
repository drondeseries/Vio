package virtuallibrary

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
)

// RetireResult reports one retirement attempt of the retired plugin.
type RetireResult struct {
	// Retired is true when an installation was uninstalled.
	Retired bool
	// InstallationID is the retired installation (0 when none existed).
	InstallationID int
}

// PluginRetirer is the minimal plugin-host surface retirement needs:
// stop the running provider process, flush in-memory caches, and delete
// the installation row (which cascades runtime configs, capabilities,
// archives per the ON DELETE CASCADE FKs, removes virtual catalog state
// via RemoveVirtualMediaInstallation, and RemoveAlls the install dir).
type PluginRetirer interface {
	Stop(installationID int) error
	ClearCaches()
	DeleteInstallation(ctx context.Context, id int) error
}

// RetirePlugin uninstalls the retired plugin installation after the core
// service is durably verified (migration marker set + Validate nil — the
// caller proves this before calling). Order:
//
//  1. Stop the provider process so no in-flight RPC resolves mid-delete.
//  2. Clear in-memory variant/stream/profile caches so the deleted
//     installation's rows can never be served stale.
//  3. Delete the installation (cascades configs; removes virtual catalog
//     state; removes install dir).
//
// Stop/Clear errors are logged, never fatal: a dead provider or empty
// cache must not block retirement. Only the Delete error fails the call.
// The call is idempotent: no installation means Retired=false, nil error.
func RetirePlugin(ctx context.Context, installations InstallationLister, plugin PluginRetirer, settings CoreSettingsWriter, logger *slog.Logger) (RetireResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var res RetireResult
	// Retirement requires a verified migration. Without the marker, the
	// core service was never proven — keep the plugin, fail explicitly.
	if marked, err := settings.Get(ctx, migratedMarkerKey); err != nil || marked != "true" {
		return res, fmt.Errorf("refusing to retire %s: migration not verified", VirtualLibraryPluginID)
	}
	list, err := installations.ListByPluginID(ctx, VirtualLibraryPluginID)
	if err != nil {
		return res, fmt.Errorf("listing %s installations: %w", VirtualLibraryPluginID, err)
	}
	if len(list) == 0 {
		return res, nil // nothing installed: idempotent no-op
	}
	for _, inst := range list {
		if inst == nil {
			continue
		}
		instMarker := "virtual_library.migrated_installation_" + strconv.Itoa(inst.ID)
		if marked, err := settings.Get(ctx, instMarker); err != nil || marked != "true" {
			logger.Warn("refusing to retire installation: specific installation not marked as migrated",
				"installation_id", inst.ID)
			continue
		}
		res.InstallationID = inst.ID
		if err := plugin.Stop(inst.ID); err != nil {
			logger.Warn("virtual library retirement: provider stop failed; continuing to delete",
				"installation_id", inst.ID, "error", err)
		}
		plugin.ClearCaches()
		if err := plugin.DeleteInstallation(ctx, inst.ID); err != nil {
			return res, fmt.Errorf("deleting retired installation %d: %w", inst.ID, err)
		}
		res.Retired = true
		logger.Info("virtual library plugin retired", "installation_id", inst.ID)
	}
	return res, nil
}
