package intromarkers

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type boundaryRefiner interface {
	RefineChapterEnd(ctx context.Context, candidate Candidate, segment Segment) (Segment, bool, error)
}

type SilenceBoundaryRefiner struct {
	config Config
}

type silenceInterval struct {
	Start float64
	End   float64
}

var (
	silenceStartPattern = regexp.MustCompile(`silence_start:\s*([0-9]+(?:\.[0-9]+)?)`)
	silenceEndPattern   = regexp.MustCompile(`silence_end:\s*([0-9]+(?:\.[0-9]+)?)`)
)

func NewSilenceBoundaryRefiner(config Config) *SilenceBoundaryRefiner {
	return &SilenceBoundaryRefiner{config: config.normalized()}
}

func (r *SilenceBoundaryRefiner) RefineChapterEnd(ctx context.Context, candidate Candidate, segment Segment) (Segment, bool, error) {
	cfg := r.config.normalized()
	if !cfg.SilenceRefinementEnabled {
		return segment, false, nil
	}
	if candidate.FilePath == "" || strings.HasPrefix(strings.ToLower(candidate.FilePath), "virtual://") || segment.End <= segment.Start {
		return segment, false, nil
	}

	windowStart := math.Max(0, segment.End-cfg.SilenceWindowBeforeSeconds)
	windowEnd := segment.End + cfg.SilenceWindowAfterSeconds
	if candidate.DurationSeconds > 0 {
		windowEnd = math.Min(windowEnd, candidate.DurationSeconds)
	}
	if windowEnd <= windowStart {
		return segment, false, nil
	}

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "info",
		"-ss", formatSeconds(windowStart),
		"-i", candidate.FilePath,
		"-t", formatSeconds(windowEnd - windowStart),
		"-vn",
		"-sn",
		"-dn",
		"-af", fmt.Sprintf("silencedetect=noise=%ddB:duration=%s", *cfg.SilenceNoiseThresholdDB, formatSeconds(cfg.SilenceMinimumDurationSeconds)),
		"-f", "null",
		"-",
	}
	output, err := exec.CommandContext(ctx, cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		return segment, false, fmt.Errorf("detecting intro boundary silence for file %d: %w", candidate.FileID, err)
	}

	intervals := parseSilenceDetectOutput(output, windowStart)
	for _, interval := range intervals {
		if interval.Start < segment.End {
			continue
		}
		if interval.Start-segment.End < cfg.SilenceMinimumExtensionSeconds {
			continue
		}
		if interval.Start-segment.End > cfg.SilenceMaximumExtensionSeconds {
			continue
		}
		if interval.Start-segment.Start > 180 {
			continue
		}
		refined := segment
		refined.End = interval.Start
		refined.Confidence = 0.98
		refined.Algorithm = ChapterSilenceAlgorithm
		return refined, true, nil
	}

	return segment, false, nil
}

func parseSilenceDetectOutput(output []byte, windowStart float64) []silenceInterval {
	matches := silenceStartPattern.FindAllSubmatch(output, -1)
	if len(matches) == 0 {
		return nil
	}

	intervals := make([]silenceInterval, 0, len(matches))
	for _, match := range matches {
		start, err := strconv.ParseFloat(string(match[1]), 64)
		if err != nil {
			continue
		}
		intervals = append(intervals, silenceInterval{Start: windowStart + start})
	}

	endMatches := silenceEndPattern.FindAllSubmatch(output, -1)
	for i, match := range endMatches {
		if i >= len(intervals) {
			break
		}
		end, err := strconv.ParseFloat(string(match[1]), 64)
		if err != nil {
			continue
		}
		intervals[i].End = windowStart + end
	}

	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].Start < intervals[j].Start
	})
	return intervals
}
