package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Silo-Server/silo-server/internal/access"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

const (
	// maxVersionCheckFiles caps the batch so a single request cannot fan out
	// unbounded provider work.
	maxVersionCheckFiles = 40
	// versionCheckConcurrency bounds simultaneous provider resolutions.
	versionCheckConcurrency = 4
	// versionCheckPerFileBudget bounds one file's provider resolution.
	versionCheckPerFileBudget = 6 * time.Second
	// versionCheckOverallBudget bounds the whole batch.
	versionCheckOverallBudget = 15 * time.Second
)

type versionCheckRequest struct {
	FileIDs []int `json:"file_ids"`
}

type versionCheckResult struct {
	FileID    int  `json:"file_id"`
	Available bool `json:"available"`
}

type versionCheckResponse struct {
	Results []versionCheckResult `json:"results"`
}

// HandleCheckVersions implements POST /catalog/versions/check: a batched
// liveness probe for the media page's version list. Each file is tested
// cheaply — virtual rows resolve their pinned ?result= candidate through the
// provider (no media transfer), local rows are read from missing_since — and
// the durable failed_at signal is stamped accordingly. Unknown or deleted
// file IDs are reported as available=false.
func (h *CatalogResourceHandler) HandleCheckVersions(w http.ResponseWriter, r *http.Request) {
	var req versionCheckRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one file ID is required")
		return
	}
	if len(req.FileIDs) > maxVersionCheckFiles {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "At most 40 file IDs are allowed per request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), versionCheckOverallBudget)
	defer cancel()

	results := make([]versionCheckResult, 0, len(req.FileIDs))
	var mu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(versionCheckConcurrency)
	for _, id := range req.FileIDs {
		if id <= 0 {
			continue
		}
		id := id
		eg.Go(func() error {
			available := h.checkVersion(egCtx, id)
			mu.Lock()
			results = append(results, versionCheckResult{FileID: id, Available: available})
			mu.Unlock()
			return nil
		})
	}
	// Per-file failures are folded into the availability verdict; the batch
	// itself never fails on one file.
	_ = eg.Wait()

	writeJSON(w, http.StatusOK, versionCheckResponse{Results: results})
}

// checkVersion tests one media file's liveness and returns whether it is
// available. The file is authorized for the requesting profile BEFORE any
// resolution: inaccessible IDs are indistinguishable from unknown IDs
// (available=false, no distinguishing error), so a restricted profile cannot
// probe arbitrary files or trigger provider work for them. Virtual rows
// resolve the pinned candidate through the provider and stamp failed_at on a
// confirmed dead pin; local rows are read from missing_since with no probe.
// Ambiguous provider errors (timeout, network, resolver not configured) leave
// the stamp unchanged and report the row's current computed availability, so
// a provider outage cannot mass-tag versions as dead.
func (h *CatalogResourceHandler) checkVersion(ctx context.Context, fileID int) bool {
	if h == nil || h.FileResolver == nil {
		return false
	}
	file, err := h.FileResolver.GetByID(ctx, fileID)
	if err != nil || file == nil {
		return false
	}
	if !h.fileAccessible(ctx, file) {
		// Denied by the profile's catalog/library access policy: report the
		// same shape as an unknown ID and never resolve or stamp.
		return false
	}
	if !isVirtualPlaybackFile(file) {
		return file.MissingSince == nil
	}
	if h.VirtualResolver == nil {
		// No provider resolver wired (playback disabled): report the durable
		// stamp only, never stamp anything.
		return file.FailedAt == nil
	}

	perFileCtx, cancel := context.WithTimeout(ctx, versionCheckPerFileBudget)
	defer cancel()
	requestedCandidateID := virtualResultCandidateID(file.FilePath)
	resolved, err := h.VirtualResolver.ResolveVirtualMediaDetailed(
		perFileCtx, file.FilePath, file.VirtualOwnerInstallationID,
		apimw.GetUserID(ctx), apimw.GetProfileID(ctx), true, nil, requestedCandidateID,
	)
	if err == nil {
		// Strict check semantics, not playback fallback semantics: with
		// forceRefresh=true the selection code does not substitute candidates[0]
		// for an absent pin, and the identity verification below is the
		// belt-and-braces guarantee. A successful resolution only counts when it
		// actually named the requested candidate — a substituted candidate is
		// evidence the pin is gone, not that it recovered. And listing
		// availability is deliberately kept separate from transport health: a
		// metadata-only check never clears an existing transport-failure stamp
		// (failed_at), because a resolved URL is not evidence the media
		// endpoint delivers bytes; only a real delivery does (see
		// StreamHandler.clearVirtualCandidateRecovered).
		if requestedCandidateID != "" && resolvedIdentityMatches(resolved, requestedCandidateID) {
			return true
		}
		if requestedCandidateID == "" {
			// The row carries no concrete pin (profile-neutral row): any
			// resolution of its identity is listing evidence, still not
			// transport evidence — report live, never clear.
			return true
		}
		// The provider answered with a different candidate: the requested pin
		// is gone. Stamp it (fenced) so the auto-pick skips it.
		if h.MarkVirtualFailed != nil {
			_ = h.MarkVirtualFailed(context.WithoutCancel(ctx), fileID, file.FilePath, file.FailedAt)
		}
		return false
	}
	if isVirtualCandidateDeadError(err) {
		// Confirmed dead pin: the provider listed but the pinned candidate is
		// gone or unusable. Stamp it so the auto-pick skips it. The stamp is
		// fenced the same way: a candidate rotated while resolution was in
		// flight is never mis-marked.
		if h.MarkVirtualFailed != nil {
			_ = h.MarkVirtualFailed(context.WithoutCancel(ctx), fileID, file.FilePath, file.FailedAt)
		}
		return false
	}
	// Ambiguous (provider down, timeout): do not stamp. Report the current
	// durable signal so an outage does not mass-tag versions.
	return file.FailedAt == nil
}

// resolvedIdentityMatches reports whether the resolver's answer named the
// requested candidate: either the returned CandidateID is the requested
// result= value, or the returned URI carries it. Substituted candidates are
// never treated as recovery evidence for the requested pin.
func resolvedIdentityMatches(resolved struct {
	URL            string
	URI            string
	CandidateID    string
	RequestHeaders map[string]string
	ExpiresAt      time.Time
}, requestedCandidateID string) bool {
	if resolved.CandidateID == requestedCandidateID {
		return true
	}
	if parsed, err := url.Parse(resolved.URI); err == nil {
		if strings.TrimSpace(parsed.Query().Get("result")) == requestedCandidateID {
			return true
		}
	}
	return false
}

// fileAccessible applies the requesting profile's catalog/library access
// policy to a media file, mirroring the playback handler's loadAuthorizedFile
// authorization: episodes authorize through their parent series, extras
// through their parent item, and plain files through their own content ID,
// followed by the file-level library/quality predicate. Any failure — missing
// lookup dependencies, an inaccessible parent, or a file outside the allowed
// libraries — denies the file exactly like an unknown ID.
func (h *CatalogResourceHandler) fileAccessible(ctx context.Context, file *models.MediaFile) bool {
	if h == nil || h.ItemAccess == nil {
		return false
	}
	filter := catalog.AccessFilter{
		AllowedLibraryIDs:  accessScopeAllowedLibraryIDs(ctx),
		DisabledLibraryIDs: accessScopeDisabledLibraryIDs(ctx),
		MaxContentRating:   accessScopeMaxContentRating(ctx),
		MaxPlaybackQuality: accessScopeMaxPlaybackQuality(ctx),
		UserID:             apimw.GetUserID(ctx),
		ProfileID:          apimw.GetProfileID(ctx),
	}
	switch {
	case file.EpisodeID != "":
		if h.EpisodeLookup == nil {
			return false
		}
		episode, err := h.EpisodeLookup.GetByID(ctx, file.EpisodeID)
		if err != nil || episode == nil {
			return false
		}
		if err := h.ItemAccess.EnsureAccessible(ctx, episode.SeriesID, filter); err != nil {
			return false
		}
	case file.ContentID != "":
		if err := h.ItemAccess.EnsureAccessible(ctx, file.ContentID, filter); err != nil {
			return false
		}
	case file.ExtraID != "":
		if h.ExtraLookup == nil {
			return false
		}
		extra, err := h.ExtraLookup.GetByID(ctx, file.ExtraID)
		if err != nil || extra == nil {
			return false
		}
		if err := h.ItemAccess.EnsureAccessible(ctx, extra.ParentID, filter); err != nil {
			return false
		}
	default:
		return false
	}
	return catalog.FileAllowedByAccess(file, filter)
}

// isVirtualCandidateDeadError classifies a resolution failure as a confirmed
// dead pin: the provider answered but the pinned candidate is no longer
// offered or usable. Provider-down/timeout errors do not match and are treated
// as ambiguous. The strings are the provider-neutral error texts the plugin
// service produces (see internal/plugins/virtual_playback.go). A joined error
// that also carries a provider RPC failure ("request failed") is ambiguous even
// when a fallback provider reported no matching candidate: the owner provider
// that owns the pin may simply be down.
func isVirtualCandidateDeadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "request failed") ||
		strings.Contains(msg, "resolver is not installed") ||
		strings.Contains(msg, "load owning virtual stream provider") {
		return false
	}
	return strings.Contains(msg, "no matching candidate") ||
		strings.Contains(msg, "no streams available") ||
		strings.Contains(msg, "no usable stream")
}

// The access-scope extractors below mirror requestAccessFilter's mapping of
// the resolved access scope onto a catalog.AccessFilter, but take a context
// instead of an *http.Request because the liveness check fans out per file
// through an errgroup. A missing scope yields the unrestricted zero values,
// exactly like requestAccessFilter.

func accessScopeAllowedLibraryIDs(ctx context.Context) []int {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.AllowedLibraryIDs
	}
	return nil
}

func accessScopeDisabledLibraryIDs(ctx context.Context) []int {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.DisabledLibraryIDs
	}
	return nil
}

func accessScopeMaxContentRating(ctx context.Context) string {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.MaxContentRating
	}
	return ""
}

func accessScopeMaxPlaybackQuality(ctx context.Context) string {
	if scope, ok := access.GetScope(ctx); ok {
		return scope.MaxPlaybackQuality
	}
	return ""
}
