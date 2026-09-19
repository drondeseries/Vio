package virtuallibrary

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// ResolvedVirtualStream is the core-resolved counterpart of the plugin's
// ResolvedVirtualStream: a validated stream URL plus candidate identity and
// request headers for playback. Provenance is always core.
type ResolvedVirtualStream struct {
	URL            string
	URI            string
	CandidateID    string
	RequestHeaders map[string]string
	ExpiresAt      time.Time
}

// PlaybackStream represents an available stream candidate formatted for
// playback selection in API handlers and Jellyfin compatibility.
type PlaybackStream struct {
	ID                  string
	Label               string
	URI                 string
	Resolution          string
	CodecVideo          string
	CodecAudio          string
	HasAtmos            bool
	QualityScore        int
	RequestHeaders      map[string]string
	ExpiresAt           time.Time
	HDR                 string
	SourceType          string
	FileSize            int64
	Container           string
	Bitrate             int
	FrameRate           string
	AudioLanguages      []string
	SubtitleLanguages   []string
	OwnerInstallationID int
	Visible             bool
	VisibilitySpecified bool
}

// validateStreamURL checks structural syntax and enforces SSRF protection
// using the central remotestream policy. When AllowInsecureHTTP is enabled,
// it permits private and local network destinations; otherwise, loopback,
// RFC 1918, link-local, and multicast addresses are rejected.
func (s *Service) validateStreamURL(ctx context.Context, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty provider stream URL")
	}
	if strings.ContainsAny(trimmed, "\x00\r\n") {
		return "", fmt.Errorf("provider stream URL contains control characters")
	}
	if s.cfg.AllowInsecureHTTP {
		parsed, err := remotestream.ValidateURLSyntaxAllowNonPublic(trimmed)
		if err != nil {
			return "", fmt.Errorf("invalid stream URL syntax: %w", err)
		}
		return parsed.String(), nil
	}
	validated, err := remotestream.ValidateURL(ctx, trimmed)
	if err != nil {
		return "", fmt.Errorf("stream URL rejected by SSRF policy: %w", err)
	}
	return validated.String(), nil
}

// withResultKey appends the candidate identity as ?result=, mirroring the
// plugin's withVirtualResultKey.
func withResultKey(virtualPath, candID string) string {
	if candID == "" {
		return virtualPath
	}
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return virtualPath + "?result=" + candID
	}
	q := parsed.Query()
	q.Set("result", candID)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// Resolve resolves a virtual path to a concrete stream URL.
func (s *Service) Resolve(ctx context.Context, virtualPath string) (string, error) {
	res, err := s.ResolveDetailed(ctx, virtualPath, false, nil, "")
	if err != nil {
		return "", err
	}
	return res.URL, nil
}

// Refresh resolves a virtual path with forceRefresh enabled.
func (s *Service) Refresh(ctx context.Context, virtualPath string) (string, error) {
	res, err := s.ResolveDetailed(ctx, virtualPath, true, nil, "")
	if err != nil {
		return "", err
	}
	return res.URL, nil
}

// ResolveDetailed resolves a virtual path to a concrete stream URL through
// the core resolver, preserving full candidate identity and selection semantics:
//
//   - candidate IDs use stream.CandidateVariantID, exactly matching the plugin;
//   - a pinned resultID (from the URI ?result=) selects that candidate first;
//   - profile filtering applies if ?profile= is present and profiles are enabled;
//   - excludedCandidateIDs are skipped;
//   - preferredCandidateID is tried first;
//   - every stream URL is validated against outbound SSRF.
//
// allowCandidateSubstitution gates the fallback that lets a pinned result= URI
// resolve to a *different* sibling candidate. It defaults to true so existing
// callers keep their behavior. A caller that excludes the pinned candidate
// without a verdict that indicts the release must pass false: a display-driven
// fallback (Dolby Vision to HDR10/HDR10+/DV8.1) changes the transformation on
// the same file and must never silently swap the release mid-stream. A pinned
// id that is merely absent from the provider list is a dead release and still
// falls back, so a genuinely unavailable provider recovers.
func (s *Service) ResolveDetailed(
	ctx context.Context,
	virtualPath string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
	allowCandidateSubstitution ...bool,
) (ResolvedVirtualStream, error) {
	allowSubstitution := true
	if len(allowCandidateSubstitution) > 0 {
		allowSubstitution = allowCandidateSubstitution[0]
	}
	if s == nil || s.Resolver == nil {
		return ResolvedVirtualStream{}, ErrVirtualLibraryUnavailable
	}

	parsedURI, parseErr := url.Parse(virtualPath)
	resultID := ""
	profileLabel := ""
	if parseErr == nil && parsedURI != nil {
		resultID = parsedURI.Query().Get("result")
		profileLabel = parsedURI.Query().Get("profile")
	}

	var (
		candidates []stream.StreamCandidate
		err        error
	)
	if forceRefresh {
		candidates, _, _, err = s.Resolver.GetCandidatesFresh(ctx, virtualPath)
	} else {
		candidates, _, _, err = s.Resolver.GetCandidates(ctx, virtualPath)
	}
	if err != nil {
		return ResolvedVirtualStream{}, err
	}

	excluded := make(map[string]struct{}, len(excludedCandidateIDs))
	for _, id := range excludedCandidateIDs {
		excluded[id] = struct{}{}
	}

	ordered := orderCandidates(candidates, preferredCandidateID)

	// Filter by quality profile if specified in the URI
	if profileLabel != "" && s.cfg.Quality.EnableProfiles {
		var matchedProfile *quality.QualityProfile
		for _, p := range s.cfg.Quality.Profiles {
			if strings.EqualFold(p.Label, profileLabel) {
				matchedProfile = &p
				break
			}
		}
		if matchedProfile != nil {
			profileFiltered := make([]stream.StreamCandidate, 0, len(ordered))
			for _, c := range ordered {
				if quality.MatchProfile(c, *matchedProfile) {
					profileFiltered = append(profileFiltered, c)
				}
			}
			if len(profileFiltered) > 0 {
				ordered = profileFiltered
			} else if !s.cfg.Quality.FallbackToAnyStream {
				return ResolvedVirtualStream{}, fmt.Errorf("no stream matches profile %q", profileLabel)
			}
		}
	}

	var lastErr error
	for _, c := range ordered {
		id := stream.CandidateVariantID(c)
		if _, skip := excluded[id]; skip {
			continue
		}
		if resultID != "" && id != resultID {
			continue
		}
		validated, validateErr := s.validateStreamURL(ctx, c.URL)
		if validateErr != nil {
			lastErr = validateErr
			continue
		}
		return ResolvedVirtualStream{
			URL:            validated,
			URI:            withResultKey(virtualPath, id),
			CandidateID:    id,
			RequestHeaders: c.RequestHeaders,
			ExpiresAt:      c.ExpiresAt,
		}, nil
	}

	// A pinned candidate the caller explicitly excluded is only substitutable
	// when the caller asked for candidate rotation. Otherwise refuse rather
	// than hand back a different release under the same session binding.
	if _, pinnedExcluded := excluded[resultID]; resultID != "" && pinnedExcluded && !allowSubstitution {
		return ResolvedVirtualStream{}, fmt.Errorf("pinned virtual candidate %q is excluded and candidate rotation was not requested", resultID)
	}

	// Pinned resultID not found/invalid: fall back to best alternative candidate
	if resultID != "" && len(ordered) > 1 {
		for _, c := range ordered {
			id := stream.CandidateVariantID(c)
			if _, skip := excluded[id]; skip {
				continue
			}
			validated, validateErr := s.validateStreamURL(ctx, c.URL)
			if validateErr != nil {
				continue
			}
			return ResolvedVirtualStream{
				URL:            validated,
				URI:            withResultKey(virtualPath, id),
				CandidateID:    id,
				RequestHeaders: c.RequestHeaders,
				ExpiresAt:      c.ExpiresAt,
			}, nil
		}
	}

	if lastErr != nil {
		return ResolvedVirtualStream{}, fmt.Errorf("virtual playback provider returned an unsafe stream URL: %w", lastErr)
	}
	return ResolvedVirtualStream{}, fmt.Errorf("no streams available from provider")
}

// ListStreams returns the complete candidate list formatted as PlaybackStream records.
func (s *Service) ListStreams(ctx context.Context, virtualPath string) ([]PlaybackStream, error) {
	if s == nil || s.Resolver == nil {
		return nil, ErrVirtualLibraryUnavailable
	}
	candidates, _, _, err := s.Resolver.GetCandidates(ctx, virtualPath)
	if err != nil {
		return nil, err
	}
	streams := make([]PlaybackStream, 0, len(candidates))
	for _, c := range candidates {
		id := stream.CandidateVariantID(c)
		label := stream.CandidateDisplayName(c)
		streams = append(streams, PlaybackStream{
			ID:                  id,
			Label:               label,
			URI:                 withResultKey(virtualPath, id),
			Resolution:          c.Resolution,
			CodecVideo:          c.CodecVideo,
			CodecAudio:          c.CodecAudio,
			HasAtmos:            c.HasAtmos,
			QualityScore:        c.QualityScore,
			RequestHeaders:      c.RequestHeaders,
			ExpiresAt:           c.ExpiresAt,
			HDR:                 c.HDR,
			SourceType:          c.SourceType,
			FileSize:            c.FileSize,
			Container:           c.Container,
			Bitrate:             0,
			FrameRate:           "",
			AudioLanguages:      c.AudioLanguages,
			SubtitleLanguages:   c.SubtitleLanguages,
			OwnerInstallationID: 0,
			Visible:             true,
			VisibilitySpecified: true,
		})
	}
	return streams, nil
}

func orderCandidates(candidates []stream.StreamCandidate, preferredID string) []stream.StreamCandidate {
	out := append([]stream.StreamCandidate(nil), candidates...)
	if preferredID == "" {
		return out
	}
	for i, c := range out {
		if stream.CandidateVariantID(c) == preferredID {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}
