package virtuallibrary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// Service is the core virtual-library service: Stremio stream resolution plus
// request monitoring, wired directly into the server without a plugin
// boundary. It reads virtual_library.* settings from the encrypted settings
// store and registers virtual media through the catalog registrar.
//
// Construction (New) is intentionally lenient: it returns nil only when the
// service is dormant (disabled or unconfigured). Configuration problems are
// reported by Validate, not New, so callers can distinguish dormant ("no
// provider configured, nothing to do") from misconfigured ("provider set up
// wrong, operator action needed"). Activation (monitor timer +
// Variants-serving) must come AFTER a successful Phase 5 migration; see
// variants.go for the boot-order gate.
type Service struct {
	Resolver *resolver.Resolver
	Monitor  *monitor.Monitor

	cfg    Config
	logger *slog.Logger
}

// ErrVirtualLibraryDormant reports that the service has no provider
// configured (disabled, or no manifest URL). It is not a failure: the caller
// must not install Variants serving or start the monitor timer. It exists so
// Validate can fail closed on real misconfiguration while callers can still
// tell "dormant" apart without re-parsing the config.
var ErrVirtualLibraryDormant = fmt.Errorf("virtual library is dormant: disabled or no manifest URL configured")

// ErrVirtualLibraryUnavailable reports that core Variants() was called while
// the service was not active (nil service, dormant, or Validate failed).
// Activation is gated on the Phase 5 migration; until then Variants closures
// surface this error rather than falling back to the retired plugin path.
// Callers surface collection sync as provider-unavailable, preserving the
// pre-cutover "no provider" behavior without the plugin.
var ErrVirtualLibraryUnavailable = fmt.Errorf("virtual library core service is unavailable: migration pending or service misconfigured")

// Config carries the virtual_library.* settings snapshot used to build the
// service. An empty ManifestURL keeps the service dormant.
type Config struct {
	Enabled                bool
	ManifestURL            string
	MovieLibraryID         int
	SeriesLibraryID        int
	TMDBAPIKey             string
	AllowInsecureHTTP      bool
	CacheTTLMinutes        int
	ScheduleRefreshMinutes int
	MonitorFile            string
	Quality                quality.QualityConfig
	IndexerRSSURL          string
	IndexerAPIKey          string
	IndexerCheckMinutes    int
	AltmountURL            string
	AltmountAPIKey         string
	AltmountCheckMinutes   int
}

// ConfigFromSettings builds a Config from a settings map (e.g. from
// EncryptedSettingsRepo.GetAll). Missing keys fall back to the
// adminSettingDefaults values.
func ConfigFromSettings(m map[string]string) Config {
	qc := quality.QualityConfig{
		Preset:                   stringOr(m, "virtual_library.quality_preset", "custom"),
		CustomFormatPreset:       stringOr(m, "virtual_library.custom_format_preset", "custom"),
		EnableProfiles:           boolOr(m, "virtual_library.enable_quality_profiles", false),
		FallbackToAnyStream:      boolOr(m, "virtual_library.fallback_to_any_stream", false),
		SingleStreamWithFailover: boolOr(m, "virtual_library.single_stream_with_failover", true),
	}
	qc.ApplyPreset()
	if rawProfiles := strings.TrimSpace(m["virtual_library.quality_profiles"]); rawProfiles != "" {
		var profiles []quality.QualityProfile
		if err := json.Unmarshal([]byte(rawProfiles), &profiles); err == nil && len(profiles) > 0 {
			qc.Profiles = profiles
		}
	}
	if rawFormats := strings.TrimSpace(m["virtual_library.custom_formats"]); rawFormats != "" {
		var formats []quality.CustomFormat
		if err := json.Unmarshal([]byte(rawFormats), &formats); err == nil && len(formats) > 0 {
			qc.CustomFormats = formats
		}
	}
	// If profiles are enabled but none configured, apply balanced preset as a
	// safe fallback so Validate does not reject startup.
	if qc.EnableProfiles && len(qc.Profiles) == 0 {
		qc.Preset = "balanced"
		qc.ApplyPreset()
	}

	return Config{
		Enabled:                boolOr(m, "virtual_library.enabled", true),
		ManifestURL:            m["virtual_library.manifest_url"],
		MovieLibraryID:         intOr(m, "virtual_library.movie_library_id", 1),
		SeriesLibraryID:        intOr(m, "virtual_library.series_library_id", 2),
		TMDBAPIKey:             m["virtual_library.tmdb_api_key"],
		AllowInsecureHTTP:      boolOr(m, "virtual_library.allow_insecure_http", false),
		CacheTTLMinutes:        intOr(m, "virtual_library.cache_ttl_minutes", 10),
		ScheduleRefreshMinutes: intOr(m, "virtual_library.schedule_refresh_minutes", 360),
		MonitorFile:            stringOr(m, "virtual_library.monitor_file", ".vio-virtual-library-monitored.json"),
		Quality:                qc,
		IndexerRSSURL:          m["virtual_library.indexer_rss_url"],
		IndexerAPIKey:          m["virtual_library.indexer_api_key"],
		IndexerCheckMinutes:    intOr(m, "virtual_library.indexer_rss_check_minutes", 15),
		AltmountURL:            m["virtual_library.altmount_url"],
		AltmountAPIKey:         m["virtual_library.altmount_api_key"],
		AltmountCheckMinutes:   intOr(m, "virtual_library.altmount_check_minutes", 15),
	}
}

func boolOr(m map[string]string, key string, def bool) bool {
	v, ok := m[key]
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func intOr(m map[string]string, key string, def int) int {
	v, ok := m[key]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func stringOr(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

// SettingsReader is the minimal settings-store surface the service needs.
type SettingsReader interface {
	GetAll(ctx context.Context) (map[string]string, error)
}

// Registrar adapts the catalog virtual-media registrar to the monitor's host
// interface. Currently unused: HostRegistrar only needs ListLibraries (served
// by the plugin host), and registration flows through the monitor's
// MediaRegistrar. Kept as the seam for Phase 3 wiring.
type Registrar interface {
	monitor.HostRegistrar
}

func registrarPool(registrar *catalog.VirtualMediaRegistrar) *pgxpool.Pool {
	if registrar == nil {
		return nil
	}
	return registrar.Pool()
}

// New builds the service from a config snapshot. Returns nil when disabled or
// when no manifest URL is configured (dormant mode). Validate() must be
// called once the settings repo is reachable to verify the manifest URL and
// quality config are sane; New only rejects dormant state.
func New(cfg Config, registrar *catalog.VirtualMediaRegistrar, logger *slog.Logger) *Service {
	if !cfg.Enabled || cfg.ManifestURL == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	resolverCfg := resolver.Config{
		ManifestURL:   cfg.ManifestURL,
		CacheTTL:      time.Duration(cfg.CacheTTLMinutes) * time.Minute,
		AllowInsecure: cfg.AllowInsecureHTTP,
		TMDBAPIKey:    cfg.TMDBAPIKey,
		Quality:       cfg.Quality,
	}
	r := resolver.New(resolverCfg)
	m := monitor.New(r, logger, nil, registrarPool(registrar))
	if err := m.Configure(monitor.Config{
		TMDBAPIKey: cfg.TMDBAPIKey,
		File:       cfg.MonitorFile,
		LibraryIDs: []int{cfg.MovieLibraryID, cfg.SeriesLibraryID},
		Quality:    cfg.Quality,
	}); err != nil {
		logger.Warn("virtual library monitor state unavailable; starting with empty state", "error", err)
	}
	if cfg.IndexerRSSURL != "" {
		if err := m.ConfigureProwlarr(cfg.IndexerRSSURL, cfg.IndexerAPIKey, cfg.IndexerCheckMinutes, ".vio-virtual-library-prowlarr-index.json"); err != nil {
			logger.Warn("virtual library prowlarr configuration error", "error", err)
		}
	}
	if cfg.AltmountURL != "" {
		if err := m.ConfigureAltmount(cfg.AltmountURL, cfg.AltmountAPIKey, cfg.AltmountCheckMinutes, ".vio-virtual-library-altmount-state.json"); err != nil {
			logger.Warn("virtual library altmount configuration error", "error", err)
		}
	}
	if registrar != nil {
		m.SetRegistrar(&catalogMonitorRegistrar{
			registrar:       registrar,
			movieLibraryID:  cfg.MovieLibraryID,
			seriesLibraryID: cfg.SeriesLibraryID,
		})
		m.SetHost(&catalogRegistrarAdapter{registrar: registrar})
	}
	return &Service{Resolver: r, Monitor: m, cfg: cfg, logger: logger}
}

// ValidateConfig checks that the service configuration is internally consistent
// (manifest URL syntax parseable, quality config valid) without requiring network
// reachability. It allows the service to boot cleanly even during temporary
// provider network outages.
func (s *Service) ValidateConfig() error {
	if s == nil {
		return ErrVirtualLibraryDormant
	}
	if !s.cfg.Enabled || s.cfg.ManifestURL == "" {
		return ErrVirtualLibraryDormant
	}
	parsed, err := url.Parse(strings.TrimSpace(s.cfg.ManifestURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("invalid manifest URL: must be valid http(s) URL")
	}
	if parsed.Scheme == "http" && !s.cfg.AllowInsecureHTTP {
		return fmt.Errorf("insecure HTTP manifest requires allow_insecure_http")
	}
	if !strings.HasSuffix(parsed.Path, "/manifest.json") {
		return fmt.Errorf("manifest URL must end in /manifest.json")
	}
	if err := s.cfg.Quality.Validate(); err != nil {
		return fmt.Errorf("validate virtual library quality config: %w", err)
	}
	return nil
}

// Validate checks that the service configuration is internally consistent
// without performing blocking network operations. It is the synchronous
// activation gate for the core service at boot, ensuring server startup is
// decoupled from upstream provider latency or transient network outages.
func (s *Service) Validate(ctx context.Context) error {
	return s.ValidateConfig()
}

// CheckRemote probes the remote provider manifest over the network with a
// bounded timeout. It is used for background health checks, connection tests,
// and admin diagnostic probes.
func (s *Service) CheckRemote(ctx context.Context) error {
	if err := s.ValidateConfig(); err != nil {
		return err
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.Resolver.ValidateConnection(timeoutCtx)
}

// catalogRegistrarAdapter bridges catalog.VirtualMediaRegistrar to the monitor's HostRegistrar interface.
type catalogRegistrarAdapter struct {
	registrar *catalog.VirtualMediaRegistrar
}

func (c *catalogRegistrarAdapter) ListLibraries(ctx context.Context, _ string) ([]*monitor.Library, error) {
	if c == nil || c.registrar == nil || c.registrar.Pool() == nil {
		return nil, nil
	}
	rows, err := c.registrar.Pool().Query(ctx, "SELECT id, name, type FROM media_folders WHERE enabled = true ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*monitor.Library
	for rows.Next() {
		var id int
		var name, typ string
		if err := rows.Scan(&id, &name, &typ); err != nil {
			return nil, err
		}
		out = append(out, &monitor.Library{
			ID:        strconv.Itoa(id),
			Name:      name,
			MediaType: typ,
		})
	}
	return out, rows.Err()
}
