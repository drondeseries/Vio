package virtuallibrary

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

const maxVirtualLabelLen = 256

// VirtualVariants builds the Variants() closure suitable for wiring into
// catalog.LibraryCollectionService.VirtualVariants. It returns a closure that
// always returns ErrVirtualLibraryUnavailable when the service is nil,
// dormant, or failed.
//
// The returned closure is safe for concurrent use. It delegates to the
// resolver for candidate fetching and quality.SortCandidatesForProfile when
// profiles are enabled, then stamps every variant with
// VirtualProvenance=models.VirtualProvenanceCore and
// OwnerInstallationID=0 (core-owned, no plugin installation).
//
// Activation (calling this and wiring it) must come AFTER a successful
// Phase 5 migration; see the boot-order comment in doc.go.
func (s *Service) VirtualVariants() func(ctx context.Context, virtualPath string, mediaType string) ([]catalog.VirtualPlaybackVariant, error) {
	if s == nil {
		return func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
			return nil, ErrVirtualLibraryUnavailable
		}
	}
	return s.Variants
}

// Variants resolves quality-profile-aware playback variants for a virtual
// path. It is the core replacement for the plugin's
// ConfiguredVirtualVariants and must reproduce the Phase 1 parity fixtures
// (URI mapping, sentinel, rejections).
//
// The flow:
//  1. resolver.GetCandidates fetches ranked provider candidates.
//  2. When profiles are enabled (Quality.EnableProfiles), each configured
//     quality profile becomes one VirtualPlaybackVariant with a
//     ?profile=<label> URI. Candidates are sorted within each profile via
//     quality.SortCandidatesForProfile so the client gets a hint about
//     which candidates match.
//  3. When no profiles are configured, a single empty-label sentinel variant
//     is returned (preserving the base URI with core provenance).
//
// Every variant is stamped with VirtualProvenanceCore and
// OwnerInstallationID=0. URI query building mirrors the Phase 1 plugin
// semantics: ?profile=label (label derived from quality.QualityProfile.Label)
// for profiled variants, ?results=all for all-results profiles, stale
// ?result= always stripped. Labels are validated: empty-label rejection,
// control-char rejection (\x00\r\n), and label length limit
// (maxVirtualLabelLen=256).
func (s *Service) Variants(ctx context.Context, virtualPath string, mediaType string) ([]catalog.VirtualPlaybackVariant, error) {
	if s == nil || s.Resolver == nil {
		return nil, ErrVirtualLibraryUnavailable
	}

	cfg := s.cfg.Quality

	// When profiles are enabled, produce one variant per configured profile,
	// in the same order as the quality profiles (which are already sorted
	// by PreferredOrder from quality.Validate). Profile-side candidate
	// matching runs at playback time; here we only produce the URI templates
	// and map profile metadata into the variant fields.
	if cfg.EnableProfiles {
		variants, err := s.variantsFromProfiles(ctx, virtualPath, cfg)
		if err != nil {
			return nil, err
		}
		if len(variants) > 0 {
			return variants, nil
		}
		// Enabled profiles but zero profiles after validation: fall through
		// to sentinel (should not happen when Validate ran, but be defensive).
	}

	// Empty-profiles sentinel: single variant preserving base URI, no profile
	// fields, core provenance. Catalog materialization skips the duplicate
	// URI while retaining its ownership.
	return []catalog.VirtualPlaybackVariant{{
		VirtualURI:          virtualPath,
		Label:               "",
		OwnerInstallationID: 0,
		VirtualProvenance:   models.VirtualProvenanceCore,
	}}, nil
}

// variantsFromProfiles produces one VirtualPlaybackVariant per configured
// quality profile, validating labels and building profile-aware URIs.
func (s *Service) variantsFromProfiles(ctx context.Context, virtualPath string, cfg quality.QualityConfig) ([]catalog.VirtualPlaybackVariant, error) {
	// Resolve candidates once; individual profiles will sort/filter at
	// playback time. We still need a live call to validate the URI is sane.
	candidates, _, _, err := s.Resolver.GetCandidates(ctx, virtualPath)
	if err != nil {
		return nil, err
	}

	variants := make([]catalog.VirtualPlaybackVariant, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		if err := validateProfileLabel(p); err != nil {
			return nil, err
		}
		label := strings.TrimSpace(p.Label)
		// For each profile, sort candidates so the quality hint is available.
		candidatesCopy := cloneStreamCandidates(candidates)
		quality.SortCandidatesForProfile(candidatesCopy, p, cfg.CustomFormats)

		variants = append(variants, catalog.VirtualPlaybackVariant{
			VirtualURI:          buildProfileURI(virtualPath, label),
			Label:               label,
			Resolution:          p.Resolution,
			CodecVideo:          p.CodecVideo,
			CodecAudio:          p.CodecAudio,
			HDR:                 p.HDR,
			OwnerInstallationID: 0,
			VirtualProvenance:   models.VirtualProvenanceCore,
		})
	}
	return variants, nil
}

// validateProfileLabel checks a quality profile label for the Phase 1
// parity rejections: empty/whitespace-only labels, control characters
// (\x00\r\n), and exceeding maxVirtualLabelLen.
func validateProfileLabel(p quality.QualityProfile) error {
	label := strings.TrimSpace(p.Label)
	if label == "" {
		return fmt.Errorf("virtual profile provider returned an unlabeled profile")
	}
	if len(label) > maxVirtualLabelLen {
		return fmt.Errorf("virtual profile provider returned an invalid field")
	}
	for _, value := range []string{
		p.Resolution, p.CodecVideo, p.CodecAudio, p.HDR,
	} {
		if len(value) > maxVirtualLabelLen || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("virtual profile provider returned an invalid field")
		}
	}
	if strings.ContainsAny(label, "\x00\r\n") {
		return fmt.Errorf("virtual profile provider returned an invalid field")
	}
	return nil
}

// buildProfileURI adds a ?profile=<label> query parameter to the base
// virtual URI, stripping any stale ?result= parameter. When the label is
// "All" (case-insensitive), it uses ?results=all instead. The result is
// canonical (Query().Encode() order) so catalog validation succeeds.
func buildProfileURI(virtualPath, label string) string {
	parsed, err := url.Parse(virtualPath)
	if err != nil || parsed.Scheme != "virtual" {
		return virtualPath
	}
	query := parsed.Query()
	if strings.EqualFold(strings.TrimSpace(label), "all") {
		query.Set("results", "all")
		query.Del("profile")
	} else {
		query.Set("profile", label)
		query.Del("results")
	}
	query.Del("result")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// cloneStreamCandidates returns a shallow copy of the candidate slice with
// independent inner slices (AudioLanguages, SubtitleLanguages, RequestHeaders,
// ProxyHeaders) so sorting one profile's copy does not affect another.
func cloneStreamCandidates(candidates []resolver.StreamCandidate) []resolver.StreamCandidate {
	out := make([]resolver.StreamCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = c
		out[i].AudioLanguages = append([]string(nil), c.AudioLanguages...)
		out[i].SubtitleLanguages = append([]string(nil), c.SubtitleLanguages...)
		if c.RequestHeaders != nil {
			out[i].RequestHeaders = make(map[string]string, len(c.RequestHeaders))
			for k, v := range c.RequestHeaders {
				out[i].RequestHeaders[k] = v
			}
		}
		if c.BehaviorHints.ProxyHeaders != nil {
			out[i].BehaviorHints.ProxyHeaders = make(map[string]any, len(c.BehaviorHints.ProxyHeaders))
			for k, v := range c.BehaviorHints.ProxyHeaders {
				out[i].BehaviorHints.ProxyHeaders[k] = v
			}
		}
	}
	return out
}
