package remuxdb

import (
	"path"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// BuildSubmission constructs a RemuxDB submission payload from a successfully
// probed MediaFile and candidate source details. It returns false when the
// file lacks essential probe evidence or source identifiers.
func BuildSubmission(file *models.MediaFile, filename, infoHash string, nzb *NzbSubmission) (SubmissionPayload, bool) {
	if file == nil || len(file.VideoTracks) == 0 || file.Duration <= 0 {
		return SubmissionPayload{}, false
	}
	if nzb != nil && !isSafeIndexerGUID(nzb.IndexerGUID) {
		nzb = nil
	}
	if infoHash == "" && nzb == nil {
		return SubmissionPayload{}, false
	}
	if filename == "" {
		filename = file.ReleaseName
	}
	filename = path.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == "/" {
		return SubmissionPayload{}, false
	}
	kind := "movie"
	var seasonPtr, episodePtr *int
	if file.EpisodeID != "" || file.SeasonNumber > 0 {
		kind = "episode"
		if file.SeasonNumber > 0 {
			seasonPtr = &file.SeasonNumber
		}
		if file.EpisodeNumber > 0 {
			episodePtr = &file.EpisodeNumber
		}
	}
	imdbID := ExtractIMDbID(file.ContentID)
	if imdbID == "" {
		imdbID = ExtractIMDbID(file.FilePath)
	}
	ext := &ExternalIDs{
		IMDbID: imdbID,
	}
	if strings.Contains(file.ContentID, "-tmdb-") {
		parts := strings.Split(file.ContentID, "-tmdb-")
		if len(parts) == 2 {
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				ext.TMDbID = id
			}
		}
	}
	if strings.Contains(file.ContentID, "-tvdb-") {
		parts := strings.Split(file.ContentID, "-tvdb-")
		if len(parts) == 2 {
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				ext.TVDbID = id
			}
		}
	}

	tracks := make([]TrackDetail, 0, len(file.VideoTracks)+len(file.AudioTracks)+len(file.SubtitleTracks))
	// VideoTrack carries no container index, so the array position is the
	// video ordinal. Audio and subtitle indices are absolute container stream
	// indices and are preserved verbatim rather than renumbered.
	for i, vt := range file.VideoTracks {
		tracks = append(tracks, TrackDetail{
			Kind:             "video",
			Index:            i,
			Codec:            vt.Codec,
			Width:            vt.Width,
			Height:           vt.Height,
			FPS:              parseFrameRateFloat(vt.FrameRate),
			BitRate:          int64(vt.Bitrate) * 1000,
			BitDepth:         vt.BitDepth,
			Profile:          vt.Profile,
			Title:            vt.Title,
			ColorPrimaries:   vt.ColorPrimaries,
			ColorRange:       vt.ColorRange,
			ColorSpace:       vt.ColorSpace,
			ColorTransfer:    vt.ColorTransfer,
			AspectRatio:      vt.AspectRatio,
			HDR10PlusPresent: vt.HDR10Plus,
			DVProfile:        vt.DVProfile,
			Level:            vt.Level,
		})
	}
	for _, at := range file.AudioTracks {
		tracks = append(tracks, TrackDetail{
			Kind:          "audio",
			Index:         at.Index,
			Codec:         at.Codec,
			Channels:      at.Channels,
			SampleRate:    at.SampleRate,
			BitRate:       int64(at.Bitrate) * 1000,
			ChannelLayout: at.Layout,
			Profile:       at.Profile,
			Title:         at.Title,
			Language:      at.Language,
			IsDefault:     at.Default,
		})
	}
	for _, st := range file.SubtitleTracks {
		tracks = append(tracks, TrackDetail{
			Kind:      "subtitle",
			Index:     st.Index,
			Codec:     st.Codec,
			Title:     st.Title,
			Language:  st.Language,
			IsDefault: st.Default,
			IsForced:  st.Forced,
		})
	}

	size := file.FileSize
	bitrate := int64(file.Bitrate) * 1000
	// Leave the container empty when unknown; "virtual" is a sentinel for an
	// unprobed source, not a real container. Never fabricate MKV.
	container := strings.TrimSpace(file.Container)
	if strings.EqualFold(container, "virtual") {
		container = ""
	}

	return SubmissionPayload{
		Kind:            kind,
		Filename:        filename,
		TorrentInfoHash: infoHash,
		NZB:             nzb,
		Container:       container,
		Size:            size,
		Duration:        float64(file.Duration),
		Bitrate:         bitrate,
		Season:          seasonPtr,
		Episode:         episodePtr,
		ExternalIDs:     ext,
		Tracks:          tracks,
	}, true
}

func isSafeIndexerGUID(guid string) bool {
	if guid == "" || len(guid) > 128 {
		return false
	}
	for _, c := range guid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// parseFrameRateFloat converts a probe frame-rate string to a float, accepting
// both rational ("30000/1001") and decimal ("23.976") forms, with an optional
// trailing "fps" label. Unparseable input returns 0.
func parseFrameRateFloat(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if lower := strings.ToLower(s); strings.HasSuffix(lower, "fps") {
		s = strings.TrimSpace(s[:len(s)-len("fps")])
		if s == "" {
			return 0
		}
	}
	if !strings.Contains(s, "/") {
		value, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return value
	}
	parts := strings.SplitN(s, "/", 2)
	numerator, errNumerator := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	denominator, errDenominator := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errNumerator != nil || errDenominator != nil || denominator == 0 {
		return 0
	}
	return numerator / denominator
}
