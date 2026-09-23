package altmount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

const (
	defaultAltmountCheckMinutes = 15
	minAltmountCheckMinutes     = 5
	maxAltmountCheckMinutes     = 10080
	maxAltmountBodyBytes        = 32 << 20
	maxAltmountStateBytes       = 16 << 20
	maxAltmountHistorySlots     = 50000
	// maxErrorBodyBytes bounds the response snippet attached to an enqueue
	// error, mirroring the Prowlarr client's error budget.
	maxErrorBodyBytes = 4 << 10
	// altmountStateRetention bounds how long a completed/failed release keeps
	// influencing playback preference. The underlying NZB and its storage are
	// normally gone well before this, so it is a safety cap, not a policy knob.
	altmountStateRetention = 30 * 24 * time.Hour
	// altmountCachedBadge is the prefix AltMount's own Stremio addon puts on
	// stream names for releases already imported and still fresh. It is a
	// zero-config completion signal available even when the API is not wired.
	altmountCachedBadge = "⚡ cached"
)

var altmountHTTPClient = newRestrictedRedirectHTTPClient(20 * time.Second)

// altmountReleaseRecord is one release's source-of-truth state as reported by
// AltMount's SABnzbd-compatible history API.
type altmountReleaseRecord struct {
	Size        int64 `json:"size,omitempty"`
	CompletedAt int64 `json:"completed_at,omitempty"`
}

type altmountStateSnapshot struct {
	Completed map[string]altmountReleaseRecord `json:"completed"`
	Failed    map[string]altmountReleaseRecord `json:"failed"`
}

// altmountStateClient caches which releases AltMount reports as completed
// (imported, with storage) versus failed. It is refreshed on the scheduled
// monitor task, never on the playback path, and persists to disk so a restart
// keeps the known-good signal.
type altmountStateClient struct {
	mu        sync.Mutex
	url       string
	apiKey    string
	interval  time.Duration
	client    *http.Client
	lastFetch time.Time
	lastErr   error
	state     altmountStateSnapshot
	indexFile string
}

// Client is the exported AltMount SABnzbd-history state client. It aliases
// the ported implementation verbatim so the resolver can consume it as a
// CandidateClassifier.
type Client = altmountStateClient

// StateClient is an alias of Client kept for call-site readability.
type StateClient = altmountStateClient

// candidateClassifier mirrors the resolver's CandidateClassifier interface so
// a compile-time assertion guards the port without importing the resolver.
type candidateClassifier interface {
	ClassifyCandidates(candidates []stream.StreamCandidate)
}

var _ candidateClassifier = (*Client)(nil)

func newAltmountStateClient(client *http.Client) *altmountStateClient {
	if client == nil {
		client = altmountHTTPClient
	}
	return &altmountStateClient{client: client}
}

// New creates an AltMount state client with the given HTTP client (nil selects
// the shared restricted-redirect client).
func New(client *http.Client) *Client {
	return newAltmountStateClient(client)
}

// NewStateClient is an alias of New kept for call-site readability.
func NewStateClient(client *http.Client) *StateClient {
	return newAltmountStateClient(client)
}

// NewClient is an alias of New kept for call-site readability.
func NewClient(client *http.Client) *Client {
	return newAltmountStateClient(client)
}

// URL returns the configured AltMount base URL, or empty string.
func (c *altmountStateClient) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// Configure sets the AltMount base URL, SABnzbd API key, and refresh interval.
func (c *altmountStateClient) Configure(baseURL, apiKey string, intervalMinutes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	newKey := strings.TrimSpace(apiKey)
	if intervalMinutes < minAltmountCheckMinutes {
		intervalMinutes = defaultAltmountCheckMinutes
	}
	if intervalMinutes > maxAltmountCheckMinutes {
		intervalMinutes = maxAltmountCheckMinutes
	}
	newInterval := time.Duration(intervalMinutes) * time.Minute
	if newURL != c.url || newKey != c.apiKey || newInterval != c.interval {
		c.state = altmountStateSnapshot{}
		c.lastFetch = time.Time{}
		c.lastErr = nil
	}
	c.url = newURL
	c.apiKey = newKey
	c.interval = newInterval
}

func (c *altmountStateClient) ConfigureIndexFile(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		path = ".vio-virtual-library-altmount-state.json"
	}
	c.indexFile = path
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open AltMount state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAltmountStateBytes+1))
	if err != nil {
		return fmt.Errorf("read AltMount state: %w", err)
	}
	if len(data) > maxAltmountStateBytes {
		return fmt.Errorf("AltMount state exceeds %d bytes", maxAltmountStateBytes)
	}
	var snapshot altmountStateSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode AltMount state: %w", err)
	}
	c.state = pruneAltmountSnapshot(snapshot, time.Now())
	return nil
}

func (c *altmountStateClient) Stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url == "" || c.lastFetch.IsZero() || time.Since(c.lastFetch) >= c.interval
}

func (c *altmountStateClient) refreshIfStale(ctx context.Context) error {
	if !c.Stale() {
		return nil
	}
	return c.refresh(ctx)
}

// altmountSABnzbdBase normalizes a configured AltMount base URL to the
// SABnzbd-compatible API path. AltMount serves that API at /sabnzbd/api (the
// path Radarr/Sonarr reach after appending /api to their SABnzbd base URL), so
// an operator may enter either the host root or "/sabnzbd".
func altmountSABnzbdBase(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	lower := strings.ToLower(raw)
	for _, suffix := range []string{"/sabnzbd/api", "/sabnzbd"} {
		if strings.HasSuffix(lower, suffix) {
			raw = raw[:len(raw)-len(suffix)]
			break
		}
	}
	return strings.TrimRight(raw, "/") + "/sabnzbd/api"
}

// historyURL builds the SABnzbd-compatible history endpoint without embedding
// the API key in the URL, so it can never leak through an error or log line.
func (c *altmountStateClient) historyURL() (string, error) {
	c.mu.Lock()
	raw := c.url
	c.mu.Unlock()
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("AltMount URL is not configured")
	}
	u, err := url.Parse(altmountSABnzbdBase(raw))
	if err != nil {
		return "", fmt.Errorf("invalid AltMount URL: %w", err)
	}
	q := u.Query()
	q.Set("mode", "history")
	q.Set("output", "json")
	q.Set("limit", "10000")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Enqueue hands one release's download URL to AltMount's SABnzbd-compatible
// addurl endpoint and returns the nzo_id AltMount assigns. It is the package's
// only write: the state client is otherwise read-only.
//
// The URL and the SABnzbd API key are secrets and are never part of any
// returned error or log line. A status:false response or an empty nzo_ids list
// is a failure, reported with a redacted body snippet so the operator can see
// why the provider refused without the release URL leaking.
//
// The request runs under the caller's ctx bounded by an internal 15s cap, so a
// hung AltMount cannot outlive the request lifetime. It reuses the shared
// restricted-redirect client (cross-origin redirects are rejected) so a
// compromised provider cannot redirect the internal download URL elsewhere.
func (c *altmountStateClient) Enqueue(ctx context.Context, downloadURL, name string) (string, error) {
	downloadURL = strings.TrimSpace(downloadURL)
	if downloadURL == "" {
		return "", errors.New("AltMount enqueue requires a download URL")
	}
	c.mu.Lock()
	raw := c.url
	key := c.apiKey
	c.mu.Unlock()
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("AltMount URL is not configured")
	}
	u, err := url.Parse(altmountSABnzbdBase(raw))
	if err != nil {
		return "", fmt.Errorf("invalid AltMount URL: %w", err)
	}
	q := u.Query()
	q.Set("mode", "addurl")
	q.Set("name", downloadURL)
	if nzbName := strings.TrimSpace(name); nzbName != "" {
		q.Set("nzbname", nzbName)
	}
	q.Set("output", "json")
	if key != "" {
		q.Set("apikey", key)
	}
	u.RawQuery = q.Encode()

	enqueueCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(enqueueCtx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// The transport error may embed the request URL (which carries the
		// download URL and key); report only that the request failed.
		return "", errors.New("AltMount enqueue request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AltMount enqueue returned status %d: %s", resp.StatusCode, c.redactedSnippet(resp.Body, downloadURL))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAltmountBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read AltMount enqueue response: %w", err)
	}
	if int64(len(body)) > maxAltmountBodyBytes {
		return "", fmt.Errorf("AltMount enqueue response exceeds %d bytes", maxAltmountBodyBytes)
	}
	var payload struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode AltMount enqueue response: %w", err)
	}
	if !payload.Status || len(payload.NzoIDs) == 0 || strings.TrimSpace(payload.NzoIDs[0]) == "" {
		return "", fmt.Errorf("AltMount rejected the enqueue: %s", c.redactEnqueueBody(body, downloadURL))
	}
	return strings.TrimSpace(payload.NzoIDs[0]), nil
}

// redactedSnippet reads a bounded response body for an error message and
// strips the configured API key and the submitted download URL. It never
// includes the request URL.
func (c *altmountStateClient) redactedSnippet(body io.Reader, downloadURL string) string {
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if len(data) > maxErrorBodyBytes {
		data = data[:maxErrorBodyBytes]
	}
	snippet := strings.TrimSpace(string(data))
	if snippet == "" {
		snippet = "(empty response body)"
	}
	return redactAltmountSecret(snippet, key, downloadURL)
}

// redactEnqueueBody trims a SABnzbd JSON failure body and strips the API key
// and submitted download URL (a provider may echo the URL in its error).
func (c *altmountStateClient) redactEnqueueBody(body []byte, downloadURL string) string {
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		snippet = "(empty response body)"
	}
	return redactAltmountSecret(snippet, key, downloadURL)
}

func redactAltmountSecret(value, key, downloadURL string) string {
	if key != "" {
		value = strings.ReplaceAll(value, key, "[redacted]")
	}
	if downloadURL != "" {
		value = strings.ReplaceAll(value, downloadURL, "[redacted]")
	}
	return value
}

// Refresh performs a history fetch now and persists the merged snapshot.
// It is the exported form of the ported refresh step; HTTP logic is identical.
func (c *altmountStateClient) Refresh(ctx context.Context) error {
	return c.refresh(ctx)
}

func (c *altmountStateClient) refresh(ctx context.Context) error {
	historyURL, err := c.historyURL()
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, historyURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		req.Header.Set("X-Api-Key", key)
		// SABnzbd clients pass the key in the query; AltMount accepts both.
		q := req.URL.Query()
		q.Set("apikey", key)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := c.client.Do(req)
	if err != nil {
		err = errors.New("AltMount history request failed")
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("AltMount history returned status %d", resp.StatusCode)
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	incoming, err := parseAltmountHistory(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
	if err != nil {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	merged := mergeAltmountSnapshots(c.state, incoming, time.Now())
	c.state = merged
	c.lastFetch = time.Now()
	c.lastErr = nil
	indexFile := c.indexFile
	c.mu.Unlock()
	if indexFile != "" {
		if err := saveAltmountState(indexFile, merged); err != nil {
			return err
		}
	}
	return nil
}

func parseAltmountHistory(r io.Reader, now time.Time) (altmountStateSnapshot, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAltmountBodyBytes+1))
	if err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("read AltMount history: %w", err)
	}
	if int64(len(data)) > maxAltmountBodyBytes {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount history exceeds %d bytes", maxAltmountBodyBytes)
	}
	var payload struct {
		History struct {
			Slots []struct {
				Name         string `json:"name"`
				NzbName      string `json:"nzb_name"`
				Status       string `json:"status"`
				Storage      string `json:"storage"`
				Path         string `json:"path"`
				Bytes        int64  `json:"bytes"`
				Completetime int64  `json:"completetime"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return altmountStateSnapshot{}, fmt.Errorf("decode AltMount history: %w", err)
	}
	snapshot := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	if len(payload.History.Slots) > maxAltmountHistorySlots {
		return altmountStateSnapshot{}, fmt.Errorf("AltMount history exceeds %d slots", maxAltmountHistorySlots)
	}
	for _, slot := range payload.History.Slots {
		completedAt := slot.Completetime
		if completedAt <= 0 {
			completedAt = now.Unix()
		}
		record := altmountReleaseRecord{Size: slot.Bytes, CompletedAt: completedAt}
		keys := altmountSlotKeys(slot.Name, slot.NzbName, slot.Storage, slot.Path)
		switch {
		case strings.EqualFold(slot.Status, "Completed") && strings.TrimSpace(slot.Storage) != "":
			for _, key := range keys {
				snapshot.Completed[key] = record
			}
		case strings.EqualFold(slot.Status, "Failed"):
			for _, key := range keys {
				snapshot.Failed[key] = record
			}
		}
	}
	return snapshot, nil
}

// altmountSlotKeys returns every normalized identity a history slot exposes.
// AltMount's own cache predicate matches on nzb path, storage path, and
// relative path, so mirroring all of them avoids missing a match when the
// display name differs from the stored filename.
func altmountSlotKeys(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	keys := make([]string, 0, len(values))
	for _, value := range values {
		key := releaseNameKey(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// mergeAltmountSnapshots resolves per-key state by newest report, so a release
// that completed and later failed (or vice versa) reflects AltMount's latest
// answer. Equal timestamps prefer completed, since a completed import is the
// stronger, non-destructive signal.
func mergeAltmountSnapshots(existing, incoming altmountStateSnapshot, now time.Time) altmountStateSnapshot {
	type entry struct {
		record altmountReleaseRecord
		failed bool
	}
	all := map[string]entry{}
	consider := func(records map[string]altmountReleaseRecord, failed bool) {
		for key, record := range records {
			current, ok := all[key]
			if !ok || record.CompletedAt > current.record.CompletedAt ||
				(record.CompletedAt == current.record.CompletedAt && !failed && current.failed) {
				all[key] = entry{record: record, failed: failed}
			}
		}
	}
	consider(existing.Completed, false)
	consider(existing.Failed, true)
	consider(incoming.Completed, false)
	consider(incoming.Failed, true)
	merged := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	for key, item := range all {
		if item.failed {
			merged.Failed[key] = item.record
		} else {
			merged.Completed[key] = item.record
		}
	}
	return pruneAltmountSnapshot(merged, now)
}

func pruneAltmountSnapshot(snapshot altmountStateSnapshot, now time.Time) altmountStateSnapshot {
	pruned := altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	cutoff := now.Add(-altmountStateRetention).Unix()
	for key, record := range snapshot.Completed {
		if record.CompletedAt != 0 && record.CompletedAt < cutoff {
			continue
		}
		pruned.Completed[key] = record
	}
	for key, record := range snapshot.Failed {
		if record.CompletedAt != 0 && record.CompletedAt < cutoff {
			continue
		}
		pruned.Failed[key] = record
	}
	return pruned
}

func saveAltmountState(path string, snapshot altmountStateSnapshot) error {
	if snapshot.Completed == nil {
		snapshot.Completed = map[string]altmountReleaseRecord{}
	}
	if snapshot.Failed == nil {
		snapshot.Failed = map[string]altmountReleaseRecord{}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxAltmountStateBytes {
		return fmt.Errorf("AltMount state exceeds %d bytes", maxAltmountStateBytes)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".silo-altmount-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ClassifyCandidates applies AltMount's authoritative state: completed
// releases are marked SourceConfirmed, failed releases SourceFailed. A
// completed record always wins over a stale failed one.
func (c *altmountStateClient) ClassifyCandidates(candidates []stream.StreamCandidate) {
	if c == nil || len(candidates) == 0 {
		return
	}
	c.mu.Lock()
	completed := c.state.Completed
	failed := c.state.Failed
	c.mu.Unlock()
	if len(completed) == 0 && len(failed) == 0 {
		return
	}
	for i := range candidates {
		key := candidateReleaseName(candidates[i])
		if key == "" {
			continue
		}
		if record, ok := completed[key]; ok && releaseSizesMatch(record.Size, candidates[i].FileSize) {
			candidates[i].SourceConfirmed = true
			continue
		}
		if _, ok := failed[key]; ok {
			candidates[i].SourceFailed = true
		}
	}
}

// markAltmountBadgeCandidates honors the completion badge AltMount's Stremio
// addon already puts on imported, fresh releases. This needs no API
// configuration and lets the plugin respect AltMount's cached-first ordering
// instead of re-sorting it away.
func markAltmountBadgeCandidates(candidates []stream.StreamCandidate) {
	for i := range candidates {
		name := strings.ToLower(candidates[i].Name)
		if strings.Contains(name, altmountCachedBadge) {
			candidates[i].SourceConfirmed = true
		}
	}
}

// Validate performs a one-shot history fetch and returns a human-readable
// status for TestConnection.
func (c *altmountStateClient) Validate(ctx context.Context) (string, error) {
	historyURL, err := c.historyURL()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, historyURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	key := c.apiKey
	c.mu.Unlock()
	if key != "" {
		q := req.URL.Query()
		q.Set("apikey", key)
		req.URL.RawQuery = q.Encode()
	}
	validateClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := validateClient.Do(req)
	if err != nil {
		return "", errors.New("connect to AltMount failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AltMount returned HTTP %d", resp.StatusCode)
	}
	snapshot, err := parseAltmountHistory(io.LimitReader(resp.Body, maxAltmountBodyBytes+1), time.Now())
	if err != nil {
		return "", fmt.Errorf("parse AltMount history: %w", err)
	}
	return fmt.Sprintf("AltMount history OK: %d completed, %d failed releases", len(snapshot.Completed), len(snapshot.Failed)), nil
}

// --- Release-identity and HTTP helpers ported verbatim with the AltMount
// state client. They mirror the plugin's prowlarr_search.go and main.go so
// the package compiles standalone without importing unexported resolver
// helpers. HTTP logic is identical. ---

// prowlarrCleanPattern reduces a release name to a comparable identity:
// lowercase, extension stripped, all non-alphanumerics removed.
var prowlarrCleanPattern = regexp.MustCompile(`[^a-z0-9]+`)

// releaseNameKey reduces a release or provider filename to a comparable
// identity: lowercase, extension stripped, all non-alphanumerics removed. Two
// postings of the same scene release normalize to the same key even when one
// uses dots and the other spaces.
func releaseNameKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if idx := strings.IndexAny(value, "?#"); idx != -1 {
		value = value[:idx]
	}
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".mov", ".webm"} {
		value = strings.TrimSuffix(value, ext)
	}
	return prowlarrCleanPattern.ReplaceAllString(value, "")
}

func firstReleaseLine(value string) string {
	if idx := strings.IndexAny(value, "\r\n"); idx >= 0 {
		return value[:idx]
	}
	return value
}

// candidateReleaseName returns the most release-like identity carried by a
// provider candidate. AltMount-style providers put the release name on the
// first line of the stream title (the remaining lines carry size and indexer
// badges), so only the first line is considered.
func candidateReleaseName(candidate stream.StreamCandidate) string {
	for _, value := range []string{
		candidate.BehaviorHints.Filename,
		firstReleaseLine(candidate.Title),
		firstReleaseLine(candidate.Name),
	} {
		if key := releaseNameKey(value); key != "" {
			return key
		}
	}
	if parsed, err := url.Parse(strings.TrimSpace(candidate.URL)); err == nil {
		if key := releaseNameKey(path.Base(parsed.Path)); key != "" {
			return key
		}
	}
	return ""
}

// releaseSizesMatch reports whether two byte sizes plausibly describe the same
// file. Provider display sizes are rounded (and parsed as decimal GB), so the
// tolerance is wider than a byte-exact comparison. Unknown sizes never reject a
// match.
func releaseSizesMatch(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	largest := a
	if b > largest {
		largest = b
	}
	return float64(diff)/float64(largest) <= 0.10
}

// sameParentDomain reports whether host a and host b share at least two
// rightmost domain labels (e.g. "v3-cinemeta.strem.io" and
// "cinemeta-live.strem.io" both end with ".strem.io").
func sameParentDomain(a, b string) bool {
	aParts := strings.Split(strings.TrimSuffix(a, "."), ".")
	bParts := strings.Split(strings.TrimSuffix(b, "."), ".")
	if len(aParts) < 3 || len(bParts) < 3 {
		return false
	}
	aLast := strings.ToLower(strings.Join(aParts[len(aParts)-2:], "."))
	bLast := strings.ToLower(strings.Join(bParts[len(bParts)-2:], "."))
	return aLast == bLast
}

func newRestrictedRedirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if len(via) == 0 {
				return nil
			}
			origin := via[0].URL
			target := request.URL
			if target.User != nil {
				return errors.New("redirect with userinfo is not allowed")
			}
			if !strings.EqualFold(origin.Scheme, target.Scheme) {
				return errors.New("cross-scheme redirects are not allowed")
			}
			if strings.EqualFold(origin.Host, target.Host) {
				return nil
			}
			// Allow same-registered-domain redirects so well-known
			// metadata providers redirect within their own domain.
			if sameParentDomain(origin.Host, target.Host) {
				return nil
			}
			return errors.New("cross-origin redirects are not allowed")
		},
	}
}

// RefreshIfStale refreshes the completed/failed state when the configured
// interval has elapsed (exported wrapper for the monitor package; logic
// unchanged).
func (c *altmountStateClient) RefreshIfStale(ctx context.Context) error {
	return c.refreshIfStale(ctx)
}
