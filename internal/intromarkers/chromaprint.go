package intromarkers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

type ChromaprintExtractor struct {
	config Config
}

func NewChromaprintExtractor(config Config) *ChromaprintExtractor {
	return &ChromaprintExtractor{config: config.normalized()}
}

func (e *ChromaprintExtractor) Preflight(ctx context.Context) error {
	muxers, err := exec.CommandContext(ctx, e.config.FFmpegPath, "-hide_banner", "-muxers").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg muxer preflight failed: %w", err)
	}
	if !bytes.Contains(bytes.ToLower(muxers), []byte("chromaprint")) {
		return fmt.Errorf("ffmpeg does not list the chromaprint muxer")
	}
	help, err := exec.CommandContext(ctx, e.config.FFmpegPath, "-hide_banner", "-h", "muxer=chromaprint").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg chromaprint help failed: %w", err)
	}
	if !bytes.Contains(bytes.ToLower(help), []byte("fp_format")) || !bytes.Contains(bytes.ToLower(help), []byte("raw")) {
		return fmt.Errorf("ffmpeg chromaprint muxer does not advertise raw fingerprint output")
	}
	return nil
}

func (e *ChromaprintExtractor) Extract(ctx context.Context, candidate Candidate) (Fingerprint, bool, error) {
	if candidate.FilePath == "" || strings.HasPrefix(strings.ToLower(candidate.FilePath), "virtual://") {
		return Fingerprint{}, false, nil
	}
	windowStart := 0.0
	windowEnd := analysisWindowEnd(candidate.DurationSeconds, e.config)
	if windowEnd <= windowStart {
		return Fingerprint{}, false, nil
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-threads", "1",
		"-ss", formatSeconds(windowStart),
		"-i", candidate.FilePath,
		"-t", formatSeconds(windowEnd - windowStart),
		"-ac", "2",
		"-f", "chromaprint",
		"-fp_format", "raw",
		"-",
	}
	output, err := exec.CommandContext(ctx, e.config.FFmpegPath, args...).Output()
	if err != nil {
		return Fingerprint{}, false, fmt.Errorf("extracting chromaprint for file %d: %w", candidate.FileID, err)
	}
	points := decodeRawPoints(output)
	if len(points) == 0 {
		return Fingerprint{}, false, nil
	}
	return Fingerprint{
		MediaFileID:           candidate.FileID,
		FileHash:              candidate.FileHash,
		FileSize:              candidate.FileSize,
		DurationSeconds:       candidate.DurationSeconds,
		WindowStartSeconds:    windowStart,
		WindowEndSeconds:      windowEnd,
		AlgorithmVersion:      AlgorithmVersion,
		ConfigHash:            e.config.ConfigHash(),
		FingerprintFormat:     ChromaprintFormat,
		SampleDurationSeconds: float64(len(points)) * DefaultPointHopSeconds,
		Points:                points,
	}, true, nil
}

func analysisWindowEnd(duration float64, cfg Config) float64 {
	if duration <= 0 {
		return 0
	}
	percentEnd := duration * (float64(cfg.AnalysisPercent) / 100)
	limitEnd := float64(cfg.AnalysisLengthLimitMinutes * 60)
	return math.Min(duration, math.Min(percentEnd, limitEnd))
}

func decodeRawPoints(output []byte) []uint32 {
	if len(output) < 4 {
		return nil
	}
	points := make([]uint32, 0, len(output)/4)
	for len(output) >= 4 {
		points = append(points, binary.LittleEndian.Uint32(output[:4]))
		output = output[4:]
	}
	return points
}

func encodeRawPoints(points []uint32) []byte {
	buf := make([]byte, len(points)*4)
	for i, point := range points {
		binary.LittleEndian.PutUint32(buf[i*4:], point)
	}
	return buf
}

func formatSeconds(seconds float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(seconds, 'f', 3, 64), "0"), ".")
}
