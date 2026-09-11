package remuxdb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/settingskeys"
)

// Server setting keys for the RemuxDB integration. The base URL ships with
// the public default so the feature works untouched, and an admin can point
// it at a mirror when the URL changes. The token is optional and enables
// write access (crowdsourced submissions).
const (
	SettingEnabled       = settingskeys.RemuxdbEnabled
	SettingBaseURL       = settingskeys.RemuxdbBaseUrl
	SettingToken         = settingskeys.RemuxdbToken
	SettingSubmitEnabled = settingskeys.RemuxdbSubmitEnabled
)

// SettingsStore reads server_settings. Satisfied by
// *catalog.ServerSettingsRepo (and its encrypting decorator).
type SettingsStore interface {
	Get(ctx context.Context, key string) (string, error)
}

// Config is the resolved RemuxDB integration configuration.
type Config struct {
	Enabled       bool
	BaseURL       string
	Token         string
	SubmitEnabled bool
}

// DefaultConfig returns the out-of-box configuration.
func DefaultConfig() Config {
	return Config{Enabled: false, BaseURL: DefaultBaseURL, SubmitEnabled: false}
}

// LoadConfig reads the RemuxDB settings, falling back to defaults for
// missing keys. A missing store behaves like defaults.
func LoadConfig(ctx context.Context, store SettingsStore) (Config, error) {
	cfg := DefaultConfig()
	if store == nil {
		return cfg, nil
	}
	get := func(key string) string {
		v, err := store.Get(ctx, key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	if raw := get(SettingEnabled); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: must be true or false", SettingEnabled, raw)
		}
		cfg.Enabled = enabled
	}
	if raw := get(SettingBaseURL); raw != "" {
		cfg.BaseURL = strings.TrimRight(raw, "/")
	}
	cfg.Token = get(SettingToken)
	if raw := get(SettingSubmitEnabled); raw != "" {
		if submitEnabled, err := strconv.ParseBool(raw); err == nil {
			cfg.SubmitEnabled = submitEnabled
		}
	}
	return cfg, nil
}

// SeedDefaults writes the RemuxDB defaults for keys not already set.
func SeedDefaults(ctx context.Context, store SettingsStore) error {
	defs := map[string]string{
		SettingEnabled:       "false",
		SettingBaseURL:       DefaultBaseURL,
		SettingSubmitEnabled: "false",
	}
	for key, value := range defs {
		existing, err := store.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("seed remuxdb default %s: %w", key, err)
		}
		if strings.TrimSpace(existing) != "" {
			continue
		}
		if setter, ok := store.(interface {
			Set(ctx context.Context, key, value string) error
		}); ok {
			if err := setter.Set(ctx, key, value); err != nil {
				return fmt.Errorf("seed remuxdb default %s: %w", key, err)
			}
		}
	}
	return nil
}
