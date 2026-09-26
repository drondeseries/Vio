package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	maxConcurrentCopySeekProbes = 4
	// CopySeekProbeTimeout is the per-attempt cap for one copy-video seek-anchor
	// probe. It is exported so the replan retry can decide whether another
	// attempt fits the caller's remaining budget; the probe itself is capped at
	// the smaller of this and the caller's remaining deadline.
	CopySeekProbeTimeout = 15 * time.Second

	// copySeekAnchorCacheTTL bounds how long a resolved anchor is reused. The
	// anchor is a property of the source bytes at a seek position, not of the
	// transport URL used to reach them: a virtual source resolves to the same
	// underlying release for the lifetime of a session's candidate. The probe
	// costs roughly 1.9s of session startup, so a short in-process TTL removes
	// that cost from resume starts, replans, and concurrent starts at the same
	// position without risking a stale anchor.
	copySeekAnchorCacheTTL = 10 * time.Minute
	// copySeekAnchorCacheMax bounds the cache. Entries are tiny and the live
	// working set is the distinct (source, requested position) pairs being
	// resumed.
	copySeekAnchorCacheMax = 256
)

var (
	copySeekProbeGroup singleflight.Group
	copySeekProbeSlots = make(chan struct{}, maxConcurrentCopySeekProbes)
	// copySeekAnchors is the process-wide resolved-anchor cache. Tests replace
	// it to inject a clock and probe runner.
	copySeekAnchors = &copySeekAnchorCache{
		entries:    make(map[string]copySeekAnchorCacheEntry),
		ttl:        copySeekAnchorCacheTTL,
		maxEntries: copySeekAnchorCacheMax,
		probe:      resolveCopySeekAnchor,
	}
)

type copySeekAnchor struct {
	seconds float64
	segment int
}

// ErrTransientProvider marks an ffmpeg failure whose cause is the upstream
// provider answering an HTTP 5xx, as opposed to the source bytes being
// undecodable or the request being canceled. Callers retry it with a bounded
// backoff instead of treating the candidate as dead or hammering the provider.
var ErrTransientProvider = errors.New("transient provider error")

// IsTransientProviderError reports whether err is (or wraps) a transient
// provider failure, so a retry path can back off before re-probing the same
// candidate.
func IsTransientProviderError(err error) bool {
	return errors.Is(err, ErrTransientProvider)
}

// upstream5xxMarkers are the ffmpeg stderr fragments that mean the upstream
// answered 5xx. ffmpeg's HTTP protocol logs "Server returned 5XX Server Error
// reply" (and the concrete 5xx codes) before "Error opening input file"; the
// status-class phrase is the stable part. Only 5xx counts here: 4xx is a
// configuration/authentication problem the provider will keep refusing, so a
// retry would not help and it is left as an ordinary failure.
var upstream5xxMarkers = []string{
	"Server returned 5XX",
	"Server returned 500",
	"Server returned 501",
	"Server returned 502",
	"Server returned 503",
	"Server returned 504",
	"Server returned 505",
}

// transientProviderCause wraps the command error with ErrTransientProvider when
// the probe's stderr tail names an upstream 5xx. The original error is kept so
// the exit status and message still reach the caller.
func transientProviderCause(err error, stderrTail string) error {
	for _, marker := range upstream5xxMarkers {
		if strings.Contains(stderrTail, marker) {
			return fmt.Errorf("%w: %w", ErrTransientProvider, err)
		}
	}
	return err
}

// anchorProbeRunner runs the FFmpeg observation that produces an anchor. It is
// a seam so tests can exercise caching without spawning a process.
type anchorProbeRunner func(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error)

type copySeekAnchorCacheEntry struct {
	anchor    copySeekAnchor
	expiresAt time.Time
}

// copySeekAnchorCache memoizes anchors by stable source identity and exact
// requested position so a resume start does not pay for a fresh FFmpeg probe.
// now and probe are injectable for tests and default to time.Now and the real
// probe.
type copySeekAnchorCache struct {
	mu         sync.Mutex
	entries    map[string]copySeekAnchorCacheEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	probe      anchorProbeRunner
}

func (c *copySeekAnchorCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *copySeekAnchorCache) get(key string) (copySeekAnchor, bool) {
	if c == nil || key == "" {
		return copySeekAnchor{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return copySeekAnchor{}, false
	}
	if !c.clock().Before(entry.expiresAt) {
		delete(c.entries, key)
		return copySeekAnchor{}, false
	}
	return entry.anchor, true
}

func (c *copySeekAnchorCache) set(key string, anchor copySeekAnchor) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]copySeekAnchorCacheEntry)
	}
	now := c.clock()
	// Drop expired entries first so a bounded cache is not crowded by entries
	// that will never be served again.
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
	c.entries[key] = copySeekAnchorCacheEntry{anchor: anchor, expiresAt: now.Add(c.ttl)}
}

// copySeekAnchorCacheKey identifies one (source, exact requested position)
// observation. The resolved FFmpeg path stays in the key because a different
// build can copy a different packet at the same position; the requested
// position stays exact because the anchor is defined as the keyframe FFmpeg's
// input seek actually uses for that exact -ss, and two nearby positions can
// straddle a keyframe boundary and resolve different anchors. A bucketed key
// paired with an exact-position probe served the neighbor's anchor for the
// straddling position, so the position is never rounded. sourceIdentity — not
// inputPath — is in the key so concurrent resume starts that resolve to
// different pinned relay URLs still share one probe, and segmentDuration is
// kept because the returned segment index is derived from it.
func copySeekAnchorCacheKey(ffmpegPath, sourceIdentity string, requestedSeekSeconds float64, segmentDuration int) string {
	return strings.Join([]string{
		ffmpegPath,
		sourceIdentity,
		strconv.FormatFloat(requestedSeekSeconds, 'g', -1, 64),
		strconv.Itoa(segmentDuration),
	}, "\x00")
}

// copySeekAnchorProbe returns the configured probe runner, falling back to the
// real FFmpeg probe when a test has not installed one.
func (c *copySeekAnchorCache) probeRunner() anchorProbeRunner {
	if c != nil && c.probe != nil {
		return c.probe
	}
	return resolveCopySeekAnchor
}

// ResolveCopySeekAnchor returns the keyframe timestamp FFmpeg's input seek will
// actually use for a copy-video restart. FFmpeg cannot discard the pre-roll
// between that keyframe and requestedSeekSeconds while preserving -c:v copy,
// so callers need both timestamps: requestedSeekSeconds remains the -ss input,
// while the returned anchor defines the stream's real timeline origin.
//
// The one-packet framecrc output is intentional. ffprobe read intervals discard
// Matroska pre-roll at an exact keyframe boundary, while FFmpeg stream copy
// emits that preceding packet. Running FFmpeg with the transport's real seek
// and timestamp policy observes the packet the HLS muxer will actually receive
// without decoding or writing media output. start_at_zero matches the transport
// and makes the returned origin relative to the source start, not raw input PTS.
func ResolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	return ResolveCopySeekAnchorForSource(
		ctx, ffmpegPath, inputPath, inputPath, requestedSeekSeconds, segmentDuration,
	)
}

// ResolveCopySeekAnchorForSource is ResolveCopySeekAnchor with a stable source
// identity for the cache and singleflight key. inputPath is the concrete probe
// target: for a virtual source it is the per-call pinned-IP relay URL.
// sourceIdentity is the provider-neutral URI or on-disk path that names the
// same source across calls, so concurrent resume starts that resolve to
// different relay URLs still share one probe. The anchor is a property of the
// source bytes at the requested position, and the caller's candidate points at
// one underlying release for the session, so a short-TTL cache is safe.
//
// The cache is keyed on the exact requested position. A nearby seek can
// straddle a keyframe boundary and resolve a different anchor, so it re-probes;
// only a repeat of the same position is served from the cache.
func ResolveCopySeekAnchorForSource(
	ctx context.Context,
	ffmpegPath string,
	sourceIdentity string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	if requestedSeekSeconds <= 0 {
		return 0, 0, nil
	}
	if strings.TrimSpace(inputPath) == "" {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: empty input path")
	}
	if segmentDuration <= 0 {
		segmentDuration = DefaultSegmentDuration
	}

	resolvedFFmpegPath := ResolveFFmpegPath(ffmpegPath)
	identity := strings.TrimSpace(sourceIdentity)
	if identity == "" {
		identity = inputPath
	}
	key := copySeekAnchorCacheKey(resolvedFFmpegPath, identity, requestedSeekSeconds, segmentDuration)

	// Fast path: a previous call already resolved this exact anchor. This is
	// what turns a repeat resume at the same position into a zero-probe start.
	if anchor, ok := copySeekAnchors.get(key); ok {
		return anchor.seconds, anchor.segment, nil
	}

	resultCh := copySeekProbeGroup.DoChan(key, func() (any, error) {
		// Another caller may have populated the cache between the fast-path
		// check and this closure starting.
		if anchor, ok := copySeekAnchors.get(key); ok {
			return anchor, nil
		}
		budget, shortened := copySeekProbeTimeoutFor(ctx)
		if budget <= 0 {
			return copySeekAnchor{}, context.DeadlineExceeded
		}
		if shortened {
			slog.InfoContext(ctx, "copy seek anchor probe shortened to the caller's remaining budget",
				"component", "playback",
				"budget", budget,
				"cap", CopySeekProbeTimeout,
			)
		}
		// WithoutCancel keeps the probe alive across a client disconnect so a
		// completed observation can still populate the cache, but the derived
		// deadline is the caller's remaining budget: the probe can no longer
		// outlive the request by a full probe timeout.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		select {
		case copySeekProbeSlots <- struct{}{}:
			defer func() { <-copySeekProbeSlots }()
		case <-probeCtx.Done():
			slog.WarnContext(ctx, "copy seek anchor probe skipped: caller budget expired before a probe slot",
				"component", "playback",
				"budget", budget,
			)
			return copySeekAnchor{}, probeCtx.Err()
		}
		seconds, segment, err := copySeekAnchors.probeRunner()(probeCtx, resolvedFFmpegPath, inputPath, requestedSeekSeconds, segmentDuration)
		if err != nil {
			// Probe failures are not cached: a transient upstream timeout must
			// be retried by the next caller rather than pinned for the TTL.
			return copySeekAnchor{}, err
		}
		anchor := copySeekAnchor{seconds: seconds, segment: segment}
		copySeekAnchors.set(key, anchor)
		return anchor, nil
	})

	select {
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return 0, 0, result.Err
		}
		anchor, ok := result.Val.(copySeekAnchor)
		if !ok {
			return 0, 0, fmt.Errorf("unexpected copy seek anchor result %T", result.Val)
		}
		return anchor.seconds, anchor.segment, nil
	}
}

// copySeekProbeTimeoutFor derives one probe's timeout from the caller's
// remaining deadline. The probe must not outlive the request by a full probe
// timeout: a resumed session's provider re-list can consume most of the replan
// budget, so a fixed 15s probe that ignored the caller's deadline was killed by
// our own timeout while the caller's budget was already gone. The returned
// budget is the smaller of CopySeekProbeTimeout and the remaining deadline;
// shortened reports that the caller's deadline, not the probe cap, set it. A
// non-positive budget means the caller had no time left and the probe must not
// run. A caller with no deadline keeps the full probe cap.
func copySeekProbeTimeoutFor(ctx context.Context) (time.Duration, bool) {
	if ctx == nil {
		return CopySeekProbeTimeout, false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return CopySeekProbeTimeout, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, true
	}
	if remaining < CopySeekProbeTimeout {
		return remaining, true
	}
	return CopySeekProbeTimeout, false
}

func resolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-v", "error",
		"-fflags", "+genpts+fastseek",
		"-analyzeduration", "3000000",
		"-probesize", "5000000",
		"-ss", fmt.Sprintf("%.3f", requestedSeekSeconds),
		"-i", inputPath,
		// Match the remux transport's 0:V:0 map. Uppercase V excludes attached
		// pictures, thumbnails, and cover art from the video stream ordinal.
		"-map", "0:V:0",
		"-c:v", "copy",
		"-copyts",
		"-start_at_zero",
		"-avoid_negative_ts", "disabled",
		"-frames:v", "1",
		"-f", "framecrc",
		"-",
	)
	var stdout bytes.Buffer
	stderr := newBoundedTailBuffer(stderrTailMaxBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		tail := truncateStderr(stderr.String())
		// Classify the full tail, not the truncated message: an upstream 5xx is
		// a transient provider failure a caller should back off and retry, while
		// a decoder/stream error is not.
		cause := transientProviderCause(err, stderr.String())
		if tail != "" {
			return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg failed: %w (stderr: %s)", cause, tail)
		}
		return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg failed: %w", cause)
	}

	timeBaseNumerator, timeBaseDenominator := int64(0), int64(0)
	for line := range bytes.SplitSeq(stdout.Bytes(), []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "#tb 0:") {
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(trimmed, "#tb 0:")), "/")
			if len(parts) != 2 {
				continue
			}
			timeBaseNumerator, _ = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			timeBaseDenominator, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || timeBaseNumerator <= 0 || timeBaseDenominator <= 0 {
			continue
		}
		fields := strings.Split(trimmed, ",")
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != "0" {
			continue
		}
		timestamp := strings.TrimSpace(fields[2])
		if timestamp == "" || strings.EqualFold(timestamp, "N/A") {
			timestamp = strings.TrimSpace(fields[1])
		}
		timestampTicks, err := strconv.ParseInt(timestamp, 10, 64)
		anchor := float64(timestampTicks) * float64(timeBaseNumerator) / float64(timeBaseDenominator)
		if err != nil || math.IsNaN(anchor) || math.IsInf(anchor, 0) {
			continue
		}
		anchor = math.Max(0, anchor)
		return anchor, int(anchor / float64(segmentDuration)), nil
	}

	return 0, 0, fmt.Errorf("resolve copy seek anchor: ffmpeg returned no video packet timestamp")
}
