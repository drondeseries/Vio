package quality

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

const (
	maxQualityProfiles       = 10
	maxCustomFormats         = 50
	maxProfileLabelBytes     = 128
	maxProfileRegexBytes     = 1024
	maxProfileAttributeBytes = 64
	maxPreferredOrder        = 10000
)

type CustomFormat struct {
	Name   string `json:"name"`
	Regex  string `json:"regex"`
	Score  int    `json:"score"`
	Reject bool   `json:"reject"`

	match *regexp.Regexp
}

// Compiled returns the compiled match regex, or nil if unset.
func (f *CustomFormat) Compiled() *regexp.Regexp { return f.match }

type QualityProfile struct {
	Label          string `json:"label"`
	Resolution     string `json:"resolution"`
	IncludeRegex   string `json:"include_regex"`
	ExcludeRegex   string `json:"exclude_regex"`
	PreferredOrder int    `json:"preferred_order"`
	CodecVideo     string `json:"codec_video"`
	CodecAudio     string `json:"codec_audio"`
	HDR            string `json:"hdr"`
	ExcludeHDR     string `json:"exclude_hdr"`

	include *regexp.Regexp
	exclude *regexp.Regexp
}

// IncludeCompiled returns the compiled include regex, or nil if unset.
func (p *QualityProfile) IncludeCompiled() *regexp.Regexp { return p.include }

// ExcludeCompiled returns the compiled exclude regex, or nil if unset.
func (p *QualityProfile) ExcludeCompiled() *regexp.Regexp { return p.exclude }

type QualityConfig struct {
	Preset                   string           `json:"quality_preset"`
	CustomFormatPreset       string           `json:"custom_format_preset"`
	EnableProfiles           bool             `json:"enable_quality_profiles"`
	Profiles                 []QualityProfile `json:"quality_profiles"`
	CustomFormats            []CustomFormat   `json:"custom_formats"`
	FallbackToAnyStream      bool             `json:"fallback_to_any_stream"`
	SingleStreamWithFailover bool             `json:"single_stream_with_failover"`
}

const defaultQualityPreset = "custom"

// qualityPresetProfiles keeps the common choices understandable in the admin
// form while leaving regex-level control available through Custom.
func qualityPresetProfiles(preset string) []QualityProfile {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "balanced":
		return []QualityProfile{{Label: "1080p", Resolution: "1080p", PreferredOrder: 1}}
	case "4k-hdr":
		return []QualityProfile{{Label: "4K HDR", Resolution: "2160p", HDR: "hdr", PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", PreferredOrder: 2}}
	case "4k-dolby-vision":
		return []QualityProfile{{Label: "4K Dolby Vision", Resolution: "2160p", HDR: "dv", PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", PreferredOrder: 2}}
	case "no-dolby-vision":
		return []QualityProfile{{Label: "4K HDR10", Resolution: "2160p", ExcludeHDR: "dv", ExcludeRegex: `(?i)(dolby[ ._-]*vision|\bdv\b|dovi)`, PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", ExcludeHDR: "dv", ExcludeRegex: `(?i)(dolby[ ._-]*vision|\bdv\b|dovi)`, PreferredOrder: 2}}
	case "no-hdr":
		return []QualityProfile{{Label: "4K SDR", Resolution: "2160p", ExcludeHDR: "*", ExcludeRegex: `(?i)(hdr|dolby[ ._-]*vision|\bdv\b|dovi|hlg)`, PreferredOrder: 1}, {Label: "1080p SDR", Resolution: "1080p", ExcludeHDR: "*", ExcludeRegex: `(?i)(hdr|dolby[ ._-]*vision|\bdv\b|dovi|hlg)`, PreferredOrder: 2}}
	case "compatibility":
		return []QualityProfile{{Label: "1080p Compatible", Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", PreferredOrder: 1}, {Label: "720p Compatible", Resolution: "720p", CodecVideo: "h264", CodecAudio: "aac", PreferredOrder: 2}}
	case "anime":
		return []QualityProfile{{Label: "Anime 1080p", Resolution: "1080p", IncludeRegex: `(?i)(anime|web-dl|web)`, PreferredOrder: 1}, {Label: "Anime 720p", Resolution: "720p", IncludeRegex: `(?i)(anime|web-dl|web)`, PreferredOrder: 2}}
	default:
		return nil
	}
}

func customFormatPresets(preset string) []CustomFormat {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "english-original":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Score: -25},
		}
	case "english-strict":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Non-English Language", Regex: `(?i)\b(?:fra|fre|fr|ita|spa|jpn|kor|zho|chi|por|rus|ara|nld|dut|pol|swe|nor|dan|fin)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Reject: true},
		}
	case "original-or-english":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German Dub", Regex: `(?i)\b(?:german|deutsch|deu|ger)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Score: -10},
		}
	case "clean-quality":
		return []CustomFormat{
			{Name: "Cam/TS/HDCam", Regex: `(?i)\b(?:cam|hdcam|telecine|tc|tele-sync|ts|hd-ts|pdvd)\b`, Reject: true},
			{Name: "3D", Regex: `(?i)\b(?:3d|sbs|tab|hsbs|htab)\b`, Reject: true},
			{Name: "BR-DISK / Iso", Regex: `(?i)\b(?:br[ ._-]*disk|iso|bdmv)\b`, Reject: true},
			{Name: "Extras / Sample", Regex: `(?i)\b(?:sample|trailer|extras)\b`, Reject: true},
		}
	case "audio-hd":
		return []CustomFormat{
			{Name: "TrueHD Atmos / DTS:X", Regex: `(?i)(?:truehd[ ._-]*atmos|dts[ ._-]*x)`, Score: 500},
			{Name: "ATMOS", Regex: `(?i)\batmos\b`, Score: 300},
			{Name: "TrueHD / DTS-HD MA", Regex: `(?i)(?:truehd|dts[ ._-]*hd[ ._-]*ma)`, Score: 250},
		}
	case "repack-proper":
		return []CustomFormat{
			{Name: "Repack / Proper", Regex: `(?i)\b(?:repack[0-9]?|proper[0-9]?|rerip)\b`, Score: 200},
		}
	case "top-web-sources":
		return []CustomFormat{
			{Name: "Top Tier WEB (NF/ATVP/AMZN/DSNP/MAX)", Regex: `(?i)\b(?:nf|atvp|atv|amzn|dsnp|hmax|max|bcore)\b`, Score: 150},
		}
	case "anime-enhanced":
		return []CustomFormat{
			{Name: "Dual Audio", Regex: `(?i)\b(?:dual[ ._-]*audio|multi[ ._-]*audio)\b`, Score: 200},
			{Name: "Uncensored", Regex: `(?i)\buncensored\b`, Score: 150},
			{Name: "10-bit Color", Regex: `(?i)\b10bit\b`, Score: 100},
		}
	case "web-tier-01":
		return []CustomFormat{
			{Name: "WEB Tier 1 Groups", Regex: `(?i)-(?:NTb|FLUX|Kitsune|ETHEL|playWEB|NOGRP|DECiBEL|DON|CtrlHD|TrollHD|KiNGS|hallowed|CRiMSON|BLUTONiUM|NIMA)\b`, Score: 300},
		}
	case "web-tier-02":
		return []CustomFormat{
			{Name: "WEB Tier 2 Groups", Regex: `(?i)-(?:QxR|SiNNERS|CasStudio|CMRG|LAZY|W4NK3R|HQMUX|GHOSTS)\b`, Score: 150},
		}
	case "remux-tier-01":
		return []CustomFormat{
			{Name: "Remux Tier 1 Groups", Regex: `(?i)-(?:FraMeSToR|EPSiLON|KRaLiMARK|HDC|DON|CtrlHD|TrollHD|BHDStudio)\b`, Score: 500},
		}
	case "remux-tier-02":
		return []CustomFormat{
			{Name: "Remux Tier 2 Groups", Regex: `(?i)-(?:BLUTONiUM|PmP|TRiToN|SNAKE|W4NK3R)\b`, Score: 250},
		}
	case "trash-recommended":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Cam/TS/HDCam", Regex: `(?i)\b(?:cam|hdcam|telecine|tc|tele-sync|ts|hd-ts|pdvd)\b`, Reject: true},
			{Name: "3D", Regex: `(?i)\b(?:3d|sbs|tab|hsbs|htab)\b`, Reject: true},
			{Name: "BR-DISK / Iso", Regex: `(?i)\b(?:br[ ._-]*disk|iso|bdmv)\b`, Reject: true},
			{Name: "Extras / Sample", Regex: `(?i)\b(?:sample|trailer|extras)\b`, Reject: true},
			{Name: "Repack / Proper", Regex: `(?i)\b(?:repack[0-9]?|proper[0-9]?|rerip)\b`, Score: 200},
			{Name: "Remux Tier 1", Regex: `(?i)-(?:FraMeSToR|EPSiLON|KRaLiMARK|HDC|DON|CtrlHD|TrollHD|BHDStudio)\b`, Score: 500},
			{Name: "WEB Tier 1", Regex: `(?i)-(?:NTb|FLUX|Kitsune|ETHEL|playWEB|NOGRP|DECiBEL|DON|CtrlHD|TrollHD|KiNGS|hallowed|CRiMSON|BLUTONiUM|NIMA)\b`, Score: 300},
			{Name: "TrueHD Atmos / DTS:X", Regex: `(?i)(?:truehd[ ._-]*atmos|dts[ ._-]*x)`, Score: 250},
		}
	default:
		return nil
	}
}

func (q *QualityConfig) ApplyPreset() {
	if profiles := qualityPresetProfiles(q.Preset); len(profiles) > 0 {
		q.Profiles = profiles
	}
	if formats := customFormatPresets(q.CustomFormatPreset); len(formats) > 0 {
		q.CustomFormats = formats
	}
}

// decodeQualityProfiles accepts the typed array declared by the plugin
// manifest. Keeping one representation prevents configuration drift between
// the UI, JSON Schema validation, and the runtime.
func decodeQualityProfiles(raw any) ([]QualityProfile, error) {
	if raw == nil {
		return nil, nil
	}
	if _, ok := raw.([]any); !ok {
		return nil, errors.New("must be an array")
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var profiles []QualityProfile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func (q *QualityConfig) Validate() error {
	if len(q.CustomFormats) > maxCustomFormats {
		return fmt.Errorf("maximum %d custom formats allowed", maxCustomFormats)
	}
	seenFormats := make(map[string]bool)
	for i := range q.CustomFormats {
		format := &q.CustomFormats[i]
		format.Name = strings.TrimSpace(format.Name)
		format.Regex = strings.TrimSpace(format.Regex)
		if format.Name == "" {
			return errors.New("custom format name cannot be empty")
		}
		if format.Regex == "" {
			return fmt.Errorf("custom format %s regex cannot be empty", format.Name)
		}
		if len(format.Name) > maxProfileLabelBytes || len(format.Regex) > maxProfileRegexBytes {
			return fmt.Errorf("custom format %s exceeds its size limit", format.Name)
		}
		key := strings.ToLower(format.Name)
		if seenFormats[key] {
			return fmt.Errorf("duplicate custom format name: %s", format.Name)
		}
		seenFormats[key] = true
		compiled, err := regexp.Compile(format.Regex)
		if err != nil {
			return fmt.Errorf("invalid regex in custom format %s: %w", format.Name, err)
		}
		format.match = compiled
	}
	if !q.EnableProfiles {
		return nil
	}
	if len(q.Profiles) == 0 {
		return errors.New("at least one quality profile is required when profiles are enabled")
	}
	if len(q.Profiles) > maxQualityProfiles {
		return fmt.Errorf("maximum %d quality profiles allowed", maxQualityProfiles)
	}
	seen := make(map[string]bool)
	for i := range q.Profiles {
		p := &q.Profiles[i]
		p.Label = strings.TrimSpace(p.Label)
		if p.Label == "" {
			return fmt.Errorf("profile label cannot be empty")
		}
		if len(p.Label) > maxProfileLabelBytes {
			return fmt.Errorf("profile label exceeds %d bytes", maxProfileLabelBytes)
		}
		for name, value := range map[string]string{
			"resolution":  p.Resolution,
			"codec_video": p.CodecVideo,
			"codec_audio": p.CodecAudio,
			"hdr":         p.HDR,
			"exclude_hdr": p.ExcludeHDR,
		} {
			if len(value) > maxProfileAttributeBytes {
				return fmt.Errorf("%s in profile %s exceeds %d bytes", name, p.Label, maxProfileAttributeBytes)
			}
		}
		if len(p.IncludeRegex) > maxProfileRegexBytes || len(p.ExcludeRegex) > maxProfileRegexBytes {
			return fmt.Errorf("profile %s regex exceeds %d bytes", p.Label, maxProfileRegexBytes)
		}
		if p.PreferredOrder < 0 || p.PreferredOrder > maxPreferredOrder {
			return fmt.Errorf("preferred_order in profile %s must be between 0 and %d", p.Label, maxPreferredOrder)
		}
		lower := strings.ToLower(p.Label)
		if seen[lower] {
			return fmt.Errorf("duplicate profile label: %s", p.Label)
		}
		seen[lower] = true

		if p.IncludeRegex != "" {
			r, err := regexp.Compile(p.IncludeRegex)
			if err != nil {
				return fmt.Errorf("invalid include_regex in profile %s: %w", p.Label, err)
			}
			p.include = r
		}
		if p.ExcludeRegex != "" {
			r, err := regexp.Compile(p.ExcludeRegex)
			if err != nil {
				return fmt.Errorf("invalid exclude_regex in profile %s: %w", p.Label, err)
			}
			p.exclude = r
		}
	}
	sort.SliceStable(q.Profiles, func(i, j int) bool {
		left, right := q.Profiles[i].PreferredOrder, q.Profiles[j].PreferredOrder
		switch {
		case left == 0 && right == 0:
			return false
		case left == 0:
			return false
		case right == 0:
			return true
		default:
			return left < right
		}
	})
	return nil
}

// MatchProfile reports whether a candidate satisfies a quality profile's
// include/exclude regex, resolution, codecs, and HDR constraints.
func MatchProfile(c stream.StreamCandidate, p QualityProfile) bool {
	fullText := c.Name + " " + c.Description + " " + c.Title + " " + c.URL
	if p.ExcludeCompiled() != nil && p.ExcludeCompiled().MatchString(fullText) {
		return false
	}
	if p.IncludeCompiled() != nil && !p.IncludeCompiled().MatchString(fullText) {
		return false
	}
	if p.Resolution != "" && stream.NormalizeResolution(c.Resolution) != stream.NormalizeResolution(p.Resolution) {
		return false
	}
	if p.CodecVideo != "" && c.CodecVideo != p.CodecVideo {
		return false
	}
	if p.CodecAudio != "" && c.CodecAudio != p.CodecAudio {
		return false
	}
	if p.HDR != "" && c.HDR != p.HDR {
		return false
	}
	if p.ExcludeHDR == "*" && c.HDR != "" {
		return false
	}
	if p.ExcludeHDR != "" && p.ExcludeHDR != "*" && strings.EqualFold(c.HDR, p.ExcludeHDR) {
		return false
	}
	return true
}

// CustomFormatScore sums the scores of matching custom formats for a
// candidate. Returns the score and whether any matching format rejects.
func CustomFormatScore(candidate stream.StreamCandidate, formats []CustomFormat) (int, bool) {
	return customFormatScore(candidate, formats)
}

func customFormatScore(candidate stream.StreamCandidate, formats []CustomFormat) (int, bool) {
	text := candidate.Name + " " + candidate.Description + " " + candidate.Title + " " + candidate.URL
	if candidate.BehaviorHints.Filename != "" {
		text += " " + candidate.BehaviorHints.Filename
		if u, err := url.QueryUnescape(candidate.BehaviorHints.Filename); err == nil && u != "" && u != candidate.BehaviorHints.Filename {
			text += " " + u
		}
		if u, err := url.PathUnescape(candidate.BehaviorHints.Filename); err == nil && u != "" && u != candidate.BehaviorHints.Filename {
			text += " " + u
		}
	}
	if u, err := url.QueryUnescape(candidate.URL); err == nil && u != "" {
		text += " " + u
	}
	if u, err := url.PathUnescape(candidate.URL); err == nil && u != "" {
		text += " " + u
	}
	score := 0
	for _, format := range formats {
		matcher := format.Compiled()
		if matcher == nil && format.Regex != "" {
			var err error
			matcher, err = regexp.Compile(format.Regex)
			if err != nil {
				continue
			}
		}
		if matcher == nil || !matcher.MatchString(text) {
			continue
		}
		if format.Reject {
			return 0, true
		}
		score += format.Score
	}
	return score, false
}

// SortCandidatesForProfile ranks candidates in place: non-rejected first,
// then by custom-format score, resolution, source type, and original order.
func SortCandidatesForProfile(candidates []stream.StreamCandidate, p QualityProfile, formats []CustomFormat) {
	sortCandidatesForProfile(candidates, p, formats)
}

func sortCandidatesForProfile(candidates []stream.StreamCandidate, p QualityProfile, formats []CustomFormat) {
	if len(candidates) == 0 {
		return
	}
	if len(candidates) == 1 {
		candidates[0].QualityScore, _ = customFormatScore(candidates[0], formats)
		return
	}
	type scoredCandidate struct {
		candidate stream.StreamCandidate
		rejected  bool
	}
	scored := make([]scoredCandidate, len(candidates))
	for idx := range candidates {
		score, reject := customFormatScore(candidates[idx], formats)
		candidates[idx].QualityScore = score
		scored[idx] = scoredCandidate{
			candidate: candidates[idx],
			rejected:  reject,
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		c1, c2 := scored[i].candidate, scored[j].candidate
		if scored[i].rejected != scored[j].rejected {
			return !scored[i].rejected
		}
		if c1.QualityScore != c2.QualityScore {
			return c1.QualityScore > c2.QualityScore
		}
		if r1, r2 := stream.ResolutionScore(c1.Resolution), stream.ResolutionScore(c2.Resolution); r1 != r2 {
			return r1 > r2
		}
		if s1, s2 := stream.SourceScore(c1.SourceType), stream.SourceScore(c2.SourceType); s1 != s2 {
			return s1 > s2
		}
		return c1.OriginalIndex < c2.OriginalIndex
	})
	for idx := range scored {
		candidates[idx] = scored[idx].candidate
	}
}
