package virtuallibrary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// VirtualLibraryPluginID is the retired plugin whose streaming config group
// migrates into virtual_library.* server settings. Anything else in the
// plugin host (tmdb/tvdb, community plugins) is out of scope.
const VirtualLibraryPluginID = "com.drondeseries.vio-virtual-library"

// migratedMarkerKey records a completed migration so boot runs it once.
// It is a plain (non-secret) server setting.
const migratedMarkerKey = "virtual_library.migrated_from_plugin"

// RunTask aliases the monitor's scheduled-task request so boot wiring
// (cmd/silo) needs only the virtuallibrary import, not the monitor package.
type RunTask = monitor.RunScheduledTaskRequest

// PluginInstallation is the minimal installation identity the migration
// needs: find the retired plugin's installations by plugin ID.
type PluginInstallation struct {
	ID       int
	PluginID string
	Enabled  bool
}

// InstallationLister lists installations for one plugin ID. Production
// callers adapt *plugins.InstallationStore via ListByPluginID, which
// returns []*plugins.Installation — the adapter lives in the caller
// (cmd/silo) to keep the plugins->virtuallibrary edge one-directional.
// For testability the interface uses the local PluginInstallation shape;
// production adapters convert.
type InstallationLister interface {
	ListByPluginID(ctx context.Context, pluginID string) ([]*PluginInstallation, error)
}

// PluginConfigReader reads one decrypted global config group for an
// installation (mirrors RuntimeConfigStore.ListGlobalConfigs filtering).
type PluginConfigReader interface {
	GlobalConfig(ctx context.Context, installationID int, key string) (map[string]any, error)
}

// InstallationListerFunc adapts a plain func to InstallationLister.
type InstallationListerFunc func(ctx context.Context, pluginID string) ([]*PluginInstallation, error)

// ListByPluginID implements InstallationLister.
func (f InstallationListerFunc) ListByPluginID(ctx context.Context, pluginID string) ([]*PluginInstallation, error) {
	return f(ctx, pluginID)
}

// PluginConfigReaderFunc adapts a plain func to PluginConfigReader.
type PluginConfigReaderFunc func(ctx context.Context, installationID int, key string) (map[string]any, error)

// GlobalConfig implements PluginConfigReader.
func (f PluginConfigReaderFunc) GlobalConfig(ctx context.Context, installationID int, key string) (map[string]any, error) {
	return f(ctx, installationID, key)
}

// CoreSettingsWriter persists migrated virtual_library.* keys. It is the
// cipher-aware server-settings repo (EncryptedSettingsRepo): values flow
// through GCM encryption, never raw SQL.
type CoreSettingsWriter interface {
	Get(ctx context.Context, key string) (string, error)
	SetMany(ctx context.Context, values map[string]string) error
}

// MigrateResult reports one migration attempt.
type MigrateResult struct {
	// Ran is false when the marker was already set (idempotent no-op).
	Ran bool
	// Migrated is the number of keys written.
	Migrated int
	// SkippedExisting counts keys left untouched because core already holds
	// a non-default value. The admin's core settings always win.
	SkippedExisting int
	// InstallationID is the retired plugin installation migrated (0 = none
	// found, nothing to migrate).
	InstallationID int
}

// streamingToCore maps plugin `streaming` group fields to virtual_library.*
// keys. Fields with no core counterpart (indexer/altmount integration,
// raw profile/format arrays) are NOT migrated here: quality profiles and
// custom formats live behind their own admin surface (quality package) and
// blindly copying arrays would bypass their validation.
var streamingToCore = []struct {
	plugin string
	core   string
	kind   string // "string" | "int" | "bool"
}{
	{"enabled", "virtual_library.enabled", "bool"},
	{"manifest_url", "virtual_library.manifest_url", "string"},
	{"movie_library_id", "virtual_library.movie_library_id", "int"},
	{"series_library_id", "virtual_library.series_library_id", "int"},
	{"tmdb_api_key", "virtual_library.tmdb_api_key", "string"},
	{"monitor_file", "virtual_library.monitor_file", "string"},
	{"allow_insecure_http", "virtual_library.allow_insecure_http", "bool"},
	{"schedule_refresh_minutes", "virtual_library.schedule_refresh_minutes", "int"},
	{"cache_ttl_minutes", "virtual_library.cache_ttl_minutes", "int"},
	{"enable_quality_profiles", "virtual_library.enable_quality_profiles", "bool"},
	{"quality_preset", "virtual_library.quality_preset", "string"},
	{"custom_format_preset", "virtual_library.custom_format_preset", "string"},
	{"fallback_to_any_stream", "virtual_library.fallback_to_any_stream", "bool"},
	{"single_stream_with_failover", "virtual_library.single_stream_with_failover", "bool"},
}

// MigrateFromPlugin copies the retired plugin's `streaming` config group
// into virtual_library.* core settings. It is idempotent (per-installation
// marker guarded), never overwrites existing core values, and never touches
// the plugin installation itself — uninstall is a separate, later step that
// runs only after migration is durably verified per installation.
//
// Boot order per Oracle: migrate -> validate -> activate. On error the
// caller keeps the plugin installed with its config and must NOT activate
// the core service.
func MigrateFromPlugin(ctx context.Context, installations InstallationLister, configs PluginConfigReader, settings CoreSettingsWriter, logger *slog.Logger) (MigrateResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var res MigrateResult
	list, err := installations.ListByPluginID(ctx, VirtualLibraryPluginID)
	if err != nil {
		return res, fmt.Errorf("listing %s installations: %w", VirtualLibraryPluginID, err)
	}
	if len(list) == 0 {
		// No installation found: clean install or already retired. Do NOT
		// write a global marker so that if an admin installs the plugin
		// later, its config can still be discovered and migrated.
		return res, nil
	}

	for _, inst := range list {
		if inst == nil {
			continue
		}
		instMarker := "virtual_library.migrated_installation_" + strconv.Itoa(inst.ID)
		if marked, err := settings.Get(ctx, instMarker); err == nil && marked == "true" {
			continue // this installation was already migrated
		}

		res.InstallationID = inst.ID
		group, err := configs.GlobalConfig(ctx, inst.ID, "streaming")
		if err != nil {
			return res, fmt.Errorf("reading streaming config for installation %d: %w", inst.ID, err)
		}
		if len(group) == 0 {
			logger.Info("virtual library migration: streaming group empty; marking installation migrated",
				"installation_id", inst.ID)
			if err := settings.SetMany(ctx, map[string]string{instMarker: "true", migratedMarkerKey: "true"}); err != nil {
				return res, fmt.Errorf("marking virtual library installation %d migrated: %w", inst.ID, err)
			}
			res.Ran = true
			continue
		}

		toWrite := make(map[string]string, len(streamingToCore)+2)
		for _, m := range streamingToCore {
			raw, ok := group[m.plugin]
			if !ok || raw == nil {
				continue
			}
			encoded, ok := encodePluginValue(raw, m.kind)
			if !ok {
				logger.Warn("virtual library migration: skipping unparsable field",
					"field", m.plugin, "installation_id", inst.ID)
				continue
			}
			current, err := settings.Get(ctx, m.core)
			if err == nil && current != "" && current != coreDefault(m.core) {
				res.SkippedExisting++
				continue
			}
			toWrite[m.core] = encoded
		}

		// quality_profiles / custom_formats arrays migrate directly to active
		// settings when core has none configured.
		if err := migrateProfileArrays(ctx, group, settings, toWrite, logger, inst.ID); err != nil {
			logger.Warn("virtual library migration: profile arrays skipped", "error", err)
		}

		toWrite[instMarker] = "true"
		toWrite[migratedMarkerKey] = "true"
		if err := settings.SetMany(ctx, toWrite); err != nil {
			return res, fmt.Errorf("writing migrated virtual library settings for installation %d: %w", inst.ID, err)
		}
		res.Ran = true
		res.Migrated += len(toWrite) - 2 // exclude markers
		logger.Info("virtual library installation migrated",
			"installation_id", inst.ID, "migrated", res.Migrated, "skipped_existing", res.SkippedExisting)
	}

	return res, nil
}

// coreDefault mirrors adminSettingDefaults for the migrated keys so "empty
// or default" counts as unset. Unknown keys default to "" (always migrate).
func coreDefault(key string) string {
	switch key {
	case "virtual_library.enabled":
		return "true"
	case "virtual_library.movie_library_id":
		return "1"
	case "virtual_library.series_library_id":
		return "2"
	case "virtual_library.cache_ttl_minutes":
		return "10"
	case "virtual_library.schedule_refresh_minutes":
		return "360"
	case "virtual_library.monitor_file":
		return ".vio-virtual-library-monitored.json"
	case "virtual_library.quality_preset", "virtual_library.custom_format_preset":
		return "custom"
	case "virtual_library.allow_insecure_http", "virtual_library.enable_quality_profiles",
		"virtual_library.fallback_to_any_stream":
		return "false"
	case "virtual_library.single_stream_with_failover":
		return "true"
	default:
		return ""
	}
}

// encodePluginValue coerces a decoded plugin config value (string, float64
// from JSON numbers, or bool) into the canonical settings string form.
func encodePluginValue(raw any, kind string) (string, bool) {
	switch kind {
	case "bool":
		switch v := raw.(type) {
		case bool:
			return strconv.FormatBool(v), true
		case string:
			// The plugin's truthiness (isTruthy) accepts yes/no on top of
			// Go's ParseBool vocabulary; mirror it so "yes" migrates true.
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "yes", "y", "on":
				return "true", true
			case "no", "n", "off":
				return "false", true
			}
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return "", false
			}
			return strconv.FormatBool(b), true
		case float64:
			return strconv.FormatBool(v != 0), true
		default:
			return "", false
		}
	case "int":
		switch v := raw.(type) {
		case float64:
			return strconv.Itoa(int(v)), true
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return "", false
			}
			return strconv.Itoa(n), true
		case bool:
			if v {
				return "1", true
			}
			return "0", true
		default:
			return "", false
		}
	default: // string
		switch v := raw.(type) {
		case string:
			return v, true
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(v), true
		default:
			return "", false
		}
	}
}

// migrateProfileArrays carries quality_profiles and custom_formats directly
// into active virtual_library.* settings when core has none configured.
func migrateProfileArrays(ctx context.Context, group map[string]any, settings CoreSettingsWriter, toWrite map[string]string, logger *slog.Logger, installationID int) error {
	for _, field := range []string{"quality_profiles", "custom_formats"} {
		raw, ok := group[field]
		if !ok || raw == nil {
			continue
		}
		arr, ok := raw.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		blob, err := json.Marshal(arr)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		targetKey := "virtual_library." + field
		if current, err := settings.Get(ctx, targetKey); err == nil && current != "" {
			continue
		}
		toWrite[targetKey] = string(blob)
		logger.Info("virtual library migration: migrated profile array", "field", field, "installation_id", installationID)
	}
	return nil
}
