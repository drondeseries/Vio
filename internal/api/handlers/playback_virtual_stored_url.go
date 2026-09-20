package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// virtualStoredURLState reports what a virtual candidate row's persisted
// resolved_url means for a session-bound re-resolve.
type virtualStoredURLState int

const (
	// virtualStoredURLMissing means the row does not verifiably own the
	// requested candidate, or carries no stored URL. The caller keeps the
	// existing list-and-resolve behavior.
	virtualStoredURLMissing virtualStoredURLState = iota
	// virtualStoredURLUsable means the row's own stored URL is present,
	// unexpired, and passes the resolver's URL validator. The caller serves
	// it without contacting the provider.
	virtualStoredURLUsable
	// virtualStoredURLExpired means the row owns the requested candidate and
	// carries a stored URL whose expiry has passed. The caller lists and
	// resolves as before, then refreshes the stored value through the
	// existing Phase-1 write path.
	virtualStoredURLExpired
)

// evaluateStoredVirtualURLCandidate decides whether a catalog row's persisted
// resolved_url may be served for the exact requested candidate.
//
// Same-row enforcement: the caller looks the row up by the requested
// candidate's file_path (exact match) and this function re-checks it with
// sameVirtualReleaseIdentity, which is strict about concrete identity — a row
// carrying the provider-neutral path (no ?result= pick) does NOT satisfy a
// pinned request. A stored URL therefore always belongs to the candidate being
// resolved; it is the pinned release itself, never a substitute, so the
// session-binding guard is satisfied by construction.
//
// The list-time URL is persisted before URL validation (Phase 1 stores the
// provider's raw URL), so it is re-validated here through
// virtuallibrary.ValidateProviderStreamURL — the same validator the resolver
// applies to a fresh listing. A URL that fails validation is never fetched;
// the caller falls back to listing.
func evaluateStoredVirtualURLCandidate(
	ctx context.Context,
	candidateURI string,
	row *models.MediaFile,
	allowInsecure bool,
	now time.Time,
) (ResolvedVirtualMedia, virtualStoredURLState) {
	candidateID := virtualResultCandidateID(candidateURI)
	if candidateID == "" || row == nil || row.ID <= 0 {
		return ResolvedVirtualMedia{}, virtualStoredURLMissing
	}
	// The row must own this concrete release. The exact-path lookup already
	// guarantees row.FilePath == candidateURI, but re-checking the release
	// identity keeps the guarantee explicit and independent of the lookup.
	if !sameVirtualReleaseIdentity(row.FilePath, candidateURI) {
		return ResolvedVirtualMedia{}, virtualStoredURLMissing
	}
	storedURL := strings.TrimSpace(row.ResolvedURL)
	if storedURL == "" {
		return ResolvedVirtualMedia{}, virtualStoredURLMissing
	}
	// A nil expiry is usable: the provider declared no lifetime. A non-nil
	// expiry at or before now is expired and must be refreshed by a resolve.
	if row.ResolvedURLExpiresAt != nil && !now.Before(*row.ResolvedURLExpiresAt) {
		return ResolvedVirtualMedia{}, virtualStoredURLExpired
	}
	validated, err := virtuallibrary.ValidateProviderStreamURL(ctx, storedURL, allowInsecure)
	if err != nil {
		// Refuse an unvalidated URL, but fall through to the provider list
		// rather than failing the resolve: the stored value is a cache, not
		// the source of truth.
		return ResolvedVirtualMedia{}, virtualStoredURLMissing
	}
	var expiresAt time.Time
	if row.ResolvedURLExpiresAt != nil {
		expiresAt = *row.ResolvedURLExpiresAt
	}
	return ResolvedVirtualMedia{
		URL:         validated,
		URI:         candidateURI,
		CandidateID: candidateID,
		OwnerID:     row.VirtualOwnerInstallationID,
		// The stored URL was recorded with the headers the relay forwards
		// (Referer/Origin/User-Agent). Serving the URL without them would break
		// a provider that authenticates by header instead of a URL token, so
		// they travel with the URL as a unit.
		RequestHeaders: cloneHeaderMap(row.ProviderRequestHeaders),
		ExpiresAt:      expiresAt,
	}, virtualStoredURLUsable
}

// storedVirtualURLAllowInsecure evaluates the allow_insecure_http opt-in for a
// row, mirroring the resolver's decision for provider URLs. virtual_library.
// allow_insecure_http is a server-wide setting, so the owner only selects the
// callback; a nil callback keeps the strict SSRF policy.
func (h *PlaybackHandler) storedVirtualURLAllowInsecure(row *models.MediaFile, ownerInstallationID int) bool {
	if h == nil || h.AllowInsecureVirtual == nil || row == nil {
		return false
	}
	return h.AllowInsecureVirtual(effectiveVirtualOwner(row.VirtualOwnerInstallationID, ownerInstallationID))
}

// persistedVirtualIdentity snapshots a catalog row's durable provider identity
// for the same-release re-match. It reports false for a legacy row whose
// identity columns are all NULL, so those keep today's dead-pin behavior with
// no re-match attempt.
func persistedVirtualIdentity(row *models.MediaFile) (virtuallibrary.PersistedCandidateIdentity, bool) {
	if row == nil {
		return virtuallibrary.PersistedCandidateIdentity{}, false
	}
	identity := virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   row.ProviderVideoHash,
		GUID:        row.ProviderGUID,
		ReleaseName: row.ProviderReleaseName,
		ReleaseSize: row.ProviderReleaseSize,
	}
	if !identity.HasDurableIdentity() {
		return virtuallibrary.PersistedCandidateIdentity{}, false
	}
	return identity, true
}

// virtualResolveContextWithPersistedIdentity threads the row's durable identity
// into a resolve so the resolver can re-identify the same release when the
// provider renumbers result ids. A row with no durable identity is left
// untouched, so a legacy row resolves exactly as before.
func virtualResolveContextWithPersistedIdentity(ctx context.Context, row *models.MediaFile) context.Context {
	identity, ok := persistedVirtualIdentity(row)
	if !ok {
		return ctx
	}
	return virtuallibrary.WithPersistedCandidateIdentity(ctx, identity)
}

// adoptRematchedVirtualResolution adopts the new result= identity the resolver
// re-matched for the same release. It reuses the Phase-1 metadata write with
// AdoptPath set, so rewriting file_path and persisting the resolution's URL,
// headers and durable identity are one CAS-fenced statement: a row that rotated
// underneath performs no write, and a live failed_at verdict still blocks the
// adoption (the saver's fence is not bypassed). The write is best-effort: the
// resolved URL is already in hand and serving it does not depend on the
// adoption landing.
func adoptRematchedVirtualResolution(
	ctx context.Context,
	row *models.MediaFile,
	resolved ResolvedVirtualMedia,
	metaSaver VirtualFileMetadataSaver,
	saver VirtualFileSaver,
) {
	if row == nil || row.ID <= 0 || (metaSaver == nil && saver == nil) {
		return
	}
	adoptPath := strings.TrimSpace(resolved.URI)
	if adoptPath == "" || adoptPath == row.FilePath || virtualResultCandidateID(adoptPath) == "" {
		return
	}
	var expiresAt *time.Time
	if !resolved.ExpiresAt.IsZero() {
		expires := resolved.ExpiresAt
		expiresAt = &expires
	}
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		VideoTracks:      marshalTracksJSON(sanitizeTrackSlice(row.VideoTracks)),
		// A rematch binds the row to a new ?result= identity. Even when the
		// durable identity proves the same release, the concrete file behind
		// the new id is not guaranteed to carry the old inventory (the
		// name+size identity tier collapses distinct muxes), and the stored
		// probe evidence describes the previous id. Replace the audio inventory
		// with the matched candidate's declared languages and drop the old
		// subtitle inventory; the probe stamp is cleared below so the next
		// start probes the real bytes and converges the row. Carrying the old
		// release's labels here is what made the menu lie about the streams.
		AudioTracks:    marshalTracksJSON(sanitizeTrackSlice(declaredVirtualAudioTracks(resolved.CodecAudio, resolved.AudioLanguages, row.CodecAudio))),
		SubtitleTracks: marshalTracksJSON(sanitizeTrackSlice([]models.SubtitleTrack{})),
		Resolution:     row.Resolution,
		CodecVideo:     row.CodecVideo,
		CodecAudio:     row.CodecAudio,
		Container:      row.Container,
		HDR:            row.HDR,
		Bitrate:        row.Bitrate,
		Duration:       row.Duration,
		UpdatedAt:      row.UpdatedAt,
		ProbeUpdatedAt: row.ProbeUpdatedAt,
		// The identity tiers above describe the release being left; the adopted
		// candidate's identity is compared against them to keep a proven
		// same-release rematch on preserve-on-omission.
		ExpectedProviderVideoHash:   row.ProviderVideoHash,
		ExpectedProviderGUID:        row.ProviderGUID,
		ExpectedProviderReleaseName: row.ProviderReleaseName,
		ExpectedProviderReleaseSize: row.ProviderReleaseSize,
		OwnerID:                     row.VirtualOwnerInstallationID,
		LibraryID:                   row.MediaFolderID,
		AdoptPath:                   adoptPath,
		RequireAdopt:                true,
		// The old probe stamp referenced the previous result= identity, so it
		// no longer describes the row; clear it so the next start re-probes
		// instead of trusting the declared placeholder inventory.
		ClearProbe:             true,
		ResolvedURL:            strings.TrimSpace(resolved.URL),
		ResolvedURLExpiresAt:   expiresAt,
		ProviderVideoHash:      resolved.ProviderVideoHash,
		ProviderGUID:           resolved.ProviderGUID,
		ProviderReleaseName:    resolved.ProviderReleaseName,
		ProviderReleaseSize:    resolved.ProviderReleaseSize,
		ProviderRequestHeaders: cloneHeaderMap(resolved.RequestHeaders),
	}
	adoptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if metaSaver != nil {
		_, _ = metaSaver(adoptCtx, args)
		return
	}
	_, _ = saver(adoptCtx, args)
}

// lookupStoredVirtualURLCandidate is the serve-layer counterpart: the stream
// handler holds the row it loaded but that row's file_path may have been bound
// to the session's virtual source, and its stored URL belongs to the original
// row. Re-reading the exact candidate path keeps the same-row guarantee.
func (h *StreamHandler) lookupStoredVirtualURLCandidate(
	ctx context.Context,
	candidateURI string,
	ownerInstallationID int,
) (ResolvedVirtualMedia, *models.MediaFile, virtualStoredURLState) {
	if h == nil || virtualResultCandidateID(candidateURI) == "" {
		return ResolvedVirtualMedia{}, nil, virtualStoredURLMissing
	}
	pathResolver, ok := h.fileResolver.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	})
	if !ok {
		return ResolvedVirtualMedia{}, nil, virtualStoredURLMissing
	}
	row, err := pathResolver.GetByPath(ctx, candidateURI)
	if err != nil || row == nil {
		return ResolvedVirtualMedia{}, nil, virtualStoredURLMissing
	}
	allowInsecure := false
	if h.AllowInsecureVirtual != nil {
		allowInsecure = h.AllowInsecureVirtual(effectiveVirtualOwner(row.VirtualOwnerInstallationID, ownerInstallationID))
	}
	usable, state := evaluateStoredVirtualURLCandidate(
		ctx, candidateURI, row, allowInsecure, time.Now(),
	)
	if state == virtualStoredURLMissing {
		return ResolvedVirtualMedia{}, nil, virtualStoredURLMissing
	}
	return usable, row, state
}

// refreshStoredVirtualResolution records a successful provider resolution on
// the candidate's own row through the existing Phase-1 saver. It never adds a
// write path: the saver writes resolved_url/resolved_url_expires_at and the
// durable identity only when the resolution carried them, and the row's
// current metadata is passed through unchanged so the write cannot clobber
// track inventory. The write is fenced on the row snapshot (CAS), so a row
// that rotated underneath is left untouched.
//
// Only the requested candidate's row is refreshed. A substituted resolution
// names a different release and must not deposit its URL on this row.
func refreshStoredVirtualResolution(
	ctx context.Context,
	row *models.MediaFile,
	resolved ResolvedVirtualMedia,
	metaSaver VirtualFileMetadataSaver,
	saver VirtualFileSaver,
) {
	if row == nil || row.ID <= 0 {
		return
	}
	if metaSaver == nil && saver == nil {
		return
	}
	if strings.TrimSpace(resolved.URL) == "" {
		return
	}
	candidateID := virtualResultCandidateID(row.FilePath)
	if candidateID == "" {
		return
	}
	resolvedID := resolved.CandidateID
	if resolvedID == "" {
		resolvedID = virtualResultCandidateID(resolved.URI)
	}
	if resolvedID != candidateID {
		// The resolver served a sibling. Its URL does not belong to this row.
		return
	}
	var expiresAt *time.Time
	if !resolved.ExpiresAt.IsZero() {
		expires := resolved.ExpiresAt
		expiresAt = &expires
	}
	args := models.VirtualFilePersistArgs{
		FileID:               row.ID,
		ExpectedFilePath:     row.FilePath,
		VideoTracks:          marshalTracksJSON(sanitizeTrackSlice(row.VideoTracks)),
		AudioTracks:          marshalTracksJSON(sanitizeTrackSlice(row.AudioTracks)),
		SubtitleTracks:       marshalTracksJSON(sanitizeTrackSlice(row.SubtitleTracks)),
		Resolution:           row.Resolution,
		CodecVideo:           row.CodecVideo,
		CodecAudio:           row.CodecAudio,
		Container:            row.Container,
		HDR:                  row.HDR,
		Bitrate:              row.Bitrate,
		Duration:             row.Duration,
		UpdatedAt:            row.UpdatedAt,
		ProbeUpdatedAt:       row.ProbeUpdatedAt,
		OwnerID:              row.VirtualOwnerInstallationID,
		LibraryID:            row.MediaFolderID,
		ResolvedURL:          strings.TrimSpace(resolved.URL),
		ResolvedURLExpiresAt: expiresAt,
		ProviderVideoHash:    resolved.ProviderVideoHash,
		ProviderGUID:         resolved.ProviderGUID,
		ProviderReleaseName:  resolved.ProviderReleaseName,
		ProviderReleaseSize:  resolved.ProviderReleaseSize,
		// The resolution's headers are stored with its URL; the saver preserves
		// a stored set when the resolution carries none.
		ProviderRequestHeaders: cloneHeaderMap(resolved.RequestHeaders),
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if metaSaver != nil {
		_, _ = metaSaver(refreshCtx, args)
		return
	}
	_, _ = saver(refreshCtx, args)
}
