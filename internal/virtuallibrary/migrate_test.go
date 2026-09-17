package virtuallibrary

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

type fakeInstallations struct {
	list []*PluginInstallation
	err  error
}

func (f *fakeInstallations) ListByPluginID(_ context.Context, pluginID string) ([]*PluginInstallation, error) {
	if f.err != nil {
		return nil, f.err
	}
	if pluginID != VirtualLibraryPluginID {
		return nil, nil
	}
	return f.list, nil
}

type fakeConfigs struct {
	groups map[int]map[string]any
	err    error
}

func (f *fakeConfigs) GlobalConfig(_ context.Context, installationID int, key string) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	if key != "streaming" {
		return nil, nil
	}
	return f.groups[installationID], nil
}

type fakeSettings struct {
	values map[string]string
	getErr map[string]error
	setErr error
	sets   int
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{values: map[string]string{}, getErr: map[string]error{}}
}

func (f *fakeSettings) Get(_ context.Context, key string) (string, error) {
	if err, ok := f.getErr[key]; ok {
		return "", err
	}
	return f.values[key], nil
}

func (f *fakeSettings) SetMany(_ context.Context, values map[string]string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.sets++
	for k, v := range values {
		f.values[k] = v
	}
	return nil
}

func testLogger() *slog.Logger { return slog.Default() }

func TestMigrateNoInstallationSafeNoop(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	res, err := MigrateFromPlugin(ctx, &fakeInstallations{}, &fakeConfigs{groups: map[int]map[string]any{}}, st, testLogger())
	if err != nil {
		t.Fatalf("MigrateFromPlugin: %v", err)
	}
	if res.Ran || res.InstallationID != 0 {
		t.Fatalf("expected no-op, got %+v", res)
	}
	// Absence of an installation must NOT mark migration globally done;
	// otherwise an installation added later would be silently deleted without migration.
	if st.values[migratedMarkerKey] != "" {
		t.Fatalf("marker must not be set on empty install: %v", st.values)
	}
	// Second run remains a safe no-op.
	res2, err := MigrateFromPlugin(ctx, &fakeInstallations{}, &fakeConfigs{groups: map[int]map[string]any{}}, st, testLogger())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Ran || st.sets != 0 {
		t.Fatalf("second run not a no-op: %+v sets=%d", res2, st.sets)
	}
}

func TestMigrateCopiesStreamingGroup(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 7, PluginID: VirtualLibraryPluginID, Enabled: true}}}
	cfg := &fakeConfigs{groups: map[int]map[string]any{
		7: {
			"manifest_url":           "https://provider.example/manifest.json",
			"movie_library_id":       float64(3),
			"allow_insecure_http":    true,
			"cache_ttl_minutes":      float64(30),
			"quality_preset":         "balanced",
			"enabled":                false,
			"unknown_future_field":   "ignored",
			"fallback_to_any_stream": "yes",
		},
	}}
	res, err := MigrateFromPlugin(ctx, inst, cfg, st, testLogger())
	if err != nil {
		t.Fatalf("MigrateFromPlugin: %v", err)
	}
	if !res.Ran || res.InstallationID != 7 {
		t.Fatalf("expected ran install 7, got %+v", res)
	}
	want := map[string]string{
		"virtual_library.manifest_url":           "https://provider.example/manifest.json",
		"virtual_library.enabled":                "false",
		"virtual_library.movie_library_id":       "3",
		"virtual_library.allow_insecure_http":    "true",
		"virtual_library.cache_ttl_minutes":      "30",
		"virtual_library.quality_preset":         "balanced",
		"virtual_library.fallback_to_any_stream": "true",
	}
	for k, v := range want {
		if st.values[k] != v {
			t.Errorf("%s = %q, want %q", k, st.values[k], v)
		}
	}
	if _, ok := st.values["virtual_library.unknown_future_field"]; ok {
		t.Errorf("unknown field leaked into settings")
	}
}

func TestMigrateNeverOverwritesCore(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.values["virtual_library.manifest_url"] = "https://admin-core.example/manifest.json"
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 9, PluginID: VirtualLibraryPluginID}}}
	cfg := &fakeConfigs{groups: map[int]map[string]any{
		9: {"manifest_url": "https://plugin.example/manifest.json", "cache_ttl_minutes": float64(45)},
	}}
	res, err := MigrateFromPlugin(ctx, inst, cfg, st, testLogger())
	if err != nil {
		t.Fatalf("MigrateFromPlugin: %v", err)
	}
	if st.values["virtual_library.manifest_url"] != "https://admin-core.example/manifest.json" {
		t.Fatalf("core value overwritten: %q", st.values["virtual_library.manifest_url"])
	}
	if st.values["virtual_library.cache_ttl_minutes"] != "45" {
		t.Fatalf("unset key not migrated: %v", st.values)
	}
	if res.SkippedExisting != 1 {
		t.Fatalf("SkippedExisting = %d, want 1", res.SkippedExisting)
	}
}

func TestMigrateFailureKeepsPlugin(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	st.setErr = errors.New("db down")
	inst := &fakeInstallations{list: []*PluginInstallation{{ID: 11, PluginID: VirtualLibraryPluginID}}}
	cfg := &fakeConfigs{groups: map[int]map[string]any{11: {"manifest_url": "https://x.example/m.json"}}}
	if _, err := MigrateFromPlugin(ctx, inst, cfg, st, testLogger()); err == nil {
		t.Fatalf("expected error")
	}
	if st.values[migratedMarkerKey] == "true" {
		t.Fatalf("marker set despite failure — plugin would be uninstalled on unverified migration")
	}
}

func TestMigrateListError(t *testing.T) {
	ctx := context.Background()
	st := newFakeSettings()
	inst := &fakeInstallations{err: errors.New("list failed")}
	if _, err := MigrateFromPlugin(ctx, inst, &fakeConfigs{}, st, testLogger()); err == nil {
		t.Fatalf("expected list error")
	}
}
