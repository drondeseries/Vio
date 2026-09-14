package remuxdb

import (
	"path"
	"strings"
)

// MatchMethod names the ladder rung that produced a variant match.
type MatchMethod string

const (
	MatchNone        MatchMethod = ""
	MatchInfoHash    MatchMethod = "info_hash"
	MatchIndexerGUID MatchMethod = "indexer_guid"
	MatchSize        MatchMethod = "size"
	MatchSizeTags    MatchMethod = "size_tags"
	MatchFilename    MatchMethod = "filename"
)

// MatchHint carries the local release identity available for matching.
// HDRKnown distinguishes a positively measured HDR status from the zero
// value of an unprobed file; only positive evidence may veto a variant.
type MatchHint struct {
	InfoHash    string
	HasFileIdx  bool
	FileIdx     int
	IndexerGUID string
	Indexer     string
	Size        int64
	Filename    string
	Resolution  string
	CodecVideo  string
	HDRKnown    bool
	HDR         bool
}

// MatchVariant selects the RemuxDB variant describing the hinted release.
// It returns nil when no rung identifies exactly one variant: an explicit
// miss is always preferable to a guessed one.
func MatchVariant(versions []MediaInfo, hint MatchHint) (*MediaInfo, MatchMethod) {
	if len(versions) == 0 {
		return nil, MatchNone
	}
	if v := matchByInfoHash(versions, hint); v != nil {
		return v, MatchInfoHash
	}
	if v := matchByIndexerGUID(versions, hint); v != nil {
		return v, MatchIndexerGUID
	}
	if v := matchBySize(versions, hint); v != nil {
		return v, MatchSize
	}
	if v := matchBySizeTags(versions, hint); v != nil {
		return v, MatchSizeTags
	}
	if v := matchByFilename(versions, hint); v != nil {
		return v, MatchFilename
	}
	return nil, MatchNone
}

func matchByInfoHash(versions []MediaInfo, hint MatchHint) *MediaInfo {
	hash := strings.ToLower(strings.TrimSpace(hint.InfoHash))
	if hash == "" {
		return nil
	}
	var found *MediaInfo
	for i := range versions {
		matched := false
		for _, s := range versions[i].Sources {
			if strings.ToLower(s.TorrentInfoHash) != hash {
				continue
			}
			if hint.HasFileIdx {
				if s.TorrentFileIdx == nil || *s.TorrentFileIdx != hint.FileIdx {
					continue
				}
			}
			matched = true
			break
		}
		if !matched {
			continue
		}
		if found != nil {
			return nil
		}
		found = &versions[i]
	}
	return found
}

func matchByIndexerGUID(versions []MediaInfo, hint MatchHint) *MediaInfo {
	guid := strings.TrimSpace(hint.IndexerGUID)
	if guid == "" {
		return nil
	}
	var found *MediaInfo
	for i := range versions {
		matched := false
		for _, s := range versions[i].Sources {
			if s.IndexerGUID != guid {
				continue
			}
			if hint.Indexer != "" && s.Indexer != "" && !strings.EqualFold(s.Indexer, hint.Indexer) {
				continue
			}
			matched = true
			break
		}
		if !matched {
			continue
		}
		if found != nil {
			return nil
		}
		found = &versions[i]
	}
	return found
}

// matchBySize requires byte-exact equality.
func matchBySize(versions []MediaInfo, hint MatchHint) *MediaInfo {
	if hint.Size <= 0 {
		return nil
	}
	var found *MediaInfo
	for i := range versions {
		if versions[i].Size != hint.Size {
			continue
		}
		if found != nil {
			return nil
		}
		found = &versions[i]
	}
	return found
}

// sizeTolerance covers rounded provider labels ("28.98 GB") against exact
// crowd-reported byte sizes.
const sizeTolerance = 0.01

func matchBySizeTags(versions []MediaInfo, hint MatchHint) *MediaInfo {
	if hint.Size <= 0 {
		return nil
	}
	var found *MediaInfo
	for i := range versions {
		size := versions[i].Size
		if size <= 0 {
			continue
		}
		rel := float64(size-hint.Size) / float64(hint.Size)
		if rel < 0 {
			rel = -rel
		}
		if rel > sizeTolerance {
			continue
		}
		if !variantAgreesWithTags(&versions[i], hint) {
			continue
		}
		if found != nil {
			return nil
		}
		found = &versions[i]
	}
	return found
}

func matchByFilename(versions []MediaInfo, hint MatchHint) *MediaInfo {
	stem := filenameStem(hint.Filename)
	if stem == "" {
		return nil
	}
	var found *MediaInfo
	for i := range versions {
		matched := false
		for _, s := range versions[i].Sources {
			if s.Filename == "" {
				continue
			}
			if filenameStem(s.Filename) == stem {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if found != nil {
			return nil
		}
		found = &versions[i]
	}
	return found
}

func filenameStem(name string) string {
	base := path.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	if ext := path.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for _, r := range base {
		switch r {
		case '.', '_', '-':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// variantAgreesWithTags requires every positively known local tag to agree
// with the variant. Unknown local tags never reject; a variant without a
// video track cannot confirm tags and is rejected on tag rungs.
func variantAgreesWithTags(v *MediaInfo, hint MatchHint) bool {
	vt := firstVideoTrack(v)
	if vt == nil {
		return false
	}
	if hint.Resolution != "" {
		res := resolutionLabel(vt.Width, vt.Height)
		if res != "" && !strings.EqualFold(res, strings.TrimSpace(hint.Resolution)) {
			return false
		}
	}
	if hint.CodecVideo != "" && !sameCodec(vt.Codec, hint.CodecVideo) {
		return false
	}
	if hint.HDRKnown && variantHDR(vt) != hint.HDR {
		return false
	}
	return true
}

func firstVideoTrack(v *MediaInfo) *TrackDetail {
	for i := range v.Tracks {
		if strings.EqualFold(v.Tracks[i].Kind, "video") {
			return &v.Tracks[i]
		}
	}
	return nil
}

func variantHDR(t *TrackDetail) bool {
	if t == nil {
		return false
	}
	if t.DVProfile > 0 || t.HDR10PlusPresent {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(t.ColorTransfer)) {
	case "smpte2084", "smpte2084 ", "arib-std-b67":
		return true
	}
	return false
}

func normalizeCodec(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "avc", "h264", "h.264":
		return "h264"
	case "hevc", "h265", "h.265", "x265", "x264":
		if strings.Contains(strings.ToLower(codec), "265") || strings.EqualFold(codec, "hevc") || strings.EqualFold(codec, "h265") || strings.EqualFold(codec, "h.265") {
			return "hevc"
		}
		return "h264"
	case "av1":
		return "av1"
	case "vc1", "vc-1":
		return "vc1"
	case "mpeg4", "mpeg-4", "xvid", "divx":
		return "mpeg4"
	case "mpeg2", "mpeg-2":
		return "mpeg2"
	default:
		return strings.ToLower(strings.TrimSpace(codec))
	}
}

func sameCodec(a, b string) bool {
	na, nb := normalizeCodec(a), normalizeCodec(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}
