// Package remuxdb provides an HTTP client to RemuxDB
// (https://remuxdb.1632022.xyz), an open crowdsourced stream metadata
// database for movies and TV shows, plus identity-ladder matching of a
// local release against the returned variants and PostgreSQL persistence
// of matched evidence.
package remuxdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://remuxdb.1632022.xyz"

// Client interacts with the RemuxDB API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      string
	ClientID   string
}

// NewClient returns a Client with defaults.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		Token:    token,
		ClientID: "silo-server",
	}
}

// ProbeSource is one torrent or NZB source grouped into a MediaInfo variant.
type ProbeSource struct {
	Kind            string `json:"kind,omitempty"`
	Filename        string `json:"filename,omitempty"`
	Indexer         string `json:"indexer,omitempty"`
	IndexerGUID     string `json:"indexer_guid,omitempty"`
	TorrentInfoHash string `json:"torrent_info_hash,omitempty"`
	TorrentFileIdx  *int   `json:"torrent_file_idx,omitempty"`
}

// TrackDetail represents a video, audio, or subtitle track returned by RemuxDB.
type TrackDetail struct {
	Kind              string  `json:"kind"`
	Index             int     `json:"idx"`
	IsDefault         bool    `json:"is_default,omitempty"`
	IsForced          bool    `json:"is_forced,omitempty"`
	IsHearingImpaired bool    `json:"is_hearing_impaired,omitempty"`
	IsExternal        bool    `json:"is_external,omitempty"`
	IsAnamorphic      bool    `json:"is_anamorphic,omitempty"`
	HDR10PlusPresent  bool    `json:"hdr10_plus_present,omitempty"`
	Codec             string  `json:"codec,omitempty"`
	Language          string  `json:"language,omitempty"`
	Title             string  `json:"title,omitempty"`
	BitRate           int64   `json:"bit_rate,omitempty"`
	BitDepth          int     `json:"bit_depth,omitempty"`
	PixelFormat       string  `json:"pixel_format,omitempty"`
	Profile           string  `json:"profile,omitempty"`
	Level             int     `json:"level,omitempty"`
	Width             int     `json:"width,omitempty"`
	Height            int     `json:"height,omitempty"`
	FPS               float64 `json:"fps,omitempty"`
	AspectRatio       string  `json:"aspect_ratio,omitempty"`
	ColorPrimaries    string  `json:"color_primaries,omitempty"`
	ColorRange        string  `json:"color_range,omitempty"`
	ColorSpace        string  `json:"color_space,omitempty"`
	ColorTransfer     string  `json:"color_transfer,omitempty"`
	DVProfile         int     `json:"dv_profile,omitempty"`
	Channels          int     `json:"channels,omitempty"`
	SampleRate        int     `json:"sample_rate,omitempty"`
	ChannelLayout     string  `json:"channel_layout,omitempty"`
}

// MediaInfo represents one stream variant entry returned by GET /api/media/info.
type MediaInfo struct {
	ContentHash string        `json:"content_hash,omitempty"`
	Container   string        `json:"container,omitempty"`
	Duration    float64       `json:"duration,omitempty"`
	Size        int64         `json:"size,omitempty"`
	Bitrate     int64         `json:"bitrate,omitempty"`
	Sources     []ProbeSource `json:"sources,omitempty"`
	Tracks      []TrackDetail `json:"tracks,omitempty"`
}

// FetchProbe fetches stream metadata versions from RemuxDB by IMDb ID.
func (c *Client) FetchProbe(ctx context.Context, imdbID string, season, episode *int) ([]MediaInfo, error) {
	if imdbID == "" {
		return nil, nil
	}
	u, err := url.Parse(c.BaseURL + "/api/media/info")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("imdb_id", imdbID)
	if season != nil {
		q.Set("season", strconv.Itoa(*season))
	}
	if episode != nil {
		q.Set("episode", strconv.Itoa(*episode))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	clientID := c.ClientID
	if clientID == "" {
		clientID = "silo-server"
	}
	req.Header.Set("x-client-id", clientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remuxdb fetch status %d", resp.StatusCode)
	}

	var results []MediaInfo
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}

// ExtractIMDbID returns the first tt+digits token in s, or "".
func ExtractIMDbID(s string) string {
	lower := strings.ToLower(s)
	idx := strings.Index(lower, "tt")
	if idx == -1 {
		return ""
	}
	sub := lower[idx:]
	end := 2
	for end < len(sub) && sub[end] >= '0' && sub[end] <= '9' {
		end++
	}
	if end >= 7 {
		return sub[:end]
	}
	return ""
}

// ExternalIDs holds external identifiers for a RemuxDB submission.
type ExternalIDs struct {
	IMDbID string `json:"imdb_id,omitempty"`
	TMDbID int64  `json:"tmdb_id,omitempty"`
	TVDbID int64  `json:"tvdb_id,omitempty"`
}

// NzbSubmission describes an NZB source for RemuxDB submission.
type NzbSubmission struct {
	Indexer     string `json:"indexer"`
	IndexerGUID string `json:"indexer_guid"`
	Title       string `json:"title,omitempty"`
}

// SubmissionPayload is the request body sent to POST /api/mediainfo.
type SubmissionPayload struct {
	ClientID        string         `json:"client_id,omitempty"`
	Kind            string         `json:"kind"` // "movie" or "episode"
	Filename        string         `json:"filename"`
	TorrentInfoHash string         `json:"torrent_info_hash,omitempty"`
	TorrentFileIdx  *int           `json:"torrent_file_idx,omitempty"`
	NZB             *NzbSubmission `json:"nzb,omitempty"`
	Container       string         `json:"container,omitempty"`
	Size            int64          `json:"size"`
	Duration        float64        `json:"duration"`
	Bitrate         int64          `json:"bitrate,omitempty"`
	Season          *int           `json:"season,omitempty"`
	Episode         *int           `json:"episode,omitempty"`
	ExternalIDs     *ExternalIDs   `json:"external_ids,omitempty"`
	Tracks          []TrackDetail  `json:"tracks"`
}

// SubmitProbe sends crowdsourced probe metadata to RemuxDB via POST /api/mediainfo.
func (c *Client) SubmitProbe(ctx context.Context, payload SubmissionPayload) error {
	if payload.Kind == "" || payload.Filename == "" || (payload.TorrentInfoHash == "" && payload.NZB == nil) {
		return errors.New("incomplete remuxdb submission: kind, filename, and info_hash or nzb required")
	}
	if payload.ClientID == "" {
		payload.ClientID = c.ClientID
		if payload.ClientID == "" {
			payload.ClientID = "silo-server"
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal remuxdb submission: %w", err)
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/api/mediainfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-id", payload.ClientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("remuxdb submit status %d", resp.StatusCode)
	}
	return nil
}
