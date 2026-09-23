package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/telemetry"

	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// startLocalPlaybackTransport is the shared local ffmpeg launch primitive for
// legacy and protocol-v3 orchestration. Callers retain ownership of lifecycle
// locking and decide whether registration is immediate or transactionally
// staged.
type localPlaybackStartupError struct {
	cause   error
	running bool
}

func (e *localPlaybackStartupError) Error() string {
	return e.cause.Error()
}

func (e *localPlaybackStartupError) Unwrap() error {
	return e.cause
}

func (h *PlaybackHandler) startLocalPlaybackTransport(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Tone-map hardware/software selection and retries are owned by the v3
	// planner (softwareToneMapRetryOptsV3) and the compat recipe resolver; this
	// primitive starts exactly the executor it was given.
	return h.startLocalPlaybackTransportOnce(ctx, opts)
}

func (h *PlaybackHandler) startTranscodeSession(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	if h != nil && h.StartTranscodeFunc != nil {
		return h.StartTranscodeFunc(ctx, opts)
	}
	return playback.StartTranscode(ctx, opts)
}

// virtualSourceRotationContextKeyV3 scopes an explicit replacement candidate to
// a single transport preparation.
type virtualSourceRotationContextKeyV3 struct{}

type virtualSourceRotationV3 struct {
	URI   string
	Owner int
}

// withVirtualSourceRotationV3 threads the replacement virtual candidate for a
// decode-driven rotation through one transport preparation. The live session is
// still bound to the rejected candidate until the durable session replacement
// commits, so the transport must be told which candidate this generation serves
// without mutating session state early.
func withVirtualSourceRotationV3(ctx context.Context, uri string, owner int) context.Context {
	if ctx == nil || strings.TrimSpace(uri) == "" {
		return ctx
	}
	return context.WithValue(ctx, virtualSourceRotationContextKeyV3{}, virtualSourceRotationV3{URI: uri, Owner: owner})
}

func virtualSourceRotationFromContextV3(ctx context.Context) (string, int, bool) {
	if ctx == nil {
		return "", 0, false
	}
	rotation, ok := ctx.Value(virtualSourceRotationContextKeyV3{}).(virtualSourceRotationV3)
	if !ok || strings.TrimSpace(rotation.URI) == "" {
		return "", 0, false
	}
	return rotation.URI, rotation.Owner, true
}

func (h *PlaybackHandler) startLocalPlaybackTransportOnce(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Repeated input demux failures stamp the virtual candidate known-bad; the
	// manager owns the callback so every local start (fresh or reconstructed)
	// reaches the same marker without the transcode package importing handlers.
	if opts.OnDemuxFailure == nil && h.tm != nil {
		opts.OnDemuxFailure = h.tm.OnDemuxFailure
	}
	if opts.OnSourceRejected == nil && h.tm != nil {
		opts.OnSourceRejected = h.tm.OnSourceRejected
	}
	if !strings.HasPrefix(strings.ToLower(opts.InputPath), virtualPlaybackPrefix) {
		session, startErr := h.startTranscodeSession(context.WithoutCancel(ctx), opts)
		if startErr != nil {
			return nil, startErr
		}
		if _, readyErr := session.WaitForManifest(8 * time.Second); readyErr != nil {
			startupErr := &localPlaybackStartupError{cause: readyErr, running: session.IsRunning()}
			_ = session.Close()
			return nil, startupErr
		}
		return session, nil
	}
	// The catalog candidate can be replaced while playback is starting. Do not
	// make a just-selected virtual row a second hard dependency; the canonical
	// URI and owner carried in opts are sufficient to resolve the provider URL.
	file := &models.MediaFile{ID: opts.MediaFileID, FilePath: opts.InputPath, VirtualOwnerInstallationID: opts.VirtualSourceOwnerInstallationID}
	if h.fileResolver != nil && opts.MediaFileID > 0 {
		if catalogFile, lookupErr := h.fileResolver.GetByID(ctx, opts.MediaFileID); lookupErr == nil && catalogFile != nil {
			origPath := file.FilePath
			file = catalogFile
			if isUnplayableVirtualURI(file.FilePath) && !isUnplayableVirtualURI(origPath) {
				file.FilePath = origPath
			}
		}
	}
	userID, profileID := 0, ""
	ownerInstallationID := file.VirtualOwnerInstallationID
	sessionVirtualURI := ""
	if rotationURI, rotationOwner, rotationSet := virtualSourceRotationFromContextV3(ctx); rotationSet {
		// A decode-driven candidate rotation deliberately serves the plan's
		// replacement release. Prefer it over the session's still-rejected
		// binding; the durable session replacement commits the same binding.
		copy := *file
		copy.FilePath = rotationURI
		if rotationOwner > 0 {
			copy.VirtualOwnerInstallationID = rotationOwner
		}
		file = &copy
		ownerInstallationID = file.VirtualOwnerInstallationID
		opts.InputPath = file.FilePath
		sessionVirtualURI = rotationURI
		if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
			userID, profileID = session.UserID, session.ProfileID
		}
	} else if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
		userID, profileID = session.UserID, session.ProfileID
		sessionVirtualURI = session.VirtualSourceURI
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(session.VirtualSourceURI)), virtualPlaybackPrefix) && (!isUnplayableVirtualURI(session.VirtualSourceURI) || isUnplayableVirtualURI(file.FilePath)) {
			copy := *file
			copy.FilePath = session.VirtualSourceURI
			if session.VirtualSourceOwnerInstallationID > 0 {
				copy.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			}
			file = &copy
			ownerInstallationID = file.VirtualOwnerInstallationID
			opts.InputPath = file.FilePath
		}
	}
	canonicalPath := opts.InputPath
	neutralPath := virtualPlaybackNeutralKey(file.FilePath)
	if neutralPath == "" || strings.HasSuffix(neutralPath, "://") {
		neutralPath = virtualPlaybackNeutralKey(opts.InputPath)
	}
	initialPinnedPath := ""
	initialPinnedResultID := ""
	if file != nil && strings.Contains(file.FilePath, "?result=") {
		initialPinnedPath = file.FilePath
		if parsed, err := url.Parse(file.FilePath); err == nil {
			initialPinnedResultID = parsed.Query().Get("result")
		}
	}
	opts.CanonicalInputPath = canonicalPath
	opts.VirtualSourceOwnerInstallationID = ownerInstallationID
	opts.RefreshInput = func(refreshCtx context.Context) (string, func(), error) {
		// A restart renews the exact candidate pinned to this session, never a
		// provider-neutral re-selection: re-resolving through the neutral path
		// can silently swap to a differently-ranked candidate mid-stream. The
		// canonical path still carries the ?result= identity the session bound
		// to during planning.
		res, cleanup, err := h.resolveVirtualInputURI(
			refreshCtx, canonicalPath, ownerInstallationID,
			userID, profileID, true, nil, "",
		)
		return res.URL, cleanup, err
	}
	var lastErr error
	startupCtx, startupCancel := context.WithTimeout(context.WithoutCancel(ctx), virtualStartupBudget)
	defer startupCancel()
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	if sessionVirtualURI != "" {
		maxAttempts = min(2, maxAttempts)
	}
	failedCandidateIDs := make([]string, 0)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		targetURI := canonicalPath
		if attempt > 0 {
			targetURI = neutralPath
		}
		preferredID := initialPinnedResultID
		for _, failed := range failedCandidateIDs {
			if preferredID == failed {
				preferredID = ""
				break
			}
		}
		// An exclusion is only a substitution verdict when this startup loop
		// just indicted that candidate (attempt > 0 with a failed id). Declare
		// it explicitly so the resolver may serve a sibling; a neutral first
		// attempt, or a failure that identified no candidate, keeps refusing.
		resolvedMedia, cleanup, resolveErr := h.resolveVirtualInputURI(
			startupCtx, targetURI, ownerInstallationID, userID, profileID, attempt > 0, failedCandidateIDs, preferredID, attempt > 0 && len(failedCandidateIDs) > 0,
		)
		if resolveErr != nil {
			lastErr = resolveErr
			failedID := resolvedMedia.CandidateID
			if failedID == "" {
				if parsed, err := url.Parse(targetURI); err == nil {
					failedID = parsed.Query().Get("result")
				}
			}
			if failedID != "" {
				failedCandidateIDs = append(failedCandidateIDs, failedID)
			}
			if h.BestResultCache != nil && file != nil {
				neutralURI := virtualPlaybackNeutralKey(canonicalPath)
				failedURI := targetURI
				if failedID != "" {
					failedURI = withVirtualResultKey(neutralURI, failedID)
				}
				h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
				h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
			}
			if targetURI == canonicalPath {
				canonicalPath = neutralPath
				if file != nil {
					file.FilePath = neutralPath
				}
			}
			continue
		}
		transcodeCtx, transcodeCancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(4*time.Hour, transcodeCancel)
		cleanupWithCancel := func() {
			timer.Stop()
			transcodeCancel()
			if cleanup != nil {
				cleanup()
			}
		}
		attemptOpts := opts
		attemptOpts.InputPath = resolvedMedia.URL
		attemptOpts.InputCleanup = cleanupWithCancel
		session, startErr := h.startTranscodeSession(transcodeCtx, attemptOpts)
		if startErr == nil {
			if _, readyErr := session.WaitForManifest(playback.ManifestStartupTimeout); readyErr == nil {
				winningURI := resolvedMedia.URI
				if winningURI == "" || winningURI == neutralPath {
					if resolvedMedia.CandidateID != "" {
						winningURI = withVirtualResultKey(neutralPath, resolvedMedia.CandidateID)
					} else {
						winningURI = targetURI
					}
				}
				canonicalPath = winningURI
				if file != nil {
					file.FilePath = winningURI
				}
				// Transport successfully ready. If this was a fallback attempt from a dead pin,
				// update the persisted pin compare-and-swap so future sessions use the live source.
				if replacer, ok := h.fileResolver.(interface {
					ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
				}); ok && file != nil && file.ID > 0 && initialPinnedPath != "" {
					if winningURI != "" && winningURI != initialPinnedPath {
						unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
						if _, err := replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, winningURI); err != nil {
							slog.WarnContext(ctx, "failed to update virtual result pin after fallback success", "component", "api", "file_id", file.ID, "error", err)
						}
						unpinCancel()
					}
				}
				return session, nil
			} else {
				startErr = readyErr
			}
			_ = session.Close()
		} else if cleanup != nil {
			cleanupWithCancel()
		}
		if resolvedMedia.CandidateID != "" {
			failedCandidateIDs = append(failedCandidateIDs, resolvedMedia.CandidateID)
		}
		if h.BestResultCache != nil && file != nil {
			neutralURI := virtualPlaybackNeutralKey(canonicalPath)
			failedURI := targetURI
			if resolvedMedia.CandidateID != "" {
				failedURI = withVirtualResultKey(neutralURI, resolvedMedia.CandidateID)
			}
			h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), failedURI)
			h.BestResultCache.RemoveCandidateForContent(file.ContentID, neutralURI, ownerInstallationID, failedURI)
		}
		if targetURI == canonicalPath {
			canonicalPath = neutralPath
			if file != nil {
				file.FilePath = neutralPath
			}
		}
		lastErr = startErr
	}
	if lastErr == nil {
		lastErr = errors.New("virtual transcode provider returned no usable stream")
	}
	// All attempts failed: conditionally clear the initial dead pin so future plays re-list
	if file != nil && file.ID > 0 {
		unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if replacer, ok := h.fileResolver.(interface {
			ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error)
		}); ok && initialPinnedPath != "" {
			_, _ = replacer.ReplaceVirtualResultPin(unpinCtx, file.ID, initialPinnedPath, neutralPath)
		} else if cleaner, ok := h.fileResolver.(interface {
			ClearVirtualResultPin(context.Context, int) error
		}); ok {
			_ = cleaner.ClearVirtualResultPin(unpinCtx, file.ID)
		}
		unpinCancel()
	}
	return nil, lastErr
}

// resolveVirtualInputURI resolves a virtual input for a transport start. The
// final rotateCandidates argument is the caller's explicit declaration that
// excluding a candidate is a verdict against that release, which authorizes
// serving a sibling. It defaults to false, so an exclusion on its own never
// authorizes a silent release swap (see resolveVirtualInputURIExcluding on the
// stream handler for the same contract). The variadic form keeps the many
// non-excluding callers unchanged; only a serve-layer failover that just
// indicted the candidate passes true.
func (h *PlaybackHandler) resolveVirtualInputURI(
	ctx context.Context,
	virtualURI string,
	ownerInstallationID int,
	userID int,
	profileID string,
	forceRefresh bool,
	excludedCandidateIDs []string,
	preferredCandidateID string,
	rotateCandidates ...bool,
) (ResolvedVirtualMedia, func(), error) {
	rotationRequested := false
	if len(rotateCandidates) > 0 {
		rotationRequested = rotateCandidates[0]
	}
	var res ResolvedVirtualMedia
	var err error
	// Stored-URL shortcut. A session-bound re-resolve of a pinned candidate —
	// the remux seek anchor, a transport restart, the subtitle/font warm — may
	// serve the row's own persisted provider URL instead of listing the
	// provider again. The URL belongs to the exact candidate being resolved
	// (see evaluateStoredVirtualURLCandidate for the same-row check), so it is
	// the pinned release itself and never a substitution. Narrow by design:
	// an explicit forceRefresh (a failover retry after this candidate failed)
	// or an exclusion list always takes the list-and-resolve path.
	var storedRow *models.MediaFile
	// Read the row once. It is both the source of the stored-URL shortcut
	// and the durable identity the same-release re-match needs when the
	// provider renumbers its result ids. On a forced refresh the stored URL
	// is never served (the shortcut below requires !forceRefresh), but the
	// durable identity is still threaded: force-refresh bypasses cached
	// transport, not identity, so a harmless provider renumbering still
	// re-matches the same release instead of failing same-release recovery.
	if h.VirtualFileLookup != nil {
		if row, lookupErr := h.VirtualFileLookup(ctx, virtualURI); lookupErr == nil && row != nil {
			storedRow = row
			ctx = virtualResolveContextWithPersistedTrust(ctx, row, time.Now(), h.virtualCandidateTrustWindow())
		}
	}
	var storedExpiredRow *models.MediaFile
	storedUsable := false
	if storedRow != nil && !forceRefresh && len(excludedCandidateIDs) == 0 {
		usable, state := evaluateStoredVirtualURLCandidate(
			ctx, virtualURI, storedRow,
			h.storedVirtualURLAllowInsecure(storedRow, ownerInstallationID), time.Now(),
			h.virtualCandidateTrustWindow(),
		)
		switch state {
		case virtualStoredURLUsable:
			res = usable
			storedUsable = true
		case virtualStoredURLExpiredWithinWindow:
			// The signed URL lapsed but the row is still trusted: resolve the
			// same candidate afresh and refresh the stored value below. Never
			// serve the expired URL itself.
			storedExpiredRow = storedRow
		case virtualStoredURLExpired:
			// The row owns this candidate but its URL lapsed. Resolve afresh
			// below, then refresh the stored value through the existing
			// Phase-1 saver.
			storedExpiredRow = storedRow
		}
	}
	if !storedUsable {
		if h.VirtualMediaDetailedResolver != nil {
			// The intent travels with the context so the resolver can distinguish a
			// serve-layer indictment from a display-driven same-file re-plan.
			ctx = withVirtualCandidateRotationV3(ctx, rotationRequested)
			// The transport serve layer re-resolves a release an existing session
			// already serves, so it declares session-bound: a profile-removed
			// candidate refuses instead of silently swapping the release.
			ctx = withVirtualSessionBindingV3(ctx, true)
			res, err = h.VirtualMediaDetailedResolver.ResolveVirtualMediaDetailed(
				ctx, virtualURI, ownerInstallationID, userID, profileID, forceRefresh, excludedCandidateIDs, preferredCandidateID,
			)
			if err == nil && res.IdentityRematched && storedRow != nil {
				// The pinned id was absent but the same release re-identified
				// under a new id. Adopt it through the Phase-1 CAS/fence write
				// so the row's ?result= and durable identity move with it.
				// storedRow is always loaded (see above), so this covers a
				// forced refresh too — not only the stored-URL shortcut path.
				adoptRematchedVirtualResolution(ctx, storedRow, res, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			} else if err == nil && storedExpiredRow != nil {
				// Reuse the Phase-1 write path; only the requested candidate's
				// own successful resolution is recorded (a substituted sibling
				// is skipped inside). The result is intentionally ignored here:
				// the serve path already holds the resolved URL, so persistence
				// is best-effort cache hygiene, not the request's outcome.
				_, _ = refreshStoredVirtualResolution(ctx, storedExpiredRow, res, h.VirtualFileMetadataSaver, h.VirtualFileSaver)
			}
		} else if forceRefresh && h.VirtualMediaRefreshResolver != nil {
			var inputPath string
			inputPath, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
				ctx, virtualURI, ownerInstallationID, userID, profileID,
			)
			res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
		} else {
			var inputPath string
			inputPath, err = resolveVirtualMediaPath(
				ctx, h.VirtualMediaResolver, virtualURI,
				ownerInstallationID, userID, profileID,
			)
			res = ResolvedVirtualMedia{URL: inputPath, URI: virtualURI}
		}
	}
	if err != nil {
		slog.WarnContext(ctx, "virtual stream resolve failed",
			"component", "api",
			"owner_installation_id", ownerInstallationID,
			"virtual_uri", virtualURI,
			"error", logredact.SanitizeURLError(err),
		)
		return res, nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return res, nil, nil
	}
	var relayURL string
	var cleanup func()
	effectiveOwner := effectiveVirtualOwner(res.OwnerID, ownerInstallationID)
	if h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(effectiveOwner) {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterInsecureWithHeaders(ctx, res.URL, res.RequestHeaders)
	} else {
		relayURL, cleanup, err = h.RemoteStreamRelay.RegisterWithHeaders(ctx, res.URL, res.RequestHeaders)
	}
	if err != nil {
		slog.WarnContext(ctx, "virtual stream relay registration failed",
			"component", "api",
			"owner_installation_id", effectiveOwner,
			"virtual_uri", virtualURI,
			"error", logredact.SanitizeURLError(err),
		)
		return ResolvedVirtualMedia{}, nil, err
	}
	res.URL = relayURL
	return res, cleanup, nil
}

// startRemotePlaybackTransport is the shared remote-node launch primitive.
// It returns the node's HTTP status separately so legacy and v3 can preserve
// their existing public error envelopes while executing identical transport
// startup and response parsing.
func (h *PlaybackHandler) startRemotePlaybackTransport(ctx context.Context, nodeURL string, request transcodenode.TranscodeStartRequest) (transcodenode.TranscodeStartResponse, int, error) {
	if strings.HasPrefix(strings.ToLower(request.InputPath), virtualPlaybackPrefix) {
		return transcodenode.TranscodeStartResponse{}, 0, errors.New("virtual sources require an integrated transcode transport")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.remotePlaybackTransportTimeout(nodeURL, request))
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, nodepool.NodeEndpoint(nodeURL, "/transcode/start"), bytes.NewReader(body))
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+h.JWTSecret)
	response, err := telemetry.DoTrustedNode(http.DefaultClient, httpRequest, "transcode_start")
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		// Drain the (small) error body so the transport can reuse the
		// connection instead of tearing it down on every failed start.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if request.ToneMapMode != "" {
			if validationErr := transcodenode.ToneMapExecutionErrorForResponse(
				response.StatusCode,
				response.Header.Get(transcodenode.ToneMapExecutionErrorHeader),
			); validationErr != nil {
				return transcodenode.TranscodeStartResponse{}, response.StatusCode, validationErr
			}
		}
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
	}
	var result transcodenode.TranscodeStartResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		// Older nodes returned an empty 202 response; accept that for ordinary
		// transcodes while treating any other malformed 202 body as a failed
		// start instead of fabricating a success from a zero-value response.
		if errors.Is(err, io.EOF) && request.ToneMapMode == "" {
			return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
		}
		slog.WarnContext(ctx, "remote transcode start response decode failed", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, fmt.Errorf("decode remote transcode start response: %w", err)
	}
	return result, response.StatusCode, nil
}

func (h *PlaybackHandler) remotePlaybackTransportTimeout(nodeURL string, request transcodenode.TranscodeStartRequest) time.Duration {
	// A burn-in node waits longer for its first segment, so the caller's HTTP
	// budget must cover the same budget or it would abort a slow-but-healthy
	// subtitle composite before the node answers. Non-burn-in plans keep the
	// historical numbers (TranscodeStartReadinessTimeout == ManifestStartupTimeout).
	readinessBudget := playback.ManifestStartupTimeoutFor(playback.TranscodeOpts{
		SubtitleBurnIn:     request.SubtitleBurnIn,
		SubtitleTrackIndex: request.SubtitleTrackIndex,
	})
	if request.ToneMapMode == "" {
		return readinessBudget + 5*time.Second
	}
	timeout := h.remoteToneMapProbeTimeoutV3(nodeURL) + readinessBudget
	if request.ToneMapPreflightRequired {
		timeout += tonemap.SourcePreflightTimeout(request.TotalDuration)
	}
	if request.RequireReady {
		timeout += readinessBudget
	}
	return timeout
}

func fetchRemoteTranscodeCapabilities(ctx context.Context, nodeURL, jwtSecret string) (playback.HWAccelInfo, error) {
	info, status, err := transcodenode.FetchHWCapabilities(ctx, http.DefaultClient, nodeURL, jwtSecret)
	if err != nil {
		return playback.HWAccelInfo{}, err
	}
	if status != http.StatusOK {
		return playback.HWAccelInfo{}, fmt.Errorf("node returned %d", status)
	}
	info.Source = "transcode_node"
	info.NodeURL = nodeURL
	return info, nil
}
