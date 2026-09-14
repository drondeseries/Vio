package playback

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/mediaprobe"
	"github.com/Silo-Server/silo-server/internal/models"
)

// Virtual release rotation: a playback session plans a subtitle URL against
// the exact subtitle layout a provider release had when it was probed, but the
// pinned URI can resolve to a rotated rendition with a different layout by
// extraction time. The helpers in this file let the native virtual extraction
// path verify the live layout cheaply and re-map the plan ordinal onto a
// same-class live track instead of letting ffmpeg fail on `0:s:N`.

// subtitleProbeTimeout bounds the live-layout ffprobe. Subtitle streams are
// described near the container head, so a healthy probe finishes in well under
// a second; anything slower is a stuck upstream and should not hold the
// request.
const subtitleProbeTimeout = 5 * time.Second

// SubtitleLayoutsEqual reports whether two embedded subtitle layouts are
// track-for-track identical on every attribute the extraction path depends on:
// container index, codec, language, forced/default dispositions,
// hearing-impaired flag, and the container track ID used as the plan-time
// identity pin. Callers compare a catalog row's layout against the session's
// plan-time evidence to detect a virtual release rotation.
func SubtitleLayoutsEqual(a, b []models.SubtitleTrack) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		at, bt := a[i], b[i]
		if at.Index != bt.Index ||
			normalizeCodecV3(at.Codec) != normalizeCodecV3(bt.Codec) ||
			lang.Canonical(at.Language) != lang.Canonical(bt.Language) ||
			at.Forced != bt.Forced ||
			at.Default != bt.Default ||
			at.HearingImpaired != bt.HearingImpaired ||
			canonicalSubtitleContainerTrackID(at.ContainerTrackID) != canonicalSubtitleContainerTrackID(bt.ContainerTrackID) {
			return false
		}
	}
	return true
}

// ProbeSubtitleLayout runs a bounded ffprobe against inputURL (a registered
// relay URL for virtual sources — the relay injects the upstream auth headers)
// and returns the container's embedded subtitle layout in stream order. Each
// returned track's position in the slice is the ffmpeg `0:s:N` ordinal the
// extraction path maps with; Index carries the container stream index.
//
// ffprobePathOrFFmpeg may be either an ffprobe path or the configured ffmpeg
// path (ffprobe ships next to ffmpeg in every distribution Silo supports).
// The probe is bounded to subtitleProbeTimeout; a failure returns an error so
// the caller can decide whether a spawn can proceed unverified.
func ProbeSubtitleLayout(ctx context.Context, ffprobePathOrFFmpeg, inputURL string) ([]models.SubtitleTrack, error) {
	probeCtx, cancel := context.WithTimeout(ctx, subtitleProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, probeSubtitleBin(ffprobePathOrFFmpeg),
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-select_streams", "s",
		inputURL,
	)
	output, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return nil, fmt.Errorf("probe subtitle layout timed out for %s: %w", inputURL, probeCtx.Err())
		}
		return nil, fmt.Errorf("probe subtitle layout failed for %s: %w", inputURL, err)
	}

	var probed struct {
		Streams []mediaprobe.Stream `json:"streams"`
	}
	if err := json.Unmarshal(output, &probed); err != nil {
		return nil, fmt.Errorf("parse subtitle layout probe for %s: %w", inputURL, err)
	}

	tracks := make([]models.SubtitleTrack, 0, len(probed.Streams))
	for _, stream := range probed.Streams {
		if stream.CodecType != "subtitle" {
			continue
		}
		codec := strings.ToLower(strings.TrimSpace(stream.CodecName))
		if codec == "" {
			continue
		}
		tracks = append(tracks, models.SubtitleTrack{
			ContainerTrackID: canonicalSubtitleContainerTrackID(string(stream.ID)),
			Index:            stream.Index,
			Codec:            codec,
			Language:         lang.Canonical(stream.Tags["language"]),
			Forced:           stream.Disposition.Forced == 1,
			Default:          stream.Disposition.Default == 1,
			HearingImpaired:  subtitleTagBoolFlag(stream.Tags, "hearing_impaired"),
		})
	}
	return tracks, nil
}

// probeSubtitleBin resolves the ffprobe executable from either an ffprobe path
// or the configured ffmpeg path (whose sibling ffprobe is used).
func probeSubtitleBin(ffprobePathOrFFmpeg string) string {
	raw := strings.TrimSpace(ffprobePathOrFFmpeg)
	if raw == "" {
		return "ffprobe"
	}
	if strings.Contains(strings.ToLower(filepath.Base(raw)), "ffprobe") {
		return raw
	}
	return ffprobePathFromFFmpeg(raw)
}

// MatchEmbeddedSubtitleTrack finds the unique live track that best matches the
// requested plan-time track under class preservation. "Class" is the output
// muxer family the sidecar URL promises: webvtt-family under .vtt, ass-copy
// under .ass, sup (PGS) under .sup. The match narrows the live candidates by
// class, then by language (when the requested track names one), then forced,
// then hearing-impaired, and only returns a match when exactly one candidate
// survives — a different-language or ambiguous layout is deliberately not
// served. The returned int is the live track's ordinal (its position in live),
// which is the ffmpeg `0:s:N` operand for the rotated source.
func MatchEmbeddedSubtitleTrack(requested models.SubtitleTrack, live []models.SubtitleTrack) (liveOrdinal int, matched models.SubtitleTrack, ok bool) {
	requestedClass := subtitleLayoutClass(requested.Codec)
	if requestedClass == "" || len(live) == 0 {
		return 0, models.SubtitleTrack{}, false
	}

	type candidate struct {
		ordinal int
		track   models.SubtitleTrack
	}
	candidates := make([]candidate, 0, len(live))
	for ordinal, track := range live {
		if subtitleLayoutClass(track.Codec) == requestedClass {
			candidates = append(candidates, candidate{ordinal: ordinal, track: track})
		}
	}
	if len(candidates) == 0 {
		return 0, models.SubtitleTrack{}, false
	}
	if len(candidates) == 1 {
		return candidates[0].ordinal, candidates[0].track, true
	}

	filter := func(match func(models.SubtitleTrack) bool) []candidate {
		out := make([]candidate, 0, len(candidates))
		for _, c := range candidates {
			if match(c.track) {
				out = append(out, c)
			}
		}
		return out
	}
	single := func(set []candidate) (int, models.SubtitleTrack, bool) {
		if len(set) == 1 {
			return set[0].ordinal, set[0].track, true
		}
		return 0, models.SubtitleTrack{}, false
	}

	// A requested language is a hard constraint: never silently serve a
	// different-language track just because the class survived the rotation.
	requestedLanguage := lang.Canonical(requested.Language)
	if requestedLanguage != "" {
		sameLanguage := filter(func(t models.SubtitleTrack) bool {
			return lang.Canonical(t.Language) == requestedLanguage
		})
		if len(sameLanguage) == 0 {
			return 0, models.SubtitleTrack{}, false
		}
		if ordinal, track, unique := single(sameLanguage); unique {
			return ordinal, track, true
		}
		candidates = sameLanguage
	}
	sameForced := filter(func(t models.SubtitleTrack) bool { return t.Forced == requested.Forced })
	if len(sameForced) == 0 {
		return 0, models.SubtitleTrack{}, false
	}
	if ordinal, track, unique := single(sameForced); unique {
		return ordinal, track, true
	}
	candidates = sameForced

	sameHI := filter(func(t models.SubtitleTrack) bool { return t.HearingImpaired == requested.HearingImpaired })
	if len(sameHI) == 0 {
		return 0, models.SubtitleTrack{}, false
	}
	return single(sameHI)
}

// SubtitleExtractMuxer returns the ffmpeg output muxer an extraction of codec
// would use, honoring an explicitly forced target format (currently only
// "vtt"). The native stream handler uses it as the class-preservation gate for
// a virtual remap: the URL extension was minted at plan time, so a remap may
// only land on a live codec that produces the same muxer.
func SubtitleExtractMuxer(codec, targetFormat string) string {
	_, muxer := streamExtractOutput(codec, targetFormat)
	return muxer
}

// subtitleLayoutClass returns the deliverable class a codec extracts as, or ""
// for codecs with no sidecar representation. It deliberately mirrors the
// stream handler's output mapping (webvtt for text, ass-copy for ASS/SSA,
// .sup for PGS) and never the catch-all webvtt default, so an unrenderable
// codec is never class-matched onto a text track.
func subtitleLayoutClass(codec string) string {
	switch {
	case IsASS(codec):
		return "ass"
	case IsPGS(codec):
		return "sup"
	case isTextSubtitleV3(codec):
		return "webvtt"
	default:
		return ""
	}
}

// canonicalSubtitleContainerTrackID mirrors the catalog scanner's container
// track ID normalization so a live probe compares equal to plan-time evidence.
// FFprobe emits MP4 track IDs as hexadecimal strings; the canonical form is
// the decimal identifier, and absent IDs are never guessed.
func canonicalSubtitleContainerTrackID(raw string) string {
	raw = strings.TrimSpace(raw)
	base := 10
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		raw = raw[2:]
		base = 16
	}
	id, err := strconv.ParseUint(raw, base, 32)
	if err != nil || id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}

// subtitleTagBoolFlag reports whether an ffprobe stream tag carries a truthy
// boolean flag (1/true/yes), matching how catalog scans derive flags that have
// no dedicated disposition field.
func subtitleTagBoolFlag(tags map[string]string, key string) bool {
	if tags == nil {
		return false
	}
	value := strings.ToLower(strings.TrimSpace(tags[key]))
	return value == "1" || value == "true" || value == "yes"
}
