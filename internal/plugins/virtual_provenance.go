package plugins

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/models"
)

// CoreInsecureAllowed is the core-side opt-in reader for the HTTP-manifest
// dispatch. It reports whether virtual_library.allow_insecure_http is enabled.
// It is a func field (not an import) so the plugins package never depends on
// the settings store: wiring sets it, and nil stays fail-closed (false).
//
// Phase 4 must set this at wiring from the live settings repo.
var CoreInsecureAllowed func(ctx context.Context) bool

// CorePrivateStreamsAllowed is the core-side opt-in reader for private
// stream destinations. It reports whether
// virtual_library.allow_private_streams is enabled. Wiring sets it from the
// live settings repo; nil stays fail-closed (false).
var CorePrivateStreamsAllowed func(ctx context.Context) bool

// CoreVirtualInsecureAllowed reads the core virtual_library.allow_insecure_http
// opt-in (HTTP manifests on private/local hosts) through the wired
// CoreInsecureAllowed callback. A nil callback fails closed (false), the same
// posture as InstallationAllowsInsecure for unknown installations.
func CoreVirtualInsecureAllowed(ctx context.Context) bool {
	if CoreInsecureAllowed == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return CoreInsecureAllowed(ctx)
}

// CoreVirtualPrivateStreamsAllowed reads the core
// virtual_library.allow_private_streams opt-in (private stream destinations)
// through the wired CorePrivateStreamsAllowed callback. A nil callback fails
// closed (false).
func CoreVirtualPrivateStreamsAllowed(ctx context.Context) bool {
	if CorePrivateStreamsAllowed == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return CorePrivateStreamsAllowed(ctx)
}

// coreProvenanceInsecure resolves the HTTP-manifest posture for
// core-provenance virtual rows: the plugin installation config is irrelevant,
// so only the core virtual_library.allow_insecure_http setting applies.
// Unknown or inconsistent provenance fails closed (false): a core row that
// does not positively carry Provenance "core" never inherits the core opt-in.
func coreProvenanceInsecure(ctx context.Context, provenance models.VirtualProvenance) bool {
	if provenance != models.VirtualProvenanceCore {
		return false
	}
	return CoreVirtualInsecureAllowed(ctx)
}

// coreProvenancePrivateStreams resolves the private stream destination posture
// for core-provenance virtual rows: only the core
// virtual_library.allow_private_streams setting applies. Unknown or
// inconsistent provenance fails closed (false).
func coreProvenancePrivateStreams(ctx context.Context, provenance models.VirtualProvenance) bool {
	if provenance != models.VirtualProvenanceCore {
		return false
	}
	return CoreVirtualPrivateStreamsAllowed(ctx)
}

// AllowInsecureForProvenance dispatches the HTTP-manifest allow-insecure
// decision by explicit provenance. Core-provenance rows read
// virtual_library.allow_insecure_http through the wired CoreInsecureAllowed
// callback (nil-safe: nil fails closed); every other provenance (plugin,
// legacy "", local, or anything unexpected) falls back to the existing
// per-installation check, whose behavior — including the <=0 fail-closed
// rejection — is preserved byte-for-byte.
func (s *Service) AllowInsecureForProvenance(ctx context.Context, provenance models.VirtualProvenance, installationID int) bool {
	if provenance == models.VirtualProvenanceCore || (provenance == "" && installationID <= 0) {
		return coreProvenanceInsecure(ctx, models.VirtualProvenanceCore)
	}
	if s == nil {
		return false
	}
	return s.InstallationAllowsInsecure(ctx, installationID)
}

func (s *Service) AllowPrivateStreamsForProvenance(ctx context.Context, provenance models.VirtualProvenance, installationID int) bool {
	if provenance == models.VirtualProvenanceCore || (provenance == "" && installationID <= 0) {
		return coreProvenancePrivateStreams(ctx, models.VirtualProvenanceCore)
	}
	return false
}

// ResolveVirtualPlaybackDetailedForProvenance resolves through the standard

// ResolveVirtualPlaybackDetailedForProvenance resolves through the standard
// installation dispatch, computing AllowInsecure (HTTP manifests) and
// AllowPrivateStreams (private stream destinations) from explicit provenance
// instead of unconditionally consulting the plugin installation config. This
// is the core-path entry point retiring
// com.drondeseries.vio-virtual-library: core rows must not depend on a plugin
// installation's local-network opt-in, and a missing core opt-in must not be
// rescued by a plugin's.
func (s *Service) ResolveVirtualPlaybackDetailedForProvenance(
	ctx context.Context,
	virtualPath string,
	userID int,
	profileID string,
	provenance models.VirtualProvenance,
	ownerInstallationID int,
	allowFallback bool,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
) (ResolvedVirtualStream, error) {
	return s.ResolveVirtualPlaybackDetailedWithRouting(ctx, virtualPath, userID, profileID, VirtualPlaybackRouting{
		OwnerInstallationID: ownerInstallationID,
		AllowFallback:       allowFallback,
		AllowInsecure:       s.AllowInsecureForProvenance(ctx, provenance, ownerInstallationID),
		AllowPrivateStreams: s.AllowPrivateStreamsForProvenance(ctx, provenance, ownerInstallationID),
	}, forceRefresh, excludedCandidateIDs, preferredCandidateID)
}
