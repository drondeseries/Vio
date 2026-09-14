package remuxdb

import (
	"context"
	"errors"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) Get(_ context.Context, key string) (string, error) {
	v, ok := f[key]
	if !ok {
		return "", nil
	}
	return v, nil
}

func (f fakeSettings) Set(_ context.Context, key, value string) error {
	f[key] = value
	return nil
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(context.Background(), fakeSettings{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Enabled || cfg.BaseURL != DefaultBaseURL || cfg.Token != "" {
		t.Fatalf("config = %+v, want disabled default url empty token", cfg)
	}
}

func TestLoadConfigNilStore(t *testing.T) {
	cfg, err := LoadConfig(context.Background(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("config = %+v, want defaults", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := LoadConfig(context.Background(), fakeSettings{
		SettingEnabled: "false",
		SettingBaseURL: "https://mirror.example.com/",
		SettingToken:   "secret",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Enabled || cfg.BaseURL != "https://mirror.example.com" || cfg.Token != "secret" {
		t.Fatalf("config = %+v, want disabled mirror secret", cfg)
	}
}

func TestLoadConfigInvalidBool(t *testing.T) {
	if _, err := LoadConfig(context.Background(), fakeSettings{SettingEnabled: "yes please"}); err == nil {
		t.Fatal("invalid bool accepted")
	}
}

type failingSettings struct{}

func (failingSettings) Get(_ context.Context, _ string) (string, error) {
	return "", errors.New("boom")
}

func (failingSettings) Set(_ context.Context, _, _ string) error { return nil }

func TestSeedDefaults(t *testing.T) {
	store := fakeSettings{}
	if err := SeedDefaults(context.Background(), store); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if store[SettingEnabled] != "false" || store[SettingBaseURL] != DefaultBaseURL {
		t.Fatalf("seeded = %v", store)
	}
	store[SettingEnabled] = "true"
	if err := SeedDefaults(context.Background(), store); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if store[SettingEnabled] != "true" {
		t.Fatalf("seed overwrote explicit value: %v", store)
	}
	if err := SeedDefaults(context.Background(), failingSettings{}); err == nil {
		t.Fatal("seed ignored read error")
	}
}

func TestExtractIMDbID(t *testing.T) {
	cases := map[string]string{
		"virtual://movie/tt0068646":      "tt0068646",
		"series-tvdb-418505":             "",
		"movie-tmdb-1108427":             "",
		"TT0133093 disk":                 "tt0133093",
		"/mnt/media/tt19035928/file.mkv": "tt19035928",
		"tt12":                           "",
		"":                               "",
	}
	for in, want := range cases {
		if got := ExtractIMDbID(in); got != want {
			t.Errorf("ExtractIMDbID(%q) = %q, want %q", in, got, want)
		}
	}
}
