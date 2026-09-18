package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/remuxdb"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/language"
)

const virtualPlaybackPrefix = "virtual://"

const maxVirtualPlaybackStreams = 50

const (
	defaultMaxVirtualFailoverAttempts = 5
	virtualStartupBudget              = 60 * time.Second
	virtualProbeBudget                = 15 * time.Second
	maxVirtualPlaybackPrefetchFiles   = 2
	virtualPlaybackPrefetchBudget     = 20 * time.Second
	// virtualProbeFailureTTL is the base damper window and
	// virtualProbeFailureMaxTTL caps its exponential growth. A candidate that
	// just consumed the whole virtualProbeBudget without producing usable
	// metadata is not probed again for the current window; the resolver falls
	// through to the candidate-declared metadata instead. Each repeated failure
	// for the same key doubles the window (5m → 10m → 20m → 40m → 60m), so a
	// permanently unprobeable source is retried at most about once per hour. A
	// successful probe clears the marker entirely: this is a damper, not a
	// cache.
	virtualProbeFailureTTL    = 5 * time.Minute
	virtualProbeFailureMaxTTL = 60 * time.Minute
	// virtualProbeFailureRepeatThreshold is the number of consecutive failures
	// for one candidate before an unpin is allowed. A single transient failure
	// (outer budget fired, provider RPC timeout) leaves the candidate's health
	// unknown; only a repeat is treated as evidence it should stop steering
	// starts.
	virtualProbeFailureRepeatThreshold = 2
)

// virtualBackgroundProbeBudget bounds a background probe. Production uses the
// scanner's shared probe timeout so the inner probe and the caller that waits
// for it cannot drift; it is a var so tests can shrink the wait.
var virtualBackgroundProbeBudget = scanner.VirtualProbeTimeout

// virtualProbeFailureMark records the most recent failure for one candidate,
// the backoff window derived from the number of consecutive failures, and the
// running failure count that gates unpinning.
type virtualProbeFailureMark struct {
	ttl       time.Duration
	expiresAt time.Time
	failures  int
}

// virtualProbeFailureCache remembers the last failed probe per candidate so a
// replan does not pay the probe budget again. It is package-level because the
// handler is shared across requests and the marker is advisory: a mutex keeps
// concurrent starts safe, and the small map is bounded by the live candidate
// set (entries that have been idle past the maximum backoff are dropped).
// now is injectable for tests and defaults to time.Now.
type virtualProbeFailureCache struct {
	mu    sync.Mutex
	marks map[string]virtualProbeFailureMark
	now   func() time.Time
}

func (c *virtualProbeFailureCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *virtualProbeFailureCache) recent(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mark, ok := c.marks[key]
	if !ok {
		return false
	}
	if !c.clock().Before(mark.expiresAt) {
		delete(c.marks, key)
		return false
	}
	return true
}

func (c *virtualProbeFailureCache) mark(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.marks == nil {
		c.marks = make(map[string]virtualProbeFailureMark)
	}
	now := c.clock()
	// Prune markers that have been idle well past their window so a long-lived
	// process only retains failures still in backoff.
	for k, mark := range c.marks {
		if now.After(mark.expiresAt.Add(virtualProbeFailureMaxTTL)) {
			delete(c.marks, k)
		}
	}
	ttl := virtualProbeFailureTTL
	failures := 1
	if prev, ok := c.marks[key]; ok {
		failures = prev.failures + 1
		ttl = prev.ttl * 2
		if ttl > virtualProbeFailureMaxTTL {
			ttl = virtualProbeFailureMaxTTL
		}
	}
	c.marks[key] = virtualProbeFailureMark{ttl: ttl, expiresAt: now.Add(ttl), failures: failures}
}

// count returns the number of consecutive failures recorded for key, or 0 when
// no live marker exists. An expired marker is dropped on read, matching recent.
func (c *virtualProbeFailureCache) count(key string) int {
	if c == nil || key == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mark, ok := c.marks[key]
	if !ok {
		return 0
	}
	if !c.clock().Before(mark.expiresAt) {
		delete(c.marks, key)
		return 0
	}
	return mark.failures
}

// virtualProbeVerdictUnknown reports whether an error leaves the candidate's
// health unknown rather than condemning it: the caller's outer budget fired or
// the request was canceled while the probe was still running under its own
// timeout. The probe may yet complete in the cache, so this must not unpin.
func virtualProbeVerdictUnknown(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (c *virtualProbeFailureCache) clear(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.marks, key)
}

// virtualProbeFailures is the process-wide probe failure damper. Tests may
// clear entries directly.
var virtualProbeFailures = &virtualProbeFailureCache{marks: make(map[string]virtualProbeFailureMark)}

// virtualProbeFailureKey identifies a probe target across replans. The resolved
// stream URL carries rotating credentials, so the candidate's provider-neutral
// identity is the stable key. The candidate's own result= identity and the
// owner installation are part of the key: two candidates under one neutral
// path, or the same candidate owned by two installations, are independent probe
// targets and a failure for one must not damp the others.
func virtualProbeFailureKey(candidateURI string, ownerInstallationID int) string {
	candidateID := virtualResultCandidateID(candidateURI)
	if candidateID == "" {
		candidateID = candidateURI
	}
	return virtualPlaybackNeutralKey(candidateURI) + "\x00" + strconv.Itoa(ownerInstallationID) + "\x00" + candidateID
}

func (h *PlaybackHandler) PrefetchVirtualPlayback(ctx context.Context, files []*models.MediaFile, profileID string) {
	if h == nil || len(files) == 0 || profileID == "" {
		return
	}
	if h.VirtualPlaybackResolver == nil && h.VirtualPlaybackStreamLister == nil {
		return
	}
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return
	}
	if len(files) > maxVirtualPlaybackPrefetchFiles {
		files = files[:maxVirtualPlaybackPrefetchFiles]
	}
	prefetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), virtualPlaybackPrefetchBudget)
	go func() {
		defer cancel()
		for _, file := range files {
			if prefetchCtx.Err() != nil || file == nil || !isVirtualPlaybackFile(file) {
				continue
			}
			neutralURI := virtualPlaybackNeutralKey(file.FilePath)
			// Listing warms the shared resolver candidate cache and stores the
			// device-neutral candidate set in the handler cache, so the first
			// click skips the provider round-trip. The resolve below is then
			// served from the resolver cache. Both are best-effort.
			h.warmVirtualPlaybackListing(prefetchCtx, file, neutralURI, userID, profileID)
			if h.VirtualPlaybackResolver != nil {
				_, _ = h.VirtualPlaybackResolver.ResolveVirtualPlayback(prefetchCtx, neutralURI, userID, profileID, file.VirtualOwnerInstallationID)
			}
		}
	}()
}

// warmVirtualPlaybackListing lists provider candidates once and stores the
// filtered, device-neutral set in the best-result cache under the neutral key
// (no device fingerprint). A later start for any device falls back to that
// entry, ranks it for the requesting device, and skips the provider list. It is
// best-effort: the caller owns the prefetch budget and this function ignores
// provider errors. Only the metadata cache is warmed here; the sticky pin is
// deliberately not set, because a candidate that has never delivered bytes is
// not yet evidence it should steer starts.
func (h *PlaybackHandler) warmVirtualPlaybackListing(ctx context.Context, file *models.MediaFile, neutralURI string, userID int, profileID string) {
	if h == nil || file == nil || neutralURI == "" {
		return
	}
	if h.VirtualPlaybackStreamLister == nil || h.BestResultCache == nil {
		return
	}
	streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(ctx, neutralURI, userID, profileID, file.VirtualOwnerInstallationID)
	if err != nil || len(streams) == 0 {
		return
	}
	if len(streams) > maxVirtualPlaybackStreams {
		streams = streams[:maxVirtualPlaybackStreams]
	}
	// Filter against the neutral row, exactly as a start would, so the cached
	// set is the same one the start path would persist.
	base := *file
	base.FilePath = neutralURI
	filtered := filterVirtualPlaybackStreams(&base, streams)
	if len(filtered) == 0 {
		return
	}
	h.BestResultCache.setWithDetails(
		bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID),
		file.ContentID, neutralURI, file.VirtualOwnerInstallationID,
		filtered, time.Now(),
	)
}

func (h *PlaybackHandler) maxVirtualFailoverAttempts(ctx context.Context) int {
	if h != nil && h.SettingsRepo != nil {
		if raw, err := h.SettingsRepo.Get(ctx, "playback.max_virtual_failover_attempts"); err == nil && strings.TrimSpace(raw) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v > 0 {
				return v
			}
		}
	}
	if h != nil && h.PlaybackConfig != nil {
		if v := h.playbackConfig().MaxVirtualFailoverAttempts; v > 0 {
			return v
		}
	}
	return defaultMaxVirtualFailoverAttempts
}

const (
	defaultBestResultCacheTTL     = 30 * time.Minute
	defaultBestResultCacheEntries = 512
)

// VirtualBestResultCache remembers which result= URI worked for a content+profile
// pair. On replay it skips the list+resolve+probe path entirely, jumping
// directly to the known-good provider-neutral URI.
type VirtualBestResultCache struct {
	mu         sync.RWMutex
	entries    map[string]bestResultCacheEntry
	ttl        time.Duration
	maxEntries int
}

type bestResultCacheEntry struct {
	contentID           string
	neutralURI          string
	ownerInstallationID int
	streams             []VirtualPlaybackStream
	expiresAt           time.Time
}

// NewVirtualBestResultCache returns an initialized cache. Zero or negative ttl
// and maxEntries pick safe defaults.
func NewVirtualBestResultCache(ttl time.Duration, maxEntries int) *VirtualBestResultCache {
	if ttl <= 0 {
		ttl = defaultBestResultCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultBestResultCacheEntries
	}
	return &VirtualBestResultCache{
		entries:    make(map[string]bestResultCacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (c *VirtualBestResultCache) get(key string, now time.Time) []VirtualPlaybackStream {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || now.After(entry.expiresAt) {
		return nil
	}
	return append([]VirtualPlaybackStream(nil), entry.streams...)
}

// Clear drops every cached result. Called on plugin lifecycle changes when
// provider configurations may have changed and cached result= URIs are
// likely stale.
func (c *VirtualBestResultCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

func (c *VirtualBestResultCache) RemoveCandidate(key string, candURI string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	candNeutral := virtualPlaybackNeutralKey(candURI)
	filtered := make([]VirtualPlaybackStream, 0, len(entry.streams))
	for _, s := range entry.streams {
		if s.URI == candURI || (candNeutral != "" && s.URI == candNeutral) {
			continue
		}
		filtered = append(filtered, s)
	}
	if len(filtered) == 0 {
		delete(c.entries, key)
		return
	}
	entry.streams = filtered
	c.entries[key] = entry
}

func (c *VirtualBestResultCache) RemoveCandidateForContent(contentID, neutralURI string, ownerInstallationID int, candURI string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	candNeutral := virtualPlaybackNeutralKey(candURI)
	targetCandID := ""
	if parsed, err := url.Parse(candURI); err == nil {
		targetCandID = parsed.Query().Get("result")
	}
	for k, entry := range c.entries {
		if (contentID == "" || entry.contentID == contentID) &&
			(neutralURI == "" || entry.neutralURI == neutralURI) &&
			entry.ownerInstallationID == ownerInstallationID {
			filtered := make([]VirtualPlaybackStream, 0, len(entry.streams))
			for _, s := range entry.streams {
				if s.URI == candURI || (candNeutral != "" && s.URI == candNeutral) || (targetCandID != "" && s.ID == targetCandID) {
					continue
				}
				filtered = append(filtered, s)
			}
			if len(filtered) == 0 {
				delete(c.entries, k)
			} else {
				entry.streams = filtered
				c.entries[k] = entry
			}
		}
	}
}

func (c *VirtualBestResultCache) set(key string, streams []VirtualPlaybackStream, now time.Time) {
	c.setWithDetails(key, "", "", 0, streams, now)
}

func (c *VirtualBestResultCache) setWithDetails(key, contentID, neutralURI string, ownerInstallationID int, streams []VirtualPlaybackStream, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= c.maxEntries {
		var oldestKey string
		var oldest time.Time
		for k, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = bestResultCacheEntry{
		contentID:           contentID,
		neutralURI:          neutralURI,
		ownerInstallationID: ownerInstallationID,
		streams:             streams,
		expiresAt:           now.Add(c.ttl),
	}
}

// bestResultCacheKey builds a deterministic key from the content_id, neutral
// URI (without result=), and owner installation ID, with an optional device fingerprint.
func bestResultCacheKey(contentID, neutralURI string, ownerInstallationID int, deviceFingerprint ...string) string {
	raw := contentID + "\x00" + neutralURI + "\x00" + strconv.Itoa(ownerInstallationID)
	if len(deviceFingerprint) > 0 && deviceFingerprint[0] != "" {
		raw += "\x00" + deviceFingerprint[0]
	}
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:16])
}

type VirtualPlaybackResolver interface {
	ResolveVirtualPlayback(ctx context.Context, virtualPath string, userID int, profileID string, ownerInstallationID int) (string, error)
}

type VirtualPlaybackResolverFunc func(context.Context, string, int, string, int) (string, error)

func (f VirtualPlaybackResolverFunc) ResolveVirtualPlayback(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
	return f(ctx, path, userID, profileID, ownerInstallationID)
}

// VirtualPlaybackStream is the provider-neutral candidate shape used by the
// just-in-time picker. Implementations must never expose provider URLs here.
type VirtualPlaybackStream struct {
	ID                  string            `json:"id"`
	Label               string            `json:"label"`
	URI                 string            `json:"uri"`
	Resolution          string            `json:"resolution,omitempty"`
	CodecVideo          string            `json:"codec_video,omitempty"`
	CodecAudio          string            `json:"codec_audio,omitempty"`
	HDR                 string            `json:"hdr,omitempty"`
	SourceType          string            `json:"source_type,omitempty"`
	FileSize            int64             `json:"file_size,omitempty"`
	Container           string            `json:"container,omitempty"`
	Bitrate             int               `json:"bitrate,omitempty"`
	FrameRate           string            `json:"frame_rate,omitempty"`
	AudioLanguages      []string          `json:"audio_languages,omitempty"`
	SubtitleLanguages   []string          `json:"subtitle_languages,omitempty"`
	HasAtmos            bool              `json:"has_atmos,omitempty"`
	QualityScore        int               `json:"quality_score,omitempty"`
	RequestHeaders      map[string]string `json:"-"`
	OwnerInstallationID int               `json:"-"`
	Visible             bool              `json:"-"`
	VisibilitySpecified bool              `json:"-"`
}

// Get* accessors satisfy plugins.VirtualStreamMetadata so the shared device
// ranker can score both this type and the plugin-layer candidate shape.
func (s VirtualPlaybackStream) GetCodecVideo() string { return s.CodecVideo }
func (s VirtualPlaybackStream) GetCodecAudio() string { return s.CodecAudio }
func (s VirtualPlaybackStream) GetHDR() string        { return s.HDR }
func (s VirtualPlaybackStream) GetContainer() string  { return s.Container }
func (s VirtualPlaybackStream) GetResolution() string { return s.Resolution }
func (s VirtualPlaybackStream) GetHasAtmos() bool     { return s.HasAtmos }
func (s VirtualPlaybackStream) GetQualityScore() int  { return s.QualityScore }

type VirtualPlaybackStreamLister interface {
	ListVirtualPlaybackStreams(ctx context.Context, virtualPath string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error)
}

type VirtualPlaybackStreamListerFunc func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error)

func (f VirtualPlaybackStreamListerFunc) ListVirtualPlaybackStreams(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
	return f(ctx, path, userID, profileID, ownerInstallationID)
}

// VirtualPlaybackStreamSink persists JIT candidates as selectable virtual
// files.
type VirtualPlaybackStreamSink func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error

type ProbeProvenance string

const (
	ProbeProvenanceDeclared ProbeProvenance = "declared"
	ProbeProvenancePending  ProbeProvenance = "pending"
	ProbeProvenanceVerified ProbeProvenance = "verified"
	ProbeProvenanceFailed   ProbeProvenance = "failed"
)

type resolvedVirtualPlaybackSource struct {
	URL               string
	URI               string
	OwnerID           int
	File              *models.MediaFile
	ProbeSucceeded    bool
	Provenance        ProbeProvenance
	AppliedRemux      bool
	ResolutionAssumed bool
}

// shouldListVirtualPlaybackCandidates reports whether the resolver must ask
// the provider for a candidate list. A pinned result= URI with complete
// probed evidence normally skips the round-trip. forceRelist overrides that so
// an explicit retry of an unavailable version sees the provider's current list
// rather than re-trying the stale pinned candidate.
func shouldListVirtualPlaybackCandidates(noResult, needsCandidateMetadata, forceRelist bool) bool {
	return noResult || needsCandidateMetadata || forceRelist
}

// virtualResolveTrace accumulates the wall-clock cost of each phase inside
// resolveVirtualPlaybackSource. The protocol v3 start timings collapse all of
// this into a single file_load_probe mark; these attrs split it into the
// provider candidate list, the RemuxDB match, the provider resolve loop, the
// synchronous probe, and the stale-source re-list fallback, so a cold-start
// attribution can name the dominant stage instead of guessing. It is
// observational only and never gates control flow.
//
// A stage that did not run must be distinguishable from one that ran in under
// a millisecond: every stage carries a <name>_ran boolean, and its <name>_ms
// duration is omitted entirely unless it ran. A plain 0 duration can therefore
// only mean "ran in under 1 ms", never "skipped". total_ms is the sum of the
// durations that ran, so the displayed stage fields add up to it exactly;
// elapsed_ms is the wall-clock time of the whole resolve.
type virtualResolveTrace struct {
	started time.Time

	list    time.Duration
	listRan bool

	remux    time.Duration
	remuxRan bool

	resolve    time.Duration
	resolveRan bool

	probe    time.Duration
	probeRan bool

	fallback    time.Duration
	fallbackRan bool

	fastPath   bool
	cached     bool
	listed     bool
	candidates int
}

// totalMS is the sum of the stage durations that ran, using the same rounded
// millisecond values the per-stage fields report, so total_ms equals
// list_ms+remux_ms+resolve_ms+probe_ms+fallback_ms exactly.
func (t *virtualResolveTrace) totalMS() int64 {
	return t.list.Milliseconds() +
		t.remux.Milliseconds() +
		t.resolve.Milliseconds() +
		t.probe.Milliseconds() +
		t.fallback.Milliseconds()
}

// fields returns the timing shape in a stable order. It is split out from log
// so tests can assert the "ran" verdict per stage without capturing the logger.
func (t *virtualResolveTrace) fields() []any {
	attrs := []any{
		"elapsed_ms", time.Since(t.started).Milliseconds(),
		"total_ms", t.totalMS(), //nolint:goconst // log attribute key/value, kept inline for readability.
		"candidates", t.candidates, //nolint:goconst // log attribute key/value, kept inline for readability.
		"cache_hit", t.cached,
		"listed", t.listed,
		"fast_path", t.fastPath,
	}
	for _, stage := range [...]struct {
		name string
		ran  bool
		d    time.Duration
	}{
		{"list", t.listRan, t.list},          //nolint:goconst // log attribute key/value, kept inline for readability.
		{"remux", t.remuxRan, t.remux},       //nolint:goconst // log attribute key/value, kept inline for readability.
		{"resolve", t.resolveRan, t.resolve}, //nolint:goconst // log attribute key/value, kept inline for readability.
		{"probe", t.probeRan, t.probe},
		{"fallback", t.fallbackRan, t.fallback},
	} {
		attrs = append(attrs, stage.name+"_ran", stage.ran)
		if stage.ran {
			attrs = append(attrs, stage.name+"_ms", stage.d.Milliseconds())
		}
	}
	return attrs
}

func (t *virtualResolveTrace) log(ctx context.Context, file *models.MediaFile) {
	if t == nil || file == nil {
		return
	}
	attrs := []any{logComponentKey, "api", "content_id", file.ContentID} //nolint:goconst // log attribute key/value, kept inline for readability.
	attrs = append(attrs, t.fields()...)
	slog.InfoContext(ctx, "virtual resolve timing", attrs...)
}

// resolveVirtualPlaybackSource chooses a ranked provider-neutral result,
// resolves it, and probes it before planning. A result URI is bound to the
// session so later Range, seek, subtitle, and transcode requests cannot silently
// switch to a technically different candidate under the original plan.
// resolveVirtualPlaybackSource resolves the provider URL for a virtual file
// and, when evidence is incomplete, probes it. deferProbe allows the start
// path to skip the synchronous probe and respond immediately on the
// deferred-metadata HLS route (see playback.DeferVirtualPlaybackMetadataV3);
// the resolved source is then probed in the background so the next play has
// complete evidence. Restart/track-change paths pass false because they need
// probed track inventory to remap selections.
//
// excludedCandidateIDs and preferredCandidateID are threaded into the detailed
// resolver so a replan can exclude the failed candidate and prefer the
// session-bound release instead of letting re-ranking drift to a different
// provider candidate. qualityPreference (already normalized) and
// bandwidthCapKbps steer the post-device-ranking reorder toward native
// lower-resolution candidates when the client asked for a fixed rung.
//
// forceRelist forces a fresh provider listing for an explicitly re-selected
// version that the catalog marked unavailable, instead of replaying the cached
// or pinned candidate. It also bypasses the detailed resolver's candidate cache
// so the retry sees the provider's current list. The pinned candidate is kept
// at index 0 while it is still listed; once the pin is gone the fresh list
// takes over.
//
// allowFailedCandidate permits re-selecting a catalog row stamped failed_at. It
// defaults to false so an auto selection always skips a known-bad row, even when
// the row already carries a concrete result= identity (the adopted-candidate
// case). It is true only for an explicit user retry or a forced relink, and for
// internal paths (replan rehydration) that resolve a session-bound candidate
// whose deadness is conveyed by excludedCandidateIDs instead of the async stamp.
// It is variadic so the many existing callers keep their positional signature;
// production callers pass the value explicitly.
func (h *PlaybackHandler) resolveVirtualPlaybackSource(r *http.Request, file *models.MediaFile, profileID string, deferProbe bool, excludedCandidateIDs []string, preferredCandidateID string, qualityPreference string, bandwidthCapKbps int, forceRelist bool, allowFailedCandidate ...bool) (resolvedVirtualPlaybackSource, error) {
	allowFailed := false
	if len(allowFailedCandidate) > 0 {
		allowFailed = allowFailedCandidate[0]
	}
	if !isVirtualPlaybackFile(file) {
		return resolvedVirtualPlaybackSource{File: file}, nil
	}
	if h.VirtualPlaybackResolver == nil {
		return resolvedVirtualPlaybackSource{}, errors.New("virtual playback resolver is not configured")
	}
	// Split file_load_probe into its provider phases so a cold-start
	// attribution is measured, not guessed. The deferred log runs on every
	// return below, including the fast paths.
	trace := &virtualResolveTrace{started: time.Now()}
	defer trace.log(r.Context(), file)
	userID := apimw.GetUserID(r.Context())
	parsed, _ := url.Parse(file.FilePath)
	candidates := []VirtualPlaybackStream{{
		URI: file.FilePath, OwnerInstallationID: file.VirtualOwnerInstallationID,
		Resolution: file.Resolution, CodecVideo: file.CodecVideo, CodecAudio: file.CodecAudio,
		HDR: mediaFileHDRString(file),
	}}
	noResult := parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) == ""
	needsCandidateMetadata := !completeVirtualVideoEvidenceV3(file) || !completeVirtualAudioEvidenceV3(file) || !completeVirtualContainerEvidenceV3(file)
	deviceCaps, hasCaps := h.requestDeviceCapabilities(r)
	fingerprint := ""
	if hasCaps {
		fingerprint = deviceCaps.Fingerprint()
	}
	stickyKey := bestResultCacheKey(file.ContentID, virtualPlaybackNeutralKey(file.FilePath), file.VirtualOwnerInstallationID, fingerprint)
	pinnedURI := h.peekVirtualSticky(stickyKey)
	// Check the best-result cache before listing candidates. A previous
	// successful play of this content may have a cached result= URI that
	// lets us skip the entire list+resolve+probe sequence on replay.
	cachedListing := false
	if noResult && h.BestResultCache != nil {
		neutralURI := virtualPlaybackNeutralKey(file.FilePath)
		cacheKey := bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID, fingerprint)
		cached := h.BestResultCache.get(cacheKey, time.Now())
		if len(cached) == 0 && fingerprint != "" {
			// A listing warmed without a device fingerprint (the bounded
			// prefetch path) is still the filtered, device-neutral candidate
			// set; only the ranking below is device-specific, so fall back to
			// it rather than paying the provider list again on this device's
			// first click.
			cached = h.BestResultCache.get(bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID), time.Now())
		}
		if len(cached) > 0 {
			// Cache holds the filtered, device-neutral candidate list; rank it
			// for this device so a TV and a phone pick their own best stream
			// without another provider round-trip.
			candidates, _ = h.rankVirtualCandidatesForDevice(r, cached)
			candidates = reorderVirtualCandidatesForQuality(candidates, qualityPreference, bandwidthCapKbps)
			noResult = false // treated as if file already had a result=
			cachedListing = len(candidates) > 0
			trace.cached = cachedListing
		}
	}
	// A valid cached listing already carries the candidate metadata the list
	// call exists to fetch, so it does not need the provider round-trip even
	// when the stored row has no probe evidence yet. Only a cold cache or an
	// explicit relist forces the list.
	//
	// A failed requested row or a pending exclusion additionally requires the
	// provider list even when the row has complete probe evidence: the fast
	// paths are gated off for those rows, so without a list the candidate set
	// would contain only the rejected row and rotation could never find a
	// sibling.
	exclusionPending := len(excludedCandidateIDs) > 0
	requestedRowUnusable := !allowFailed && file.FailedAt != nil
	if (shouldListVirtualPlaybackCandidates(noResult, needsCandidateMetadata && !cachedListing, forceRelist) ||
		((exclusionPending || requestedRowUnusable) && !cachedListing)) && h.VirtualPlaybackStreamLister != nil {
		trace.listed = true
		trace.listRan = true
		listStart := time.Now()
		// Candidate listing is part of the startup critical path. Keep it
		// bounded so the first-byte SLA cannot be defeated before resolution.
		listCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
			listCtx, file.FilePath, userID, profileID, file.VirtualOwnerInstallationID,
		)
		if err == nil && len(streams) > 0 {
			if len(streams) > maxVirtualPlaybackStreams {
				streams = streams[:maxVirtualPlaybackStreams]
			}
			// A selected result= URI is still an active catalog row referenced by
			// the playback attempt. Refresh metadata in memory, but do not replace
			// the candidate set while this request is using that row.
			if noResult && h.VirtualPlaybackStreamSink != nil {
				visible := visibleVirtualPlaybackStreams(streams)
				sinkFn := h.VirtualPlaybackStreamSink
				sinkFile := *file
				go func() {
					sinkCtx, cancel := context.WithTimeout(context.WithoutCancel(listCtx), 15*time.Second)
					defer cancel()
					_ = sinkFn(sinkCtx, &sinkFile, visible)
				}()
			}
			filtered := filterVirtualPlaybackStreams(file, streams)
			if h.BestResultCache != nil && len(filtered) > 0 {
				neutralURI := virtualPlaybackNeutralKey(file.FilePath)
				cacheKey := bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID, fingerprint)
				h.BestResultCache.setWithDetails(cacheKey, file.ContentID, neutralURI, file.VirtualOwnerInstallationID, filtered, time.Now())
			}
			if noResult {
				if len(filtered) > 0 {
					candidates, _ = h.rankVirtualCandidatesForDevice(r, filtered)
					candidates = reorderVirtualCandidatesForQuality(candidates, qualityPreference, bandwidthCapKbps)
				}
			} else {
				// Explicit candidate selected. Find it in streams to enrich its metadata,
				// and keep it at index 0 without overwriting it with device re-ranking.
				resultID := ""
				if parsed != nil {
					resultID = strings.TrimSpace(parsed.Query().Get("result"))
				}
				pinFound := false
				for _, s := range streams {
					if s.URI == file.FilePath || (resultID != "" && s.ID == resultID) {
						candidates[0] = s
						pinFound = true
						break
					}
				}
				if len(filtered) > 0 {
					rankedAlternatives, _ := h.rankVirtualCandidatesForDevice(r, filtered)
					rankedAlternatives = reorderVirtualCandidatesForQuality(rankedAlternatives, qualityPreference, bandwidthCapKbps)
					if forceRelist && !pinFound {
						// The forced fresh listing no longer carries the pinned
						// version. Drop the stale pin instead of retrying a
						// candidate the provider stopped listing; the ranked
						// fresh list takes over and rotation/stale-fallback can
						// work from it.
						candidates = rankedAlternatives
					} else {
						candidates = append([]VirtualPlaybackStream{candidates[0]}, rankedAlternatives...)
					}
				}
			}
		}
		cancel()
		trace.list = time.Since(listStart)
	}
	maxAttempts := h.maxVirtualFailoverAttempts(r.Context())
	if noResult {
		candidates = h.applyVirtualStickyPin(stickyKey, pinnedURI, candidates, deviceCaps)
	}
	remuxMatches := map[string]remuxdb.Evidence{}
	remuxEnabled := false
	if needsCandidateMetadata && len(candidates) > 0 {
		trace.remuxRan = true
		remuxStart := time.Now()
		remuxMatches, remuxEnabled = h.matchRemuxDBCandidates(r.Context(), file, candidates)
		trace.remux = time.Since(remuxStart)
	}
	if len(candidates) > maxAttempts {
		candidates = candidates[:maxAttempts]
	}
	trace.candidates = len(candidates)
	attemptCtx, cancel := context.WithTimeout(r.Context(), virtualStartupBudget)
	defer cancel()

	// persistedResultURI is true when the catalog row already points at an
	// adopted provider-neutral candidate rather than the neutral virtual path.
	persistedResultURI := parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) != ""

	// fastPathHit records that resolveAndProbe returned the repeat-play fast
	// path so the candidate loop can return it immediately instead of treating
	// a deferred (ProbeSucceeded=false) pinned result as a failed candidate.
	fastPathHit := false

	resolveAndProbe := func(i int, cand VirtualPlaybackStream) (*resolvedVirtualPlaybackSource, error) {
		// Evidence is keyed by the pre-resolution candidate URI: the detailed
		// resolver below may rewrite cand.URI, but the match map was populated
		// from the original candidate list.
		origKey := remuxEvidenceKey(cand.URI)
		oid := cand.OwnerInstallationID
		if oid <= 0 {
			oid = file.VirtualOwnerInstallationID
		}
		// Repeat-play fast path. When the requested row already owns a pinned
		// or adopted provider-neutral candidate, complete probed evidence, and
		// a probe stamp, the start path does not need the provider URL: the
		// stream relay re-resolves at serve time and owns first-byte failover
		// (see stream.go and playback_transport.go), so a replay costs DB +
		// tokens only. Gated on the deferred start branch; the synchronous
		// replan/alternate/stale-fallback callers (deferProbe=false) still
		// resolve because they need probed track inventory to remap selections.
		//
		// The candidate must be the one the row actually points at. A
		// BestResultCache hit clears noResult for a neutral row and can re-rank a
		// different release to index 0; binding that release here would pin it
		// and copy the row's probed inventory onto a candidate that never
		// produced it.
		// The fast path skips the provider resolve, so it must not bypass the
		// caller's exclusion list or the known-bad stamp: with an exclusion
		// pending (decode rotation) or a failed row, fall through to the
		// resolve path, which honors both.
		if deferProbe && !forceRelist && !noResult &&
			len(excludedCandidateIDs) == 0 && (allowFailed || file.FailedAt == nil) &&
			h.VirtualMediaDetailedResolver != nil &&
			((persistedResultURI && cand.URI == file.FilePath) || (pinnedURI != "" && cand.URI == pinnedURI)) &&
			file.ProbeUpdatedAt != nil &&
			completeVirtualVideoEvidenceV3(file) &&
			completeVirtualAudioEvidenceV3(file) &&
			completeVirtualContainerEvidenceV3(file) {
			fastPathHit = true
			trace.fastPath = true
			transient := *file
			transient.FilePath = cand.URI
			transient.VirtualOwnerInstallationID = oid
			h.pinVirtualSticky(stickyKey, cand.URI)
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: "", URI: cand.URI, OwnerID: oid, File: &transient,
				ProbeSucceeded: false, Provenance: ProbeProvenancePending,
			}, nil
		}
		// Optimistic start within the delivery grace. When the P0 gate above
		// cannot apply because the probe stamp is missing or the stored
		// evidence is incomplete, but the row already delivered bytes inside
		// the scanner's delivery grace, the candidate is still known-good.
		// Return the persisted URI now and revalidate in the background: the
		// serve relay re-resolves at serve time and owns first-byte failover
		// (see stream.go and playback_transport.go), while the background chain
		// resolves and probes so the next start takes the P0 fast path. Gated
		// on a real pinned/adopted candidate plus a configured resolver and
		// prober; no pin or no delivery grace keeps the synchronous resolve.
		if deferProbe && !forceRelist && !noResult &&
			len(excludedCandidateIDs) == 0 && (allowFailed || file.FailedAt == nil) &&
			(persistedResultURI || pinnedURI != "") &&
			(h.VirtualMediaDetailedResolver != nil || h.VirtualPlaybackResolver != nil) &&
			(h.VirtualPlaybackSourceProber != nil || h.VirtualPlaybackSourceProberWithHeaders != nil) &&
			virtualDeliveredWithinGrace(file) {
			fastPathHit = true
			trace.fastPath = true
			transient := *file
			transient.FilePath = cand.URI
			transient.VirtualOwnerInstallationID = oid
			h.pinVirtualSticky(stickyKey, cand.URI)
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			h.revalidateVirtualCandidateBackground(r.Context(), stickyKey, file, cand, oid, userID, profileID, transient.ID)
			return &resolvedVirtualPlaybackSource{
				URL: "", URI: cand.URI, OwnerID: oid, File: &transient,
				ProbeSucceeded: false, Provenance: ProbeProvenancePending,
			}, nil
		}
		var streamURL string
		var resolveErr error
		trace.resolveRan = true
		resolveStart := time.Now()
		if h.VirtualMediaDetailedResolver != nil {
			res, err := h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
				attemptCtx, cand.URI, oid, userID, profileID, forceRelist, excludedCandidateIDs, preferredCandidateID,
			)
			if err == nil {
				streamURL = res.URL
				cand.RequestHeaders = cloneHeaderMap(res.RequestHeaders)
				if res.URI != "" {
					cand.URI = res.URI
				}
				if res.CandidateID != "" {
					cand.ID = res.CandidateID
				}
				// The provider that answered is the runtime owner for a
				// legacy file row whose stored owner is 0. Adopt it so the
				// transient file, the session, and every downstream recipe
				// carry the effective owner instead of 0.
				if res.OwnerID > 0 {
					oid = res.OwnerID
				}
			} else {
				resolveErr = err
			}
		} else if h.VirtualPlaybackResolver != nil {
			streamURL, resolveErr = h.VirtualPlaybackResolver.ResolveVirtualPlayback(
				attemptCtx, cand.URI, userID, profileID, oid,
			)
			cand.RequestHeaders = nil
		} else {
			resolveErr = errors.New("virtual playback resolver is not configured")
		}
		trace.resolve += time.Since(resolveStart)
		if resolveErr != nil {
			return nil, resolveErr
		}
		// URL syntax and SSRF validation happen in the provider service and
		// again when the relay opens the source. Do not perform a blocking body
		// fetch here; the provider may legitimately take time before its first
		// byte, and that would consume the entire startup budget.

		transient := *file
		transient.FilePath = cand.URI
		transient.VirtualOwnerInstallationID = oid
		dbFile := (*models.MediaFile)(nil)
		if h.VirtualFileLookup != nil {
			dbFile, _ = h.VirtualFileLookup(attemptCtx, cand.URI)
		}
		if (dbFile == nil || dbFile.ID <= 0) && h.VirtualCandidateFileLookup != nil {
			dbFile, _ = h.VirtualCandidateFileLookup(attemptCtx, virtualPlaybackNeutralKey(cand.URI), file.ContentID, file.EpisodeID, oid)
		}
		if dbFile != nil && dbFile.ID > 0 {
			// Auto-pick skips candidates whose catalog row is marked failed
			// (a transport produced no bytes, or the decoder rejected the
			// source, on a prior attempt). An explicit selection and a forced
			// relink allow a manual retry; a decode-driven rotation carries its
			// exclusion explicitly so it never depends on the async stamp.
			if !allowFailed && dbFile.FailedAt != nil {
				return nil, fmt.Errorf("candidate %s is marked failed", cand.URI)
			}
			transient = *dbFile
			transient.FilePath = cand.URI
			transient.VirtualOwnerInstallationID = oid
		}
		if transient.Duration <= 0 {
			if file.Duration > 0 {
				transient.Duration = file.Duration
			} else if file.EpisodeID != "" && h.EpisodeLookup != nil {
				if ep, err := h.EpisodeLookup.GetByID(attemptCtx, file.EpisodeID); err == nil && ep != nil && ep.Runtime > 0 {
					transient.Duration = ep.Runtime * 60
				}
			} else if file.ContentID != "" && h.ItemLookup != nil {
				if item, err := h.ItemLookup.GetByID(attemptCtx, file.ContentID); err == nil && item != nil && item.Runtime > 0 {
					transient.Duration = item.Runtime * 60
				}
			}
		}
		localResolution := transient.Resolution
		localCodec := transient.CodecVideo
		hasCompleteVideoEvidence := completeVirtualVideoEvidenceV3(&transient)
		hasCompleteAudioEvidence := completeVirtualAudioEvidenceV3(&transient)
		hasCompleteContainerEvidence := completeVirtualContainerEvidenceV3(&transient)
		skipProbe := hasCompleteVideoEvidence && hasCompleteAudioEvidence && hasCompleteContainerEvidence
		// A row that has never been probed carries NULL tracks and no probe
		// stamp. Candidate-declared metadata can synthesize complete-looking
		// evidence for the immediate plan, but it must not short-circuit the
		// real probe that stamps the row and persists its true track inventory.
		// Capture this before the candidate merge so a candidate-declared
		// inventory never counts as stored evidence.
		storedProbeMissing := transient.ProbeUpdatedAt == nil
		if !skipProbe && cand.CodecVideo != "" && cand.Resolution != "" && cand.CodecAudio != "" && canSkipProbeForContainer(cand.Container) {
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			hasCompleteVideoEvidence = completeVirtualVideoEvidenceV3(&transient)
			hasCompleteAudioEvidence = completeVirtualAudioEvidenceV3(&transient)
			hasCompleteContainerEvidence = completeVirtualContainerEvidenceV3(&transient)
			skipProbe = hasCompleteVideoEvidence && hasCompleteAudioEvidence && hasCompleteContainerEvidence
		}
		if skipProbe && !storedProbeMissing {
			h.pinVirtualSticky(stickyKey, cand.URI)
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: true, Provenance: ProbeProvenanceVerified,
			}, nil
		}
		ev, _ := remuxMatches[origKey]
		appliedRemux := false
		if backfilled := applyRemuxDBEvidence(&transient, remuxMatches, origKey); backfilled != &transient {
			transient = *backfilled
			appliedRemux = true
		}
		allowDefer := allowDeferredProbe(
			deferProbe,
			remuxEnabled,
			ev.MatchMethod,
			localResolution,
			cand.Resolution,
			localCodec,
			cand.CodecVideo,
			ev.Resolution,
			ev.CodecVideo,
		)
		if allowDefer {
			h.pinVirtualSticky(stickyKey, cand.URI)
			// Resolution precedence: stored evidence wins; otherwise adopt
			// the candidate's declared label; only when both are absent is
			// the 1080p baseline assumed. Only the last case marks
			// ResolutionAssumed, so a declared 2160p is never clobbered.
			if transient.Resolution == "" {
				transient.Resolution = cand.Resolution
			}
			resolutionAssumed := transient.Resolution == ""
			if resolutionAssumed {
				transient.Resolution = transcodeResolution1080p
			}
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			if h.VirtualPlaybackSourceProber != nil || h.VirtualPlaybackSourceProberWithHeaders != nil {
				probeKey := virtualProbeFailureKey(cand.URI, oid)
				probeTransient := cloneVirtualProbeTransient(transient)
				// Zero the duration so the background probe measures the
				// empirical duration instead of inheriting the catalog value.
				probeTransient.Duration = 0
				if virtualProbeFailures.recent(probeKey) {
					// A fresh failure already consumed the probe budget. The
					// inner probe may still have completed and landed in the
					// cache, so try a cache-only recovery before falling back
					// to declared metadata; otherwise the row stays unprobed
					// until the damper lapses.
					h.recoverVirtualProbeFromCache(r.Context(), file, streamURL, probeTransient, cand, oid)
					return &resolvedVirtualPlaybackSource{
						URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: false, Provenance: ProbeProvenanceDeclared, ResolutionAssumed: resolutionAssumed,
					}, nil
				}
				probeCand := cand
				expectedRuntimeMinutes := h.virtualExpectedRuntimeMinutes(r.Context(), file)
				go func() {
					// The start path may outlive the request (the client can
					// disconnect while the probe completes), so it keeps a
					// WithoutCancel context. The goroutine starts after the
					// synchronous provider resolve returned, so the fetch time
					// does not consume this budget.
					bgCtx, bgCancel := context.WithTimeout(context.WithoutCancel(r.Context()), virtualBackgroundProbeBudget)
					defer bgCancel()
					h.probeVirtualSourceAndPersist(bgCtx, stickyKey, file, streamURL, probeTransient, probeCand, expectedRuntimeMinutes, oid)
				}()
			}
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: false, Provenance: ProbeProvenancePending, AppliedRemux: appliedRemux, ResolutionAssumed: resolutionAssumed,
			}, nil
		}
		if h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil {
			// Resolution precedence: stored evidence wins; otherwise adopt
			// the candidate's declared label; only when both are absent is
			// the 1080p baseline assumed. Only the last case marks
			// ResolutionAssumed, so a declared 2160p is never clobbered.
			if transient.Resolution == "" {
				transient.Resolution = cand.Resolution
			}
			resolutionAssumed := transient.Resolution == ""
			if resolutionAssumed {
				transient.Resolution = transcodeResolution1080p
			}
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: false, Provenance: ProbeProvenanceDeclared, AppliedRemux: appliedRemux, ResolutionAssumed: resolutionAssumed,
			}, nil
		}
		probeKey := virtualProbeFailureKey(cand.URI, oid)
		declaredFallback := func() (*resolvedVirtualPlaybackSource, error) {
			// Resolution precedence: stored evidence wins; otherwise adopt
			// the candidate's declared label; only when both are absent is
			// the 1080p baseline assumed. Only the last case marks
			// ResolutionAssumed, so a declared 2160p is never clobbered.
			if transient.Resolution == "" {
				transient.Resolution = cand.Resolution
			}
			resolutionAssumed := transient.Resolution == ""
			if resolutionAssumed {
				transient.Resolution = transcodeResolution1080p
			}
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: false, Provenance: ProbeProvenanceFailed, AppliedRemux: appliedRemux, ResolutionAssumed: resolutionAssumed,
			}, nil
		}
		if virtualProbeFailures.recent(probeKey) {
			// A recent probe failure already consumed the probe budget. A
			// completed inner probe may still be cached; recover it before
			// settling for the candidate-declared metadata.
			syncProbeFile := cloneVirtualProbeTransient(transient)
			syncProbeFile.Duration = 0
			h.recoverVirtualProbeFromCache(attemptCtx, file, streamURL, syncProbeFile, cand, oid)
			return declaredFallback()
		}
		probeCtx, probeCancel := context.WithTimeout(attemptCtx, virtualProbeBudget)
		syncProbeFile := cloneVirtualProbeTransient(transient)
		// Zero the duration so the synchronous probe measures the empirical
		// duration instead of inheriting the catalog value.
		syncProbeFile.Duration = 0
		trace.probeRan = true
		probeStart := time.Now()
		probed, probeErr := h.probeVirtualSource(probeCtx, streamURL, &syncProbeFile, cand.RequestHeaders)
		trace.probe += time.Since(probeStart)
		probeCancel()
		if probeErr != nil || probed == nil {
			virtualProbeFailures.mark(probeKey)
			slog.DebugContext(r.Context(), "virtual stream probe timed out or failed; using candidate metadata", "component", "api", "candidate_uri", cand.URI, "error", probeErr)
			return declaredFallback()
		}
		virtualProbeFailures.clear(probeKey)
		empiricalDuration := probed.Duration
		if transient.ID > 0 {
			probed.ID = transient.ID
			probed.MediaFolderID = transient.MediaFolderID
		}
		if empiricalDuration > 0 {
			h.maybeSubmitRemuxDBEvidence(attemptCtx, probed, cand)
		}
		if transient.Duration > 0 && probed.Duration <= 0 {
			probed.Duration = transient.Duration
		}
		mergeVirtualCandidateTracks(probed, cand)
		h.maybeTriggerSubtitleSearch(probeCtx, probed, cand)
		return &resolvedVirtualPlaybackSource{
			URL: streamURL, URI: cand.URI, OwnerID: oid, File: probed, ProbeSucceeded: true, Provenance: ProbeProvenanceVerified, AppliedRemux: appliedRemux,
		}, nil
	}

	var firstResolved *resolvedVirtualPlaybackSource
	var attemptErr error
	for i, candidate := range candidates {
		result, err := resolveAndProbe(i, candidate)
		if fastPathHit {
			// The repeat-play fast path already committed the persisted
			// candidate; return it without re-ranking or unpinning the pin.
			if result == nil {
				return resolvedVirtualPlaybackSource{}, errors.New("virtual playback fast path returned no source")
			}
			return *result, nil
		}
		if err != nil || result.Provenance == ProbeProvenanceFailed {
			if candidate.URI == pinnedURI && h != nil {
				// The pinned source stopped working; release it so the next
				// start re-ranks candidates instead of retrying a dead URI.
				h.unpinVirtualSticky(stickyKey, candidate.URI)
			}
			if firstResolved == nil && result != nil {
				firstResolved = result
			}
			if err != nil {
				slog.WarnContext(r.Context(), "virtual playback candidate failed",
					"component", "api", "candidate_uri", candidate.URI, "candidate_index", i,
					"file_id", file.ID, "content_id", file.ContentID, "error", err)
			}
			attemptErr = errors.Join(attemptErr, err)
			continue
		}
		// A deferred result already fired its background probe; return it
		// now. The original loop kept scanning trailing candidates for a
		// verified sibling, but every extra iteration pays a full provider
		// resolve RPC (a plugin round-trip, seconds each) whose work the
		// loop then throws away — the final return is this same first
		// usable source anyway. Declared results (no prober) fall through
		// to the finalization block below so they still get the runtime
		// check, evidence persist, and sticky pin.
		if result.Provenance == ProbeProvenancePending {
			return *result, nil
		}
		if result.Provenance == ProbeProvenanceVerified || (!result.AppliedRemux && !result.ResolutionAssumed && h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil) {
			// Content ground truth: a probed duration wildly different from the
			// catalog runtime means the provider handed us mislabeled content.
			// Skip persisting its metadata onto this content's rows and rotate.
			expectedRuntimeMinutes := 0
			if file.EpisodeID != "" && h.EpisodeLookup != nil {
				if ep, epErr := h.EpisodeLookup.GetByID(r.Context(), file.EpisodeID); epErr == nil && ep != nil {
					expectedRuntimeMinutes = ep.Runtime
				}
			}
			if expectedRuntimeMinutes == 0 && h.ItemLookup != nil {
				if item, itemErr := h.ItemLookup.GetByID(r.Context(), file.ContentID); itemErr == nil && item != nil {
					expectedRuntimeMinutes = item.Runtime
				}
			}
			if !virtualRuntimePlausible(result.File.Duration, expectedRuntimeMinutes) {
				slog.WarnContext(r.Context(), "virtual candidate rejected: probed duration implausible",
					"component", "api", "candidate_uri", candidate.URI,
					"file_id", file.ID, "content_id", file.ContentID,
					"probed_duration_seconds", result.File.Duration,
					"expected_runtime_minutes", expectedRuntimeMinutes)
				h.unpinVirtualSticky(stickyKey, candidate.URI)
				attemptErr = errors.Join(attemptErr, fmt.Errorf("candidate %s probed duration %ds implausible for %dm runtime",
					candidate.URI, result.File.Duration, expectedRuntimeMinutes))
				continue
			}
			// Persist probed audio/subtitle tracks back to the DB so
			// the watch detail and player UI show track options on
			// subsequent views without re-probing.
			h.persistVirtualProbeEvidence(r.Context(), file, result.File.FilePath, result.File, result.Provenance == ProbeProvenanceVerified)
			// The filtered candidate list is already cached device-neutrally
			// above (and ranked for this device), so replays skip the provider
			// round-trip and re-rank for the requesting device. Pin this URI
			// as sticky so rotation cannot churn future sessions.
			h.pinVirtualSticky(stickyKey, candidate.URI)
			return *result, nil
		}
		if firstResolved == nil {
			copy := *result
			firstResolved = &copy
		}
	}
	if firstResolved != nil {
		// Re-merge against the candidate that actually produced this result,
		// not candidates[0]: after failover the usable source may come from
		// a later candidate, and merging the wrong candidate contaminates
		// tracks with another release's metadata.
		for _, candidate := range candidates {
			if firstResolved.URI != "" && candidate.URI == firstResolved.URI {
				mergeVirtualCandidateTracks(firstResolved.File, candidate)
				break
			}
		}
		if firstResolved.Provenance == ProbeProvenanceVerified {
			h.persistVirtualProbeEvidence(r.Context(), file, firstResolved.File.FilePath, firstResolved.File, true)
		}
		return *firstResolved, nil
	}
	if attemptErr == nil {
		attemptErr = errors.New("virtual playback provider returned no usable stream")
	}
	// When the primary resolution fails — commonly because a previously
	// persisted "result=" candidate has rotated or expired at the provider —
	// re-rank the current provider candidates provider-neutrally and retry
	// before failing. This keeps one stale indexer/debrid result from turning
	// a still-streamable item into a hard playback failure, without crossing
	// the user's selected quality when a same-profile candidate exists.
	// The stale-source fallback does its own provider re-list internally; time
	// it as one stage so a fallback-driven resolve is attributable rather than
	// silently folded into the loop's resolve time.
	trace.fallbackRan = true
	fallbackStart := time.Now()
	fb := h.fallbackResolveStaleVirtualSource(attemptCtx, file, userID, profileID)
	trace.fallback = time.Since(fallbackStart)
	if fb != nil {
		return *fb, nil
	}
	return resolvedVirtualPlaybackSource{}, attemptErr
}

// virtualDeliveredWithinGrace reports whether the row's last transport
// delivery is inside the scanner's delivery grace. A nil stamp means the row
// never delivered, so the start path must resolve synchronously.
func virtualDeliveredWithinGrace(file *models.MediaFile) bool {
	if file == nil || file.LastDeliveredAt == nil {
		return false
	}
	return time.Since(*file.LastDeliveredAt) < scanner.VirtualCandidateDeliveryGrace
}

// cloneVirtualProbeTransient copies a MediaFile with its track slices
// deep-copied. The synchronous caller keeps the original while the background
// probe mutates its own copy, so the two must not share backing arrays.
func cloneVirtualProbeTransient(base models.MediaFile) models.MediaFile {
	clone := base
	if len(base.VideoTracks) > 0 {
		clone.VideoTracks = append([]models.VideoTrack(nil), base.VideoTracks...)
	}
	if len(base.AudioTracks) > 0 {
		clone.AudioTracks = append([]models.AudioTrack(nil), base.AudioTracks...)
	}
	if len(base.SubtitleTracks) > 0 {
		clone.SubtitleTracks = append([]models.SubtitleTrack(nil), base.SubtitleTracks...)
	}
	return clone
}

// virtualExpectedRuntimeMinutes resolves the catalog runtime used to sanity
// check a probed duration. It prefers the episode runtime and falls back to the
// content item runtime. Zero means no expectation is available.
func (h *PlaybackHandler) virtualExpectedRuntimeMinutes(ctx context.Context, file *models.MediaFile) int {
	if file == nil {
		return 0
	}
	expected := 0
	if file.EpisodeID != "" && h.EpisodeLookup != nil {
		if ep, err := h.EpisodeLookup.GetByID(ctx, file.EpisodeID); err == nil && ep != nil {
			expected = ep.Runtime
		}
	}
	if expected == 0 && file.ContentID != "" && h.ItemLookup != nil {
		if item, err := h.ItemLookup.GetByID(ctx, file.ContentID); err == nil && item != nil {
			expected = item.Runtime
		}
	}
	return expected
}

// probeVirtualSourceAndPersist probes an already-resolved provider URL and
// persists the probed inventory back to the catalog row the client requested,
// so the next start of that row reuses the evidence instead of re-probing. It
// is the shared tail of the deferred start-path probe and the optimistic-start
// revalidation. bgCtx bounds the whole probe; the runtime-plausibility guard
// and the probe-failure damper are applied here so both callers behave
// identically.
func (h *PlaybackHandler) probeVirtualSourceAndPersist(
	bgCtx context.Context,
	stickyKey string,
	catalogFile *models.MediaFile,
	probeURL string,
	probeTransient models.MediaFile,
	probeCand VirtualPlaybackStream,
	expectedRuntimeMinutes int,
	ownerInstallationID int,
) {
	probeKey := virtualProbeFailureKey(probeCand.URI, ownerInstallationID)
	probeCtx, probeCancel := context.WithTimeout(bgCtx, virtualBackgroundProbeBudget)
	probed, probeErr := h.probeVirtualSource(probeCtx, probeURL, &probeTransient, probeCand.RequestHeaders)
	probeCancel()
	if probeErr != nil || probed == nil {
		virtualProbeFailures.mark(probeKey)
		slog.WarnContext(bgCtx, "background virtual stream probe failed", "component", "api", "candidate_uri", probeCand.URI, "error", probeErr)
		// A context error (outer budget fired, request canceled) means the
		// verdict is unknown: the inner probe keeps its own timeout and may
		// still complete and land in the cache. A candidate-specific rejection
		// is definitive; every other error must repeat before the pin is
		// released, so one slow provider cannot steer later starts.
		if errors.Is(probeErr, scanner.ErrVirtualProbeNoTracks) ||
			virtualProbeFailures.count(probeKey) >= virtualProbeFailureRepeatThreshold {
			h.unpinVirtualSticky(stickyKey, probeCand.URI)
		}
		return
	}
	if !virtualRuntimePlausible(probed.Duration, expectedRuntimeMinutes) {
		virtualProbeFailures.mark(probeKey)
		slog.WarnContext(bgCtx, "background virtual probe rejected: probed duration implausible",
			"component", "api", "candidate_uri", probeCand.URI, "file_id", catalogFile.ID,
			"probed_duration_seconds", probed.Duration, "expected_runtime_minutes", expectedRuntimeMinutes)
		h.unpinVirtualSticky(stickyKey, probeCand.URI)
		return
	}
	virtualProbeFailures.clear(probeKey)
	if probeTransient.ID > 0 {
		probed.ID = probeTransient.ID
		probed.MediaFolderID = probeTransient.MediaFolderID
	}
	if probeTransient.Duration > 0 && probed.Duration <= 0 {
		probed.Duration = probeTransient.Duration
	}
	mergeVirtualCandidateTracks(probed, probeCand)
	h.persistVirtualProbeEvidence(bgCtx, catalogFile, probeCand.URI, probed, true)
}

// recoverVirtualProbeFromCache attempts a cache-only probe for a candidate the
// failure damper would otherwise skip. A transient outer timeout can leave the
// inner probe running under its own scanner.VirtualProbeTimeout; when it
// completes it lands in the probe cache, so the completed evidence is available
// without paying the provider round-trip again. Recovering it persists the
// evidence and clears the damper, so a transient timeout does not starve the
// row until the backoff lapses. It reports whether cached evidence was used.
func (h *PlaybackHandler) recoverVirtualProbeFromCache(
	ctx context.Context,
	catalogFile *models.MediaFile,
	sourceURL string,
	probeFile models.MediaFile,
	probeCand VirtualPlaybackStream,
	ownerInstallationID int,
) bool {
	if h == nil || h.VirtualProbeCacheLookup == nil || catalogFile == nil {
		return false
	}
	probed := h.VirtualProbeCacheLookup(sourceURL, &probeFile)
	if probed == nil {
		return false
	}
	virtualProbeFailures.clear(virtualProbeFailureKey(probeCand.URI, ownerInstallationID))
	if probeFile.ID > 0 {
		probed.ID = probeFile.ID
		probed.MediaFolderID = probeFile.MediaFolderID
	}
	if probeFile.Duration > 0 && probed.Duration <= 0 {
		probed.Duration = probeFile.Duration
	}
	mergeVirtualCandidateTracks(probed, probeCand)
	h.persistVirtualProbeEvidence(ctx, catalogFile, probeCand.URI, probed, true)
	return true
}

// revalidateVirtualCandidateBackground resolves the provider URL for a
// candidate the start path returned optimistically and runs the probe+persist
// chain in the background. The start response may already be sent, so the
// whole chain keeps a WithoutCancel context; a failed revalidation only marks
// the probe damper and clears the sticky pin so the next start re-ranks.
func (h *PlaybackHandler) revalidateVirtualCandidateBackground(
	requestCtx context.Context,
	stickyKey string,
	file *models.MediaFile,
	cand VirtualPlaybackStream,
	oid int,
	userID int,
	profileID string,
	targetID int,
) {
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.WithoutCancel(requestCtx), virtualStartupBudget)
		defer bgCancel()

		var streamURL string
		var resolveErr error
		if h.VirtualMediaDetailedResolver != nil {
			res, err := h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
				bgCtx, cand.URI, oid, userID, profileID, false, nil, "",
			)
			if err != nil {
				resolveErr = err
			} else {
				streamURL = res.URL
				cand.RequestHeaders = cloneHeaderMap(res.RequestHeaders)
				if res.URI != "" {
					cand.URI = res.URI
				}
				if res.CandidateID != "" {
					cand.ID = res.CandidateID
				}
			}
		} else if h.VirtualPlaybackResolver != nil {
			streamURL, resolveErr = h.VirtualPlaybackResolver.ResolveVirtualPlayback(
				bgCtx, cand.URI, userID, profileID, oid,
			)
			cand.RequestHeaders = nil
		} else {
			return
		}
		if resolveErr != nil {
			resolveKey := virtualProbeFailureKey(cand.URI, oid)
			virtualProbeFailures.mark(resolveKey)
			slog.WarnContext(bgCtx, "optimistic virtual revalidation resolve failed", "component", "api", "candidate_uri", cand.URI, "error", resolveErr)
			// A transient RPC timeout leaves the candidate's health unknown;
			// only a repeat (or a concrete provider rejection) releases it.
			if !virtualProbeVerdictUnknown(resolveErr) ||
				virtualProbeFailures.count(resolveKey) >= virtualProbeFailureRepeatThreshold {
				h.unpinVirtualSticky(stickyKey, cand.URI)
			}
			return
		}

		probeTransient := cloneVirtualProbeTransient(*file)
		var dbFile *models.MediaFile
		if h.VirtualFileLookup != nil {
			dbFile, _ = h.VirtualFileLookup(bgCtx, cand.URI)
		}
		if (dbFile == nil || dbFile.ID <= 0) && h.VirtualCandidateFileLookup != nil {
			dbFile, _ = h.VirtualCandidateFileLookup(bgCtx, virtualPlaybackNeutralKey(cand.URI), file.ContentID, file.EpisodeID, oid)
		}
		if dbFile != nil && dbFile.ID > 0 {
			probeTransient = cloneVirtualProbeTransient(*dbFile)
		}
		probeTransient.FilePath = cand.URI
		probeTransient.VirtualOwnerInstallationID = oid
		if virtualProbeFailures.recent(virtualProbeFailureKey(cand.URI, oid)) {
			// The damper would skip the probe, but a completed inner probe may
			// already be cached. Recover it so the row is not left unprobed.
			h.recoverVirtualProbeFromCache(bgCtx, file, streamURL, probeTransient, cand, oid)
			return
		}
		h.probeVirtualSourceAndPersist(bgCtx, stickyKey, file, streamURL, probeTransient, cand, h.virtualExpectedRuntimeMinutes(bgCtx, file), oid)
	}()
}

// VirtualFileMetadataUpdateSQL persists a probed virtual inventory back to
// media_files. It also stamps probe_source/probe_updated_at so the playback
// probe gate can recognize the row as really probed and stop re-probing it on
// every start. probe_source stays 'virtual_collection' on collection-owned
// rows so the collection materializer keeps recognizing them; a real playback
// probe (stampProbe=true) still stamps probe_updated_at so collection rows
// converge to probed evidence instead of re-probing on every start.
//
// Path adoption is skipped when a sibling row (same virtual owner and library)
// already owns the target path. Adopting it anyway would violate the
// media_files_virtual_file_owner_key unique index and drop the probe evidence;
// the row keeps its current path while the metadata and stamp still apply. The
// probe_source guard is IS DISTINCT FROM so a row whose probe_source is NULL
// (never stamped) adopts its resolved path like any other non-collection row.
const VirtualFileMetadataUpdateSQL = `
UPDATE media_files SET
  video_tracks     = $1::jsonb,
  audio_tracks     = $2::jsonb,
  subtitle_tracks  = $3::jsonb,
  resolution       = NULLIF($4,''),
  codec_video      = NULLIF($5,''),
  codec_audio      = NULLIF($6,''),
  container        = NULLIF($7,''),
  hdr              = $8,
  bitrate          = NULLIF($9,0),
  duration         = CASE WHEN $10 > 0 THEN $10 ELSE duration END,
  audio_channels   = COALESCE(
    (SELECT (elem->>'channels')::int
     FROM jsonb_array_elements(
       CASE WHEN jsonb_typeof($2::jsonb) = 'array' THEN $2::jsonb ELSE '[]'::jsonb END
     ) elem LIMIT 1),
    audio_channels
  ),
  file_path        = CASE
    WHEN $18 != '' AND probe_source IS DISTINCT FROM 'virtual_collection'
         AND NOT EXISTS (
           SELECT 1 FROM media_files sibling
           WHERE sibling.id <> media_files.id
             AND sibling.file_path = $18
             AND sibling.virtual_owner_installation_id IS NOT DISTINCT FROM $16
             AND sibling.media_folder_id IS NOT DISTINCT FROM $17
         )
    THEN $18
    ELSE file_path
  END,
  probe_source     = CASE
    WHEN probe_source = 'virtual_collection' THEN probe_source
    WHEN NOT $13::boolean THEN probe_source
    ELSE 'virtual'
  END,
  probe_updated_at = CASE
    WHEN probe_source = 'virtual_collection' THEN probe_updated_at
    WHEN NOT $13::boolean THEN probe_updated_at
    ELSE GREATEST(clock_timestamp(), probe_updated_at + interval '1 microsecond')
  END,
  updated_at       = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')
WHERE id = $11
  AND (NULLIF($12, '') IS NULL OR file_path = $12)
  AND updated_at     = $14
  AND probe_updated_at IS NOT DISTINCT FROM $15::timestamptz
  AND virtual_owner_installation_id IS NOT DISTINCT FROM $16
  AND media_folder_id IS NOT DISTINCT FROM $17
`

// VirtualFileMetadataDB is the minimal database surface the shared virtual
// metadata update needs. Both the native router wiring and the jellycompat
// wiring pass a *pgxpool.Pool, which satisfies this interface.
type VirtualFileMetadataDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// ExecVirtualFileMetadataUpdate executes VirtualFileMetadataUpdateSQL and owns
// the adoption-race retry shared by the native and jellycompat savers.
//
// The SQL's sibling guard keeps adoption from colliding with an existing owner
// of the target path, but two concurrent probes can both pass it and one still
// loses the unique-index race (media_files_virtual_file_owner_key). When that
// happens with a non-empty AdoptPath, retry exactly once with AdoptPath cleared
// so the probe evidence lands on the row's current path instead of being
// dropped. The retry keys on SQLSTATE 23505 rather than the constraint name,
// and a second failure is returned unchanged. A nil db is a no-op so callers
// that run without a database stay safe.
func ExecVirtualFileMetadataUpdate(ctx context.Context, db VirtualFileMetadataDB, args models.VirtualFilePersistArgs) (int64, error) {
	if db == nil {
		return 0, nil
	}
	vStr := string(args.VideoTracks)
	if vStr == "" || vStr == jsonNullLiteral {
		vStr = "[]"
	}
	aStr := string(args.AudioTracks)
	if aStr == "" || aStr == jsonNullLiteral {
		aStr = "[]"
	}
	sStr := string(args.SubtitleTracks)
	if sStr == "" || sStr == jsonNullLiteral {
		sStr = "[]"
	}
	exec := func(adoptPath string) (int64, error) {
		tag, err := db.Exec(ctx, VirtualFileMetadataUpdateSQL,
			vStr, aStr, sStr, args.Resolution, args.CodecVideo, args.CodecAudio, args.Container, args.HDR, args.Bitrate, args.Duration,
			args.FileID, args.ExpectedFilePath, args.StampProbe,
			args.UpdatedAt, args.ProbeUpdatedAt, args.OwnerID, args.LibraryID, adoptPath,
		)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
	rows, err := exec(args.AdoptPath)
	if err == nil {
		return rows, nil
	}
	var pgErr *pgconn.PgError
	if args.AdoptPath == "" || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return 0, err
	}
	slog.WarnContext(ctx, "virtual probe evidence persist adoption raced an existing path owner; retrying without adoption",
		"component", "api", "file_id", args.FileID, "adopt_path", args.AdoptPath, "error", err)
	return exec("")
}

// virtualSnapshot captures a catalog row's identity and generation before
// resolution/probing so the CAS fence can detect stale writes.
type virtualSnapshot struct {
	FileID         int
	FilePath       string
	UpdatedAt      time.Time
	ProbeUpdatedAt *time.Time
	OwnerID        int
	LibraryID      int
}

func snapshotVirtualRow(file *models.MediaFile) virtualSnapshot {
	return virtualSnapshot{
		FileID:         file.ID,
		FilePath:       file.FilePath,
		UpdatedAt:      file.UpdatedAt,
		ProbeUpdatedAt: file.ProbeUpdatedAt,
		OwnerID:        file.VirtualOwnerInstallationID,
		LibraryID:      file.MediaFolderID,
	}
}

func (h *PlaybackHandler) persistVirtualMetadataBounded(ctx context.Context, snap virtualSnapshot, expectedFilePath string, file *models.MediaFile, stampProbe bool) (int64, error) {
	if h == nil || h.VirtualFileSaver == nil || file == nil || snap.FileID <= 0 {
		return 0, nil
	}
	videoJSON := marshalTracksJSON(sanitizeTrackSlice(file.VideoTracks))
	audioJSON := marshalTracksJSON(sanitizeTrackSlice(file.AudioTracks))
	subJSON := marshalTracksJSON(sanitizeTrackSlice(file.SubtitleTracks))
	res, vCodec, aCodec, container, hdr, bitrate, duration := file.Resolution, file.CodecVideo, file.CodecAudio, file.Container, file.HDR, file.Bitrate, file.Duration
	return h.VirtualFileSaver(ctx, models.VirtualFilePersistArgs{
		FileID:           snap.FileID,
		ExpectedFilePath: expectedFilePath,
		VideoTracks:      videoJSON,
		AudioTracks:      audioJSON,
		SubtitleTracks:   subJSON,
		Resolution:       res,
		CodecVideo:       vCodec,
		CodecAudio:       aCodec,
		Container:        container,
		HDR:              hdr,
		Bitrate:          bitrate,
		Duration:         duration,
		StampProbe:       stampProbe,
		UpdatedAt:        snap.UpdatedAt,
		ProbeUpdatedAt:   snap.ProbeUpdatedAt,
		OwnerID:          snap.OwnerID,
		LibraryID:        snap.LibraryID,
	})
}

// persistVirtualProbeEvidence writes probed track inventory and the probe stamp
// back to the catalog row for the virtual content the client requested, so the
// next start of that row can reuse the persisted evidence instead of
// re-listing and re-probing.
//
// The probed candidate carries a resolved ?result= URI while the catalog row
// usually stores the neutral URI, so the metadata update's file_path guard
// would miss the row. Non-collection rows adopt the resolved URI onto the row
// first (mirroring the stale-fallback path) so the guard matches and the row
// owns the pinned candidate on the next start. Collection-owned rows are never
// rewritten: the collection sync reconciles their file_path against its
// desired set and would delete an adopted path as stale. They are stamped in
// place under their neutral path instead, which is still enough for the
// repeat-play gates (cache/pin + probe stamp + complete evidence) to fire.
func (h *PlaybackHandler) persistVirtualProbeEvidence(ctx context.Context, catalogFile *models.MediaFile, resolvedPath string, probed *models.MediaFile, stampProbe bool) {
	if h == nil || h.VirtualFileSaver == nil || catalogFile == nil || probed == nil || catalogFile.ID <= 0 {
		return
	}
	snap := snapshotVirtualRow(catalogFile)
	expectedPath := catalogFile.FilePath
	adoptPath := ""
	if resolvedPath != "" && resolvedPath != catalogFile.FilePath && catalogFile.ProbeSource != "virtual_collection" {
		adoptPath = resolvedPath
	}
	args := models.VirtualFilePersistArgs{
		FileID:           snap.FileID,
		ExpectedFilePath: expectedPath,
		VideoTracks:      marshalTracksJSON(sanitizeTrackSlice(probed.VideoTracks)),
		AudioTracks:      marshalTracksJSON(sanitizeTrackSlice(probed.AudioTracks)),
		SubtitleTracks:   marshalTracksJSON(sanitizeTrackSlice(probed.SubtitleTracks)),
		Resolution:       probed.Resolution,
		CodecVideo:       probed.CodecVideo,
		CodecAudio:       probed.CodecAudio,
		Container:        probed.Container,
		HDR:              probed.HDR,
		Bitrate:          probed.Bitrate,
		Duration:         probed.Duration,
		StampProbe:       stampProbe,
		UpdatedAt:        snap.UpdatedAt,
		ProbeUpdatedAt:   snap.ProbeUpdatedAt,
		OwnerID:          snap.OwnerID,
		LibraryID:        snap.LibraryID,
		AdoptPath:        adoptPath,
	}
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer bgCancel()
		if _, err := h.VirtualFileSaver(bgCtx, args); err != nil {
			slog.ErrorContext(bgCtx, "virtual probe evidence persist failed",
				"component", "api", "file_id", args.FileID, "error", err)
		}
	}()
}

// fallbackResolveStaleVirtualSource re-lists the provider's current candidates
// and resolves the first healthy provider-neutral stream. It returns nil when
// the original URI carried no stale result= pick, or when no substitute
// candidate can be resolved, so the caller preserves its original error.
func (h *PlaybackHandler) fallbackResolveStaleVirtualSource(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
) *resolvedVirtualPlaybackSource {
	parsed, _ := url.Parse(file.FilePath)
	if parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) == "" {
		return nil
	}
	if h.VirtualPlaybackStreamLister == nil {
		return nil
	}
	neutralKey := virtualPlaybackNeutralKey(file.FilePath)
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	streams, listErr := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
		listCtx, neutralKey, userID, profileID, file.VirtualOwnerInstallationID,
	)
	cancel()
	if listErr != nil {
		slog.ErrorContext(ctx, "virtual stale fallback: list failed", "component", "api", "neutral_key", neutralKey, "error", listErr)
		return nil
	}
	if len(streams) == 0 {
		slog.ErrorContext(ctx, "virtual stale fallback: no streams listed", "component", "api", "neutral_key", neutralKey)
		return nil
	}
	if len(streams) > maxVirtualPlaybackStreams {
		streams = streams[:maxVirtualPlaybackStreams]
	}
	// Guard against cross-identity candidates: only consider streams that
	// share the same scheme, host, path, and profile as the original file.
	streams = filterVirtualPlaybackStreams(file, streams)
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	attempts := 0
	for _, stream := range streams {
		if stream.URI == "" || stream.URI == file.FilePath {
			continue
		}
		attempts++
		if attempts > maxAttempts {
			break
		}
		resolved, err := h.resolveVirtualCandidateSource(ctx, file, stream, userID, profileID)
		if err == nil {
			slog.InfoContext(ctx, "virtual stale fallback: resolved substitute", "component", "api", "original", file.FilePath, "substitute", stream.URI)
			// Persist the substitute's path and probed metadata back to the
			// catalog row in a single CAS-fenced save, replacing the stale
			// result= URI the next start would have to re-list.
			if resolved.File != nil && resolved.Provenance == ProbeProvenanceVerified && h.VirtualFileSaver != nil {
				snap := snapshotVirtualRow(file)
				if _, saveErr := h.VirtualFileSaver(ctx, models.VirtualFilePersistArgs{
					FileID:           snap.FileID,
					ExpectedFilePath: file.FilePath,
					VideoTracks:      marshalTracksJSON(sanitizeTrackSlice(resolved.File.VideoTracks)),
					AudioTracks:      marshalTracksJSON(sanitizeTrackSlice(resolved.File.AudioTracks)),
					SubtitleTracks:   marshalTracksJSON(sanitizeTrackSlice(resolved.File.SubtitleTracks)),
					Resolution:       resolved.File.Resolution,
					CodecVideo:       resolved.File.CodecVideo,
					CodecAudio:       resolved.File.CodecAudio,
					Container:        resolved.File.Container,
					HDR:              resolved.File.HDR,
					Bitrate:          resolved.File.Bitrate,
					Duration:         resolved.File.Duration,
					StampProbe:       true,
					UpdatedAt:        snap.UpdatedAt,
					ProbeUpdatedAt:   snap.ProbeUpdatedAt,
					OwnerID:          snap.OwnerID,
					LibraryID:        snap.LibraryID,
					AdoptPath:        stream.URI,
				}); saveErr != nil {
					slog.ErrorContext(ctx, "virtual stale fallback: persist failed", "component", "api", "file_id", file.ID, "error", saveErr)
				}
			}
			return resolved
		}
		slog.ErrorContext(ctx, "virtual stale fallback: candidate failed", "component", "api", "candidate", stream.URI, "error", err)
	}
	return nil
}

// resolveVirtualCandidateSource resolves and probes a single virtual stream
// candidate, returning a fully-probed source on success.
func (h *PlaybackHandler) resolveVirtualCandidateSource(
	ctx context.Context,
	file *models.MediaFile,
	candidate VirtualPlaybackStream,
	userID int,
	profileID string,
) (*resolvedVirtualPlaybackSource, error) {
	ownerID := candidate.OwnerInstallationID
	if ownerID <= 0 {
		ownerID = file.VirtualOwnerInstallationID
	}
	var streamURL string
	if h.VirtualMediaDetailedResolver != nil {
		res, err := h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
			ctx, candidate.URI, ownerID, userID, profileID, false, nil, "",
		)
		if err != nil {
			return nil, err
		}
		streamURL = res.URL
		candidate.RequestHeaders = cloneHeaderMap(res.RequestHeaders)
		if res.URI != "" {
			candidate.URI = res.URI
		}
	} else if h.VirtualPlaybackResolver != nil {
		var err error
		streamURL, err = h.VirtualPlaybackResolver.ResolveVirtualPlayback(
			ctx, candidate.URI, userID, profileID, ownerID,
		)
		if err != nil {
			return nil, err
		}
		candidate.RequestHeaders = nil
	} else {
		return nil, errors.New("virtual playback resolver is not configured")
	}
	transient := *file
	transient.FilePath = candidate.URI
	transient.VirtualOwnerInstallationID = ownerID
	resolved := resolvedVirtualPlaybackSource{URL: streamURL, URI: candidate.URI, OwnerID: ownerID, File: &transient, Provenance: ProbeProvenanceDeclared}
	if h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil {
		return &resolved, nil
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, virtualProbeBudget)
	probed, probeErr := h.probeVirtualSource(probeCtx, streamURL, &transient, candidate.RequestHeaders)
	probeCancel()
	if probeErr != nil || probed == nil {
		return nil, errors.New("virtual stream probe failed during fallback")
	}
	resolved.File = probed
	mergeVirtualCandidateTracks(resolved.File, candidate)
	resolved.ProbeSucceeded = true
	resolved.Provenance = ProbeProvenanceVerified
	return &resolved, nil
}

func filterVirtualPlaybackStreams(file *models.MediaFile, streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	if file == nil {
		return nil
	}
	base, err := url.Parse(file.FilePath)
	if err != nil {
		return nil
	}
	baseProfile := strings.TrimSpace(base.Query().Get("profile"))
	seen := map[string]struct{}{file.FilePath: {}}
	alternatives := make([]VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		if len(alternatives) >= maxVirtualPlaybackStreams-1 {
			break
		}
		stream.URI = strings.TrimSpace(stream.URI)
		if !strings.HasPrefix(strings.ToLower(stream.URI), virtualPlaybackPrefix) {
			continue
		}
		candidate, parseErr := url.Parse(stream.URI)
		if parseErr != nil ||
			!strings.EqualFold(candidate.Scheme, base.Scheme) ||
			!strings.EqualFold(candidate.Host, base.Host) ||
			candidate.EscapedPath() != base.EscapedPath() {
			continue
		}
		if baseProfile != "" && !strings.EqualFold(
			strings.TrimSpace(candidate.Query().Get("profile")), baseProfile,
		) {
			continue
		}
		if _, duplicate := seen[stream.URI]; duplicate {
			continue
		}
		seen[stream.URI] = struct{}{}
		alternatives = append(alternatives, stream)
	}
	return alternatives
}

func visibleVirtualPlaybackStreams(streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	visible := make([]VirtualPlaybackStream, 0, len(streams))
	hasHidden := hasHiddenVirtualPlaybackStreams(streams)
	for _, stream := range streams {
		if stream.Visible || !hasHidden {
			visible = append(visible, stream)
		}
	}
	return visible
}

func hasHiddenVirtualPlaybackStreams(streams []VirtualPlaybackStream) bool {
	for _, stream := range streams {
		if stream.VisibilitySpecified && !stream.Visible {
			return true
		}
	}
	return false
}

func cloneHeaderMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isVirtualPlaybackFile(file *models.MediaFile) bool {
	return file != nil && strings.HasPrefix(file.FilePath, virtualPlaybackPrefix)
}

func isUnplayableVirtualURI(uri string) bool {
	raw := strings.TrimSpace(strings.ToLower(uri))
	if strings.HasPrefix(raw, "virtual://series/") || strings.HasPrefix(raw, "virtual://show/") {
		parsed, err := url.Parse(raw)
		if err == nil {
			trimmed := strings.Trim(parsed.Path, "/")
			if trimmed == "" {
				return true
			}
			parts := strings.Split(trimmed, "/")
			if len(parts) < 3 {
				return true
			}
		}
	}
	return false
}

func withVirtualResultKey(virtualPath, candID string) string {
	if candID == "" {
		return virtualPath
	}
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return virtualPath + "?result=" + candID
	}
	q := parsed.Query()
	q.Set("result", candID)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// virtualPlaybackNeutralKey returns the virtual URI with any concrete "result="
// pick removed, preserving the scheme/host/path and the profile so a stale
// provider candidate can be re-resolved provider-neutrally within the same
// quality selection.
func virtualPlaybackNeutralKey(virtualPath string) string {
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return virtualPath
	}
	q := parsed.Query()
	if strings.TrimSpace(q.Get("result")) == "" {
		return virtualPath
	}
	q.Del("result")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// virtualResultCandidateID returns the concrete "result=" candidate ID bound
// to a virtual URI, or "" when the URI carries no explicit pick.
func virtualResultCandidateID(virtualPath string) string {
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("result"))
}

// maybeTriggerSubtitleSearch kicks off a background subtitle search when a
// virtual stream enters playback with no embedded or external subtitle tracks.
// Results are downloaded and associated with the file so they appear in the
// player's subtitle selector without blocking playback start.
func (h *PlaybackHandler) maybeTriggerSubtitleSearch(
	ctx context.Context,
	file *models.MediaFile,
	cand VirtualPlaybackStream,
) {
	if h.VirtualSubtitleSearcher == nil || file == nil {
		return
	}
	if len(file.SubtitleTracks) > 0 || len(file.ExternalSubtitles) > 0 {
		return
	}
	searchKey := any(file.ID)
	if file.ID <= 0 {
		searchKey = "virtual:" + file.ContentID + ":" + cand.URI
	}
	// Dedupe: one in-flight search per file. Rapid replays or multiple
	// candidates resolving the same file must not hammer subtitle providers.
	if _, loaded := h.SubtitleSearchInFlight.LoadOrStore(searchKey, struct{}{}); loaded {
		return
	}
	go func() {
		defer h.SubtitleSearchInFlight.Delete(searchKey)
		h.VirtualSubtitleSearcher(
			context.Background(),
			file.ContentID,
			"", // IMDb ID resolved from contentID by the caller
			"", // title resolved by the caller
			0,  // year resolved by the caller
			0,  // season
			0,  // episode
			file.ID,
			cand.SubtitleLanguages,
		)
	}()
}

const (
	remuxDBFetchBudget  = 3 * time.Second
	remuxDBCacheBudget  = 500 * time.Millisecond
	remuxDBConfigBudget = 1 * time.Second
)

// allowDeferredProbe reports whether the virtual probe may be pushed to the
// background. With RemuxDB disabled it follows the caller's deferProbe flag
// exactly (the pre-RemuxDB behavior); when RemuxDB is enabled it additionally
// requires resolution and codec evidence so a deferred probe starts with
// meaningful metadata confidence. Loose RemuxDB matches (1% size variance or
// filename stem) are unverified guesses and must not allow probe deferral
// without local catalog or candidate declarations.
func allowDeferredProbe(
	deferProbe bool,
	remuxEnabled bool,
	matchMethod remuxdb.MatchMethod,
	localResolution string,
	candResolution string,
	localCodec string,
	candCodec string,
	remuxResolution string,
	remuxCodec string,
) bool {
	if !deferProbe {
		return false
	}
	if !remuxEnabled {
		return true
	}
	isLoose := matchMethod == remuxdb.MatchSizeTags || matchMethod == remuxdb.MatchFilename
	res := localResolution
	if res == "" {
		res = candResolution
	}
	if !isLoose && res == "" {
		res = remuxResolution
	}

	codec := localCodec
	if codec == "" {
		codec = candCodec
	}
	if !isLoose && codec == "" {
		codec = remuxCodec
	}

	return res != "" && codec != ""
}

// remuxEvidenceKey returns a stable key identifying one concrete provider
// release within a virtual URI. The key keeps only the scheme/host/path plus
// the single "result" pick so profile ordering and other query params cannot
// make the same release key differently between match and apply time.
func remuxEvidenceKey(candidateURI string) string {
	parsed, err := url.Parse(candidateURI)
	if err != nil {
		return virtualPlaybackNeutralKey(candidateURI)
	}
	result := strings.TrimSpace(parsed.Query().Get("result"))
	if result == "" {
		return virtualPlaybackNeutralKey(candidateURI)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String() + "?" + url.Values{"result": []string{result}}.Encode()
}

func remuxHintForCandidate(file *models.MediaFile, cand VirtualPlaybackStream) remuxdb.MatchHint {
	size := cand.FileSize
	resolution := cand.Resolution
	codecVideo := cand.CodecVideo
	hdrKnown := false
	hdr := false
	sameRelease := file != nil && (file.FilePath == cand.URI || (cand.ID != "" && virtualResultCandidateID(file.FilePath) == cand.ID))
	if file != nil && sameRelease {
		if size <= 0 {
			size = file.FileSize
		}
		if resolution == "" {
			resolution = file.Resolution
		}
		if codecVideo == "" {
			codecVideo = file.CodecVideo
		}
		if len(file.VideoTracks) > 0 {
			hdrKnown = true
			hdr = file.HDR
		}
	}
	if cand.HDR != "" {
		hdrKnown = true
		hdr = true
	}
	filename := strings.TrimSpace(cand.Label)
	if filename == "" && sameRelease {
		filename = strings.TrimSpace(file.ReleaseName)
	}
	infoHash := ""
	indexerGUID := ""
	indexer := ""
	if cand.URI != "" {
		if parsed, err := url.Parse(cand.URI); err == nil {
			if h := strings.TrimSpace(parsed.Query().Get("hash")); len(h) == 40 && isHexString(h) {
				infoHash = h
			} else if h := strings.TrimSpace(parsed.Query().Get("info_hash")); len(h) == 40 && isHexString(h) {
				infoHash = h
			}
			if g := strings.TrimSpace(parsed.Query().Get("guid")); g != "" {
				indexerGUID = g
			} else if g := strings.TrimSpace(parsed.Query().Get("indexer_guid")); g != "" {
				indexerGUID = g
			}
			if idx := strings.TrimSpace(parsed.Query().Get("indexer")); idx != "" {
				indexer = idx
			}
		}
	}
	hint := remuxdb.HintFromCandidate(infoHash, nil, size, filename, resolution, codecVideo, hdrKnown, hdr)
	hint.IndexerGUID = indexerGUID
	hint.Indexer = indexer
	return hint
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (h *PlaybackHandler) matchRemuxDBCandidates(ctx context.Context, file *models.MediaFile, candidates []VirtualPlaybackStream) (map[string]remuxdb.Evidence, bool) {
	matched := map[string]remuxdb.Evidence{}
	if h == nil || file == nil || len(candidates) == 0 {
		return matched, false
	}
	cfg := remuxdb.DefaultConfig()
	if h.RemuxDBConfig != nil {
		// Config load reads several settings rows; keep it bounded so a slow
		// settings store cannot delay playback start. DefaultConfig disables
		// RemuxDB, so a timeout fails closed.
		cfgCtx, cfgCancel := context.WithTimeout(ctx, remuxDBConfigBudget)
		cfg = h.RemuxDBConfig(cfgCtx)
		if cfgCtx.Err() != nil {
			cfg = remuxdb.DefaultConfig()
		}
		cfgCancel()
	}
	if !cfg.Enabled {
		return matched, false
	}
	imdbID := remuxdb.ExtractIMDbID(file.ContentID)
	if imdbID == "" {
		imdbID = remuxdb.ExtractIMDbID(file.FilePath)
	}
	if imdbID == "" {
		return matched, true
	}
	var seasonPtr, episodePtr *int
	if file.SeasonNumber > 0 {
		seasonPtr = &file.SeasonNumber
	}
	if file.EpisodeNumber > 0 {
		episodePtr = &file.EpisodeNumber
	}
	type pendingCandidate struct {
		key  string
		hint remuxdb.MatchHint
	}
	// Cache reads get their own short budget, separate from the network fetch
	// budget below so a slow store cannot starve the HTTP request.
	cacheCtx, cacheCancel := context.WithTimeout(ctx, remuxDBCacheBudget)
	defer cacheCancel()

	pending := make([]pendingCandidate, 0, len(candidates))
	for _, cand := range candidates {
		key := remuxEvidenceKey(cand.URI)
		if key == "" {
			continue
		}
		if _, dup := matched[key]; dup {
			continue
		}
		if h.RemuxDBStore != nil {
			if ev, ok, err := h.RemuxDBStore.Get(cacheCtx, file.ContentID, file.EpisodeID, file.MediaFolderID, key); err == nil && ok && len(ev.VideoTracks) > 0 {
				matched[key] = ev
				continue
			}
		}
		pending = append(pending, pendingCandidate{key: key, hint: remuxHintForCandidate(file, cand)})
	}
	if len(pending) == 0 {
		return matched, true
	}
	fetchCtx, fetchCancel := context.WithTimeout(ctx, remuxDBFetchBudget)
	defer fetchCancel()
	client := remuxdb.NewClient(cfg.BaseURL, cfg.Token)
	versions, err := client.FetchProbe(fetchCtx, imdbID, seasonPtr, episodePtr)
	slog.InfoContext(fetchCtx, "remuxdb match candidates", "component", "api", "imdb_id", imdbID, "pending", len(pending), "versions", len(versions), "error", err)
	if err != nil || len(versions) == 0 {
		return matched, true
	}
	for _, p := range pending {
		variant, method := remuxdb.MatchVariant(versions, p.hint)
		if variant == nil {
			continue
		}
		slog.InfoContext(fetchCtx, "remuxdb candidate matched", "component", "api", "key", p.key, "method", method)
		ev := remuxdb.EvidenceFromVariant(file.ContentID, file.EpisodeID, file.MediaFolderID, p.key, method, variant)
		matched[p.key] = ev
		if h.RemuxDBStore != nil {
			if recErr := h.RemuxDBStore.Record(fetchCtx, ev); recErr != nil {
				slog.WarnContext(fetchCtx, "remuxdb store record failed", "component", "api", "key", p.key, "error", recErr)
			}
		}
	}
	slog.InfoContext(fetchCtx, "remuxdb match complete", "component", "api", "imdb_id", imdbID, "matched", len(matched))
	return matched, true
}

type remuxSubmitTask struct {
	payload remuxdb.SubmissionPayload
	baseURL string
	token   string
}

const (
	remuxSubmitQueueSize = 64
	remuxSubmitWorkers   = 2
)

func (h *PlaybackHandler) maybeSubmitRemuxDBEvidence(ctx context.Context, probed *models.MediaFile, cand VirtualPlaybackStream) {
	if h == nil || probed == nil {
		return
	}
	cfg := remuxdb.DefaultConfig()
	if h.RemuxDBConfig != nil {
		cfg = h.RemuxDBConfig(ctx)
	}
	if !cfg.Enabled || !cfg.SubmitEnabled || strings.TrimSpace(cfg.Token) == "" {
		return
	}
	hint := remuxHintForCandidate(probed, cand)
	if hint.InfoHash == "" && hint.IndexerGUID == "" {
		return
	}
	var nzb *remuxdb.NzbSubmission
	if hint.IndexerGUID != "" {
		nzb = &remuxdb.NzbSubmission{
			Indexer:     hint.Indexer,
			IndexerGUID: hint.IndexerGUID,
			Title:       hint.Filename,
		}
	}
	payload, ok := remuxdb.BuildSubmission(probed, hint.Filename, hint.InfoHash, nzb)
	if !ok {
		return
	}
	h.remuxSubmitOnce.Do(func() {
		h.remuxSubmitCh = make(chan remuxSubmitTask, remuxSubmitQueueSize)
		for range remuxSubmitWorkers {
			go func() {
				for task := range h.remuxSubmitCh {
					submitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					client := remuxdb.NewClient(task.baseURL, task.token)
					if err := client.SubmitProbe(submitCtx, task.payload); err != nil {
						slog.DebugContext(submitCtx, "remuxdb probe submission failed", "component", "api", "filename", task.payload.Filename, "error", err)
					} else {
						slog.InfoContext(submitCtx, "remuxdb probe submitted", "component", "api", "filename", task.payload.Filename, "kind", task.payload.Kind)
					}
					cancel()
				}
			}()
		}
	})
	task := remuxSubmitTask{
		payload: payload,
		baseURL: cfg.BaseURL,
		token:   cfg.Token,
	}
	select {
	case h.remuxSubmitCh <- task:
	default:
		slog.WarnContext(ctx, "remuxdb submission queue full; dropping probe submission",
			"component", "api", "filename", payload.Filename)
	}
}

func applyRemuxDBEvidence(file *models.MediaFile, matched map[string]remuxdb.Evidence, key string) *models.MediaFile {
	if file == nil || len(matched) == 0 {
		return file
	}
	ev, ok := matched[key]
	if !ok || len(ev.VideoTracks) == 0 {
		return file
	}
	out := *file
	if remuxdb.ApplyEvidence(ev, &out) {
		return &out
	}
	return file
}

// mergeVirtualCandidateTracks supplements probed virtual file tracks with
// metadata from the provider candidate. ffprobe may not always detect
// language tags, codecs, or dimensions on remote streams (especially HLS
// and DASH), so candidate metadata fills the gaps so virtual files appear
// as close to local files as possible.
func mergeVirtualCandidateTracks(probed *models.MediaFile, candidate VirtualPlaybackStream) {
	if probed == nil {
		return
	}

	// Fill empty top-level fields that ffprobe may miss on remote streams.
	// A trackless, resolution-less file keeps CodecVideo/CodecAudio/Container
	// empty so the evidence gates below report it incomplete: without a
	// resolution the planner cannot pick a route, and claiming synthesized
	// evidence would only hide the missing metadata that still blocks
	// planning. The caller-supplied baselines (declaredFallback fixes up
	// transient.Resolution first) flow through because a non-empty candidate
	// label lands in probed.Resolution on the line below.
	if probed.Resolution == "" {
		probed.Resolution = candidate.Resolution
	}
	// Top-level codec derivation defers to the track-evidence-first blocks
	// below (lines ~1883+, ~1903+): existing tracks win over candidate blobs,
	// and the final CodecVideo/CodecAudio assignment happens there exactly
	// once so merge stays idempotent.
	hasResolution := probed.Resolution != ""
	if !probed.HDR && candidate.HDR != "" {
		probed.HDR = true
	}
	if probed.Container == "" || strings.EqualFold(probed.Container, "virtual") {
		if candidate.Container != "" && !strings.EqualFold(candidate.Container, "virtual") {
			probed.Container = candidate.Container
		} else if hasResolution {
			probed.Container = "mkv"
		}
	}
	if probed.FileSize == 0 {
		probed.FileSize = candidate.FileSize
	}
	if probed.Bitrate == 0 {
		probed.Bitrate = candidate.Bitrate
	}
	if probed.Bitrate == 0 && probed.FileSize > 0 && probed.Duration > 0 {
		probed.Bitrate = int((probed.FileSize * 8) / int64(probed.Duration) / 1000)
	}
	if probed.Bitrate == 0 {
		probed.Bitrate = virtualBitrateFallback(probed.Resolution)
	}

	// Codec precedence: existing top-level wins; if empty, use first track
	// codec; then candidate; then resolution-gated default. The top-level
	// scalar is preserved when non-empty because it may reflect a probed
	// value that is more authoritative than the first track in the slice.
	if probed.CodecAudio == "" && len(probed.AudioTracks) > 0 && probed.AudioTracks[0].Codec != "" {
		probed.CodecAudio = probed.AudioTracks[0].Codec
	}
	if probed.CodecAudio == "" {
		probed.CodecAudio = candidate.CodecAudio
	}
	if probed.CodecAudio == "" && hasResolution {
		probed.CodecAudio = "aac"
	}
	channels := inferChannelsFromCodec(probed.CodecAudio)
	if channels <= 0 {
		channels = 2
	}
	if probed.AudioChannels <= 0 {
		probed.AudioChannels = channels
	}

	// Create a basic video track when ffprobe didn't detect any. Codec
	// precedence: existing top-level > first track > candidate > h264 default
	// (only when resolution is present, to avoid false claims on
	// resolution-less incomplete metadata).
	videoCodec := probed.CodecVideo
	if videoCodec == "" && len(probed.VideoTracks) > 0 && probed.VideoTracks[0].Codec != "" {
		videoCodec = probed.VideoTracks[0].Codec
	}
	if videoCodec == "" {
		videoCodec = candidate.CodecVideo
	}
	if videoCodec == "" && probed.Resolution != "" {
		videoCodec = "h264"
	}
	if probed.CodecVideo == "" && videoCodec != "" {
		probed.CodecVideo = videoCodec
	}
	isDV, dvProfile := virtualDVMetadata(candidate.HDR)
	isHDR := probed.HDR || candidate.HDR != ""
	defaultProfile, defaultLevel, defaultBitDepth := defaultVirtualVideoProfileAndLevel(videoCodec, isHDR, isDV, probed.Resolution)
	if len(probed.VideoTracks) == 0 && probed.Resolution != "" {
		videoRange := "SDR"
		videoRangeType := "SDR"
		if isDV {
			if dvProfile == 0 {
				dvProfile = 8
			}
			videoRange = "DolbyVision"
			videoRangeType = "DOVI"
		} else if isHDR {
			videoRange = "HDR"
			videoRangeType = "HDR10"
		}
		vt := models.VideoTrack{
			Codec:          videoCodec,
			Profile:        defaultProfile,
			Level:          defaultLevel,
			Width:          resolutionWidth(probed.Resolution),
			Height:         resolutionHeight(probed.Resolution),
			FrameRate:      defaultVirtualFrameRate(candidate.FrameRate),
			BitDepth:       defaultBitDepth,
			Bitrate:        probed.Bitrate,
			VideoRange:     videoRange,
			VideoRangeType: videoRangeType,
			DVProfile:      dvProfile,
			DolbyVision:    virtualDVLabel(isDV, dvProfile),
		}
		if isDV && dvProfile != 5 {
			vt.DVConfigPresent = true
			vt.DVBLCompatIDPresent = true
			vt.DVBLCompatID = 1
			vt.DVBLPresent = true
		}
		probed.VideoTracks = append(probed.VideoTracks, vt)
	}
	for i := range probed.VideoTracks {
		if probed.VideoTracks[i].Codec == "" {
			probed.VideoTracks[i].Codec = videoCodec
		}
		trackProf, trackLvl, trackDepth := defaultVirtualVideoProfileAndLevel(probed.VideoTracks[i].Codec, isHDR, isDV, probed.Resolution)
		if probed.VideoTracks[i].Profile == "" {
			probed.VideoTracks[i].Profile = trackProf
		}
		if probed.VideoTracks[i].Level <= 0 {
			probed.VideoTracks[i].Level = trackLvl
		}
		if probed.VideoTracks[i].Width <= 0 {
			probed.VideoTracks[i].Width = resolutionWidth(probed.Resolution)
		}
		if probed.VideoTracks[i].Height <= 0 {
			probed.VideoTracks[i].Height = resolutionHeight(probed.Resolution)
		}
		if probed.VideoTracks[i].FrameRate == "" {
			probed.VideoTracks[i].FrameRate = defaultVirtualFrameRate(candidate.FrameRate)
		}
		if probed.VideoTracks[i].BitDepth <= 0 {
			probed.VideoTracks[i].BitDepth = trackDepth
		} else if probed.VideoTracks[i].BitDepth == 8 && (isHDR || isDV || strings.EqualFold(probed.VideoTracks[i].Profile, "main 10")) {
			probed.VideoTracks[i].BitDepth = 10
		}
		if probed.VideoTracks[i].Bitrate <= 0 {
			probed.VideoTracks[i].Bitrate = probed.Bitrate
		}
		if isDV {
			profile := probed.VideoTracks[i].DVProfile
			if profile == 0 {
				profile = dvProfile
				if profile == 0 {
					profile = 8
				}
				probed.VideoTracks[i].DVProfile = profile
			}
			if probed.VideoTracks[i].DolbyVision == "" {
				probed.VideoTracks[i].DolbyVision = virtualDVLabel(true, profile)
			}
			if probed.VideoTracks[i].VideoRange == "" || probed.VideoTracks[i].VideoRange == "SDR" {
				probed.VideoTracks[i].VideoRange = "DolbyVision"
				probed.VideoTracks[i].VideoRangeType = "DOVI"
			}
			if profile != 5 && !probed.VideoTracks[i].DVConfigPresent {
				probed.VideoTracks[i].DVConfigPresent = true
				probed.VideoTracks[i].DVBLCompatIDPresent = true
				probed.VideoTracks[i].DVBLCompatID = 1
				probed.VideoTracks[i].DVBLPresent = true
			}
		} else if isHDR && (probed.VideoTracks[i].VideoRange == "" ||
			strings.EqualFold(probed.VideoTracks[i].VideoRange, "sdr") ||
			strings.EqualFold(probed.VideoTracks[i].VideoRangeType, "sdr")) {
			probed.VideoTracks[i].VideoRange = "HDR"
			probed.VideoTracks[i].VideoRangeType = "HDR10"
		}
		if probed.VideoTracks[i].VideoRange == "" && !probed.HDR {
			probed.VideoTracks[i].VideoRange = "SDR"
			probed.VideoTracks[i].VideoRangeType = "SDR"
		}
	}

	// Synthesize audio tracks from the provider-declared languages when the
	// probe left the inventory empty. A file with no resolution and no tracks
	// cannot produce a routable plan, so language synthesis is gated on the
	// caller supplying a resolution (declared, backfilled, or probed) — via
	// the baseline or a real candidate value. Video track synthesis below
	// follows the same gate so both inventories stay consistent.
	mergeVirtualCandidateLanguages(probed, candidate)

	if len(probed.AudioTracks) == 0 && probed.Resolution != "" {
		probed.AudioTracks = []models.AudioTrack{{
			Codec:    probed.CodecAudio,
			Channels: channels,
			Default:  true,
		}}
	}

	// Fill audio channels and codec on existing tracks that lack them.
	for i := range probed.AudioTracks {
		if probed.AudioTracks[i].Codec == "" {
			probed.AudioTracks[i].Codec = probed.CodecAudio
		}
		if probed.AudioTracks[i].Channels == 0 {
			probed.AudioTracks[i].Channels = channels
		}
	}

	if len(probed.AudioTracks) > 0 {
		hasDefault := false
		for _, t := range probed.AudioTracks {
			if t.Default {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			probed.AudioTracks[0].Default = true
		}
	}
}

func defaultVirtualVideoProfileAndLevel(codec string, isHDR, isDV bool, res string) (string, int, int) {
	c := strings.ToLower(strings.TrimSpace(codec))
	switch c {
	case "hevc", "h265", "hev1", "hvc1":
		if isHDR || isDV || strings.EqualFold(res, "2160p") || strings.EqualFold(res, "4k") {
			return "main 10", 153, 10
		}
		return "main", 120, 8
	case "h264", "avc", "avc1":
		return "high", 41, 8
	case "av1", "av01":
		if isHDR || isDV {
			return "main", 153, 10
		}
		return "main", 153, 8
	case "vp9":
		if isHDR || isDV {
			return "profile 2", 41, 10
		}
		return "profile 0", 41, 8
	default:
		if isHDR || isDV {
			return "main 10", 153, 10
		}
		return "high", 41, 8
	}

}

var virtualDVProfileRegex = regexp.MustCompile(`(?i)(?:profile|dovi|dv|dolby\s*vision)\s*[-._:]?\s*0*([1-9]|1\d|20)(?:[.]\d+)?(?:[^a-z0-9]|$)`)

func virtualDVMetadata(raw string) (bool, int) {
	lower := strings.ToLower(strings.TrimSpace(raw))
	isDV := strings.Contains(lower, "dolby vision") || strings.Contains(lower, "dovi") ||
		lower == "dv" || strings.HasPrefix(lower, "dv ") || strings.HasSuffix(lower, " dv") || strings.Contains(lower, " dv ") ||
		virtualDVProfileMarker(lower)
	if !isDV {
		return false, 0
	}
	if matches := virtualDVProfileRegex.FindStringSubmatch(lower); len(matches) > 1 {
		if profile, err := strconv.Atoi(matches[1]); err == nil && profile > 0 && profile <= 20 {
			return true, profile
		}
	}
	return true, 0
}

func virtualDVProfileMarker(raw string) bool {
	for _, profile := range []string{"profile 5", "profile 7", "profile 8", "dv5", "dv7", "dv8"} {
		if strings.Contains(raw, profile) {
			return true
		}
	}
	return false
}

func mediaFileHDRString(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	if dvProfile := file.PrimaryDVProfile(); dvProfile > 0 {
		return fmt.Sprintf("Dolby Vision Profile %d", dvProfile)
	}
	if len(file.VideoTracks) > 0 {
		vt := file.VideoTracks[0]
		if vt.DolbyVision != "" {
			return vt.DolbyVision
		}
		if strings.EqualFold(vt.VideoRange, "DolbyVision") || strings.EqualFold(vt.VideoRangeType, "DOVI") {
			return "Dolby Vision"
		}
		if vt.VideoRange != "" && !strings.EqualFold(vt.VideoRange, "SDR") {
			return vt.VideoRange
		}
	}
	if file.HDR {
		return "true"
	}
	return ""
}

func virtualDVLabel(isDV bool, profile int) string {
	if !isDV || profile <= 0 {
		return ""
	}
	return "Profile " + strconv.Itoa(profile)
}

// mergeVirtualCandidateLanguages appends provider-declared audio languages as
// tracks when the probed inventory does not already carry them. Candidate
// language lists come from the release metadata (e.g. ITA-ENG in a release
// name), not from ffprobe, so tracks synthesized here are never authoritative —
// a later probe fills real codec/channel evidence on top. Release-group markers
// that are not real languages (e.g. MULTI, DUAL) are skipped so a bogus track
// never appears in the player's picker.
//
// Provider-declared subtitle languages are deliberately NOT synthesized into
// embedded tracks here: a synthesized SubtitleTrack carries a stream ordinal
// that no real subtitle stream backs, and the extractor maps it straight to
// ffmpeg's `0:s:N` specifier. That produced phantom subtitle selections that
// always failed at FFmpeg ("Stream map ” matches no streams"). Real embedded
// subtitle inventory comes only from the probe; provider subtitle hints stay on
// the candidate stream for the picker and drive the background subtitle search.
func mergeVirtualCandidateLanguages(probed *models.MediaFile, candidate VirtualPlaybackStream) {
	if probed == nil || len(probed.AudioTracks) > 0 {
		return
	}
	audioCodec := probed.CodecAudio
	if audioCodec == "" {
		audioCodec = candidate.CodecAudio
	}
	if audioCodec == "" {
		audioCodec = "aac"
	}
	channels := inferChannelsFromCodec(audioCodec)
	if len(candidate.AudioLanguages) > 0 {
		existing := make(map[string]bool, len(candidate.AudioLanguages))
		for _, lang := range candidate.AudioLanguages {
			lang = strings.TrimSpace(lang)
			if lang == "" || !isRealVirtualLanguageTag(lang) {
				continue
			}
			canonical := virtualLanguageBaseSubtag(lang)
			if existing[canonical] {
				continue
			}
			existing[canonical] = true
			probed.AudioTracks = append(probed.AudioTracks, models.AudioTrack{
				// Synthesized tracks carry no real container stream index; the
				// array position is the ordinal (audioStreamOrdinalV3 falls back
				// to it when Index <= 0).
				Language: lang,
				Codec:    audioCodec,
				Channels: channels,
			})
		}
	}
}

// virtualLanguageBaseSubtag canonicalizes a language token to its ISO base
// subtag ("ITA" → "it", "en-US" → "en"), so probe-recorded codes and
// provider-declared codes dedup against the same key. Unparseable tokens fall
// back to the lowercased trimmed input.
func virtualLanguageBaseSubtag(value string) string {
	trimmed := strings.TrimSpace(value)
	if tag, err := language.Parse(trimmed); err == nil {
		if base, conf := tag.Base(); conf != language.No && base.String() != "" {
			return base.String()
		}
	}
	return strings.ToLower(trimmed)
}

// isRealVirtualLanguageTag reports whether a provider-declared language token
// parses as a real ISO language subtag. The naive lowercased canonical form
// cannot tell "multi" from a valid three-letter code like "fil", so parsing
// with the Unicode language tagger and requiring a concrete base subtag keeps
// release markers like MULTI/DUAL out of the synthesized track inventory.
func isRealVirtualLanguageTag(value string) bool {
	tag, err := language.Parse(value)
	if err != nil {
		return false
	}
	base, conf := tag.Base()
	return conf != language.No && base.String() != ""
}

func defaultVirtualFrameRate(rate string) string {
	if strings.TrimSpace(rate) == "" {
		return "24"
	}
	return rate
}

func virtualBitrateFallback(resolution string) int {
	switch strings.ToLower(strings.TrimSpace(resolution)) {
	case "2160p", "4k", "uhd":
		return 24000
	case "720p":
		return 5000
	case "480p":
		return 2500
	default:
		return 10000
	}
}

// inferChannelsFromCodec returns a plausible channel count for a codec string.
func inferChannelsFromCodec(codec string) int {
	switch strings.ToLower(codec) {
	case "atmos":
		return 8
	case "truehd", "dts-hd", "dts", "eac3", "ac3":
		return 6
	default:
		return 2
	}
}

// resolutionWidth returns a typical width for a resolution label.
func resolutionWidth(label string) int {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "4320p", "8k":
		return 7680
	case "2160p", "4k", "uhd":
		return 3840
	case "1080p":
		return 1920
	case "720p":
		return 1280
	case "480p":
		return 720
	default:
		if left, right, ok := strings.Cut(strings.TrimSpace(label), "x"); ok {
			if w, err := strconv.Atoi(strings.TrimSpace(left)); err == nil && w > 0 && w <= 32768 {
				if h, err := strconv.Atoi(strings.TrimSpace(right)); err == nil && h > 0 && h <= 32768 {
					return w
				}
			}
		}
		return 0
	}
}

// resolutionHeight returns a typical height for a resolution label.
func resolutionHeight(label string) int {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "4320p", "8k":
		return 4320
	case "2160p", "4k", "uhd":
		return 2160
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	default:
		if left, right, ok := strings.Cut(strings.TrimSpace(label), "x"); ok {
			if w, err := strconv.Atoi(strings.TrimSpace(left)); err == nil && w > 0 && w <= 32768 {
				if h, err := strconv.Atoi(strings.TrimSpace(right)); err == nil && h > 0 && h <= 32768 {
					return h
				}
			}
		}
		return 0
	}
}

// qualityRungHeightV3 maps a normalized quality preference to its resolution
// class height. Only explicit fixed rungs return a height; "auto" and
// "original" return 0 so the caller keeps the device ranking unchanged.
// Compound ladder rungs ("1080p-high") carry their resolution class in the
// label prefix.
func qualityRungHeightV3(qualityPreference string) int {
	normalized, _ := playback.NormalizeQualityV3(qualityPreference)
	class := normalized
	if idx := strings.IndexByte(class, '-'); idx > 0 {
		class = class[:idx]
	}
	switch class {
	case "2160p":
		return 2160
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	case "420p":
		return 420
	case "328p":
		return 328
	default:
		return 0
	}
}

// virtualCapRungHeightV3 derives a resolution-class height from a bandwidth
// cap, mirroring the planner's ladderHeightForBandwidthV3 thresholds so a
// client's delivery ceiling is honored when picking a native provider stream.
// The planner applies a 0.8 safety factor to the cap before selecting a rung
// (ladderHeightForBandwidthV3(int(float64(capKbps) * 0.8))), so the same
// factor is applied here to keep the virtual candidate pick consistent with
// the transcode ladder.
func virtualCapRungHeightV3(bandwidthCapKbps int) int {
	effective := int(float64(bandwidthCapKbps) * 0.8)
	switch {
	case effective >= 20_000:
		return 2160
	case effective >= 8_000:
		return 1080
	case effective >= 4_000:
		return 720
	default:
		return 480
	}
}

// reorderVirtualCandidatesForQuality prefers candidates whose resolution class
// is at or below the requested fixed rung (further constrained by the
// bandwidth cap), keeping the device ranking stable within each group. When no
// candidate matches the rung (a provider that only offers higher
// resolutions), the device ranking is returned unchanged. The reorder is a
// preference, never a hard filter: a client that asked for 720p still gets the
// best device-ranked stream when no native 720p-or-below candidate exists.
func reorderVirtualCandidatesForQuality(candidates []VirtualPlaybackStream, qualityPreference string, bandwidthCapKbps int) []VirtualPlaybackStream {
	rungHeight := qualityRungHeightV3(qualityPreference)
	if rungHeight <= 0 || len(candidates) <= 1 {
		return candidates
	}
	if bandwidthCapKbps > 0 {
		if capHeight := virtualCapRungHeightV3(bandwidthCapKbps); capHeight < rungHeight {
			rungHeight = capHeight
		}
	}
	preferred := make([]VirtualPlaybackStream, 0, len(candidates))
	rest := make([]VirtualPlaybackStream, 0, len(candidates))
	for _, cand := range candidates {
		height := resolutionHeight(cand.Resolution)
		if height > 0 && height <= rungHeight {
			preferred = append(preferred, cand)
		} else {
			rest = append(rest, cand)
		}
	}
	if len(preferred) == 0 {
		return candidates
	}
	return append(preferred, rest...)
}

// canSkipProbeForContainer returns true for container formats that ffmpeg
// handles natively without needing ffprobe metadata. When a candidate
// already declares codecs, we skip the probe for these formats.
// ffprobe may return compound formats like "matroska,webm" or capitalized
// variants like "Matroska"; we check whether any recognized token appears.
func canSkipProbeForContainer(container string) bool {
	lowered := strings.ToLower(strings.TrimSpace(container))
	if lowered == "" {
		return false
	}
	known := []string{"mp4", "mkv", "webm", "ts", "m2ts", "mov", "avi", "flv", "wmv", "m4v", "mpeg", "mpg", "ogv", "3gp", "matroska"}
	for _, k := range known {
		if lowered == k || strings.Contains(lowered, k) {
			return true
		}
	}
	return false
}

// completeVirtualVideoEvidenceV3 reports whether a virtual file carries the
// detailed ffprobe video evidence the v3 planner needs to validate direct-play
// and stream-copy remux routes (profile/level/bit-depth/dimensions/frame rate/
// bitrate), as opposed to bare candidate codec declarations.
func completeVirtualVideoEvidenceV3(file *models.MediaFile) bool {
	if file == nil || len(file.VideoTracks) == 0 || file.VideoTracks[0].Codec == "" {
		return false
	}
	track := file.VideoTracks[0]
	if track.Width <= 0 || track.Height <= 0 || track.FrameRate == "" {
		return false
	}
	if file.CodecVideo == "" || file.Resolution == "" {
		return false
	}
	return true
}

// completeVirtualAudioEvidenceV3 reports whether a virtual file carries the
// probed audio track evidence (codecs, channels, and layouts) required for
// the planner to validate direct play and surround sound passthrough.
func completeVirtualAudioEvidenceV3(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	if file.IsAudioOnly() {
		return len(file.AudioTracks) > 0 && file.CodecAudio != ""
	}
	return len(file.AudioTracks) > 0 && file.AudioTracks[0].Codec != "" && file.AudioTracks[0].Channels > 0
}

// completeVirtualContainerEvidenceV3 reports whether the file's real container
// is known and usable for a server-mediated route. The canonical virtual row
// keeps Container="virtual" until a probe fills it in; once a prior play
// persisted the real container (mkv/webm/mp4/...), re-probing the same stream
// on every cold start adds nothing but a provider round-trip.
func completeVirtualContainerEvidenceV3(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	return canSkipProbeForContainer(file.Container)
}

// marshalTracksJSON safely marshals track slices to JSON bytes for DB storage.
func marshalTracksJSON(tracks any) []byte {
	if tracks == nil {
		return []byte("[]")
	}
	data, err := json.Marshal(tracks)
	if err != nil {
		return []byte("[]")
	}
	return data
}

// sanitizeTrackSlice ensures the value is a slice/array, not a scalar.
// PostgreSQL jsonb array operations (like in triggers) fail with
// "cannot extract elements from a scalar" when given non-array jsonb.
func sanitizeTrackSlice(v any) any {
	if v == nil {
		return []any{}
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return v
	}
	// Wrap scalar in a slice
	return []any{v}
}

// virtualStickyPin remembers the last virtual candidate that played
// successfully for a content key.
type virtualStickyPin struct {
	uri      string
	pinnedAt time.Time
}

// virtualStickyTTL bounds how long a pin can steer selection without being
// re-confirmed by another successful play.
const (
	virtualStickyTTL     = 24 * time.Hour
	virtualStickyMaxPins = 4096
)

// virtualRuntimeTolerance is the maximum fraction a probed candidate duration
// may deviate from the catalog runtime before the candidate is treated as
// mislabeled content rather than a legitimate release.
//
// 0.30 accommodates anime and international content where fansub groups trim
// OP/ED sequences and streaming cuts differ from broadcast metadata by 3–5
// minutes on a typical 24-minute episode. Genuinely wrong content (a trailer,
// a sample, or a different show) differs by far more than this.
const virtualRuntimeTolerance = 0.30

// virtualRuntimePlausible reports whether a probed candidate's duration is
// consistent with the item's catalog runtime. Unknown values pass: the gate
// only rejects when both sides are known and clearly disagree.
func virtualRuntimePlausible(probedSeconds, runtimeMinutes int) bool {
	if probedSeconds <= 0 || runtimeMinutes <= 0 {
		return true
	}
	expected := float64(runtimeMinutes) * 60
	diff := math.Abs(float64(probedSeconds) - expected)
	return diff <= expected*virtualRuntimeTolerance
}

// peekVirtualSticky returns the pinned candidate URI for key, if fresh.
func (h *PlaybackHandler) peekVirtualSticky(key string) string {
	if h == nil || key == "" {
		return ""
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	pin, ok := h.virtualStickyPins[key]
	if !ok {
		return ""
	}
	if time.Since(pin.pinnedAt) > virtualStickyTTL {
		delete(h.virtualStickyPins, key)
		return ""
	}
	return pin.uri
}

// pinVirtualSticky records uri as the sticky candidate for key. The map grows
// by one entry per distinct virtual content key; entries expire lazily on
// access, so no sweeper goroutine is needed.
func (h *PlaybackHandler) pinVirtualSticky(key, uri string) {
	if h == nil || key == "" || uri == "" {
		return
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	if h.virtualStickyPins == nil {
		h.virtualStickyPins = make(map[string]virtualStickyPin)
	}
	// Opportunistic hygiene on write: drop expired pins and bound the map so
	// long-lived processes cannot accumulate one entry per content key forever.
	now := time.Now()
	for k, pin := range h.virtualStickyPins {
		if now.Sub(pin.pinnedAt) > virtualStickyTTL {
			delete(h.virtualStickyPins, k)
		}
	}
	for len(h.virtualStickyPins) >= virtualStickyMaxPins {
		oldestKey := ""
		var oldest time.Time
		for k, pin := range h.virtualStickyPins {
			if oldestKey == "" || pin.pinnedAt.Before(oldest) {
				oldestKey, oldest = k, pin.pinnedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(h.virtualStickyPins, oldestKey)
	}
	h.virtualStickyPins[key] = virtualStickyPin{uri: uri, pinnedAt: now}
}

// unpinVirtualSticky releases the pin for key when it still refers to uri,
// so a source that stopped working cannot steer later starts.
func (h *PlaybackHandler) unpinVirtualSticky(key, uri string) {
	if h == nil || key == "" || uri == "" {
		return
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	if pin, ok := h.virtualStickyPins[key]; ok && pin.uri == uri {
		delete(h.virtualStickyPins, key)
	}
}

// applyVirtualStickyPin moves the pinned candidate to the front of the list
// while it is still offered, provided it meets score parity with the device-preferred
// candidate at index 0. If the pinned candidate has vanished or has a lower compatibility
// score for the requesting device, it is not promoted.
func (h *PlaybackHandler) applyVirtualStickyPin(key, pinnedURI string, candidates []VirtualPlaybackStream, device ...plugins.DeviceCapabilities) []VirtualPlaybackStream {
	if pinnedURI == "" || len(candidates) == 0 {
		return candidates
	}
	pinnedIdx := -1
	for i, cand := range candidates {
		if cand.URI == pinnedURI {
			pinnedIdx = i
			break
		}
	}
	if pinnedIdx < 0 {
		// Pinned URI vanished from the offered set.
		h.unpinVirtualSticky(key, pinnedURI)
		return candidates
	}
	if pinnedIdx == 0 {
		return candidates
	}

	// Score parity guard: if device capabilities are present, only promote the pinned
	// candidate if its score is >= the device-ranked candidate at index 0.
	if len(device) > 0 {
		d := device[0]
		if len(d.CodecsVideo) > 0 || len(d.CodecsAudio) > 0 || d.HDR || d.DolbyVision || d.MaxResolution != "" {
			pinnedScore := plugins.ScoreCandidate(&candidates[pinnedIdx], d)
			topScore := plugins.ScoreCandidate(&candidates[0], d)
			if pinnedScore < topScore {
				return candidates
			}
		}
	}

	cand := candidates[pinnedIdx]
	reordered := make([]VirtualPlaybackStream, 0, len(candidates))
	reordered = append(reordered, cand)
	reordered = append(reordered, candidates[:pinnedIdx]...)
	reordered = append(reordered, candidates[pinnedIdx+1:]...)
	return reordered
}
