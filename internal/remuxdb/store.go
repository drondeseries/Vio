package remuxdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Evidence is one matched RemuxDB variant persisted for a release.
type Evidence struct {
	ContentID      string
	EpisodeID      string
	MediaFolderID  int
	CandidateURI   string
	MatchMethod    MatchMethod
	MatchedSize    int64
	MatchedHash    string
	Container      string
	CodecVideo     string
	CodecAudio     string
	Resolution     string
	HDR            bool
	HDRKnown       bool
	Duration       float64
	Bitrate        int64
	VideoTracks    []TrackDetail
	AudioTracks    []TrackDetail
	SubtitleTracks []TrackDetail
}

// EvidenceFromVariant captures a matched variant for persistence.
func EvidenceFromVariant(contentID, episodeID string, folderID int, candidateURI string, method MatchMethod, v *MediaInfo) Evidence {
	ev := Evidence{
		ContentID:     contentID,
		EpisodeID:     episodeID,
		MediaFolderID: folderID,
		CandidateURI:  candidateURI,
		MatchMethod:   method,
	}
	if v == nil {
		return ev
	}
	ev.MatchedSize = v.Size
	ev.MatchedHash = v.ContentHash
	ev.Container = v.Container
	ev.Duration = v.Duration
	ev.Bitrate = v.Bitrate
	for _, t := range v.Tracks {
		switch lowerKind(t.Kind) {
		case "video":
			ev.VideoTracks = append(ev.VideoTracks, t)
		case "audio":
			ev.AudioTracks = append(ev.AudioTracks, t)
		case "subtitle":
			ev.SubtitleTracks = append(ev.SubtitleTracks, t)
		}
	}
	if vt := firstVideoTrack(v); vt != nil {
		ev.CodecVideo = vt.Codec
		ev.Resolution = resolutionLabel(vt.Width, vt.Height)
		ev.HDR = variantHDR(vt)
		ev.HDRKnown = true
	}
	if len(ev.AudioTracks) > 0 {
		ev.CodecAudio = ev.AudioTracks[0].Codec
	}
	return ev
}

func lowerKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

func resolutionLabel(width, height int) string {
	if width <= 0 && height <= 0 {
		return ""
	}
	if width <= 0 {
		switch {
		case height >= 2160:
			return "2160p"
		case height >= 1080:
			return "1080p"
		case height >= 720:
			return "720p"
		case height >= 480:
			return "480p"
		default:
			return "SD"
		}
	}
	if height <= 0 {
		switch {
		case width >= 3840:
			return "2160p"
		case width >= 1920:
			return "1080p"
		case width >= 1280:
			return "720p"
		case width >= 640:
			return "480p"
		default:
			return "SD"
		}
	}
	switch {
	case width <= 854 && height <= 480:
		return "480p"
	case width <= 1280 && height <= 962:
		return "720p"
	case width <= 2560 && height <= 1440:
		return "1080p"
	case width <= 4096 && height <= 3072:
		return "2160p"
	default:
		if width > 4096 || height > 3072 {
			return "4320p"
		}
		return "SD"
	}
}

// Store persists matched RemuxDB evidence in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get returns the recorded evidence for a release identity.
func (s *Store) Get(ctx context.Context, contentID, episodeID string, folderID int, candidateURI string) (Evidence, bool, error) {
	var ev Evidence
	if s == nil || s.pool == nil {
		return ev, false, nil
	}
	var videoJSON, audioJSON, subtitleJSON []byte
	var method string
	err := s.pool.QueryRow(ctx, `
		SELECT content_id, episode_id, media_folder_id, candidate_uri, match_method,
		       matched_size, matched_content_hash, container, codec_video, codec_audio,
		       resolution, hdr, hdr_known, duration, bitrate,
		       video_tracks, audio_tracks, subtitle_tracks
		FROM remuxdb_match_evidence
		WHERE content_id=$1 AND episode_id=$2 AND media_folder_id=$3 AND candidate_uri=$4 AND expires_at > now()`,
		contentID, episodeID, folderID, candidateURI).Scan(
		&ev.ContentID, &ev.EpisodeID, &ev.MediaFolderID, &ev.CandidateURI, &method,
		&ev.MatchedSize, &ev.MatchedHash, &ev.Container, &ev.CodecVideo, &ev.CodecAudio,
		&ev.Resolution, &ev.HDR, &ev.HDRKnown, &ev.Duration, &ev.Bitrate,
		&videoJSON, &audioJSON, &subtitleJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Evidence{}, false, nil
		}
		return Evidence{}, false, fmt.Errorf("get remuxdb match evidence: %w", err)
	}
	ev.MatchMethod = MatchMethod(method)
	if err := decodeTrackList(videoJSON, &ev.VideoTracks, "video tracks"); err != nil {
		return Evidence{}, false, err
	}
	if err := decodeTrackList(audioJSON, &ev.AudioTracks, "audio tracks"); err != nil {
		return Evidence{}, false, err
	}
	if err := decodeTrackList(subtitleJSON, &ev.SubtitleTracks, "subtitle tracks"); err != nil {
		return Evidence{}, false, err
	}
	return ev, true, nil
}

// decodeTrackList unmarshals one stored track inventory, surfacing corrupt
// JSON as an error instead of leaving a silently empty inventory. A nil/empty
// payload is treated as "not yet probed" and yields no tracks.
func decodeTrackList(data []byte, dst any, label string) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode remuxdb %s: %w", label, err)
	}
	return nil
}

// Record upserts the matched evidence for a release identity.
func (s *Store) Record(ctx context.Context, ev Evidence) error {
	if s == nil || s.pool == nil {
		return nil
	}
	videoJSON, err := json.Marshal(ev.VideoTracks)
	if err != nil {
		return fmt.Errorf("marshal remuxdb video tracks: %w", err)
	}
	audioJSON, err := json.Marshal(ev.AudioTracks)
	if err != nil {
		return fmt.Errorf("marshal remuxdb audio tracks: %w", err)
	}
	subtitleJSON, err := json.Marshal(ev.SubtitleTracks)
	if err != nil {
		return fmt.Errorf("marshal remuxdb subtitle tracks: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO remuxdb_match_evidence(
			content_id, episode_id, media_folder_id, candidate_uri, match_method,
			matched_size, matched_content_hash, container, codec_video, codec_audio,
			resolution, hdr, hdr_known, duration, bitrate,
			video_tracks, audio_tracks, subtitle_tracks, matched_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,NOW(),NOW() + interval '30 days')
		ON CONFLICT (content_id, episode_id, media_folder_id, candidate_uri)
		DO UPDATE SET match_method=EXCLUDED.match_method,
			matched_size=EXCLUDED.matched_size,
			matched_content_hash=EXCLUDED.matched_content_hash,
			container=EXCLUDED.container,
			codec_video=EXCLUDED.codec_video,
			codec_audio=EXCLUDED.codec_audio,
			resolution=EXCLUDED.resolution,
			hdr=EXCLUDED.hdr,
			hdr_known=EXCLUDED.hdr_known,
			duration=EXCLUDED.duration,
			bitrate=EXCLUDED.bitrate,
			video_tracks=EXCLUDED.video_tracks,
			audio_tracks=EXCLUDED.audio_tracks,
			subtitle_tracks=EXCLUDED.subtitle_tracks,
			matched_at=NOW(),
			expires_at=NOW() + interval '30 days'`,
		ev.ContentID, ev.EpisodeID, ev.MediaFolderID, ev.CandidateURI, string(ev.MatchMethod),
		ev.MatchedSize, ev.MatchedHash, ev.Container, ev.CodecVideo, ev.CodecAudio,
		ev.Resolution, ev.HDR, ev.HDRKnown, ev.Duration, ev.Bitrate,
		videoJSON, audioJSON, subtitleJSON)
	if err != nil {
		return fmt.Errorf("record remuxdb match evidence: %w", err)
	}
	return nil
}

// PruneExpired deletes evidence rows whose expires_at timestamp is in the past.
func (s *Store) PruneExpired(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM remuxdb_match_evidence WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("prune expired remuxdb match evidence: %w", err)
	}
	return tag.RowsAffected(), nil
}
