package streamtelemetry

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Silo-Server/silo-server/internal/envutil"
)

const (
	enabledEnv            = "VIO_STREAM_TELEMETRY_ENABLED"
	enabledEnvLegacy      = "SILO_STREAM_TELEMETRY_ENABLED"
	familiesEnv           = "VIO_STREAM_TELEMETRY_FAMILIES"
	familiesEnvLegacy     = "SILO_STREAM_TELEMETRY_FAMILIES"
	sweepIntervalEnv      = "VIO_STREAM_TELEMETRY_SWEEP_INTERVAL"
	sweepIntervalEnvOld   = "SILO_STREAM_TELEMETRY_SWEEP_INTERVAL"
	retentionEnv          = "VIO_STREAM_TELEMETRY_RETENTION"
	retentionEnvLegacy    = "SILO_STREAM_TELEMETRY_RETENTION"
	maxSessionsEnv        = "VIO_STREAM_TELEMETRY_MAX_SESSIONS"
	maxSessionsEnvLegacy  = "SILO_STREAM_TELEMETRY_MAX_SESSIONS"
	maxTransfersEnv       = "VIO_STREAM_TELEMETRY_MAX_TRANSFERS"
	maxTransfersEnvLegacy = "SILO_STREAM_TELEMETRY_MAX_TRANSFERS"
	maxObservationsEnv    = "VIO_STREAM_TELEMETRY_MAX_OBSERVATIONS"
	maxObservationsOld    = "SILO_STREAM_TELEMETRY_MAX_OBSERVATIONS"
	distributedEnv        = "VIO_STREAM_TELEMETRY_DISTRIBUTED"
	distributedEnvLegacy  = "SILO_STREAM_TELEMETRY_DISTRIBUTED"
	freshnessEnv          = "VIO_STREAM_TELEMETRY_FRESHNESS"
	freshnessEnvLegacy    = "SILO_STREAM_TELEMETRY_FRESHNESS"
	membershipTTLEnv      = "VIO_STREAM_TELEMETRY_MEMBERSHIP_TTL"
	membershipTTLLegacy   = "SILO_STREAM_TELEMETRY_MEMBERSHIP_TTL"
	keyPrefixEnv          = "VIO_STREAM_TELEMETRY_KEY_PREFIX"
	keyPrefixEnvLegacy    = "SILO_STREAM_TELEMETRY_KEY_PREFIX"
	fullResyncEveryEnv    = "VIO_STREAM_TELEMETRY_FULL_RESYNC_EVERY"
	fullResyncEveryOld    = "SILO_STREAM_TELEMETRY_FULL_RESYNC_EVERY"
	maxPublishersEnv      = "VIO_STREAM_TELEMETRY_MAX_PUBLISHERS"
	maxPublishersOld      = "SILO_STREAM_TELEMETRY_MAX_PUBLISHERS"
	maxMergedSessionsEnv  = "VIO_STREAM_TELEMETRY_MAX_MERGED_SESSIONS"
	maxMergedSessionsOld  = "SILO_STREAM_TELEMETRY_MAX_MERGED_SESSIONS"
	maxMergedTransfersEnv = "VIO_STREAM_TELEMETRY_MAX_MERGED_TRANSFERS"
	maxMergedTransfersOld = "SILO_STREAM_TELEMETRY_MAX_MERGED_TRANSFERS"
	viewTTLEnv            = "VIO_STREAM_TELEMETRY_VIEW_TTL"
	viewTTLEnvLegacy      = "SILO_STREAM_TELEMETRY_VIEW_TTL"
)

type Config struct {
	// Enabled turns observation on for this process, and defaults ON:
	// SILO_STREAM_TELEMETRY_ENABLED=false is the per-process kill switch. A value
	// that is set but unparseable also reads as off (envutil.BoolDefault), so a
	// mistyped kill switch fails towards silence rather than towards running.
	Enabled        bool
	NodeID         string
	PublisherID    string
	PublisherEpoch int64
	// Distributed publishes and reads snapshots through Redis. ConfigFromEnv only
	// reads the variable; when it was not set, wiring derives the mode from
	// whether Redis is configured, so a single-process deployment stays local and
	// a clustered one merges without either having to name a second variable.
	Distributed bool
	// DistributedExplicit reports that Distributed is already settled and must not
	// be auto-derived — either the operator set SILO_STREAM_TELEMETRY_DISTRIBUTED,
	// or an invalid distributed configuration has forced the mode off.
	DistributedExplicit bool
	// Families narrows which route families are observed. Empty means every
	// declared family (AllFamilies) — observation is on for all five by default.
	// The variable exists to narrow observation or drop one misbehaving family
	// without losing the rest; it is a kill switch, not a staged rollout.
	Families map[Family]bool

	SweepInterval time.Duration
	Retention     time.Duration
	Freshness     time.Duration
	MembershipTTL time.Duration
	KeyPrefix     string
	// ViewTTL bounds how stale a served merged view may be. It gates a rebuild
	// that measured ~347 ms at the 50 000-session cap, so it is a cost control
	// rather than a freshness preference.
	ViewTTL            time.Duration
	FullResyncEvery    int
	MaxPublishers      int
	MaxMergedSessions  int
	MaxMergedTransfers int

	MaxSessions                    int64
	MaxTransfers                   int64
	MaxObservations                int64
	MaxObservationsPerSession      int
	MaxViewerIPsPerSession         int
	MaxIdentityConflictsPerSession int
	MaxDeviceIDsPerSession         int
	MaxClientVariantsPerSession    int
	MaxMediaFileIDsPerSession      int
	MaxPlayMethodsPerSession       int
	MaxTokenIssuedAtPerSession     int
	MaxRoutesPerSession            int
}

// defaultFreshness is how long a published snapshot stays current. It doubles as
// the decay window for a publisher's Truncated flag, since both answer the same
// question: is this publisher's picture of the world usable right now?
const defaultFreshness = 5 * time.Second

func DefaultConfig(nodeID string) Config {
	return Config{
		NodeID: nodeID, SweepInterval: time.Second, Retention: 5 * time.Minute,
		Freshness: defaultFreshness, MembershipTTL: time.Minute, KeyPrefix: "vio:stelem",
		ViewTTL:         DefaultViewTTL,
		FullResyncEvery: 60, MaxPublishers: 256, MaxMergedSessions: 50_000, MaxMergedTransfers: 50_000,
		MaxSessions: 10_000, MaxTransfers: 10_000, MaxObservations: 50_000,
		MaxObservationsPerSession: 64, MaxViewerIPsPerSession: 32,
		MaxIdentityConflictsPerSession: 16, MaxDeviceIDsPerSession: 32,
		MaxClientVariantsPerSession: 16, MaxMediaFileIDsPerSession: 32,
		MaxPlayMethodsPerSession: 16, MaxTokenIssuedAtPerSession: 32,
		MaxRoutesPerSession: 32,
	}
}

// ConfigFromEnv returns a safe configuration. Telemetry is on unless
// VIO_STREAM_TELEMETRY_ENABLED turns it off (SILO_* legacy honored), and
// distributed mode is left for
// the caller to derive from Redis availability unless the operator pinned
// VIO_STREAM_TELEMETRY_DISTRIBUTED. Invalid core settings disable telemetry;
// invalid distributed-only settings retain local telemetry.
func ConfigFromEnv(nodeID string) Config {
	cfg := DefaultConfig(nodeID)
	cfg.Enabled = envutil.BoolDefaultNames(true, enabledEnv, enabledEnvLegacy)
	coreInvalid := make([]string, 0)
	distributedInvalid := make([]string, 0)
	// The operator only owns the variables they actually set. The cross-checks
	// below relate two knobs, and a violation involving an unset knob is not the
	// operator's mistake — it is a default that has to move. Only the timing and
	// sizing knobs are ever cross-checked; ENABLED and DISTRIBUTED are never
	// blamed by crossCheckFailed, so defaulting them on cannot misreport anyone.
	explicit := make(map[string]bool)
	cfg.Distributed = envutil.BoolNames(distributedEnv, distributedEnvLegacy)
	cfg.DistributedExplicit = envutil.IsSet(distributedEnv) || envutil.IsSet(distributedEnvLegacy)
	parseDuration := func(names []string, dst *time.Duration) {
		name := names[0]
		value := strings.TrimSpace(envutil.GetenvFirst(names...))
		if value == "" {
			return
		}
		explicit[name] = true
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			coreInvalid = append(coreInvalid, name)
			return
		}
		*dst = parsed
	}
	parseDistributedDuration := func(names []string, dst *time.Duration) {
		name := names[0]
		value := strings.TrimSpace(envutil.GetenvFirst(names...))
		if value == "" {
			return
		}
		explicit[name] = true
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			distributedInvalid = append(distributedInvalid, name)
			return
		}
		*dst = parsed
	}
	parsePositive := func(names []string, dst *int64) {
		name := names[0]
		value := strings.TrimSpace(envutil.GetenvFirst(names...))
		if value == "" {
			return
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			coreInvalid = append(coreInvalid, name)
			return
		}
		*dst = parsed
	}
	parseDistributedPositive := func(names []string, dst *int) {
		name := names[0]
		value := strings.TrimSpace(envutil.GetenvFirst(names...))
		if value == "" {
			return
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			distributedInvalid = append(distributedInvalid, name)
			return
		}
		*dst = parsed
	}
	parseDuration([]string{sweepIntervalEnv, sweepIntervalEnvOld}, &cfg.SweepInterval)
	parseDuration([]string{retentionEnv, retentionEnvLegacy}, &cfg.Retention)
	parsePositive([]string{maxSessionsEnv, maxSessionsEnvLegacy}, &cfg.MaxSessions)
	parsePositive([]string{maxTransfersEnv, maxTransfersEnvLegacy}, &cfg.MaxTransfers)
	parsePositive([]string{maxObservationsEnv, maxObservationsOld}, &cfg.MaxObservations)
	parseDistributedDuration([]string{freshnessEnv, freshnessEnvLegacy}, &cfg.Freshness)
	parseDistributedDuration([]string{membershipTTLEnv, membershipTTLLegacy}, &cfg.MembershipTTL)
	parseDistributedDuration([]string{viewTTLEnv, viewTTLEnvLegacy}, &cfg.ViewTTL)
	parseDistributedPositive([]string{fullResyncEveryEnv, fullResyncEveryOld}, &cfg.FullResyncEvery)
	parseDistributedPositive([]string{maxPublishersEnv, maxPublishersOld}, &cfg.MaxPublishers)
	parseDistributedPositive([]string{maxMergedSessionsEnv, maxMergedSessionsOld}, &cfg.MaxMergedSessions)
	parseDistributedPositive([]string{maxMergedTransfersEnv, maxMergedTransfersOld}, &cfg.MaxMergedTransfers)
	if value := strings.TrimSpace(envutil.GetenvFirst(familiesEnv, familiesEnvLegacy)); value != "" {
		if families, ok := parseFamilies(value); ok {
			cfg.Families = families
		} else {
			coreInvalid = append(coreInvalid, familiesEnv)
		}
	}
	if value := envutil.GetenvFirst(keyPrefixEnv, keyPrefixEnvLegacy); value != "" {
		if strings.TrimSpace(value) == "" || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
			distributedInvalid = append(distributedInvalid, keyPrefixEnv)
		} else {
			cfg.KeyPrefix = value
		}
	}
	// Cross-checks. Each relates a knob to another knob, so comparing an
	// env-supplied value against the other's DEFAULT and then rejecting the
	// config is wrong twice over: it disables distributed mode for a single
	// variable, and it blames a variable the operator never touched. When only
	// one side was set, the unset side moves to satisfy the invariant; only a
	// pair the operator pinned to genuinely inconsistent values is an error, and
	// then only the variables they set are named.
	crossCheckFailed := func(involved ...string) {
		named := make([]string, 0, len(involved))
		for _, name := range involved {
			if explicit[name] {
				named = append(named, name)
			}
		}
		if len(named) == 0 {
			// Defaults that violate their own invariant: a code bug, not an
			// operator one. Name both so it is findable.
			named = involved
		}
		distributedInvalid = append(distributedInvalid, named...)
	}
	// Repair first, then validate the RESOLVED values. Repairs only ever move a
	// knob the operator left at its default, and are ordered so a later one
	// cannot undo an earlier one.
	if !explicit[freshnessEnv] && cfg.SweepInterval <= time.Duration(1<<63-1)/3 && cfg.Freshness < 3*cfg.SweepInterval {
		cfg.Freshness = 3 * cfg.SweepInterval
	}
	if !explicit[sweepIntervalEnv] && cfg.Freshness < 3*cfg.SweepInterval {
		cfg.SweepInterval = cfg.Freshness / 3
	}
	if !explicit[membershipTTLEnv] && cfg.MembershipTTL <= cfg.Freshness {
		cfg.MembershipTTL = 2 * cfg.Freshness
	}
	if !explicit[freshnessEnv] && cfg.MembershipTTL <= cfg.Freshness {
		cfg.Freshness = cfg.MembershipTTL / 2
		if !explicit[sweepIntervalEnv] && cfg.Freshness < 3*cfg.SweepInterval {
			cfg.SweepInterval = cfg.Freshness / 3
		}
	}
	// A snapshot older than three sweeps is stale; overflow-guard the
	// multiplication the comparison depends on.
	if cfg.SweepInterval <= 0 || cfg.SweepInterval > time.Duration(1<<63-1)/3 || cfg.Freshness < 3*cfg.SweepInterval {
		crossCheckFailed(sweepIntervalEnv, freshnessEnv)
	}
	// Membership has to outlive freshness, or a publisher leaves the roster
	// before it is even considered stale.
	if cfg.MembershipTTL <= cfg.Freshness {
		crossCheckFailed(freshnessEnv, membershipTTLEnv)
	}
	if cfg.MembershipTTL > time.Duration(1<<63-1)/10 {
		crossCheckFailed(membershipTTLEnv)
	}
	// Telemetry now runs by default, so this error is the common shape of a
	// misconfiguration: the operator broke a variable and lost observation they
	// never asked for. The warn branch is reserved for a process that had already
	// been switched off.
	if len(coreInvalid) > 0 {
		if cfg.Enabled {
			cfg.Enabled = false
			slog.Error("stream telemetry disabled because configuration is invalid", "variables", strings.Join(coreInvalid, ","))
		} else {
			slog.Warn("ignoring invalid disabled stream telemetry configuration", "variables", strings.Join(coreInvalid, ","))
		}
	}
	if len(distributedInvalid) > 0 {
		// Now that the mode is derived, "off" and "would have stayed off" are
		// different outcomes: a rejected config that suppresses a merge the
		// deployment would otherwise have run is an error, while a process that
		// pinned the mode off loses nothing and only needs the noise recorded.
		if cfg.Distributed || !cfg.DistributedExplicit {
			cfg.Distributed = false
			slog.Error("stream telemetry distributed mode disabled because configuration is invalid", "variables", strings.Join(distributedInvalid, ","))
		} else {
			slog.Warn("ignoring invalid distributed stream telemetry configuration", "variables", strings.Join(distributedInvalid, ","))
		}
		// A rejected distributed configuration settles the mode as firmly as the
		// operator setting the variable would. Without this, the caller's "no
		// DISTRIBUTED variable means derive it from Redis" rule would turn
		// distributed mode straight back on with the configuration just refused.
		cfg.DistributedExplicit = true
	}
	return cfg
}

// ObservesFamily reports whether routes in this family are wrapped. It is read
// once per route at mount time, never on the hot path. An unset
// SILO_STREAM_TELEMETRY_FAMILIES observes every declared family; naming the
// variable narrows or kills observation from there.
func (c Config) ObservesFamily(family Family) bool {
	if len(c.Families) == 0 {
		return true
	}
	return c.Families[family]
}

// ObservedFamilies lists the observed families in a stable order, for the
// startup log that makes the resolved set visible.
func (c Config) ObservedFamilies() []string {
	if len(c.Families) == 0 {
		names := make([]string, 0, len(AllFamilies))
		for _, family := range AllFamilies {
			names = append(names, string(family))
		}
		sort.Strings(names)
		return names
	}
	names := make([]string, 0, len(c.Families))
	for family, observed := range c.Families {
		if observed {
			names = append(names, string(family))
		}
	}
	sort.Strings(names)
	return names
}

// parseFamilies decodes the comma-separated family list. An unrecognized name is
// a core-invalid setting rather than a distributed-only one: a typo that silently
// observed nothing would be worse than no telemetry at all.
func parseFamilies(value string) (map[Family]bool, bool) {
	families := make(map[Family]bool)
	for _, entry := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(entry))
		if name == "" {
			continue
		}
		family := Family(name)
		switch family {
		case FamilyNative, FamilyJellycompat, FamilyProxy, FamilyABS, FamilyTranscodeNode:
			families[family] = true
		default:
			return nil, false
		}
	}
	return families, true
}
