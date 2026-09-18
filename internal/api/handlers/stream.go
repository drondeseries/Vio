package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Silo-Server/silo-server/internal/activitylog"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/config"
	evt "github.com/Silo-Server/silo-server/internal/events"
	"github.com/Silo-Server/silo-server/internal/httpstream"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/streamtoken"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

const (
	subtitleFormatASS = "ass"
	subtitleFormatSSA = "ssa"
	subtitleFormatSUP = "sup"
)

// FilePathResolver looks up a media file by its ID.
type FilePathResolver interface {
	GetByID(ctx context.Context, id int) (*models.MediaFile, error)
}

// StreamHandler handles HTTP endpoints for streaming media content.
type StreamHandler struct {
	sessionMgr    SessionManagerInterface
	fileResolver  FilePathResolver
	MissingMarker MissingFileMarker
	EventsHub     *evt.Hub
	AdminStore    PlaybackAdminStore
	SessionSyncer PlaybackSessionSyncer
	// TM is the shared transcode/reconstruct manager (same instance as the
	// PlaybackHandler's). It lets a direct/remux stream rebuild its playback
	// Session from the recipe card after a server restart instead of 404-ing.
	// May be nil (tests / minimal setups) — reconstruct is then simply off.
	TM *playback.TranscodeManager
	// JWTSecret verifies the stream token carried on the serve URL (?st=), which
	// is the reconstruction descriptor for direct/remux after a restart. Empty
	// disables token-based reconstruct (tests / minimal setups).
	JWTSecret string
	// StreamDeny is the shared session-deny marker (same instance as the
	// PlaybackHandler's). A denied session is answered 410 and never
	// reconstructed. Nil-safe: without Redis nothing is ever denied.
	StreamDeny *playback.StreamDeny
	// PlanStoreV3 is the shared attempt store (same instance as the
	// PlaybackHandler's); an aborted session's attempt row is marked stopped
	// through it. May be nil (tests / minimal setups).
	PlanStoreV3 playback.PlanStoreV3
	// PlaybackConfig returns the current playback config; read it through
	// ffmpegPath(). May be nil (tests).
	PlaybackConfig func() config.PlaybackConfig
	// CopySafetyRacer gates and covers a revived progressive remux: it answers
	// whether this replica already condemns a video stream-copy of the source,
	// and re-engages the copy-safety race for one whose verdict is still open.
	// Optional — without it a revived remux is gated on the persisted row alone
	// and no race is started here.
	CopySafetyRacer PlaybackCopySafetyRacer
	// SubtitleCache stores complete embedded subtitle extracts under the transcode
	// dir so repeat selections skip the whole-file ffmpeg demux. May be nil
	// (tests / minimal setups) — extraction then always streams uncached.
	SubtitleCache *playback.SubtitleCache
	SubtitleRepo  subtitles.Repository // optional; enables S3-sourced subtitles
	S3Client      subtitles.S3Client   // optional; needed for fetching S3 subtitles
	S3Bucket      string               // bucket for subtitle storage
	// VirtualMediaResolver resolves virtual:// URIs to a real provider URL.
	// Required for embedded subtitle extraction from virtual sources.
	VirtualMediaResolver         VirtualMediaResolver
	VirtualMediaRefreshResolver  VirtualMediaRefreshResolver
	VirtualMediaDetailedResolver VirtualMediaDetailedResolver
	// RemoteStreamRelay pins the resolved provider URL to a loopback relay
	// so ffmpeg reads through it with a stable IP.
	RemoteStreamRelay *remotestream.Relay
	// AllowInsecureVirtual reports whether the owning plugin installation has
	// explicitly enabled allow_insecure_http for private/local stream hosts.
	AllowInsecureVirtual func(installationID int) bool
	// VirtualCandidateFailMarker stamps a virtual candidate row as known-bad
	// after a transport produced no bytes, so the auto-pick skips it on the
	// next play while the dropdown still shows it for a manual retry.
	VirtualCandidateFailMarker func(ctx context.Context, fileID int) error
	// VirtualCandidateRecoveredMarker clears a known-bad stamp after the
	// candidate actually delivered media bytes to a client — the only evidence
	// that forgives a transport failure. The callback is fenced on the
	// delivered candidate identity and the failure state observed when the
	// transport started, so a rotation or a newer failure is never cleared.
	// Metadata-only liveness checks resolve URLs without opening media and
	// must never clear it.
	VirtualCandidateRecoveredMarker func(ctx context.Context, fileID int, deliveredFilePath string, observedFailedAt *time.Time) error
}

// ffmpegPath returns the currently configured ffmpeg binary path.
func (h *StreamHandler) ffmpegPath() string {
	if h.PlaybackConfig != nil {
		return h.PlaybackConfig().FFmpegPath
	}
	return ""
}

// bindSessionVirtualSource returns a copy of a virtual file bound to the
// provider-neutral source captured by the playback session.
func bindSessionVirtualSource(file *models.MediaFile, session *playback.Session) *models.MediaFile {
	if file == nil || session == nil || session.VirtualSourceURI == "" || !isVirtualPlaybackFile(file) {
		return file
	}
	bound := *file
	bound.FilePath = session.VirtualSourceURI
	bound.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
	return &bound
}

// bindSessionVirtualSourceWithTracks binds the session's virtual source and
// prefers the subtitle evidence captured at plan time. The catalog row is
// mutable: candidate rotation re-probes it and can replace its subtitle
// tracks after this session planned against a specific release. Pinned
// subtitle URLs name plan-time ordinals/stream indices, so the extraction
// must use the evidence the plan promised, not whatever the row holds now.
func bindSessionVirtualSourceWithTracks(ctx context.Context, file *models.MediaFile, session *playback.Session, resolver FilePathResolver) *models.MediaFile {
	bound := bindSessionVirtualSource(file, session)
	if bound == nil || !isVirtualPlaybackFile(bound) {
		return bound
	}

	if len(session.VirtualSubtitleTracks) > 0 || len(session.VirtualExternalSubtitles) > 0 {
		boundCopy := *bound
		boundCopy.SubtitleTracks = session.VirtualSubtitleTracks
		boundCopy.ExternalSubtitles = session.VirtualExternalSubtitles
		return &boundCopy
	}

	// No session evidence (e.g. a reconstructed session): fall back to the
	// live candidate row when the bound file only carries provider-declared
	// placeholders, mirroring the historical behavior.
	if resolver == nil || hasUsableSubtitleTracks(bound) {
		return bound
	}

	var candidate *models.MediaFile
	if session.MediaFileID > 0 && session.MediaFileID != file.ID {
		candidate, _ = resolver.GetByID(ctx, session.MediaFileID)
	}
	if (candidate == nil || !hasUsableSubtitleTracks(candidate)) && session.VirtualSourceURI != "" {
		if pathResolver, ok := resolver.(interface {
			GetByPath(context.Context, string) (*models.MediaFile, error)
		}); ok {
			candidate, _ = pathResolver.GetByPath(ctx, session.VirtualSourceURI)
		}
	}
	if candidate != nil && hasUsableSubtitleTracks(candidate) {
		boundCopy := *bound
		boundCopy.SubtitleTracks = candidate.SubtitleTracks
		if len(boundCopy.ExternalSubtitles) == 0 {
			boundCopy.ExternalSubtitles = candidate.ExternalSubtitles
		}
		return &boundCopy
	}

	return bound
}

// hasUsableSubtitleTracks reports whether a file carries embedded subtitle
// tracks with real codec evidence. Provider-declared language placeholders
// (Index 0, no Codec, no ContainerTrackID) are not usable for extraction:
// the stream handler would pick the wrong output format and ffmpeg would
// fail against the real provider stream.
func hasUsableSubtitleTracks(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	for _, track := range file.SubtitleTracks {
		if strings.TrimSpace(track.Codec) != "" {
			return true
		}
	}
	return false
}

func resolvedVirtualCandidatePath(resolved ResolvedVirtualMedia) string {
	uri := strings.TrimSpace(resolved.URI)
	id := virtualResultCandidateID(uri)
	if !strings.HasPrefix(uri, "virtual://") || id == "" || resolved.CandidateID == "" || resolved.CandidateID != id {
		return ""
	}
	return uri
}

func hasVirtualMediaResolver(h *StreamHandler) bool {
	return h != nil && (h.VirtualMediaResolver != nil || h.VirtualMediaDetailedResolver != nil || h.VirtualMediaRefreshResolver != nil)
}

func (h *StreamHandler) resolveVirtualInputURI(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
	forceRefresh bool,
) (ResolvedVirtualMedia, func(), error) {
	return h.resolveVirtualInputURIExcluding(ctx, file, userID, profileID, forceRefresh, nil)
}

// resolveVirtualInputURIExcluding resolves a virtual input, optionally
// excluding a failed candidate so the next-ranked release is tried. The
// excluded candidate ID is threaded into the detailed resolver, which re-lists
// and skips it (see plugins.ResolveVirtualPlaybackDetailedWithRouting).
func (h *StreamHandler) resolveVirtualInputURIExcluding(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
) (ResolvedVirtualMedia, func(), error) {
	resolved := ResolvedVirtualMedia{}
	var err error
	if h.VirtualMediaDetailedResolver != nil {
		resolved, err = h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
			ctx, file.FilePath, file.VirtualOwnerInstallationID, userID, profileID, forceRefresh, excludedCandidateIDs, "",
		)
	} else if forceRefresh && h.VirtualMediaRefreshResolver != nil {
		resolved.URL, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
			ctx, file.FilePath, file.VirtualOwnerInstallationID, userID, profileID,
		)
	} else {
		resolved.URL, err = resolveVirtualMediaPath(
			ctx, h.VirtualMediaResolver, file.FilePath,
			file.VirtualOwnerInstallationID, userID, profileID,
		)
	}
	if err != nil {
		return ResolvedVirtualMedia{}, nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return resolved, func() {}, nil
	}
	var relayURL string
	var cleanup func()
	ownerID := effectiveVirtualOwner(resolved.OwnerID, file.VirtualOwnerInstallationID)
	if h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(ownerID) {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeaders(ctx, resolved.URL, resolved.RequestHeaders)
	} else {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeaders(ctx, resolved.URL, resolved.RequestHeaders)
	}
	if err != nil {
		return ResolvedVirtualMedia{}, nil, err
	}
	resolved.URL = relayURL
	return resolved, cleanup, nil
}

// NewStreamHandler creates a new StreamHandler backed by the given session
// manager and file resolver.
func NewStreamHandler(sessionMgr SessionManagerInterface, fileResolver FilePathResolver) *StreamHandler {
	return &StreamHandler{
		sessionMgr:   sessionMgr,
		fileResolver: fileResolver,
		// A bare manager (no recipe store) behaves as "no reconstruct" — plain
		// GetSession + ownership — so HandleStream has a single code path. The
		// router overwrites this with the shared manager to enable reconstruct.
		TM: playback.NewTranscodeManager(),
	}
}

// HandleStream serves the video stream for a playback session.
// For direct play: serves the file with HTTP byte-range support.
// For remux: starts an ffmpeg remux and streams the output.
// For transcode: returns 400 (transcode uses manifest/segment endpoints).
func (h *StreamHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	// Look up the session, reconstructing it from the recipe card on a not-found
	// miss (e.g. after a server restart) so a direct/remux stream resumes instead
	// of 404-ing. The client re-supplies its position (HTTP Range for direct, the
	// ?seek= query for remux), so no runtime beyond the Session needs rebuilding.
	// Without a token (or signing secret) reconstruct is off, collapsing to a
	// plain GetSession + ownership check.
	card, claims := verifiedStreamCardFromToken(r.URL.Query().Get(streamTokenParam), sessionID, h.JWTSecret)
	loadCard := card
	if _, err := h.sessionMgr.GetSession(sessionID); err == nil {
		// A live route may have been replanned since this token was issued. Do not
		// let stale recipe routing override the current session, and do not revive
		// the stale recipe if the live session disappears during this request.
		loadCard = nil
	} else if errors.Is(err, playback.ErrSessionNotFound) && !requireNativeRecipeAPIEgressV3(w, card) {
		return
	} else if err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		// Do not turn an inconsistent backend read into authority to reconstruct
		// from a recipe whose route was never checked against a clean miss.
		loadCard = nil
	}
	session, status, reconstructed := h.TM.LoadOrReconstructSessionDetail(r.Context(), h.sessionMgr.GetSession, sessionID, userID, loadCard)
	switch status {
	case playback.SessionMissing:
		writePlaybackSessionNotFound(w)
		return
	case playback.SessionLoadFailed:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	case playback.SessionForbidden:
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}
	if !requireNativeSessionAPIEgressV3(w, session) {
		return
	}
	if !requireOwningProfile(w, r, session) {
		return
	}

	file, err := h.fileResolver.GetByID(r.Context(), session.MediaFileID)
	if err != nil {
		if isPlaybackFileLookupMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
			writeError(w, http.StatusNotFound, "not_found", "Media file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load media file")
		return
	}
	if file == nil {
		h.abortPlaybackSession(r.Context(), session)
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	if err := preflightPlaybackFile(r.Context(), file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
		}
		writePlaybackFilePreflightError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if reconstructed && session.PlayMethod == playback.PlayRemux &&
		videoCopyRevivalRefused(r.Context(), h.CopySafetyRacer, file, sessionID) {
		h.abortPlaybackSession(r.Context(), session)
		writePlaybackSessionNotFound(w)
		return
	}

	// Bind to the session's planned virtual URI when available: the catalog
	// row's path is mutable (candidate rotation), but the session captured
	// the exact URI that was resolved and probed during planning.
	file = bindSessionVirtualSource(file, session)

	// Capture the delivered identity and observed health state at transport
	// start: the delivered candidate is the path the transport will serve
	// (retained by the session even if the row rotates mid-stream), and the
	// failure stamp seen now is the only health state a successful delivery
	// may clear. A rotation to B or a newer failure on A that lands while the
	// stream is being served is preserved.
	virtualObservedFailedAt := (*time.Time)(nil)
	if file != nil && isVirtualPlaybackFile(file) {
		if current, err := h.fileResolver.GetByID(r.Context(), file.ID); err == nil && current != nil {
			virtualObservedFailedAt = current.FailedAt
		}
	}

	inputPath := file.FilePath
	deliveredPath := ""
	releaseInput := func() {}
	if isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) {
		resolved, cleanup, resolveErr := h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
		if resolveErr != nil {
			logVirtualStreamFailure(r.Context(), sessionID, file, resolveErr)
			writeError(w, http.StatusBadGateway, "virtual_resolve_failed", "Failed to resolve virtual source")
			return
		}
		inputPath = resolved.URL
		deliveredPath = resolvedVirtualCandidatePath(resolved)
		releaseInput = cleanup
	}
	defer func() {
		if releaseInput != nil {
			releaseInput()
		}
	}()

	switch session.PlayMethod {
	case playback.PlayDirect:
		if err := h.sessionMgr.BeginTransport(sessionID); err == nil {
			defer func() {
				_ = h.sessionMgr.EndTransport(sessionID)
			}()
		}
		if isVirtualPlaybackFile(file) {
			streamWriter := httpstream.NewRollingDeadlineWriter(w)
			targetURL, err := url.Parse(inputPath)
			if err == nil && targetURL.Scheme != "http" {
				err = fmt.Errorf("unsupported virtual stream scheme %q", targetURL.Scheme)
			}
			if err != nil {
				logVirtualStreamFailure(r.Context(), sessionID, file, err)
				h.handleTransportStartFailure(r.Context(), session, file, err)
				writeError(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source")
				return
			}
			// This proxy forwards client headers to the target by design; that
			// is only safe because virtual inputs always resolve to the local
			// relay. Assert the invariant rather than trusting every caller.
			host := targetURL.Hostname()
			if host != "127.0.0.1" && host != "::1" && host != "[::1]" {
				err := fmt.Errorf("virtual direct-play proxy target %q is not the local relay", targetURL.Host)
				logVirtualStreamFailure(r.Context(), sessionID, file, err)
				h.handleTransportStartFailure(r.Context(), session, file, err)
				writeError(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source")
				return
			}
			var lastProxyErr error
			proxy := &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.Out.URL = targetURL
					pr.Out.Host = targetURL.Host
				},
				ModifyResponse: func(res *http.Response) error {
					if res.StatusCode >= http.StatusInternalServerError {
						return fmt.Errorf("relay returned HTTP %d", res.StatusCode)
					}
					return nil
				},
				ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
					lastProxyErr = proxyErr
				},
			}
			proxy.ServeHTTP(streamWriter, r)
			if lastProxyErr != nil {
				if streamWriter.StatusCode() == 0 && (h.VirtualMediaDetailedResolver != nil || h.VirtualMediaRefreshResolver != nil) {
					if releaseInput != nil {
						releaseInput()
						releaseInput = nil
					}
					// The pinned candidate served no bytes (corrupted NZB, dead
					// provider URL). Mark it failed and re-resolve with it
					// excluded so the next-ranked release is tried.
					failedID := virtualResultCandidateID(deliveredPath)
					if failedID != "" {
						h.markVirtualCandidateFailed(r.Context(), file, failedID)
					}
					excluded := []string{failedID}
					if failedID == "" {
						excluded = nil
					}
					refreshedMedia, refreshCleanup, refreshErr := h.resolveVirtualInputURIExcluding(r.Context(), file, session.UserID, session.ProfileID, true, excluded)
					if refreshErr == nil {
						expectedCandidateID := ""
						if parsed, err := url.Parse(file.FilePath); err == nil {
							expectedCandidateID = parsed.Query().Get("result")
						}
						if expectedCandidateID != "" && refreshedMedia.CandidateID != "" && refreshedMedia.CandidateID != expectedCandidateID {
							if refreshCleanup != nil {
								refreshCleanup()
							}
							lastProxyErr = fmt.Errorf("refreshed candidate %q does not match pinned candidate %q", refreshedMedia.CandidateID, expectedCandidateID)
						} else {
							releaseInput = refreshCleanup
							refreshedURL, parseErr := url.Parse(refreshedMedia.URL)
							if parseErr == nil && refreshedURL.Scheme == "http" {
								refreshedHost := refreshedURL.Hostname()
								if refreshedHost == "127.0.0.1" || refreshedHost == "::1" || refreshedHost == "[::1]" {
									targetURL = refreshedURL
									deliveredPath = resolvedVirtualCandidatePath(refreshedMedia)
									lastProxyErr = nil
									proxy.ServeHTTP(streamWriter, r)
								}
							}
						}
					}
				}
				if lastProxyErr != nil {
					h.handleTransportStartFailure(r.Context(), session, file, lastProxyErr)
					if streamWriter.StatusCode() == 0 {
						logVirtualStreamFailure(r.Context(), sessionID, file, lastProxyErr)
						writeError(streamWriter, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source")
					}
				}
			}
			if lastProxyErr == nil && virtualCandidateDeliveryEvidence(streamWriter.StatusCode(), streamWriter.BytesWritten()) {
				h.clearVirtualCandidateRecovered(r.Context(), file, deliveredPath, virtualObservedFailedAt)
			}
			return
		}
		if err := playback.ServeDirectPlay(w, r, inputPath); err != nil {
			h.handleTransportStartFailure(r.Context(), session, file, err)
		}

	case playback.PlayRemux:
		if err := h.sessionMgr.BeginTransport(sessionID); err == nil {
			defer func() {
				_ = h.sessionMgr.EndTransport(sessionID)
			}()
		}
		seekSeconds := 0.0
		if seekStr := r.URL.Query().Get("seek"); seekStr != "" {
			if s, err := strconv.ParseFloat(seekStr, 64); err == nil && s >= 0 {
				seekSeconds = s
			}
		}
		// An audio-only source muxes an audio-only fMP4. The v3 plan promises
		// audio/mp4 for it, and a declared-tier client refuses to attach a
		// source buffer whose advertised type its probe rejected — so the
		// response has to keep the same promise the plan made.
		dvProfile := session.DVProfile
		if dvProfile == 0 {
			dvProfile = file.PrimaryDVProfile()
		}
		serveRemux := func() error {
			// The remux writes through the raw writer; wrap it to observe how
			// many bytes actually reached the client. nil return is NOT
			// delivery evidence (remux.go can return nil when the first
			// client write fails) — recovery is gated on positive bytes.
			remuxWriter := httpstream.NewRollingDeadlineWriter(w)
			err := playback.ServeRemuxWithOptions(remuxWriter, r, inputPath, "mp4", seekSeconds, session.TranscodeAudio, audioStreamOrdinalV3(file, session.AudioTrackIndex), dvProfile, playback.RemuxServeOptions{
				DVMode:                 session.RemuxDVMode,
				FFmpegPath:             h.ffmpegPath(),
				ContentType:            playback.RemuxContentType(file.IsAudioOnly()),
				AudioOnly:              file.IsAudioOnly(),
				SourceAudioChannels:    session.SourceAudioChannels,
				TargetAudioChannels:    session.TargetAudioChannels,
				TargetAudioBitrateKbps: session.TargetAudioBitrateKbps,
			})
			if err == nil && isVirtualPlaybackFile(file) &&
				virtualCandidateDeliveryEvidence(http.StatusOK, remuxWriter.BytesWritten()) {
				// Media bytes actually flowed to the client for the candidate
				// the session planned — the only evidence that forgives a
				// transport failure. Fenced on the delivered identity and the
				// health state observed at transport start.
				h.clearVirtualCandidateRecovered(r.Context(), file, deliveredPath, virtualObservedFailedAt)
			}
			return err
		}
		remuxErr := serveRemux()
		if remuxErr != nil {
			// The remux only commits 200 after FFmpeg produces media bytes, so
			// a failure here means the provider release served no output
			// (corrupted NZB, dead URL). Mark the candidate failed and retry
			// once with it excluded so the next-ranked release is tried.
			if isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) {
				failedID := virtualResultCandidateID(deliveredPath)
				if failedID != "" {
					h.markVirtualCandidateFailed(r.Context(), file, failedID)
				}
				if releaseInput != nil {
					releaseInput()
					releaseInput = nil
				}
				excluded := []string{failedID}
				if failedID == "" {
					excluded = nil
				}
				retried, retryCleanup, retryErr := h.resolveVirtualInputURIExcluding(r.Context(), file, session.UserID, session.ProfileID, true, excluded)
				if retryErr == nil {
					releaseInput = retryCleanup
					retryURL, parseErr := url.Parse(retried.URL)
					if parseErr == nil && retryURL.Scheme == "http" {
						retryHost := retryURL.Hostname()
						if retryHost == "127.0.0.1" || retryHost == "::1" || retryHost == "[::1]" {
							inputPath = retried.URL
							deliveredPath = resolvedVirtualCandidatePath(retried)
							remuxErr = serveRemux()
						}
					}
				}
			}
			if remuxErr != nil {
				h.handleTransportStartFailure(r.Context(), session, file, remuxErr)
			}
		}

	case playback.PlayTranscode:
		writeError(w, http.StatusBadRequest, "bad_request",
			"Transcode streams use manifest/segment endpoints")

	default:
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Unknown play method")
	}
}

// loadSidecarSession resolves the session a subtitle or font request names.
// It reconstructs from the signed stream reference after a restart or on a
// replica that never served the media, and checks account and selected-profile
// ownership before exposing a sidecar.
func (h *StreamHandler) loadSidecarSession(ctx context.Context, reference, sessionID string, userID int) (*playback.Session, *streamtoken.Claims, error) {
	card, claims := verifiedStreamCardFromToken(reference, sessionID, h.JWTSecret)
	loadCard := card
	if _, err := h.sessionMgr.GetSession(sessionID); err == nil {
		loadCard = nil
	} else if !errors.Is(err, playback.ErrSessionNotFound) {
		loadCard = nil
	}
	session, status, _ := h.TM.LoadOrReconstructSessionDetail(ctx, h.sessionMgr.GetSession, sessionID, userID, loadCard)
	switch status {
	case playback.SessionMissing:
		return nil, nil, apiError(http.StatusNotFound, playbackSessionNotFoundErrorCode, "Playback session not found")
	case playback.SessionLoadFailed:
		return nil, nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to load playback session")
	case playback.SessionForbidden:
		return nil, nil, apiError(http.StatusForbidden, "forbidden", "Session belongs to another user")
	case playback.SessionUnauthorized:
		return nil, nil, apiError(http.StatusUnauthorized, "unauthorized", "Authentication required")
	}
	if session == nil {
		return nil, nil, apiError(http.StatusNotFound, playbackSessionNotFoundErrorCode, "Playback session not found")
	}
	if profileID := apimw.GetProfileID(ctx); profileID != "" && session.ProfileID != "" && profileID != session.ProfileID {
		return nil, nil, apiError(http.StatusForbidden, "forbidden", "Session belongs to another profile")
	}
	return session, claims, nil
}

// HandleSubtitle extracts a subtitle track from the media file associated with
// a playback session and serves it as WebVTT or raw ASS depending on the
// URL extension (e.g. /subtitles/2.ass or /subtitles/2.vtt).
func (h *StreamHandler) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	trackParam := chi.URLParam(r, "track")
	trackIndex, requestedFormat, err := playback.ParseSubtitleTrackParam(trackParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
		return
	}

	session, claims, err := h.loadSidecarSession(r.Context(), r.URL.Query().Get(streamTokenParam), sessionID, userID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}

	fileID, err := subtitleSourceFileID(r, session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	file, err := h.fileResolver.GetByID(r.Context(), fileID)
	if err != nil || file == nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}

	// Capture the catalog row's subtitle layout before the session overlay. A
	// virtual release can rotate between planning and extraction: the row is
	// re-probed against the current candidate while this session's URLs still
	// name the layout captured at plan time. When the two diverge, extraction
	// must verify the live source (and possibly re-map the plan ordinal) before
	// spawning ffmpeg, because the ordinal is only valid against the pinned
	// release's actual layout.
	rowSubs := file.SubtitleTracks
	driftSuspected := isVirtualPlaybackFile(file) &&
		session.VirtualSourceURI != "" &&
		session.VirtualSubtitleEvidenceSet &&
		!playback.SubtitleLayoutsEqual(rowSubs, session.VirtualSubtitleTracks)

	// Bind to the session's planned virtual URI when available: the catalog
	// row's path is mutable (candidate rotation, stale pin removal), but the
	// session captured the exact URI that was resolved and probed during
	// planning. Extracting from a different row would silently switch the
	// source under an in-flight play.
	file = bindSessionVirtualSourceWithTracks(r.Context(), file, session, h.fileResolver)
	trackIndex, err = subtitleRouteIndex(file, trackIndex, r.URL.Query())
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}

	// trackIndex is a combined ordinal, resolved through the same three
	// consecutive ranges playback.BuildSubtitleInventoryV3 assigns them from:
	// externals, then embedded container tracks, then downloaded ones. The
	// ranges cover the full track arrays — including bitmap tracks that have no
	// sidecar shape — so an ordinal always names the same track here as it does
	// in the published inventory.
	// Downloaded subtitle URLs additionally bind that ordinal to a stable row
	// identity. The path ordinal remains for compatibility and display, but it
	// must not be re-resolved against a mutable inventory after a seek reanchor.
	if rawID := strings.TrimSpace(r.URL.Query().Get(playback.DownloadedSubtitleIDParamV3)); rawID != "" {
		downloadedID, parseErr := strconv.Atoi(rawID)
		if parseErr != nil || downloadedID <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid downloaded subtitle identity")
			return
		}
		if h.SubtitleRepo == nil || h.S3Client == nil {
			writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
			return
		}
		downloaded, lookupErr := h.SubtitleRepo.GetDownloadedSubtitle(r.Context(), downloadedID)
		if lookupErr != nil {
			slog.ErrorContext(r.Context(), "get downloaded subtitle failed", "component", "api",
				"file_id", file.ID,
				"downloaded_subtitle_id", downloadedID,
				"error", lookupErr,
			)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load downloaded subtitle")
			return
		}
		if downloaded == nil || downloaded.MediaFileID != file.ID {
			writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
			return
		}
		if r.Method == http.MethodHead {
			writeSubtitleRepresentationHead(w, requestedFormat)
			return
		}
		h.serveDownloadedSubtitle(w, r, *downloaded, requestedFormat)
		return
	}
	externalCount := len(file.ExternalSubtitles)
	if trackIndex < externalCount {
		sub := file.ExternalSubtitles[trackIndex]
		if !subtitleSidecarFormatSupported(sub.Format, requestedFormat, false) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Requested subtitle extension does not match the selected track")
			return
		}
		if r.Method == http.MethodHead {
			writeSubtitleRepresentationHead(w, requestedFormat)
			return
		}

		// Serve ASS/SSA external subtitles as raw data for client-side rendering.
		if playback.IsASS(sub.Format) && requestedFormat != "vtt" {
			data, err := playback.LoadExternalSubtitleRaw(sub.Path)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error",
					"Failed to load external subtitle")
				return
			}
			playback.ServeSubtitle(w, data, subtitleFormatASS)
			return
		}

		vttData, err := playback.LoadExternalSubtitleAsVTT(r.Context(), sub.Path, sub.Format, h.ffmpegPath())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"Failed to load external subtitle")
			return
		}
		playback.ServeSubtitle(w, vttData, "vtt")
		return
	}

	embeddedIndex := trackIndex - externalCount

	// Check embedded tracks.
	if embeddedIndex < len(file.SubtitleTracks) {
		track := file.SubtitleTracks[embeddedIndex]
		// PGS is the one bitmap codec we can deliver without burn-in: the
		// track is copied losslessly into a .sup stream and rendered
		// client-side. DVD/DVB bitmap subs still require burn-in.
		if playback.NeedsBurnIn(track.Codec) && !playback.IsPGS(track.Codec) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"Bitmap subtitle tracks cannot be extracted as text")
			return
		}
		if !subtitleSidecarFormatSupported(track.Codec, requestedFormat, true) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Requested subtitle extension does not match the selected track")
			return
		}
		if r.Method == http.MethodHead && requestedFormat != subtitleFormatSUP {
			writeSubtitleRepresentationHead(w, requestedFormat)
			return
		}

		// Dedicated streaming extract — ffmpeg seeks to the current
		// playback position and pipes cues to the response as they're
		// demuxed, so the first byte lands within ~1s even on network
		// storage. Works identically for direct-play, remux, and
		// transcode because it doesn't depend on any other ffmpeg.
		h.streamEmbeddedSubtitle(w, r, file, embeddedIndex, session, driftSuspected, requestedFormat)
		return
	}

	// Check downloaded subtitles (from S3).
	if h.SubtitleRepo != nil && h.S3Client != nil {
		downloaded, err := h.SubtitleRepo.ListDownloadedSubtitles(r.Context(), file.ID)
		if err != nil {
			// A DB failure here must not masquerade as "track not found":
			// surface it as an internal error (with a server-side signal)
			// so the real failure is diagnosable instead of looking like an
			// intermittent 404 to the client.
			slog.ErrorContext(r.Context(), "list downloaded subtitles failed", "component", "api",
				"file_id", file.ID,
				"track", trackIndex,
				"error", err,
			)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list downloaded subtitles")
			return
		}

		downloadedIndex := embeddedIndex - len(file.SubtitleTracks)
		if downloadedIndex >= 0 && downloadedIndex < len(downloaded) {
			if r.Method == http.MethodHead {
				writeSubtitleRepresentationHead(w, requestedFormat)
				return
			}
			h.serveDownloadedSubtitle(w, r, downloaded[downloadedIndex], requestedFormat)
			return
		}
	}

	writeError(w, http.StatusNotFound, "not_found", "Subtitle track not found")
}

func (h *StreamHandler) serveDownloadedSubtitle(w http.ResponseWriter, r *http.Request, subtitle subtitles.DownloadedSubtitle, requestedFormat string) {
	if !subtitleSidecarFormatSupported(string(subtitle.Format), requestedFormat, false) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Requested subtitle extension does not match the selected track")
		return
	}
	data, err := h.S3Client.GetObject(r.Context(), h.S3Bucket, subtitle.S3Key)
	if err != nil {
		writeError(w, http.StatusBadGateway, "s3_error", "Failed to load subtitle from storage")
		return
	}

	// Serve ASS/SSA downloaded subtitles as raw data.
	if playback.IsASS(string(subtitle.Format)) && requestedFormat != "vtt" {
		playback.ServeSubtitle(w, data, subtitleFormatASS)
		return
	}

	// If the subtitle is already VTT, serve directly.
	if subtitle.Format == subtitles.FormatVTT {
		playback.ServeSubtitle(w, data, "vtt")
		return
	}

	// Convert other text formats to VTT using the playback conversion pipeline.
	vttData, err := playback.ConvertToVTTWithFFmpeg(r.Context(), data, string(subtitle.Format), h.ffmpegPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "convert_error", "Failed to convert subtitle")
		return
	}
	playback.ServeSubtitle(w, vttData, "vtt")
}

// subtitleSidecarFormatSupported keeps bitmap and styled-text requests within
// the representations the server can produce. Plain text tracks preserve the
// v1 endpoint's permissive extension behavior and are always returned as VTT;
// ASS/SSA may also be served losslessly, and only an embedded PGS track has a
// binary .sup representation.
func subtitleSidecarFormatSupported(codec, requestedFormat string, embeddedPGS bool) bool {
	requestedFormat = strings.ToLower(strings.TrimSpace(requestedFormat))
	if requestedFormat == "" {
		return true
	}
	if playback.IsPGS(codec) {
		return embeddedPGS && requestedFormat == subtitleFormatSUP
	}
	if playback.NeedsBurnIn(codec) {
		return false
	}
	if playback.IsASS(codec) {
		return requestedFormat == subtitleFormatASS || requestedFormat == subtitleFormatSSA || requestedFormat == "vtt"
	}
	return true
}

func writeSubtitleRepresentationHead(w http.ResponseWriter, requestedFormat string) {
	switch strings.ToLower(strings.TrimSpace(requestedFormat)) {
	case subtitleFormatASS, subtitleFormatSSA:
		w.Header().Set("Content-Type", "text/x-ssa; charset=utf-8")
	case subtitleFormatSUP:
		w.Header().Set("Content-Type", "application/octet-stream")
	default:
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
}

// subtitleSourceFileID pins a subtitle URL to the file whose track list was
// used to create it. A quality/seek restart may change session.MediaFileID to
// an alternate version; interpreting the old combined track index against the
// alternate file can silently serve a different language. Only the session's
// requested or current effective file may be named by the authenticated URL.
func subtitleSourceFileID(r *http.Request, session *playback.Session) (int, error) {
	return subtitleSourceFile(r.URL.Query().Get("file_id"), session)
}

func subtitleSourceFile(raw string, session *playback.Session) (int, error) {
	if session == nil {
		return 0, errors.New("playback session is required")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return session.MediaFileID, nil
	}
	fileID, err := strconv.Atoi(raw)
	if err != nil || fileID <= 0 {
		return 0, errors.New("invalid subtitle source file")
	}
	if fileID != session.MediaFileID && fileID != session.RequestedMediaFileID {
		return 0, errors.New("subtitle source file does not belong to playback session")
	}
	return fileID, nil
}

// SubtitleFontRequest identifies the session and frozen subtitle inventory
// selection. Query preserves repeated identity parameters so validation cannot
// silently choose one of conflicting stream pins.
type SubtitleFontRequest struct {
	SessionID string
	Track     string
	Query     url.Values
}

// SubtitleFonts loads a bounded bundle of embedded ASS/SSA fonts. Both API
// transports use this operation so reconstruction, deny markers and source-file
// admission stay identical without invoking another transport's HTTP handler.
func (h *StreamHandler) SubtitleFonts(ctx context.Context, in SubtitleFontRequest) ([]playback.SubtitleFontBundleItem, error) {
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return nil, apiError(http.StatusUnauthorized, "unauthorized", "Authentication required")
	}
	if in.SessionID == "" {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Session ID is required")
	}
	if lc := activitylog.GetPlaybackLogContext(ctx); lc != nil {
		lc.PlaybackSessionID = in.SessionID
	}
	if h.StreamDeny.Denied(ctx, in.SessionID) {
		return nil, apiError(http.StatusGone, playbackSessionEndedErrorCode, "Playback session has ended")
	}
	session, claims, err := h.loadSidecarSession(ctx, in.Query.Get(streamTokenParam), in.SessionID, userID)
	if err != nil {
		return nil, err
	}
	attachPlaybackSession(ctx, session, claims)

	fileID, err := subtitleSourceFile(in.Query.Get("file_id"), session)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "bad_request", err.Error())
	}
	file, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || file == nil {
		return nil, apiError(http.StatusNotFound, "not_found", "Media file not found")
	}
	if err := preflightPlaybackFile(ctx, file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(ctx, session)
			return nil, apiError(http.StatusNotFound, "not_found", "Source media file is missing")
		}
		return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to access source media file")
	}

	trackIndex, _, err := playback.ParseSubtitleTrackParam(in.Track)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
	}
	trackIndex, err = subtitleRouteIndex(file, trackIndex, in.Query)
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			return nil, apiError(http.StatusBadRequest, "bad_request", err.Error())
		}
		return nil, apiError(http.StatusNotFound, "not_found", err.Error())
	}

	embeddedIndex := trackIndex - len(file.ExternalSubtitles)
	if embeddedIndex < 0 || embeddedIndex >= len(file.SubtitleTracks) {
		return nil, apiError(http.StatusNotFound, "not_found", "Embedded subtitle track not found")
	}
	if !playback.IsASS(file.SubtitleTracks[embeddedIndex].Codec) {
		return nil, apiError(http.StatusBadRequest, "bad_request", "Subtitle font bundles are only available for ASS/SSA tracks")
	}
	fonts, err := playback.ExtractAttachedSubtitleFonts(ctx, file.FilePath, h.ffmpegPath())
	if err != nil {
		slog.WarnContext(ctx, "subtitle font extraction failed", "component", "api",
			"file_id", file.ID, "track", trackIndex, "error", err)
		return nil, apiError(http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
	}
	return playback.EncodeSubtitleFontBundle(fonts), nil
}

// HandleSubtitleFonts preserves the bridge API's array response and serves
// font bundles through the subtitle cache for virtual-source pre-warm.
func (h *StreamHandler) HandleSubtitleFonts(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}
	setPlaybackSessionLogContext(r, sessionID)
	if h.StreamDeny.Denied(r.Context(), sessionID) {
		writePlaybackSessionEnded(w)
		return
	}

	session, claims, err := h.loadSidecarSession(r.Context(), r.URL.Query().Get(streamTokenParam), sessionID, userID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachPlaybackSession(r.Context(), session, claims)

	if session.UserID != userID {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another user")
		return
	}

	fileID, err := subtitleSourceFileID(r, session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	file, err := h.fileResolver.GetByID(r.Context(), fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "not_found", "Media file not found")
		return
	}
	file = bindSessionVirtualSourceWithTracks(r.Context(), file, session, h.fileResolver)
	if err := preflightPlaybackFile(r.Context(), file, h.MissingMarker, h.EventsHub); err != nil {
		if isPlaybackFileMissing(err) {
			h.abortPlaybackSession(r.Context(), session)
		}
		writePlaybackFilePreflightError(w, err)
		return
	}

	trackParam := chi.URLParam(r, "track")
	trackIndex, _, err := playback.ParseSubtitleTrackParam(trackParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid subtitle track index")
		return
	}
	trackIndex, err = subtitleRouteIndex(file, trackIndex, r.URL.Query())
	if err != nil {
		if errors.Is(err, errSubtitleIdentityInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		} else {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		}
		return
	}

	embeddedIndex := trackIndex - len(file.ExternalSubtitles)
	if embeddedIndex < 0 || embeddedIndex >= len(file.SubtitleTracks) {
		writeError(w, http.StatusNotFound, "not_found", "Embedded subtitle track not found")
		return
	}
	if !playback.IsASS(file.SubtitleTracks[embeddedIndex].Codec) {
		writeError(w, http.StatusBadRequest, "bad_request", "Subtitle font bundles are only available for ASS/SSA tracks")
		return
	}

	// Build the font-bundle cache key before any virtual resolve: the identity
	// must never depend on the resolved relay URL, which rotates per
	// registration. Shared with the playback pre-warm so the two cannot drift.
	virtualFontSource := isVirtualPlaybackFile(file) && session.VirtualSourceURI != ""
	cacheKey := fontBundleCacheKey(file, session.VirtualSourceURI, h.ffmpegPath())

	// Virtual keys without a pinned result= param are intentionally
	// uncacheable: the identity would be unstable without the candidate
	// anchor, so we fall through to the uncached extract path below.
	if virtualFontSource && cacheKey.PinnedResult == "" {
		slog.DebugContext(r.Context(), "virtual font bundle has no pinned result= param; skipping cache", "component", "api", "file_id", file.ID)
	}

	// A cache hit serves the encoded bundle immediately: no provider round-trip,
	// no relay registration, no ffmpeg spawn.
	if h.SubtitleCache != nil {
		if cached, ok := h.SubtitleCache.LookupFontBundle(cacheKey); ok {
			writeFontBundleResponse(w, cached)
			return
		}
	}

	if h.SubtitleCache == nil {
		// No cache configured: keep the historical uncached path, resolved and
		// released within the request.
		inputPath := file.FilePath
		releaseInput := func() {}
		if virtualFontSource && hasVirtualMediaResolver(h) {
			var resolved ResolvedVirtualMedia
			resolved, releaseInput, err = h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
			if err != nil {
				logVirtualStreamFailure(r.Context(), session.ID, file, err)
				writeError(w, http.StatusBadGateway, "virtual_resolve_failed", "Failed to resolve virtual source")
				return
			}
			inputPath = resolved.URL
		}
		defer releaseInput()

		fonts, extractErr := playback.ExtractAttachedSubtitleFonts(r.Context(), inputPath, h.ffmpegPath())
		if extractErr != nil {
			slog.WarnContext(r.Context(), "subtitle font extraction failed", "component", "api",
				"file_id", file.ID,
				"track", trackIndex,
				"error", extractErr,
			)
			writeError(w, http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(playback.EncodeSubtitleFontBundle(fonts)); err != nil {
			slog.WarnContext(r.Context(), "subtitle font response encode failed", "component", "api", "error", err)
		}
		return
	}

	// The detached single-flight owns the virtual relay registration: it is
	// resolved inside the extract closure and released when that closure
	// returns, so a cold HTTP-led flight that outlives the short client wait
	// still has a valid source for the whole extraction. Releasing at HTTP
	// return instead would let ffmpeg start against a relay entry that no
	// longer exists.
	localPath := file.FilePath
	bundle, ready, err := h.SubtitleCache.ExtractFontBundleWithin(r.Context(), cacheKey, fontBundleClientWait, func(extractCtx context.Context) ([]byte, error) {
		inputPath := localPath
		releaseInput := func() {}
		if virtualFontSource && hasVirtualMediaResolver(h) {
			resolved, cleanup, resolveErr := h.resolveVirtualInputURI(extractCtx, file, session.UserID, session.ProfileID, false)
			if resolveErr != nil {
				logVirtualStreamFailure(extractCtx, session.ID, file, resolveErr)
				return nil, fmt.Errorf("%w: %w", errVirtualFontResolve, resolveErr)
			}
			inputPath = resolved.URL
			releaseInput = cleanup
		}
		defer releaseInput()

		fonts, extractErr := playback.ExtractAttachedSubtitleFonts(extractCtx, inputPath, h.ffmpegPath())
		if extractErr != nil {
			return nil, extractErr
		}
		return json.Marshal(playback.EncodeSubtitleFontBundle(fonts))
	})
	if err != nil {
		if errors.Is(err, errVirtualFontResolve) {
			logVirtualStreamFailure(r.Context(), session.ID, file, err)
			writeError(w, http.StatusBadGateway, "virtual_resolve_failed", "Failed to resolve virtual source")
			return
		}
		slog.WarnContext(r.Context(), "subtitle font extraction failed", "component", "api",
			"file_id", file.ID,
			"track", trackIndex,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, "font_extract_failed", "Failed to extract subtitle fonts")
		return
	}
	if !ready {
		// The extraction is still running detached. Hold the request only for
		// fontBundleClientWait, then hand the client a distinguishable,
		// uncacheable pending bundle so it falls back to default fonts
		// immediately and re-fetches; the single-flighted extraction keeps
		// running and lands in the cache for that next fetch.
		slog.DebugContext(r.Context(), "subtitle font bundle extraction in flight; serving pending bundle",
			"component", "api", "file_id", file.ID, "track", trackIndex)
		writePendingFontBundleResponse(w)
		return
	}
	writeFontBundleResponse(w, bundle)
}

// errVirtualFontResolve marks a font-bundle extraction failure caused by the
// virtual relay resolution rather than the extraction itself, so the HTTP
// handler can answer with the provider-resolution status instead of a generic
// extraction failure.
var errVirtualFontResolve = errors.New("virtual font bundle resolve failed")

// fontBundleClientWait bounds how long the font-bundle handler waits for a
// cold extraction before returning an empty bundle. It must stay well below
// the web client's FONT_BUNDLE_BUDGET_MS (3s) so the client never waits on the
// server; the extraction continues in the background.
var fontBundleClientWait = 2 * time.Second

// emptyFontBundle is the valid, empty JSON bundle served when an extraction is
// still in flight or has no fonts to return.
var emptyFontBundle = []byte("[]")

// fontBundleCacheKey builds the FontBundleKey for a media file the same way the
// stream font handler does, and is shared with the playback-time pre-warm so
// the two can never drift. Virtual rows key on the pinned result candidate
// because relay URLs rotate per registration; local rows key on the file row's
// size and mtime so a re-probed or replaced file reads as a miss.
func fontBundleCacheKey(file *models.MediaFile, virtualSourceURI, ffmpegPath string) playback.FontBundleKey {
	if file != nil && isVirtualPlaybackFile(file) && virtualSourceURI != "" {
		return playback.FontBundleKey{
			FileID:       file.ID,
			PinnedResult: virtualResultCandidateID(virtualSourceURI),
			FFmpegPath:   ffmpegPath,
		}
	}
	key := playback.FontBundleKey{FFmpegPath: ffmpegPath}
	if file != nil {
		key.FileID = file.ID
		key.Size = file.FileSize
		if file.FileModifiedAt != nil {
			key.MtimeUnixNano = file.FileModifiedAt.UnixNano()
		}
	}
	return key
}

// writeFontBundleResponse writes an encoded font-bundle payload with the
// shared cache headers. Both the cache-hit and cache-miss paths serve the same
// bytes, so the response is identical whichever path produced them.
func writeFontBundleResponse(w http.ResponseWriter, bundle []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "private, max-age=600")
	_, _ = w.Write(bundle)
}

// fontBundlePendingHeader marks a font-bundle response whose extraction is
// still in flight. The web client treats either this header or a no-store
// Cache-Control as pending: it must not persist the empty body as a definitive
// font-less result and instead re-fetches for the completed bundle.
const fontBundlePendingHeader = "X-Vio-Font-Bundle-Pending"

// writePendingFontBundleResponse writes a valid empty bundle for an extraction
// that is still running. Unlike a definitive font-less file, it is marked
// pending and uncacheable, so a client cannot retain the empty state for ten
// minutes while the real bundle is being produced.
func writePendingFontBundleResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(fontBundlePendingHeader, "true")
	_, _ = w.Write(emptyFontBundle)
}

func (h *StreamHandler) syncSessionsNow(ctx context.Context, reason string) {
	if h == nil || h.SessionSyncer == nil {
		return
	}
	if err := h.SessionSyncer.SyncNow(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to sync sessions", "component", "api", "reason", reason, "error", err)
	}
}

func (h *StreamHandler) finalizeSessionAbort(ctx context.Context, session *playback.Session, syncNow bool, syncReason string) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if h.AdminStore != nil {
		if err := h.AdminStore.DeleteSession(ctx, session.ID); err != nil {
			slog.ErrorContext(ctx, "failed to delete synced session", "component", "api", "session", session.ID, "error", err)
		}
	}
	if syncNow {
		h.syncSessionsNow(ctx, syncReason)
	}
}

func (h *StreamHandler) abortPlaybackSession(ctx context.Context, session *playback.Session) {
	if h == nil || session == nil || session.ID == "" {
		return
	}
	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return
	}
	h.finalizeSessionAbort(ctx, session, true, "stream_abort")
	markAttemptStoppedServerSide(ctx, h.PlanStoreV3, h.StreamDeny, session.ID)
}

func (h *StreamHandler) handleTransportStartFailure(ctx context.Context, session *playback.Session, file *models.MediaFile, err error) {
	if ctx == nil || session == nil || err == nil {
		return
	}
	if preflightErr := preflightPlaybackFile(ctx, file, h.MissingMarker, h.EventsHub); preflightErr != nil {
		err = preflightErr
	}
	if isPlaybackFileMissing(err) || errors.Is(err, os.ErrNotExist) {
		h.abortPlaybackSession(ctx, session)
		return
	}
	slog.WarnContext(ctx, "stream transport startup failed", "component", "api",
		"session", session.ID,
		"file_id", session.MediaFileID,
		"error", err,
		"playback_session_id", session.ID,
	)
}

// markVirtualCandidateFailed stamps the catalog row for a virtual candidate
// as known-bad after a transport produced no bytes, so the auto-pick skips it
// on the next play while the dropdown still shows it (clickable) for a manual
// retry. Best-effort: a persistence failure must not turn a 502 into a 500.
func (h *StreamHandler) markVirtualCandidateFailed(ctx context.Context, file *models.MediaFile, candidateID string) {
	if h == nil || file == nil || candidateID == "" || candidateID != virtualResultCandidateID(file.FilePath) {
		return
	}
	if h.VirtualCandidateFailMarker == nil {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.VirtualCandidateFailMarker(markCtx, file.ID); err != nil {
		slog.WarnContext(ctx, "mark virtual candidate failed", "component", "api", "file_id", file.ID, "candidate", candidateID, "error", err)
	}
}

// clearVirtualCandidateRecovered clears a virtual candidate's known-bad stamp
// after the candidate actually delivered media bytes to a client. This is the
// only evidence that forgives a transport failure: a resolved URL (liveness
// check) is not, because resolution never opens the media, and written
// response headers alone are not either (the relay forwards header-only 204,
// 304, 416, and zero-length 200 responses). The clear is fenced on the
// DELIVERED candidate identity (the file path the transport served, which the
// session retains even after the catalog row rotates) and the failure state
// observed when the transport started, so a late delivery of candidate A never
// clears a rotation to B or a newer failure on A.
// Best-effort: a persistence failure must not fail a delivering stream.
func (h *StreamHandler) clearVirtualCandidateRecovered(ctx context.Context, file *models.MediaFile, deliveredFilePath string, observedFailedAt *time.Time) {
	if h == nil || file == nil || strings.TrimSpace(deliveredFilePath) == "" {
		return
	}
	if h.VirtualCandidateRecoveredMarker == nil {
		return
	}
	clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.VirtualCandidateRecoveredMarker(clearCtx, file.ID, deliveredFilePath, observedFailedAt); err != nil {
		slog.WarnContext(ctx, "clear virtual candidate recovered", "component", "api", "file_id", file.ID, "delivered", deliveredFilePath, "error", err)
	}
}

// virtualCandidateDeliveryEvidence reports whether a direct-play transfer
// actually delivered media: a 200/206 status AND positive body bytes.
// Header-only responses (204/304/416/zero-length 200) are explicitly forwarded
// by the relay and are not evidence the media endpoint works.
func virtualCandidateDeliveryEvidence(statusCode int, bytesWritten int64) bool {
	if statusCode != http.StatusOK && statusCode != http.StatusPartialContent {
		return false
	}
	return bytesWritten > 0
}

// streamEmbeddedSubtitle runs a dedicated ffmpeg for a single embedded
// track, optionally windowed by explicit client parameters, and pipes its
// stdout directly to w. Because this ffmpeg is independent of the video
// pipeline, it works the same for direct play, remux, and transcode.
func (h *StreamHandler) streamEmbeddedSubtitle(w http.ResponseWriter, r *http.Request, file *models.MediaFile, embeddedIndex int, session *playback.Session, driftSuspected bool, requestedFormat ...string) {
	track := file.SubtitleTracks[embeddedIndex]
	outFormat := "vtt"
	switch {
	case playback.IsASS(track.Codec):
		outFormat = subtitleFormatASS
	case playback.IsPGS(track.Codec):
		outFormat = subtitleFormatSUP
	}

	// A subtitle URL describes the complete track unless the caller supplies
	// an explicit window. Native players fetch once and must retain cues beyond
	// ten minutes and before a resumed playback position. WebVTT and ASS honor
	// explicit ?position/?duration (ASS windows only when a position is given,
	// preserving the whole script for native consumers); PGS window consumers
	// opt in with ?windowed=1.
	allowWindow, seek, duration := subtitleExtractWindow(r, outFormat)
	windowRequested := subtitleWindowRequested(r)
	slog.InfoContext(r.Context(), "subtitle stream requested", "component", "api",
		"file_id", file.ID,
		"embedded_index", embeddedIndex,
		"track_language", track.Language,
		"track_codec", track.Codec,
		"track_probed_index", track.Index,
		"seek_seconds", seek,
		"duration_seconds", duration,
		"virtual_drift", driftSuspected,
	)

	opts := playback.StreamExtractOpts{
		InputPath:       file.FilePath,
		TrackIndex:      embeddedIndex,
		SourceCodec:     track.Codec,
		SeekSeconds:     seek,
		DurationSeconds: duration,
		AllowWindow:     allowWindow,
		WindowRequested: windowRequested,
		FFmpegPath:      h.ffmpegPath(),
	}
	// Virtual sources are provider-neutral URIs, not FFmpeg inputs. Resolve
	// through the relay so ffmpeg reads the real stream. Subtitle extraction
	// spawns its own ffmpeg independent of the video pipeline, so it must
	// resolve separately even though the transcode transport already did.
	releaseInput := func() {}
	virtualResolved := false
	if isVirtualPlaybackFile(file) && hasVirtualMediaResolver(h) && h.RemoteStreamRelay != nil && session != nil {
		resolved, cleanup, resolveErr := h.resolveVirtualInputURI(r.Context(), file, session.UserID, session.ProfileID, false)
		if resolveErr != nil {
			writeError(w, http.StatusBadGateway, "virtual_resolve_failed",
				"Failed to resolve virtual source for subtitle extraction.")
			return
		}
		opts.InputPath = resolved.URL
		releaseInput = cleanup
		virtualResolved = true
	}
	defer releaseInput()
	if len(requestedFormat) > 0 && requestedFormat[0] == "vtt" {
		// Only text sources can be converted to WebVTT. A bitmap track (PGS
		// reaches here because it is deliverable as .sup; DVD/DVB are rejected
		// upstream) carries no text, so honoring the override would spawn an
		// ffmpeg that always fails after the 200 and headers are committed.
		// Reject before any spawn or header write.
		if playback.NeedsBurnIn(track.Codec) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Bitmap subtitle tracks cannot be converted to WebVTT")
			return
		}
		opts.TargetFormat = "vtt"
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Only complete successful extracts enter the cache; explicit windows
	// remain streamed. Keep failures distinguishable from a clean subtitle EOF.
	response := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	virtualActive := virtualResolved && session != nil && session.VirtualSourceURI != ""

	// Virtual relay inputs never enter the payload cache under their rotating
	// URL; key on the pinned source + effective ordinal instead. The identity
	// is computed before the drift probe so a committed artifact can
	// short-circuit it, and recomputed after a remap to keep the warm and
	// serve keys aligned. The handler owns the detached warm
	// (warmVirtualSubtitleAfterWindowMiss), because this request's relay
	// registration is released when the request ends; the cache's own detached
	// warm is therefore disabled for virtual inputs.
	if virtualActive {
		opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		opts.DisableBackgroundWarm = true
	}

	// Row-vs-evidence drift (flagged in handleSubtitle) means the catalog row
	// no longer describes the release this session planned against. Probe the
	// live relay input once before any spawn or header commit and re-map the
	// plan ordinal onto a same-class live track when the pinned release
	// rotated. Mandatory for PGS, whose .sup response commits 200 before
	// ffmpeg spawns and therefore can never be retrofitted after a failed map.
	// A committed full-track text artifact pins the plan-time release, and a
	// cached-text input remaps to its sole stream, so the probe is unnecessary
	// then — skip its multi-second tax and serve the warm.
	if virtualActive && driftSuspected {
		if artifact, ok := h.resolveCommittedTextSubtitleEntry(&opts); ok {
			// A committed full-track text artifact pins the plan-time release and
			// a windowed extract reads it instead of the container, so the live
			// probe is unnecessary. Pin the exact artifact: the serve uses that
			// path (or reports ErrCommittedTextArtifactGone) rather than
			// re-resolving, so a generation-bucket rollover or eviction cannot
			// turn the validated artifact into an unvalidated source extract.
			opts.PinnedTextArtifact = &artifact
		} else {
			proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, session, &opts)
			if !proceed {
				writeSubtitleSourceChanged(w)
				return
			}
			if probeErr != nil && playback.IsPGS(track.Codec) {
				// PGS (.sup) commits 200 before ffmpeg spawns, so a probe that
				// could not establish the live layout must fail closed here,
				// before any header write, with a retryable error. Text/ASS
				// keeps its post-spawn map-error safety net instead.
				writeSubtitleSourceUnavailable(w)
				return
			}
			// The probe may have remapped the ordinal; the cache identity must
			// track the effective map so a remapped extraction lands under its
			// own key.
			opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		}
	}

	h.applyImplicitVirtualTextWindow(&opts, file, session, virtualActive)

	_, extractErr := h.SubtitleCache.ServeExtractWithResult(response, r, opts, playback.StreamExtractSubtitle)
	if errors.Is(extractErr, playback.ErrCommittedTextArtifactGone) {
		// The artifact that allowed the drift probe to be skipped was evicted
		// before the serve read it. No response has been written, so drop the
		// pin, validate the live layout, and retry rather than stream an
		// unvalidated source extract.
		opts.PinnedTextArtifact = nil
		proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, session, &opts)
		if !proceed {
			writeSubtitleSourceChanged(w)
			return
		}
		if probeErr != nil && playback.IsPGS(track.Codec) {
			writeSubtitleSourceUnavailable(w)
			return
		}
		opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		// The pinned artifact is gone, so the committed-entry check that
		// suppressed the implicit window on the first attempt no longer holds.
		// Re-evaluate against the now-cold source before retrying.
		h.applyImplicitVirtualTextWindow(&opts, file, session, virtualActive)
		_, extractErr = h.SubtitleCache.ServeExtractWithResult(response, r, opts, playback.StreamExtractSubtitle)
	}
	if extractErr != nil {
		playback.LogSubtitleStreamError(r.Context(), extractErr, file.ID, embeddedIndex)
		if r.Context().Err() != nil {
			return
		}
		// A successful HTTP EOF would make clients accept the partial track.
		if response.Status() != 0 {
			panic(http.ErrAbortHandler)
		}
		if !virtualActive || !playback.IsSubtitleStreamMapError(extractErr) {
			writeError(w, http.StatusInternalServerError, "subtitle_extract_failed", "Failed to extract subtitles")
			return
		}
		// Post-spawn safety net: the plan ordinal named a subtitle stream the
		// live input does not have, meaning the pinned release rotated between
		// the last probe and this spawn (or no pre-spawn probe ran because the
		// row still matched the plan evidence). Re-probe the already-registered
		// relay URL once — no second resolve — and re-map; a source that still
		// cannot satisfy the requested representation gets a clean retryable 4xx
		// instead of a 500.
		proceed, probeErr := h.verifyVirtualSubtitleLayout(r.Context(), track, session, &opts)
		if !proceed {
			writeSubtitleSourceChanged(w)
			return
		}
		if probeErr != nil && playback.IsPGS(opts.SourceCodec) {
			writeSubtitleSourceUnavailable(w)
			return
		}
		// The retry may have remapped to a different live ordinal; the cache
		// identity must track the effective map so a remapped extraction lands
		// under its own key.
		if virtualActive {
			opts.CacheIdentity = playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, opts.TrackIndex)
		}
		if retryErr := h.SubtitleCache.ServeExtract(response, r, opts, playback.StreamExtractSubtitle); retryErr != nil {
			playback.LogSubtitleStreamError(r.Context(), retryErr, file.ID, embeddedIndex)
			if r.Context().Err() != nil {
				return
			}
			if response.Status() != 0 {
				panic(http.ErrAbortHandler)
			}
			if playback.IsSubtitleStreamMapError(retryErr) {
				writeSubtitleSourceChanged(w)
				return
			}
			writeError(w, http.StatusInternalServerError, "subtitle_extract_failed", "Failed to extract subtitles")
			return
		}
	}

	// The windowed request just read the remote source and, by design,
	// committed nothing. Populate the full-track cache so the next window hits
	// the small artifact instead of re-demuxing the source.
	h.warmVirtualSubtitleAfterWindowMiss(file, session, opts, virtualActive)
}

// resolveCommittedTextSubtitleEntry resolves the committed full-track text
// artifact that a windowed extract for this identity would read, returning a
// binding token. Callers that skip track-identity validation because the
// artifact pins the plan-time release must pin this token on the options: the
// serve then reads that exact path (or reports
// ErrCommittedTextArtifactGone) rather than re-resolving, so a generation-bucket
// rollover or eviction between the identity check and the read cannot turn the
// validated artifact into an unvalidated source extract. Bitmap (PGS) codecs, an
// unkeyable source, and a nil cache read as false.
func (h *StreamHandler) resolveCommittedTextSubtitleEntry(opts *playback.StreamExtractOpts) (playback.CommittedTextArtifact, bool) {
	if h == nil || h.SubtitleCache == nil || opts == nil {
		return playback.CommittedTextArtifact{}, false
	}
	return h.SubtitleCache.ResolveCommittedTextEntry(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, opts.SourceCodec, opts.TargetFormat)
}

// hasCommittedTextSubtitleEntry reports whether such an artifact currently
// exists. It is a non-binding presence check used only for warm admission
// decisions; callers that skip track-identity validation must use
// resolveCommittedTextSubtitleEntry and pin the returned token.
func (h *StreamHandler) hasCommittedTextSubtitleEntry(opts *playback.StreamExtractOpts) bool {
	_, ok := h.resolveCommittedTextSubtitleEntry(opts)
	return ok
}

// virtualSubtitleWarmResolveTimeout bounds the relay resolution a detached
// virtual subtitle warm performs. It is a child of the extraction context the
// cache hands the warm, so resolution can never outlive extraction; a resolver
// that never returns is canceled with the extraction instead of pinning the
// cache's warm slot and in-flight fill for the whole extraction budget. Tests
// override it to keep the release assertion fast.
var virtualSubtitleWarmResolveTimeout = 2 * time.Minute

// warmVirtualSubtitleAfterWindowMiss starts a detached full-track warm for a
// windowed virtual text request that could not be served from the cache. It is
// called after the window itself has streamed, so the warm cannot contend with
// the triggering request's own read.
//
// The warm resolves and holds its own relay registration: the request's
// registration is released when the request ends, which is exactly why the
// cache's own detached warm is disabled for virtual inputs
// (opts.DisableBackgroundWarm). Resolution happens lazily inside the extract
// closure on a context derived from the extraction context, so the cache's warm
// budget (warmSem) and in-flight coalescing (beginFill) gate it before any
// remote work and a stuck resolver cannot pin either: concurrent window misses
// on the same serve identity resolve once and demux once, and the registration
// is released exactly once when the warm settles.
func (h *StreamHandler) warmVirtualSubtitleAfterWindowMiss(file *models.MediaFile, session *playback.Session, opts playback.StreamExtractOpts, virtualActive bool) {
	if h == nil || h.SubtitleCache == nil || file == nil || session == nil || !virtualActive {
		return
	}
	if opts.CacheIdentity == "" {
		return
	}
	// Only text tracks get this handler-owned warm. PGS keeps its existing
	// window path (its mandatory drift probe and progressive .sup handling are
	// deliberately unchanged).
	if playback.IsPGS(opts.SourceCodec) {
		return
	}
	// Full-track requests already fill the cache inline through ServeExtract's
	// tee; only explicit windows need a detached warm.
	if opts.SeekSeconds == 0 && opts.DurationSeconds == 0 {
		return
	}
	// A committed entry means this request served its window from the cached
	// artifact; nothing to warm.
	if h.hasCommittedTextSubtitleEntry(&opts) {
		return
	}

	go func() {
		var cleanup func()
		defer func() {
			if cleanup != nil {
				cleanup()
			}
		}()
		extract := func(extractCtx context.Context, extractOpts playback.StreamExtractOpts) error {
			// Resolve on a child of the extraction context, never the detached
			// request context: a resolver that hangs is canceled when the warm
			// budget is exhausted, releasing the warm slot and fill instead of
			// holding them forever.
			resolveCtx, cancel := context.WithTimeout(extractCtx, virtualSubtitleWarmResolveTimeout)
			defer cancel()
			resolved, resolvedCleanup, err := h.resolveVirtualInputURI(resolveCtx, file, session.UserID, session.ProfileID, false)
			if err != nil {
				logVirtualStreamFailure(resolveCtx, session.ID, file, err)
				return fmt.Errorf("resolve virtual input for subtitle warm: %w", err)
			}
			cleanup = resolvedCleanup
			extractOpts.InputPath = resolved.URL
			return playback.StreamExtractSubtitle(extractCtx, extractOpts)
		}
		<-h.SubtitleCache.WarmTrackInBackground(opts, extract)
	}()
}

// Defaults for the implicit first window served to a cold whole-track text
// subtitle request against a virtual/remote source. The duration matches the
// web player's sliding-window cadence (see web/src/player/hooks/useSubtitleTracks.ts)
// so a client that manages its own windows and one that does not observe the
// same coverage. The backoff pulls the window start behind the session
// position so a small scrub back stays covered, mirroring that player's
// SEEK_BACKOFF.
const (
	virtualSubtitleImplicitWindowSeconds        = 600.0
	virtualSubtitleImplicitWindowBackoffSeconds = 2.0
	// virtualSubtitleImplicitWindowMinSourceBytes is the source size above
	// which a whole-track virtual text read is considered untenable and is
	// bounded to the implicit window. A small known source is read whole (cheap
	// and complete); an unknown size (0) is treated as large, because a virtual
	// row's size is not always populated.
	virtualSubtitleImplicitWindowMinSourceBytes = 256 << 20
)

// applyImplicitVirtualTextWindow bounds a cold whole-track text subtitle fetch
// against a virtual/remote source to the first playback-sized window instead of
// an unbounded whole-container extract.
//
// A virtual source is served over HTTP from a provider; extracting a complete
// text track requires demuxing every byte of the container, because subtitle
// blocks are interleaved with the audio/video packets. A 6.6 GB remux therefore
// costs ~100 s of sequential reads before the response can complete, while the
// client's fetch deadline is ~30-60 s — the request is aborted and the partial
// fill is discarded, so retrying never makes progress. Local files are cheap to
// read whole and keep the existing whole-track behavior.
//
// The window is only synthesized when the caller did not ask for a specific
// window and no committed full-track artifact exists. A windowed serve is fast
// (ffmpeg seeks near the requested position) and the handler starts the usual
// detached full-track warm afterwards, so this request completes in seconds and
// later requests — or a client that fetched this window — read the small cached
// artifact. When an artifact is already present the whole track is served, so
// an established virtual track still returns every cue in one response.
//
// Correctness: this changes what a cold, implicit whole-track virtual text
// response contains — a bounded window rather than every cue — so a client that
// never re-requests will stop seeing cues past the window. Explicitly-windowed
// callers (the web player) are unaffected, and local, small virtual, ASS/SSA,
// and PGS tracks keep their existing paths. The old whole-track behavior
// remains available by sending an explicit position/duration (any window intent
// bypasses this).
//
// This deliberately bends the documented default in
// docs/architecture/playback-protocol-v3.md §4.2/§8 ("embedded text URLs return
// the complete track from source time zero by default; consumers that maintain
// a sliding window may explicitly supply position and duration"). A remote
// multi-GB source makes that default unservable within a client fetch deadline.
// The fully contract-conformant fix is for every client to request windows the
// way the web player does (or a v3 contract amendment making windowed sidecars
// the default for virtual sources); this helper is a server-side stopgap until
// that lands. It is gated to large/unknown virtual text sources so nothing else
// changes.
func (h *StreamHandler) applyImplicitVirtualTextWindow(opts *playback.StreamExtractOpts, file *models.MediaFile, session *playback.Session, virtualActive bool) {
	if h == nil || opts == nil || !virtualActive {
		return
	}
	// A small known source costs little to read whole, so keep the complete
	// artifact. Unknown size (0) stays windowed: virtual rows do not always
	// carry a populated size.
	if file != nil && file.FileSize > 0 && file.FileSize < virtualSubtitleImplicitWindowMinSourceBytes {
		return
	}
	// ASS preserves authored styling and is copied whole script; PGS is a
	// bitmap elementary stream. Only converted text (WebVTT) is safe to slice
	// with the same whole-artifact semantics.
	if playback.IsASS(opts.SourceCodec) || playback.IsPGS(opts.SourceCodec) {
		return
	}
	// Any explicit window intent is authoritative, including position=0 and a
	// duration-only request; leave it untouched.
	if opts.WindowRequested || opts.SeekSeconds > 0 || opts.DurationSeconds > 0 {
		return
	}
	// A committed full-track artifact is cheap to serve whole, so do not slice
	// it. (A pinned artifact resolves as committed here, so the drift-probe
	// skip path also serves whole.)
	if h.hasCommittedTextSubtitleEntry(opts) {
		return
	}

	opts.WindowRequested = true
	opts.SeekSeconds = implicitVirtualTextWindowStart(session)
	opts.DurationSeconds = virtualSubtitleImplicitWindowSeconds
	slog.Info("virtual text subtitle served as an implicit window",
		"component", "api",
		"seek_seconds", opts.SeekSeconds,
		"duration_seconds", opts.DurationSeconds,
		"track", opts.TrackIndex)
}

// implicitVirtualTextWindowStart returns the source-time start of the implicit
// window: the session position pulled back slightly, or zero when the session
// has no meaningful position yet (a fresh start, so the window covers the
// opening of the track).
func implicitVirtualTextWindowStart(session *playback.Session) float64 {
	if session == nil || session.Position <= virtualSubtitleImplicitWindowBackoffSeconds {
		return 0
	}
	return session.Position - virtualSubtitleImplicitWindowBackoffSeconds
}

// verifyVirtualSubtitleLayout probes the live relay input once and, when its
// subtitle layout drifted from the plan-time evidence this session captured,
// re-maps the extract options onto a same-class live track. It reports whether
// extraction may proceed, plus the probe error when the probe itself could not
// run. proceed=false means a successful probe positively found that the live
// source cannot satisfy the requested representation — rotation to a different
// subtitle class, or an ambiguous or absent match — and the caller must answer
// with a clean retryable 4xx before ffmpeg spawns or headers commit. A non-nil
// probeErr is not evidence of rotation; text/ASS callers deliberately proceed
// on the plan ordinal (a genuine rotation still surfaces via the post-spawn map
// error), while a PGS caller must fail closed because its .sup response commits
// 200 before ffmpeg spawns. Virtual inputs are request-local probe state; the
// session's published evidence is never rewritten.
func (h *StreamHandler) verifyVirtualSubtitleLayout(ctx context.Context, requestedTrack models.SubtitleTrack, session *playback.Session, opts *playback.StreamExtractOpts) (bool, error) {
	if session == nil || opts == nil || strings.TrimSpace(opts.InputPath) == "" {
		return true, nil
	}
	liveTracks, err := playback.ProbeSubtitleLayout(ctx, h.ffmpegPath(), opts.InputPath)
	if err != nil {
		// A probe failure (context canceled, relay timeout, transient upstream
		// error) is not evidence that the pinned source rotated. Serve the
		// planned ordinal rather than forcing a replan: a genuine rotation is
		// still caught for text/ASS by the post-spawn map-error safety net, and
		// treating a probe failure as a rotation produced a churn loop when the
		// relay itself was what timed out. Only a probe that SUCCEEDS and
		// positively reports a different layout warrants a 409. The error is
		// returned so a bitmap caller can fail closed instead of committing a
		// possibly-truncated .sup.
		slog.WarnContext(ctx, "virtual subtitle layout probe failed", "component", "api",
			"track_codec", requestedTrack.Codec,
			"error", err)
		return true, err
	}
	if playback.SubtitleLayoutsEqual(liveTracks, session.VirtualSubtitleTracks) {
		// The pinned release is unchanged — the catalog row was re-probed
		// against a different candidate. The plan ordinal already names the
		// live layout.
		return true, nil
	}
	liveOrdinal, liveTrack, matched := playback.MatchEmbeddedSubtitleTrack(requestedTrack, liveTracks)
	if !matched {
		slog.WarnContext(ctx, "virtual subtitle layout rotated without a usable match", "component", "api",
			"requested_codec", requestedTrack.Codec,
			"requested_language", requestedTrack.Language,
			"live_subtitle_count", len(liveTracks))
		return false, nil
	}
	// Class preservation is the hard rule: the URL extension was minted at
	// plan time, so a re-map may only land on a codec whose extraction uses
	// the same output muxer.
	if playback.SubtitleExtractMuxer(requestedTrack.Codec, opts.TargetFormat) != playback.SubtitleExtractMuxer(liveTrack.Codec, opts.TargetFormat) {
		slog.WarnContext(ctx, "virtual subtitle remap rejected: output muxer mismatch", "component", "api",
			"plan_codec", requestedTrack.Codec,
			"live_codec", liveTrack.Codec)
		return false, nil
	}
	planOrdinal := opts.TrackIndex
	opts.TrackIndex = liveOrdinal
	opts.SourceCodec = liveTrack.Codec
	slog.InfoContext(ctx, "virtual subtitle track remapped onto live layout", "component", "api",
		"plan_ordinal", planOrdinal,
		"live_ordinal", liveOrdinal,
		"codec", liveTrack.Codec,
		"language", liveTrack.Language)
	return true, nil
}

// writeSubtitleSourceChanged answers a clean retryable 4xx when a virtual
// release rotated so the requested subtitle representation can no longer be
// produced from the live source. Clients already retry through the
// sliding-window fetcher / replan flow, so the response is deliberately a
// retryable 4xx, never a 500 or an ambiguous partial stream.
func writeSubtitleSourceChanged(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "subtitle_source_changed",
		"The selected subtitle track changed on the media source; retry")
}

// writeSubtitleSourceUnavailable answers a retryable 503 when a virtual bitmap
// (PGS) source's live layout could not be established before its .sup response
// would commit 200. A .sup commits its status before ffmpeg spawns and has no
// post-spawn recovery, so the server fails closed rather than stream a
// possibly-truncated track; the client can retry once the source settles.
func writeSubtitleSourceUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "subtitle_source_unavailable",
		"Unable to verify the subtitle source; retry")
}

// subtitleExtractWindow resolves the extraction window for an embedded
// subtitle request from its explicit query parameters. WebVTT and ASS slices
// read ?position/?duration directly; the explicit window intent is carried
// separately on StreamExtractOpts.WindowRequested (see subtitleWindowRequested)
// so a position=0 window is not mistaken for a whole-track fetch. PGS requires
// the explicit ?windowed=1 opt-in via PGSWindowRequest so a whole-track consumer
// never silently loses cues outside an implicit window.
func subtitleExtractWindow(r *http.Request, outFormat string) (allowWindow bool, seek, duration float64) {
	switch outFormat {
	case "vtt", subtitleFormatASS:
		return false, subtitleSeekPosition(r), subtitleWindowDuration(r)
	case subtitleFormatSUP:
		return playback.PGSWindowRequest(r.URL.Query())
	}
	return false, 0, 0
}

// subtitleWindowRequested reports whether the caller explicitly asked for a
// bounded window by supplying a position parameter. A present position counts
// even at zero: position=0&duration=600 is the first bounded slice, whereas a
// request that omits position (and duration) is a whole-track fetch. Only an
// explicit position sets the intent; a duration-only request keeps its existing
// behavior (text is still capped by DurationSeconds, ASS stays whole-track), so
// this cannot silently turn an ordinary artifact fetch into a window.
func subtitleWindowRequested(r *http.Request) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("position"))
	if raw == "" {
		return false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// subtitleSeekPosition uses only the caller's explicit position. Session
// progress must never silently remove cues from a complete subtitle artifact.
func subtitleSeekPosition(r *http.Request) float64 {
	if raw := r.URL.Query().Get("position"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
			return v
		}
	}
	return 0
}

// subtitleWindowDuration bounds extraction only when the client explicitly
// requests a valid duration. Ordinary artifact consumers fetch the whole track.
func subtitleWindowDuration(r *http.Request) float64 {
	const maxDuration = 3600.0
	if raw := r.URL.Query().Get("duration"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= maxDuration {
			return v
		}
	}
	return 0
}
