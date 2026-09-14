package userstore

import "encoding/json"

// AudioTrackSignature captures the stable track traits needed to preserve an
// audio variant across episodes in a series.
type AudioTrackSignature struct {
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	EmbeddedTitle string `json:"embedded_title,omitempty"`
	Codec         string `json:"codec,omitempty"`
	Layout        string `json:"layout,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	// Languages is the sorted, canonicalized MULTi language list (["de","en"])
	// so signatures compare as multisets regardless of container ordering.
	Languages []string `json:"languages,omitempty"`
	Default   bool     `json:"default"`
}

// IsZero reports whether the signature carries no identifying information.
func (s *AudioTrackSignature) IsZero() bool {
	return s == nil ||
		(s.Language == "" &&
			s.Title == "" &&
			s.EmbeddedTitle == "" &&
			s.Codec == "" &&
			s.Layout == "" &&
			s.Channels == 0 &&
			len(s.Languages) == 0 &&
			!s.Default)
}

// SubtitleTrackSignature captures the stable track traits needed to preserve a
// subtitle variant across episodes in a series.
type SubtitleTrackSignature struct {
	Source          string `json:"source,omitempty"`
	Language        string `json:"language,omitempty"`
	Codec           string `json:"codec,omitempty"`
	Label           string `json:"label,omitempty"`
	Forced          bool   `json:"forced"`
	HearingImpaired bool   `json:"hearing_impaired"`
}

// IsZero reports whether the signature carries no identifying information.
func (s *SubtitleTrackSignature) IsZero() bool {
	return s == nil ||
		(s.Source == "" &&
			s.Language == "" &&
			s.Codec == "" &&
			s.Label == "" &&
			!s.Forced &&
			!s.HearingImpaired)
}

func encodeTrackSignature[T interface{ IsZero() bool }](sig T) ([]byte, error) {
	if sig.IsZero() {
		return []byte("{}"), nil
	}
	return json.Marshal(sig)
}

// MarshalAudioTrackSignature serializes an audio track signature for storage.
func MarshalAudioTrackSignature(sig *AudioTrackSignature) ([]byte, error) {
	return encodeTrackSignature(sig)
}

// MarshalSubtitleTrackSignature serializes a subtitle track signature for storage.
func MarshalSubtitleTrackSignature(sig *SubtitleTrackSignature) ([]byte, error) {
	return encodeTrackSignature(sig)
}

// UnmarshalAudioTrackSignature parses a stored audio track signature.
func UnmarshalAudioTrackSignature(data []byte) (*AudioTrackSignature, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var sig AudioTrackSignature
	if err := json.Unmarshal(data, &sig); err != nil {
		return nil, err
	}
	if sig.IsZero() {
		return nil, nil
	}
	return &sig, nil
}

// UnmarshalSubtitleTrackSignature parses a stored subtitle track signature.
func UnmarshalSubtitleTrackSignature(data []byte) (*SubtitleTrackSignature, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var sig SubtitleTrackSignature
	if err := json.Unmarshal(data, &sig); err != nil {
		return nil, err
	}
	if sig.IsZero() {
		return nil, nil
	}
	return &sig, nil
}
