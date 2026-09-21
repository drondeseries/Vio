package playback

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveCopySeekAnchorUsesKeyPacketTimestamp(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	argsPath := filepath.Join(dir, "ffmpeg-args")
	probe := `#!/bin/sh
printf '%s\n' "$@" > "` + argsPath + `"
printf '%s\n' '#tb 0: 1/1000'
printf '%s\n' '0,      14500,      14750,       41,   178989, 0xd16e41c4'
`
	if err := os.WriteFile(ffmpegPath, []byte(probe), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	anchor, segment, err := ResolveCopySeekAnchor(context.Background(), ffmpegPath, "/media/movie.mkv", 18.261, 2)
	if err != nil {
		t.Fatalf("ResolveCopySeekAnchor: %v", err)
	}
	if anchor != 14.75 || segment != 7 {
		t.Fatalf("resolved anchor = %v, segment = %d; want 14.75, 7", anchor, segment)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read ffmpeg args: %v", err)
	}
	for _, want := range []string{"-fflags\n+genpts+fastseek\n", "-analyzeduration\n3000000\n", "-probesize\n5000000\n", "-ss\n18.261\n", "-map\n0:V:0\n", "-c:v\ncopy\n", "-copyts\n", "-avoid_negative_ts\ndisabled\n", "-frames:v\n1\n", "-f\nframecrc\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("ffmpeg args missing %q:\n%s", want, args)
		}
	}
}

func TestResolveCopySeekAnchorCoalescesMatchingConcurrentProbes(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	countPath := filepath.Join(dir, "probe-count")
	probe := "#!/bin/sh\n" +
		"printf x >> \"" + countPath + "\"\n" +
		"sleep 0.1\n" +
		"printf '%s\\n' '#tb 0: 1/1000'\n" +
		"printf '%s\\n' '0,      15833,      16000,       41,   178989, 0xd16e41c4'\n"
	if err := os.WriteFile(ffmpegPath, []byte(probe), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			anchor, segment, err := ResolveCopySeekAnchor(context.Background(), ffmpegPath, "/media/movie.mkv", 18, 2)
			if err == nil && (anchor != 16 || segment != 8) {
				err = fmt.Errorf("resolved anchor = %v, segment = %d", anchor, segment)
			}
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read probe count: %v", err)
	}
	if string(count) != "x" {
		t.Fatalf("ffmpeg probe executions = %d, want 1", len(count))
	}
}

func TestResolveCopySeekAnchorMatchesRealLongGOPHEVC(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg integration test")
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath(ffprobePathFromFFmpeg(ffmpegPath)); err != nil {
		t.Skip("ffprobe is not installed beside ffmpeg")
	}
	encoders, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil || !strings.Contains(string(encoders), "libx265") {
		t.Skip("ffmpeg does not provide libx265")
	}

	sourcePath := filepath.Join(t.TempDir(), "long-gop-hevc.mkv")
	encodeCtx, cancelEncode := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelEncode()
	encode := exec.CommandContext(encodeCtx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24",
		"-t", "22",
		"-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "keyint=240:min-keyint=240:scenecut=0:log-level=error:pools=1:frame-threads=1",
		"-an", "-y", sourcePath,
	)
	if output, err := encode.CombinedOutput(); err != nil {
		t.Fatalf("generate long-GOP HEVC fixture: %v\n%s", err, output)
	}

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelProbe()
	anchor, segment, err := ResolveCopySeekAnchor(probeCtx, ffmpegPath, sourcePath, 18.261, 2)
	if err != nil {
		t.Fatalf("ResolveCopySeekAnchor: %v", err)
	}
	if math.Abs(anchor-10) > 0.001 || segment != 5 {
		t.Fatalf("resolved anchor = %v, segment = %d; want 10, 5", anchor, segment)
	}

	// An exact keyframe seek is the recovery boundary that regressed in #839:
	// Matroska emits the preceding keyframe while MP4 starts at the requested
	// keyframe. The resolver must model FFmpeg's real copy path for both.
	exactMKVAnchor, exactMKVSegment, err := ResolveCopySeekAnchor(probeCtx, ffmpegPath, sourcePath, 10, 2)
	if err != nil {
		t.Fatalf("ResolveCopySeekAnchor exact MKV keyframe: %v", err)
	}
	if math.Abs(exactMKVAnchor) > 0.001 || exactMKVSegment != 0 {
		t.Fatalf("exact MKV anchor = %v, segment = %d; want 0, 0", exactMKVAnchor, exactMKVSegment)
	}

	mp4Path := filepath.Join(t.TempDir(), "long-gop-hevc.mp4")
	remux := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", sourcePath,
		"-map", "0:V:0", "-c:v", "copy", "-an", "-y", mp4Path,
	)
	if output, err := remux.CombinedOutput(); err != nil {
		t.Fatalf("remux long-GOP HEVC fixture to MP4: %v\n%s", err, output)
	}
	exactMP4Anchor, exactMP4Segment, err := ResolveCopySeekAnchor(probeCtx, ffmpegPath, mp4Path, 10, 2)
	if err != nil {
		t.Fatalf("ResolveCopySeekAnchor exact MP4 keyframe: %v", err)
	}
	if math.Abs(exactMP4Anchor-10) > 0.001 || exactMP4Segment != 5 {
		t.Fatalf("exact MP4 anchor = %v, segment = %d; want 10, 5", exactMP4Anchor, exactMP4Segment)
	}

	outputDir := filepath.Join(t.TempDir(), "hls")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("create HLS output: %v", err)
	}
	opts := TranscodeOpts{
		InputPath:              sourcePath,
		OutputDir:              outputDir,
		SessionID:              "copy-anchor-integration",
		SourceVideoCodec:       "hevc",
		VideoSampleEntry:       VideoSampleEntryHVC1,
		SeekSeconds:            18.261,
		StreamOriginSeconds:    anchor,
		CopySeekAnchorResolved: true,
		TargetCodecVideo:       "copy",
		TargetCodecAudio:       "copy",
		SegmentDuration:        2,
		StartSegmentNumber:     segment,
		FFmpegPath:             ffmpegPath,
	}
	packageCtx, cancelPackage := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPackage()
	packageHLS := exec.CommandContext(packageCtx, ffmpegPath, buildFFmpegArgs(opts)...)
	if output, err := packageHLS.CombinedOutput(); err != nil {
		t.Fatalf("package copy HLS: %v\n%s", err, output)
	}

	manifestPath := filepath.Join(outputDir, "stream.m3u8")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read copy HLS manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "#EXT-X-MEDIA-SEQUENCE:5") {
		t.Fatalf("copy HLS media sequence does not use resolved anchor:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), `#EXT-X-MAP:URI="init.mp4"`) {
		t.Fatalf("copy HLS manifest missing init map:\n%s", manifest)
	}
	initInfo, err := os.Stat(filepath.Join(outputDir, "init.mp4"))
	if err != nil || initInfo.Size() == 0 {
		t.Fatalf("copy HLS init segment: info=%v err=%v", initInfo, err)
	}
	segments, err := filepath.Glob(filepath.Join(outputDir, "seg_*.m4s"))
	if err != nil || len(segments) == 0 {
		t.Fatalf("copy HLS media segments = %v err=%v", segments, err)
	}
	firstFrame, err := exec.Command(ffprobePathFromFFmpeg(ffmpegPath),
		"-v", "error",
		"-select_streams", "v:0",
		"-read_intervals", "%+0.1",
		"-show_entries", "frame=best_effort_timestamp_time,key_frame",
		"-of", "csv=p=0",
		manifestPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("probe copy HLS first frame: %v\n%s", err, firstFrame)
	}
	if !strings.Contains(string(firstFrame), "10.000000") {
		t.Fatalf("copy HLS first frame = %q, want resolved 10-second keyframe", firstFrame)
	}
	tag, err := exec.Command(ffprobePathFromFFmpeg(ffmpegPath),
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_tag_string",
		"-of", "default=nw=1:nk=1",
		manifestPath,
	).CombinedOutput()
	tags := strings.Fields(string(tag))
	if err != nil || len(tags) == 0 {
		t.Fatalf("copy HLS sample entry = %q err=%v", tag, err)
	}
	for _, got := range tags {
		if got != VideoSampleEntryHVC1 {
			t.Fatalf("copy HLS sample entry = %q, want only %q", tag, VideoSampleEntryHVC1)
		}
	}
}

// resetCopySeekAnchorCache swaps in an isolated cache with an injectable clock
// and probe runner, restoring the process-wide cache when the test ends.
func resetCopySeekAnchorCache(t *testing.T, now func() time.Time, probe anchorProbeRunner) *copySeekAnchorCache {
	t.Helper()
	previous := copySeekAnchors
	cache := &copySeekAnchorCache{
		entries:    make(map[string]copySeekAnchorCacheEntry),
		ttl:        copySeekAnchorCacheTTL,
		maxEntries: copySeekAnchorCacheMax,
		now:        now,
		probe:      probe,
	}
	copySeekAnchors = cache
	t.Cleanup(func() { copySeekAnchors = previous })
	return cache
}

func TestResolveCopySeekAnchorForSourceCachesByStableIdentity(t *testing.T) {
	var calls int
	resetCopySeekAnchorCache(t, nil, func(_ context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		calls++
		return requested - 1, int((requested - 1) / float64(segmentDuration)), nil
	})

	for i := range 2 {
		// Each call resolves a fresh relay URL for the same virtual source.
		inputPath := fmt.Sprintf("http://relay/stream/%d", i)
		anchor, segment, err := ResolveCopySeekAnchorForSource(context.Background(), "ffmpeg", "virtual://movie/42", inputPath, 120, 2)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if anchor != 119 || segment != 59 {
			t.Fatalf("call %d anchor = %v segment = %d; want 119, 59", i, anchor, segment)
		}
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (second call must hit the stable-identity cache)", calls)
	}
}

func TestResolveCopySeekAnchorForSourceSeparatesPositionAndIdentity(t *testing.T) {
	var calls int
	resetCopySeekAnchorCache(t, nil, func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		calls++
		return requested, 0, nil
	})
	ctx := context.Background()
	for _, probe := range []struct {
		identity  string
		inputPath string
		requested float64
	}{
		{"virtual://movie/a", "http://relay/a", 30},
		{"virtual://movie/a", "http://relay/a", 60},
		{"virtual://movie/b", "http://relay/b", 30},
	} {
		if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", probe.identity, probe.inputPath, probe.requested, 2); err != nil {
			t.Fatalf("probe %+v: %v", probe, err)
		}
	}
	if calls != 3 {
		t.Fatalf("probe calls = %d, want 3 (position and identity are part of the key)", calls)
	}
}

func TestResolveCopySeekAnchorForSourceReProbesAfterTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	var calls int
	resetCopySeekAnchorCache(t, func() time.Time { return now }, func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		calls++
		return requested, 0, nil
	})
	ctx := context.Background()
	call := func() {
		t.Helper()
		if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/ttl", "http://relay/ttl", 90, 2); err != nil {
			t.Fatal(err)
		}
	}
	call()
	now = now.Add(copySeekAnchorCacheTTL - time.Second)
	call()
	if calls != 1 {
		t.Fatalf("probe calls before expiry = %d, want 1", calls)
	}
	now = now.Add(2 * time.Second)
	call()
	if calls != 2 {
		t.Fatalf("probe calls after expiry = %d, want 2", calls)
	}
}

func TestResolveCopySeekAnchorForSourceDoesNotCacheProbeFailures(t *testing.T) {
	var calls int
	resetCopySeekAnchorCache(t, nil, func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		calls++
		if calls == 1 {
			return 0, 0, errors.New("transient upstream timeout")
		}
		return requested, 1, nil
	})
	ctx := context.Background()
	if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/retry", "http://relay/retry", 45, 2); err == nil {
		t.Fatal("first probe error was swallowed")
	}
	anchor, segment, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/retry", "http://relay/retry", 45, 2)
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if anchor != 45 || segment != 1 {
		t.Fatalf("second probe anchor = %v segment = %d; want 45, 1", anchor, segment)
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want 2 (failures are never cached)", calls)
	}
}

func TestResolveCopySeekAnchorCacheBoundedToMaxEntries(t *testing.T) {
	tick := time.Unix(1_000, 0)
	var calls int
	cache := resetCopySeekAnchorCache(t, func() time.Time {
		tick = tick.Add(time.Millisecond)
		return tick
	}, func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		calls++
		return requested, 0, nil
	})
	ctx := context.Background()
	total := copySeekAnchorCacheMax + 1
	for i := range total {
		identity := "virtual://movie/" + strconv.Itoa(i)
		if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", identity, identity, 60, 2); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	if calls != total {
		t.Fatalf("probe calls = %d, want %d", calls, total)
	}
	if len(cache.entries) != copySeekAnchorCacheMax {
		t.Fatalf("cache entries = %d, want %d", len(cache.entries), copySeekAnchorCacheMax)
	}
	oldestKey := copySeekAnchorCacheKey("ffmpeg", "virtual://movie/0", 60, 2)
	if _, ok := cache.entries[oldestKey]; ok {
		t.Fatal("oldest entry was not evicted")
	}
	newestKey := copySeekAnchorCacheKey("ffmpeg", "virtual://movie/"+strconv.Itoa(total-1), 60, 2)
	if _, ok := cache.entries[newestKey]; !ok {
		t.Fatal("newest entry was evicted")
	}
}

func TestResolveCopySeekAnchorForSourceCoalescesConcurrentIdenticalProbes(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	resetCopySeekAnchorCache(t, nil, func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return requested - 5, 12, nil
	})
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Distinct relay URLs still name the same stable source identity.
			anchor, segment, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/parallel", fmt.Sprintf("http://relay/%d", i), 80, 2)
			if err == nil && (anchor != 75 || segment != 12) {
				err = fmt.Errorf("anchor = %v segment = %d; want 75, 12", anchor, segment)
			}
			errs <- err
		}(i)
	}
	close(start)
	// Let at least one probe start, then release it. singleflight admits one
	// closure per key, so the peer either joins the flight or hits the cache.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("probe never started")
		}
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("probe calls = %d, want 1", got)
	}
}

// TestResolveCopySeekAnchorProbeBudgetCappedByCallerDeadline proves the probe no
// longer ignores the caller's deadline: with less remaining than the per-probe
// cap, the probe context is bounded by the caller's deadline.
func TestResolveCopySeekAnchorProbeBudgetCappedByCallerDeadline(t *testing.T) {
	var gotBudget time.Duration
	var calls int
	resetCopySeekAnchorCache(t, nil, func(ctx context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		calls++
		if deadline, ok := ctx.Deadline(); ok {
			gotBudget = time.Until(deadline)
		}
		return requested - 1, int((requested - 1) / float64(segmentDuration)), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/short-budget", "http://relay/short", 120, 2); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if gotBudget <= 0 || gotBudget > 3*time.Second {
		t.Fatalf("probe budget = %s, want capped by the 3s caller deadline", gotBudget)
	}
	if gotBudget >= CopySeekProbeTimeout {
		t.Fatalf("probe budget = %s, want less than the %s per-probe cap", gotBudget, CopySeekProbeTimeout)
	}
}

// TestResolveCopySeekAnchorProbeUsesFullCapWithLargerCallerBudget proves a
// caller with more than the probe cap still gets the full cap, not an
// unbounded or caller-deadline-sized budget.
func TestResolveCopySeekAnchorProbeUsesFullCapWithLargerCallerBudget(t *testing.T) {
	var gotBudget time.Duration
	resetCopySeekAnchorCache(t, nil, func(ctx context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		if deadline, ok := ctx.Deadline(); ok {
			gotBudget = time.Until(deadline)
		}
		return requested, 0, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/long-budget", "http://relay/long", 120, 2); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotBudget < CopySeekProbeTimeout-time.Second || gotBudget > CopySeekProbeTimeout {
		t.Fatalf("probe budget = %s, want the %s probe cap", gotBudget, CopySeekProbeTimeout)
	}
}

// TestResolveCopySeekAnchorSkipsProbeWhenCallerBudgetExpired proves an already
// exhausted caller budget never starts an ffmpeg probe.
func TestResolveCopySeekAnchorSkipsProbeWhenCallerBudgetExpired(t *testing.T) {
	var calls int32
	resetCopySeekAnchorCache(t, nil, func(context.Context, string, string, float64, int) (float64, int, error) {
		atomic.AddInt32(&calls, 1)
		return 0, 0, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if _, _, err := ResolveCopySeekAnchorForSource(ctx, "ffmpeg", "virtual://movie/expired", "http://relay/expired", 120, 2); err == nil {
		t.Fatal("an exhausted caller budget resolved an anchor")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("probe calls = %d, want 0 with no caller budget", got)
	}
}
