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
	// Rejected marks a candidate a configured custom format rejects. It is a
	// transient ranking signal, recomputed on every list; reject means
	// rank-last and last-resort selectable, never a hard drop.
	Rejected bool
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

// qualityProfileForPath returns the quality profile selected by a virtual
// URI's ?profile= label. It returns the zero profile when profiles are
// disabled or the label is unknown; ranking still applies custom formats with
// a zero profile, it just leaves the profile-specific tie-breaks inert.
func (s *Service) qualityProfileForPath(virtualPath string) quality.QualityProfile {
	if s == nil || !s.cfg.Quality.EnableProfiles {
		return quality.QualityProfile{}
	}
	parsed, err := url.Parse(virtualPath)
	if err != nil || parsed == nil {
		return quality.QualityProfile{}
	}
	label := strings.TrimSpace(parsed.Query().Get("profile"))
	if label == "" {
		return quality.QualityProfile{}
	}
	for _, p := range s.cfg.Quality.Profiles {
		if strings.EqualFold(strings.TrimSpace(p.Label), label) {
			return p
		}
	}
	return quality.QualityProfile{}
}

// rankCandidatesForVirtualPath is the single ranking step shared by
// ListStreams and ResolveDetailed. It scores custom formats, records the
// rejected verdict on each candidate, and orders accepted before rejected,
// then by score, resolution, source, language and OriginalIndex. It mutates
// the slice in place; the resolver hands out a private clone per call, so the
// caller owns it.
func (s *Service) rankCandidatesForVirtualPath(virtualPath string, candidates []stream.StreamCandidate) {
	quality.SortCandidatesForProfile(candidates, s.qualityProfileForPath(virtualPath), s.cfg.Quality.CustomFormats)
}

// warnIfAllRejected logs once when every candidate in the set is rejected by
// custom formats. Reject is rank-last, last-resort selectable, never a hard
// drop, so the caller still uses the best rejected candidate; the Warn tells
// the operator that no accepted release was available.
func (s *Service) warnIfAllRejected(ctx context.Context, virtualPath string, candidates []stream.StreamCandidate) {
	if s == nil || s.logger == nil || len(candidates) == 0 {
		return
	}
	for _, c := range candidates {
		if !c.CustomFormatRejected {
			return
		}
	}
	profile := s.qualityProfileForPath(virtualPath)
	s.logger.WarnContext(ctx, "every virtual candidate is rejected by custom formats; using the best rejected stream",
		"profile", strings.TrimSpace(profile.Label),
		"candidates", len(candidates),
		"rejected_by", quality.RejectingFormatNames(candidates[0], s.cfg.Quality.CustomFormats))
}

// Resolve resolves a virtual path to a concrete stream URL.
func (s *Service) Resolve(ctx context.Context, virtualPath string) (string, error) {
	res, err := s.ResolveDetailed(ctx, virtualPath, false, nil, "", true)
	if err != nil {
		return "", err
	}
	return res.URL, nil
}

// Refresh resolves a virtual path with forceRefresh enabled.
func (s *Service) Refresh(ctx context.Context, virtualPath string) (string, error) {
	res, err := s.ResolveDetailed(ctx, virtualPath, true, nil, "", true)
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
//   - sessionBound declares whether the caller is resolving a release an
//     existing session is already serving. A quality profile is a selection
//     preference, not a gate: a session-bound candidate that still exists in
//     the provider list is served even when it fails the profile (the mismatch
//     is logged, never an error), because refusing would not prevent a release
//     swap and would only break playback. Only an indicted candidate (an
//     explicit exclusion, a collapsed-keeper exclusion, or a confirmed decode
//     rejection) is refused when substitution is disallowed. A fresh selection
//     (sessionBound false) whose probed pin is profile-removed, stale-failed,
//     or absent falls through to the best live candidate that satisfies the
//     profile and is re-pinned by the caller; when nothing satisfies the
//     profile and fallback is disallowed the caller gets the existing
//     no-stream-matches-profile error. A dead/absent pin falls back either way;
//   - a pin whose multi-file variant dedup collapsed resolves to the surviving
//     keeper of that release (a file swap inside the release, never a release
//     swap), because dedup preserves exactly one candidate per release and
//     reports the dropped -> keeper map; the translated pin then overrides
//     rank and reject and respects exclusions, the profile filter and
//     allowCandidateSubstitution exactly like the original pin;
//   - a genuinely dead pin (absent with no keeper) still falls back;
//   - when substitution is refused and a preferredCandidateID names a
//     resolvable session release, the candidate actually served must belong to
//     that release: a present resultID for a different release is refused
//     rather than swapped in, while substitution allowed still lets the
//     explicit resultID win;
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
	sessionBound bool,
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
	if parseErr == nil && parsedURI != nil {
		resultID = parsedURI.Query().Get("result")
	}

	var (
		candidates []stream.StreamCandidate
		keepers    map[string]string
		err        error
	)
	if forceRefresh {
		candidates, keepers, _, _, err = s.Resolver.GetCandidatesFreshWithKeepers(ctx, virtualPath)
	} else {
		candidates, keepers, _, _, err = s.Resolver.GetCandidatesWithKeepers(ctx, virtualPath)
	}
	if err != nil {
		return ResolvedVirtualStream{}, err
	}

	excluded := make(map[string]struct{}, len(excludedCandidateIDs))
	for _, id := range excludedCandidateIDs {
		excluded[id] = struct{}{}
	}

	if len(candidates) == 0 {
		return ResolvedVirtualStream{}, fmt.Errorf("no streams available from provider")
	}

	// Rank through the same helper ListStreams uses, so the version list and
	// the resolver agree on order and on the rejected verdict. Reject is
	// rank-last, last-resort selectable; an all-rejected set still resolves.
	s.rankCandidatesForVirtualPath(virtualPath, candidates)
	s.warnIfAllRejected(ctx, virtualPath, candidates)

	// A pin whose variant dedup collapsed is absent from the list, but its
	// release survives as a keeper. Translate it to that keeper before any pin
	// state is computed, so a collapsed pin behaves exactly like the real pin:
	// it overrides rank and reject, honors its exclusions and the profile
	// filter, and respects allowSubstitution=false. A genuinely dead pin (no
	// keeper) is left unchanged and still falls back as before.
	requestedResultID := resultID
	effectiveResultID := resultID
	if effectiveResultID != "" && !candidateIDPresent(candidates, effectiveResultID) {
		if keeperID := keepers[effectiveResultID]; keeperID != "" {
			effectiveResultID = keeperID
		}
	}
	effectivePreferredID := preferredCandidateID
	if effectivePreferredID != "" && !candidateIDPresent(candidates, effectivePreferredID) {
		if keeperID := keepers[effectivePreferredID]; keeperID != "" {
			effectivePreferredID = keeperID
		}
	}
	// Whether the session's pinned release, as resolved through the keeper map,
	// is present in the ranked set. Captured before the profile filter so a
	// session release the profile removes still blocks substitution below.
	sessionReleaseResolvable := preferredCandidateID != "" && candidateIDPresent(candidates, effectivePreferredID)

	profile := s.qualityProfileForPath(virtualPath)
	profileActive := strings.TrimSpace(profile.Label) != ""

	// A quality profile is a selection preference, not a gate on a release an
	// existing session is already bound to. When the session's own candidate
	// still exists in the provider list it is served even if it fails the
	// profile: refusing would not prevent a release swap (nothing is
	// substituted) and would only break playback. The mismatch is logged for
	// diagnosis.
	sessionCandidatePresent := sessionBound && effectiveResultID != "" && candidateIDPresent(candidates, effectiveResultID)
	if sessionCandidatePresent && profileActive {
		for _, c := range candidates {
			if stream.CandidateVariantID(c) != effectiveResultID {
				continue
			}
			if !quality.MatchProfile(c, profile) && s.logger != nil {
				s.logger.InfoContext(ctx, "session-bound virtual candidate does not satisfy the quality profile; serving the bound candidate",
					"candidate_id", effectiveResultID, "profile", strings.TrimSpace(profile.Label))
			}
			break
		}
	}

	// The profile filter selects among candidates. It must not remove a
	// session-bound candidate that still exists: the session binding wins over
	// the selection preference. For a fresh selection (or a session candidate
	// that is gone) it applies as before, and the no-stream-matches-profile
	// error stays for a selection that cannot be satisfied with fallback
	// disallowed.
	if profileActive && !sessionCandidatePresent {
		filtered := make([]stream.StreamCandidate, 0, len(candidates))
		for _, c := range candidates {
			if quality.MatchProfile(c, profile) {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 && !s.cfg.Quality.FallbackToAnyStream {
			if s.logger != nil {
				s.logger.WarnContext(ctx, "virtual candidate set empty after profile filter",
					"profile", strings.TrimSpace(profile.Label),
					"total", len(candidates), "matched", 0, "fallback", false)
			}
			return ResolvedVirtualStream{}, fmt.Errorf("no stream matches profile %q", strings.TrimSpace(profile.Label))
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}

	// Only an indicted candidate is refused when substitution is disallowed: an
	// explicit exclusion, a collapsed-keeper exclusion, or a confirmed decode
	// rejection. All three arrive through excludedCandidateIDs. A profile
	// mismatch is a selection preference and never blocks.
	_, requestedExcluded := excluded[requestedResultID]
	_, keeperExcluded := excluded[effectiveResultID]
	pinBlocked := effectiveResultID != "" && (requestedExcluded || keeperExcluded)
	// A blocked pin is only substitutable when the caller asked for candidate
	// rotation. Otherwise refuse rather than hand back a different release
	// under the same session binding.
	if pinBlocked && !allowSubstitution {
		return ResolvedVirtualStream{}, fmt.Errorf("pinned virtual candidate %q is excluded and candidate rotation was not requested", effectiveResultID)
	}
	// When substitution is refused and the session's pinned release is
	// resolvable, the candidate actually served must belong to that release. The
	// handler probes candidates by URI, so resultID is the probed candidate and
	// the session pin arrives only as preferredCandidateID; without this guard a
	// present resultID for a different release wins and swaps the release under
	// the session binding even though substitution was refused. A session pin
	// whose release is not resolvable (a genuinely dead release) is left to the
	// documented dead-pin fallback. When substitution is allowed the explicit
	// resultID still wins.
	if !allowSubstitution && sessionReleaseResolvable && effectiveResultID != "" && effectiveResultID != effectivePreferredID {
		return ResolvedVirtualStream{}, fmt.Errorf(
			"session-bound virtual candidate %q does not match resolved candidate %q and candidate rotation was not requested",
			effectivePreferredID, effectiveResultID)
	}

	ordered := orderCandidates(candidates, effectivePreferredID)

	var lastErr error
	// tryCandidate returns the resolved stream for one candidate, or false when
	// the candidate is excluded, is the blocked pin, does not satisfy
	// requirePin, or fails URL validation.
	tryCandidate := func(c stream.StreamCandidate, requirePin bool) (ResolvedVirtualStream, bool) {
		id := stream.CandidateVariantID(c)
		if _, skip := excluded[id]; skip {
			return ResolvedVirtualStream{}, false
		}
		if pinBlocked && id == effectiveResultID {
			return ResolvedVirtualStream{}, false
		}
		if requirePin && id != effectiveResultID {
			return ResolvedVirtualStream{}, false
		}
		validated, validateErr := s.validateStreamURL(ctx, c.URL)
		if validateErr != nil {
			lastErr = validateErr
			return ResolvedVirtualStream{}, false
		}
		return ResolvedVirtualStream{
			URL:            validated,
			URI:            withResultKey(virtualPath, id),
			CandidateID:    id,
			RequestHeaders: c.RequestHeaders,
			ExpiresAt:      c.ExpiresAt,
		}, true
	}

	// The explicit pin is tried first even when rank or a custom-format reject
	// would place it last: a pin overrides rank and reject.
	if effectiveResultID != "" && !pinBlocked {
		for _, c := range ordered {
			if resolved, ok := tryCandidate(c, true); ok {
				return resolved, nil
			}
		}
	}
	// Ranked alternatives. A session preferredCandidateID is already promoted
	// to the front by orderCandidates; a dead pin, or a blocked pin whose
	// substitution was requested, resolves here.
	for _, c := range ordered {
		if resolved, ok := tryCandidate(c, false); ok {
			return resolved, nil
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
	// Rank here too: the handler's auto-pick walks ListStreams, not
	// ResolveDetailed, so the profile/custom-format order and the rejected
	// verdict must be carried on the stream records.
	s.rankCandidatesForVirtualPath(virtualPath, candidates)
	s.warnIfAllRejected(ctx, virtualPath, candidates)
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
			Rejected:            c.CustomFormatRejected,
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

// candidateIDPresent reports whether any candidate carries the given stable
// variant id.
func candidateIDPresent(candidates []stream.StreamCandidate, id string) bool {
	if id == "" {
		return false
	}
	for _, c := range candidates {
		if stream.CandidateVariantID(c) == id {
			return true
		}
	}
	return false
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
