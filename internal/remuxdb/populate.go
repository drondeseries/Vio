package remuxdb

import (
	"fmt"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// ApplyEvidence backfills matched RemuxDB evidence into a file whose own
// probe evidence is missing. Only facts the file does not already carry are
// filled; per-stream probe detail always wins. It reports whether video
// evidence was applied.
func ApplyEvidence(ev Evidence, file *models.MediaFile) bool {
	if file == nil || len(ev.VideoTracks) == 0 {
		return false
	}
	if ev.Container != "" && (file.Container == "" || strings.EqualFold(file.Container, "virtual")) {
		file.Container = NormalizeContainer(ev.Container)
	}
	if ev.Duration > 0 && file.Duration <= 0 {
		file.Duration = int(ev.Duration)
	}
	if ev.Bitrate > 0 && file.Bitrate <= 0 {
		file.Bitrate = int(ev.Bitrate / 1000)
	}

	videoTracks := make([]models.VideoTrack, 0, len(ev.VideoTracks))
	audioTracks := make([]models.AudioTrack, 0, len(ev.AudioTracks))
	subTracks := make([]models.SubtitleTrack, 0, len(ev.SubtitleTracks))
	externalSubs := make([]models.ExternalSubtitle, 0, len(ev.SubtitleTracks))

	for _, t := range ev.VideoTracks {
		videoRange := "SDR"
		videoRangeType := "SDR"
		if strings.EqualFold(strings.TrimSpace(t.ColorTransfer), "smpte2084") || t.DVProfile > 0 || t.HDR10PlusPresent {
			videoRange = "HDR"
			if t.DVProfile > 0 {
				videoRangeType = "DOVI"
			} else if t.HDR10PlusPresent {
				videoRangeType = "HDR10Plus"
			} else {
				videoRangeType = "HDR10"
			}
		}
		doviStr := ""
		if t.DVProfile > 0 {
			doviStr = "true"
		}
		fpsStr := ""
		if t.FPS > 0 {
			fpsStr = fmt.Sprintf("%.3f", t.FPS)
		}
		vt := models.VideoTrack{
			Title:          t.Title,
			Codec:          t.Codec,
			DolbyVision:    doviStr,
			DVProfile:      t.DVProfile,
			HDR10Plus:      t.HDR10PlusPresent,
			Profile:        t.Profile,
			Level:          t.Level,
			Width:          t.Width,
			Height:         t.Height,
			AspectRatio:    t.AspectRatio,
			FrameRate:      fpsStr,
			Bitrate:        int(t.BitRate / 1000),
			BitDepth:       t.BitDepth,
			VideoRange:     videoRange,
			VideoRangeType: videoRangeType,
			ColorSpace:     t.ColorSpace,
			ColorTransfer:  t.ColorTransfer,
			ColorPrimaries: t.ColorPrimaries,
		}
		videoTracks = append(videoTracks, vt)

		if file.CodecVideo == "" {
			file.CodecVideo = t.Codec
		}
		if file.Resolution == "" {
			file.Resolution = resolutionLabel(t.Width, t.Height)
		}
		if t.DVProfile > 0 || t.HDR10PlusPresent || videoRange == "HDR" {
			file.HDR = true
		}
	}

	for _, t := range ev.AudioTracks {
		at := models.AudioTrack{
			Index:    t.Index,
			Title:    t.Title,
			Language: t.Language,
			Codec:    t.Codec,
			Channels: t.Channels,
			Layout:   t.ChannelLayout,
			Bitrate:  int(t.BitRate / 1000),
			Default:  t.IsDefault,
		}
		audioTracks = append(audioTracks, at)
		if file.CodecAudio == "" {
			file.CodecAudio = t.Codec
		}
		if file.AudioChannels == 0 {
			file.AudioChannels = t.Channels
		}
	}

	for _, t := range ev.SubtitleTracks {
		if t.IsExternal {
			externalSubs = append(externalSubs, models.ExternalSubtitle{
				Language:        t.Language,
				Format:          t.Codec,
				Title:           t.Title,
				Forced:          t.IsForced,
				Default:         t.IsDefault,
				HearingImpaired: t.IsHearingImpaired,
			})
			continue
		}
		st := models.SubtitleTrack{
			Index:           t.Index,
			Title:           t.Title,
			Language:        t.Language,
			Codec:           t.Codec,
			Default:         t.IsDefault,
			Forced:          t.IsForced,
			HearingImpaired: t.IsHearingImpaired,
		}
		subTracks = append(subTracks, st)
	}

	if len(videoTracks) > 0 && len(file.VideoTracks) == 0 {
		file.VideoTracks = videoTracks
	}
	if len(audioTracks) > 0 && len(file.AudioTracks) == 0 {
		file.AudioTracks = audioTracks
	}
	if len(subTracks) > 0 && len(file.SubtitleTracks) == 0 {
		file.SubtitleTracks = subTracks
	}
	if len(externalSubs) > 0 && len(file.ExternalSubtitles) == 0 {
		file.ExternalSubtitles = externalSubs
	}
	return len(videoTracks) > 0
}

// HintFromCandidate builds a MatchHint from locally known release identity.
// hdrKnown must be true only when hdr carries positively measured evidence,
// never for the zero value of an unprobed file.
func HintFromCandidate(infoHash string, fileIdx *int, size int64, filename, resolution, codecVideo string, hdrKnown, hdr bool) MatchHint {
	hint := MatchHint{
		InfoHash:   infoHash,
		Size:       size,
		Filename:   filename,
		Resolution: resolution,
		CodecVideo: codecVideo,
		HDRKnown:   hdrKnown,
		HDR:        hdr,
	}
	if fileIdx != nil {
		hint.HasFileIdx = true
		hint.FileIdx = *fileIdx
	}
	return hint
}

// NormalizeContainer canonicalizes ffprobe's raw comma-joined format_name
// (e.g. "matroska,webm"), which crowd submitters are not guaranteed to
// normalize before submitting.
func NormalizeContainer(container string) string {
	first := strings.TrimSpace(strings.Split(container, ",")[0])
	return strings.ToLower(strings.TrimSpace(first))
}
