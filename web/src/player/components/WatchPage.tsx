import { isSourceFallbackReason } from "@/api/v2/watchTogetherSourceFallback";
import { playbackCapabilitiesV2 } from "../start-v2";
import { playerV2Origin } from "../player-v2";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { PlayerFileVersion, PlayerPlaybackStateChange, WatchPageProps } from "../types";
import type { PlaybackRealtimeEventEnvelope } from "../realtime-protocol";
import type { SubtitleInventoryItemV3 } from "../protocol-v3";
import { usePlaybackSession } from "../hooks/usePlaybackSession";
import { usePlayerConfig } from "../context/PlayerConfigContext";
import {
  hasSelectableSessionSubtitles,
  resolvePlayableSubtitles,
} from "../utils/playableSubtitles";
import { patchVersionMarkers, resolveActiveVersionMarkers } from "../utils/watchPageMarkers";
import {
  buildSubtitleChoiceRequests,
  sendSubtitleChoiceRequest,
} from "../utils/subtitleChoicePersistence";
import { resolveEffectiveVersion } from "../utils/resolveEffectiveVersion";
import { VideoPlayer } from "./VideoPlayer";
import { fetchWatchDetail } from "@/hooks/queries/items";
import { refreshVirtualCandidates } from "@/api/v2/mediaCandidates";
import { itemKeys } from "@/hooks/queries/keys";
import { useWatchPlaybackController } from "@/playback/watchPlaybackContext";
import { useWatchTogetherRoomConnection } from "../hooks/useWatchTogetherRoomConnection";
import { toast } from "sonner";

/**
 * Live inventory refresh. A session that started before the server finished
 * probing a virtual file carries the synthesized inventory the plan had then.
 * These bound how often the client re-reads the catalog to fill the menus in.
 * The poll only ever updates menu data; it never restarts the stream.
 */
export const INVENTORY_REFRESH_INTERVAL_MS = 20_000;
export const INVENTORY_REFRESH_MAX_ATTEMPTS = 5;
// Wall-clock backstop: failed requests do not count toward the attempt cap, so
// a persistent error loop also needs an absolute deadline to stop at.
export const INVENTORY_REFRESH_DEADLINE_MS = 5 * 60_000;
// Early cadence for the first attempts. A virtual file's probe typically
// persists its inventory within seconds of the optimistic start, so the first
// reads must not wait a full 20 s interval to see it.
export const INVENTORY_REFRESH_EARLY_DELAYS_MS: readonly number[] = [2_000, 4_000, 8_000];
// Must match `useWatchDetail`'s staleTime so the inventory poll, chapter
// refresh, and realtime marker reconcile share the mounted query's cache
// instead of each issuing an independent fetch.
const WATCH_DETAIL_STALE_TIME_MS = 30_000;

/**
 * Delay before the next inventory poll. `attempt` is the number of polls
 * already scheduled, so 0 yields the first early delay. Once the inventory is
 * found the poll is complete and the delay is 0 (no further poll).
 */
export function inventoryPollDelayMs(attempt: number, found: boolean): number {
  if (found) return 0;
  return INVENTORY_REFRESH_EARLY_DELAYS_MS[attempt] ?? INVENTORY_REFRESH_INTERVAL_MS;
}

function patchChapterThumbnail(
  versions: PlayerFileVersion[],
  fileId: number,
  chapterIndex: number,
  thumbnailUrl: string,
  thumbnailThumbhash?: string,
): PlayerFileVersion[] {
  let changed = false;
  const nextVersions = versions.map((version) => {
    if (version.file_id !== fileId || !version.chapters?.length) {
      return version;
    }

    let versionChanged = false;
    const nextChapters = version.chapters.map((chapter) => {
      if (chapter.index !== chapterIndex) {
        return chapter;
      }
      if (
        chapter.thumbnail_url === thumbnailUrl &&
        chapter.thumbnail_thumbhash === thumbnailThumbhash
      ) {
        return chapter;
      }
      changed = true;
      versionChanged = true;
      return {
        ...chapter,
        thumbnail_url: thumbnailUrl,
        thumbnail_thumbhash: thumbnailThumbhash,
      };
    });

    return versionChanged ? { ...version, chapters: nextChapters } : version;
  });

  return changed ? nextVersions : versions;
}

/**
 * Short human label for the catalogue row the plan landed on, used to name the
 * substituted source in the version-swap notice. Returns null when the row
 * carries nothing recognizable, so the notice can fall back to generic copy.
 */
function buildEffectiveVersionLabel(version: PlayerFileVersion): string | null {
  const video = version.codec_video ? version.codec_video.toUpperCase() : "";
  const parts = [version.resolution, video].filter((part) => part.trim().length > 0);
  if (parts.length === 0) {
    return null;
  }
  return `${parts.join(" ")}${version.hdr ? " HDR" : ""}`;
}

/**
 * WatchPage is the top-level player component.
 * Starts a playback session, then renders the VideoPlayer once the stream is ready.
 */
export function WatchPage(props: WatchPageProps) {
  const config = usePlayerConfig();
  return props.watchTogetherRoomId ? (
    <WatchPartyPlaybackGate
      key={`${playerV2Origin(config)}:${props.watchTogetherRoomId}`}
      {...props}
    />
  ) : (
    <WatchPagePlayer {...props} />
  );
}

function WatchPartyPlaybackGate(props: WatchPageProps) {
  const config = usePlayerConfig();
  const [status, setStatus] = useState<"checking" | "supported" | "unsupported" | "failed">(
    "checking",
  );
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let cancelled = false;
    void playbackCapabilitiesV2(config)
      .then(({ features }) => {
        if (!cancelled) {
          setStatus(
            features.includes("watch_party_coordinator_v1") &&
              features.includes("fixed_media_file_v1")
              ? "supported"
              : "unsupported",
          );
        }
      })
      .catch(() => {
        if (!cancelled) setStatus("failed");
      });
    return () => {
      cancelled = true;
    };
  }, [config, attempt]);

  if (status === "supported") return <WatchPagePlayer {...props} />;
  return (
    <div className="bg-background fixed inset-0 z-50 flex items-center justify-center px-6">
      <div className="surface-panel-subtle flex max-w-md flex-col items-center gap-4 rounded-[1.8rem] px-8 py-8 text-center">
        <p className="text-base font-semibold text-white">
          {status === "checking" ? "Checking Watch Party support..." : "Watch Party unavailable"}
        </p>
        {status !== "checking" && (
          <p className="text-sm text-white/60">
            {status === "unsupported"
              ? "This server needs an update to support Watch Party."
              : "Unable to check Watch Party support. Please try again."}
          </p>
        )}
        {status === "failed" && (
          <button
            type="button"
            className="rounded-[0.95rem] bg-white/10 px-4 py-2 text-sm font-medium text-white"
            onClick={() => {
              setStatus("checking");
              setAttempt((value) => value + 1);
            }}
          >
            Try Again
          </button>
        )}
        <button
          type="button"
          className="rounded-[0.95rem] bg-white/10 px-4 py-2 text-sm font-medium text-white"
          onClick={() => {
            void props.onExit();
          }}
        >
          Go Back
        </button>
      </div>
    </div>
  );
}

function WatchPagePlayer({
  contentId,
  title,
  year,
  playbackRequestKey,
  fileId,
  libraryId,
  versions,
  playbackVariants = [],
  subtitles,
  initialPosition,
  forceInitialPosition,
  qualityPreference,
  maxBitrateKbps,
  explicitAudioTrackIndex,
  initialSubtitleTrackIndexByFileId,
  initialBitmapSubtitleTrackIndexByFileId,
  explicitFileSelection = false,
  forceRelink = false,
  preferredSubtitleLanguage,
  preferredSubtitleTrackSignature,
  subtitleMode,
  showForcedSubtitles,
  profileLanguage,
  introSkipMode,
  autoSkipRecap,
  autoPlayNextPreview,
  canEditMarkers,
  seriesContext,
  onNavigateEpisode,
  onEnded,
  onExit,
  onMinimize,
  resumeHints,
  displayMode,
  onPictureInPictureChange,
  autoEnterPictureInPicture,
  onPlaybackStateChange,
  onPlaybackTransportReady,
  onReturnFromPostRoll,
  watchTogetherRoomId,
  watchTogetherRoomToken,
}: WatchPageProps) {
  const config = usePlayerConfig();
  const queryClient = useQueryClient();
  const playbackController = useWatchPlaybackController();
  const chapterRefreshAttemptsRef = useRef<Set<number>>(new Set());
  const handledSelectionRevisionRef = useRef<number | null>(null);
  const playbackPositionRef = useRef(initialPosition ?? 0);
  const [playbackVersions, setPlaybackVersions] = useState(versions);
  const [versionSwapNoticeDismissed, setVersionSwapNoticeDismissed] = useState(false);
  const [realtimeConnectionState, setRealtimeConnectionState] = useState<
    "disconnected" | "connecting" | "connected"
  >("disconnected");
  const watchTogetherConnection = useWatchTogetherRoomConnection({
    roomId: watchTogetherRoomId,
    roomToken: watchTogetherRoomToken,
  });

  useEffect(() => {
    setPlaybackVersions(versions);
  }, [versions]);

  const session = usePlaybackSession(
    playbackRequestKey ??
      JSON.stringify([contentId, fileId ?? null, initialPosition, forceInitialPosition]),
    playbackVersions,
    playbackVariants,
    fileId,
    initialPosition,
    forceInitialPosition,
    qualityPreference,
    maxBitrateKbps,
    resumeHints,
    explicitAudioTrackIndex,
    initialSubtitleTrackIndexByFileId,
    initialBitmapSubtitleTrackIndexByFileId,
    explicitFileSelection,
    forceRelink,
    !watchTogetherRoomId,
  );

  const sessionRef = useRef(session);
  sessionRef.current = session;

  const fallbackHandledRef = useRef<string | null>(null);
  const [pendingFallbackKey, setPendingFallbackKey] = useState<string | null>(null);
  const fallbackRoom = watchTogetherConnection.room;
  const fallbackSource = watchTogetherConnection.fallbackSource;
  const fallbackReason = session.errorReason;
  const fallbackKey =
    watchTogetherRoomId &&
    watchTogetherRoomToken &&
    fallbackRoom &&
    fileId === fallbackRoom.selected_file_id &&
    isSourceFallbackReason(fallbackReason)
      ? `${watchTogetherRoomId}:${watchTogetherRoomToken}:${fallbackRoom.selection_revision}:${fileId}:${session.playbackAttemptId}:${fallbackReason}`
      : null;
  const fallingBack = fallbackKey !== null && pendingFallbackKey === fallbackKey;

  useEffect(() => {
    if (
      !fallbackKey ||
      fallbackHandledRef.current === fallbackKey ||
      !fallbackRoom ||
      !fileId ||
      !isSourceFallbackReason(fallbackReason) ||
      !fallbackRoom.members?.some((member) => member.is_self && member.connected) ||
      watchTogetherConnection.connectionState !== "connected"
    )
      return;
    fallbackHandledRef.current = fallbackKey;
    setPendingFallbackKey(fallbackKey);
    void playbackCapabilitiesV2(config)
      .then((capabilities) => {
        if (!capabilities.features.includes("watch_party_source_fallback_v1")) return null;
        return fallbackSource({
          selectionRevision: fallbackRoom.selection_revision,
          failedFileId: fileId,
          reason: fallbackReason,
        });
      })
      .catch(() => {
        // Keep the original playback refusal if there is no common fallback or
        // the request fails. A fresh playback attempt can try again.
      })
      .finally(() => {
        setPendingFallbackKey((current) => (current === fallbackKey ? null : current));
      });
  }, [
    config,
    fallbackKey,
    fallbackRoom,
    fallbackReason,
    fallbackSource,
    fileId,
    watchTogetherConnection.connectionState,
  ]);

  const initialSubtitleErrorKeyRef = useRef<string | null>(null);
  useEffect(() => {
    if (!session.initialSubtitleError || !session.playbackAttemptId) return;
    const key = `${session.playbackAttemptId}:${session.initialSubtitleError}`;
    if (initialSubtitleErrorKeyRef.current === key) return;
    initialSubtitleErrorKeyRef.current = key;
    toast.error(session.initialSubtitleErrorTitle ?? "That subtitle track can't be used", {
      description: session.initialSubtitleError,
    });
  }, [session.initialSubtitleError, session.initialSubtitleErrorTitle, session.playbackAttemptId]);

  // The plan's audio inventory is authoritative for the effective source after
  // a version fallback; item metadata can be stale. Fall back to the version's
  // probed tracks only when the plan publishes none (old plans, audiobooks).
  const audioTracks = useMemo(
    () =>
      session.planAudioTracks.length > 0
        ? session.planAudioTracks
        : (playbackVersions.find((v) => v.file_id === session.mediaFileId)?.audio_tracks ?? []),
    [playbackVersions, session.mediaFileId, session.planAudioTracks],
  );
  const playableSubtitles = useMemo(
    () => resolvePlayableSubtitles(session.subtitleUrls, subtitles),
    [session.subtitleUrls, subtitles],
  );

  const handleSwitchVersion = useCallback(
    (newFileId: number, currentPosition: number) => {
      session.switchVersion(newFileId, currentPosition);
    },
    [session],
  );

  /**
   * Manually re-lists the title's video candidates for the version menu.
   *
   * The server preserves candidates already known to be working, so its answer
   * replaces the list wholesale — but only on success. A rejected refresh
   * throws before the state write, so the rows already on screen stay put
   * rather than disappearing behind a failed request.
   */
  const handleRefreshVersions = useCallback(async () => {
    const refreshed = await refreshVirtualCandidates(contentId);
    setPlaybackVersions(refreshed);
  }, [contentId]);

  const activePlaybackVersion = useMemo(
    () => playbackVersions.find((version) => version.file_id === session.mediaFileId),
    [playbackVersions, session.mediaFileId],
  );

  const handleEnded = useCallback(() => {
    onEnded?.({
      positionSeconds: session.durationSeconds ?? 0,
      durationSeconds: session.durationSeconds ?? undefined,
      lastFileId: session.mediaFileId,
      lastResolution: activePlaybackVersion?.resolution,
      lastHDR: activePlaybackVersion?.hdr,
      lastCodecVideo: activePlaybackVersion?.codec_video,
      lastEditionKey: activePlaybackVersion?.edition_key,
    });
  }, [activePlaybackVersion, onEnded, session.durationSeconds, session.mediaFileId]);

  const handleSwitchAudio = useCallback(
    (index: number, currentPosition: number) => {
      session.switchAudioTrack(index, currentPosition);
    },
    [session],
  );

  const updatePlaybackState = session.updatePlaybackState;
  const handlePlaybackStateChange = useCallback(
    (state: PlayerPlaybackStateChange) => {
      playbackPositionRef.current = state.currentTime;
      updatePlaybackState(state.currentTime, state.playing);
      onPlaybackStateChange?.(state);
    },
    [onPlaybackStateChange, updatePlaybackState],
  );

  // Audio is complete once a virtual file has a real multi-track inventory (or
  // the file is local); subtitles once the plan publishes any inventory.
  const isVirtualActiveFile = activePlaybackVersion?.container === "virtual";

  const applyAudioInventory = session.applyAudioInventory;
  const refreshSubtitles = session.refreshSubtitles;
  useEffect(() => {
    if (!session.sessionId || !session.mediaFileId || session.loading || session.replacing) {
      return;
    }

    const needsAudio = isVirtualActiveFile && session.planAudioTracks.length <= 1;
    // Gate on what the menu can actually render, not on whether the plan
    // published any entry: a non-selectable placeholder must not suppress the
    // poll, or the probed embedded tracks never reach the menu.
    const needsSubtitles = playableSubtitles.length === 0;
    if (!needsAudio && !needsSubtitles) return;

    const mediaFileId = session.mediaFileId;
    const sessionId = session.sessionId;
    let cancelled = false;
    let completedAttempts = 0;
    // Counts every scheduled attempt, successful or not, so the early cadence
    // advances even when requests fail and the completed-attempt cap does not.
    let scheduledAttempts = 0;
    let timer: number | null = null;
    let audioComplete = !needsAudio;
    let subtitlesComplete = !needsSubtitles;
    // Absolute wall-clock deadline so an error loop that never completes a
    // fetch cannot poll past the safety window.
    const deadline = Date.now() + INVENTORY_REFRESH_DEADLINE_MS;

    const scheduleNextPoll = () => {
      if (cancelled) return;
      const delay = inventoryPollDelayMs(scheduledAttempts, audioComplete && subtitlesComplete);
      if (delay === 0) return;
      if (completedAttempts >= INVENTORY_REFRESH_MAX_ATTEMPTS) return;
      if (Date.now() >= deadline) return;
      scheduledAttempts += 1;
      timer = window.setTimeout(() => void poll(), delay);
    };

    const poll = async () => {
      // The first attempt must read past the mounted query's stale window: a
      // payload fetched at page mount would otherwise come back from the cache
      // without a request, hiding the inventory the probe just persisted.
      const isFirstAttempt = scheduledAttempts === 1;
      try {
        // Shared with the mounted `useWatchDetail` query: the same key means a
        // poll inside the stale window reuses that payload, and concurrent
        // callers dedupe onto one in-flight request.
        const detail = await queryClient.fetchQuery({
          queryKey: itemKeys.watchDetail(contentId, fileId, libraryId),
          queryFn: () => fetchWatchDetail(contentId, fileId, libraryId),
          staleTime: isFirstAttempt ? 0 : WATCH_DETAIL_STALE_TIME_MS,
        });
        if (cancelled) return;
        // Only completed responses count toward the cap; transient fetch
        // errors are retried without burning the attempt budget.
        completedAttempts += 1;
        const current = sessionRef.current;
        // A version switch can land while the request is in flight. If the
        // session no longer targets the file/session we polled for, discard
        // the response silently; the restarted effect picks up the new target.
        if (current.mediaFileId !== mediaFileId || current.sessionId !== sessionId) {
          return;
        }
        // Probe metadata is persisted to the effective candidate row, not the
        // collapsed virtual row the session id names, so resolve the target the
        // same way the menus do: effective virtual URI first, collapsed id as
        // the fallback for ordinary files and older plans.
        const version = resolveEffectiveVersion(detail.versions, {
          mediaFileId,
          effectiveVirtualUri: current.effectiveVirtualUri,
        });
        if (version) {
          const nextAudioTracks = version.audio_tracks ?? [];
          if (nextAudioTracks.length > current.planAudioTracks.length) {
            applyAudioInventory(nextAudioTracks);
            audioComplete = true;
          }
          const resolvedSubtitleTracks = version.subtitle_tracks ?? [];
          // Probe repair persists embedded tracks to the effective candidate's
          // file row, which a first-play plan may not identify yet (no
          // `effective_virtual_uri`, so `resolveEffectiveVersion` returns the
          // collapsed row). Fall back to any row the probe actually wrote so
          // the no-op replan can pull the inventory in instead of waiting on a
          // resolved snapshot that stays empty.
          const nextSubtitleTracks =
            resolvedSubtitleTracks.length > 0
              ? resolvedSubtitleTracks
              : isVirtualActiveFile
                ? (detail.versions.find((candidate) => (candidate.subtitle_tracks?.length ?? 0) > 0)
                    ?.subtitle_tracks ?? [])
                : resolvedSubtitleTracks;
          if (
            !hasSelectableSessionSubtitles(current.subtitleUrls) &&
            nextSubtitleTracks.length > 0
          ) {
            // The catalog carries no playable URLs; a no-op track_change
            // replan re-reads the plan's inventory (URLs included) without
            // changing the A/V transport, so the stream keeps playing. Only
            // treat the inventory as filled once a fresh plan actually lands:
            // a transient replan failure must not end the retry budget.
            const filled = await refreshSubtitles(playbackPositionRef.current);
            if (cancelled) return;
            if (filled || hasSelectableSessionSubtitles(sessionRef.current.subtitleUrls)) {
              subtitlesComplete = true;
            }
          }
        }
      } catch {
        // Best effort; a later attempt may still succeed.
      }
      scheduleNextPoll();
    };

    scheduleNextPoll();

    return () => {
      cancelled = true;
      if (timer !== null) window.clearTimeout(timer);
    };
    // The track counts that gate the poll are read once when it starts. They
    // are deliberately not dependencies: filling the inventory in must not
    // restart the attempt budget.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    applyAudioInventory,
    contentId,
    fileId,
    isVirtualActiveFile,
    libraryId,
    queryClient,
    refreshSubtitles,
    session.loading,
    session.mediaFileId,
    session.replacing,
    session.sessionId,
  ]);

  /**
   * Persists an in-player subtitle choice for the whole series.
   *
   * buildSubtitleChoiceRequests decides what a pick is worth storing and
   * where; this only issues the requests. They are independent on purpose: a
   * failed settings write must not cost the user the track they picked, and a
   * failed track write must not cost them the language, so each is best effort
   * on its own rather than one composite request that half-applies.
   */
  const handleSubtitleChanged = useCallback(
    (index: number | null, inventoryTrack?: SubtitleInventoryItemV3) => {
      const requests = buildSubtitleChoiceRequests({
        seriesId: seriesContext?.seriesId ?? contentId,
        index,
        tracks: playableSubtitles,
        inventoryTrack,
        showForcedSubtitles,
      });
      for (const request of requests) {
        void sendSubtitleChoiceRequest(config, request).catch(() => {
          // Best effort.
        });
      }
    },
    [config, seriesContext, contentId, playableSubtitles, showForcedSubtitles],
  );

  useEffect(() => {
    chapterRefreshAttemptsRef.current.clear();
  }, [contentId, playbackRequestKey]);

  useEffect(() => {
    if (watchTogetherConnection.replacementReason) return;
    const room = watchTogetherConnection.room;
    if (!watchTogetherRoomId || !watchTogetherRoomToken || !room) {
      handledSelectionRevisionRef.current = null;
      return;
    }

    const sameSelection =
      room.selected_content_id === contentId &&
      room.selected_file_id === fileId &&
      room.selected_library_id === libraryId;
    if (sameSelection) {
      handledSelectionRevisionRef.current = room.selection_revision;
      return;
    }
    if (room.phase !== "playing" || !room.selected_content_id) {
      return;
    }
    if (handledSelectionRevisionRef.current === room.selection_revision) {
      return;
    }

    handledSelectionRevisionRef.current = room.selection_revision;
    playbackController.startPlayback({
      contentId: room.selected_content_id,
      fileId: room.selected_file_id,
      libraryId: room.selected_library_id,
      roomId: watchTogetherRoomId,
      roomToken: watchTogetherRoomToken,
      restart: true,
    });
  }, [
    contentId,
    fileId,
    libraryId,
    playbackController,
    watchTogetherConnection.room,
    watchTogetherConnection.replacementReason,
    watchTogetherRoomId,
    watchTogetherRoomToken,
  ]);

  useEffect(() => {
    if (!session.sessionId || !session.mediaFileId || session.loading || session.replacing) {
      return;
    }

    const activeVersion = playbackVersions.find(
      (version) => version.file_id === session.mediaFileId,
    );
    if (!activeVersion || (activeVersion.chapters?.length ?? 0) > 0) {
      return;
    }

    if (chapterRefreshAttemptsRef.current.has(session.mediaFileId)) {
      return;
    }
    chapterRefreshAttemptsRef.current.add(session.mediaFileId);

    // Force the read past the mounted query's stale window. A fresh cached
    // payload that still lacks chapters would otherwise be returned without a
    // network request, spending this file's single repair attempt for nothing.
    void queryClient.fetchQuery({
      queryKey: itemKeys.watchDetail(contentId, fileId, libraryId),
      queryFn: () => fetchWatchDetail(contentId, fileId, libraryId),
      staleTime: 0,
    });
  }, [
    contentId,
    fileId,
    libraryId,
    queryClient,
    session.loading,
    session.mediaFileId,
    session.replacing,
    session.sessionId,
    playbackVersions,
  ]);

  useEffect(() => {
    if (
      realtimeConnectionState !== "connected" ||
      !session.sessionId ||
      !session.mediaFileId ||
      session.loading ||
      session.replacing
    ) {
      return;
    }

    let cancelled = false;
    // Same key as the mounted `useWatchDetail` query so reconnecting does not
    // issue a second fetch of the payload that query already holds.
    void queryClient
      .fetchQuery({
        queryKey: itemKeys.watchDetail(contentId, fileId, libraryId),
        queryFn: () => fetchWatchDetail(contentId, fileId, libraryId),
        staleTime: WATCH_DETAIL_STALE_TIME_MS,
      })
      .then((detail) => {
        if (!cancelled) {
          setPlaybackVersions(detail.versions);
        }
      })
      .catch(() => {
        // Reconcile again on the next connection; keep the current markers meanwhile.
      });

    return () => {
      cancelled = true;
    };
  }, [
    contentId,
    fileId,
    libraryId,
    queryClient,
    realtimeConnectionState,
    session.loading,
    session.mediaFileId,
    session.replacing,
    session.sessionId,
  ]);

  const handleRealtimeEvent = useCallback(
    (event: PlaybackRealtimeEventEnvelope) => {
      if (event.name === "chapter_thumbnail_ready") {
        const { file_id, chapter_index, thumbnail_url, thumbnail_thumbhash } = event.payload;
        if (file_id !== session.mediaFileId) {
          return;
        }

        setPlaybackVersions((current) =>
          patchChapterThumbnail(
            current,
            file_id,
            chapter_index,
            thumbnail_url,
            thumbnail_thumbhash,
          ),
        );
        return;
      }

      if (event.name !== "markers_updated") {
        return;
      }

      const {
        file_id,
        intro: nextIntro,
        credits: nextCredits,
        recap: nextRecap,
        preview: nextPreview,
        marker_segments: nextSegments,
      } = event.payload;
      if (file_id !== session.mediaFileId) {
        return;
      }

      setPlaybackVersions((current) =>
        patchVersionMarkers(
          current,
          file_id,
          nextIntro,
          nextCredits,
          nextRecap,
          nextPreview,
          nextSegments,
        ),
      );
    },
    [session.mediaFileId],
  );

  // The server tells us whether a terminal is worth retrying. A retryable
  // virtual-source refusal gets a Try again action; every other terminal keeps
  // the plain Go Back dead-end.
  const canRetryTerminal =
    !session.plan && session.errorReason === "virtual_source_unavailable" && session.errorRetryable;
  const retryInFlight = session.retrying;

  // The plan is the player's contract: without one there is no transport, no
  // timeline and no track inventory to render against.
  if (!session.plan || !session.streamUrl || !session.sessionId) {
    if (session.loading || fallingBack) {
      return (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black">
          <div className="flex flex-col items-center gap-3">
            <div className="h-8 w-8 animate-spin rounded-full border-2 border-white/20 border-t-white" />
            <span className="text-sm text-white/60">
              {fallingBack ? "Finding a compatible version for everyone..." : "Loading player..."}
            </span>
          </div>
        </div>
      );
    }

    return (
      <div className="bg-background fixed inset-0 z-50 flex items-center justify-center px-6">
        <div className="surface-panel-subtle flex max-w-md flex-col items-center gap-4 rounded-[1.8rem] px-8 py-8 text-center">
          <div className="space-y-2">
            <p className="text-base font-semibold text-white">
              {session.errorTitle ?? "Playback unavailable"}
            </p>
            <p className="text-sm text-white/60">
              {session.error ?? "Vio could not start playback."}
            </p>
          </div>
          <div className="flex flex-col items-center gap-2">
            {canRetryTerminal ? (
              <button
                onClick={() => {
                  session.retryStart();
                }}
                type="button"
                disabled={retryInFlight}
                className="rounded-[0.95rem] bg-white px-4 py-2 text-sm font-semibold text-black transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-60"
              >
                Try again
              </button>
            ) : null}
            <button
              onClick={() => {
                void onExit();
              }}
              type="button"
              className="rounded-[0.95rem] bg-white/10 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-white/20"
            >
              Go Back
            </button>
          </div>
        </div>
      </div>
    );
  }

  // A version switch keeps the old stream playing while the replacement plan
  // is resolved (for virtual versions the server round trip can take seconds).
  // Surface that with a small non-blocking chip near the controls instead of
  // replacing the whole page — the viewer keeps watching the old stream.
  const switchingIndicator = session.replacing ? (
    <div
      role="status"
      aria-label="Switching version"
      className="pointer-events-none absolute top-[max(4.5rem,calc(env(safe-area-inset-top)+3.5rem))] left-1/2 z-50 -translate-x-1/2"
    >
      <div className="flex items-center gap-2 rounded-full border border-white/15 bg-black/70 px-3 py-1.5 text-xs font-medium text-white/80 shadow-lg backdrop-blur">
        <span className="h-3 w-3 animate-spin rounded-full border-2 border-white/25 border-t-white" />
        Switching version…
      </div>
    </div>
  ) : null;

  // The server may substitute a different version (e.g. HDR→SDR) when the
  // requested one is not playable on this device. Only the auto path allows
  // that, so surface a dismissible notice when it happened.
  //
  // A virtual requested row defeats the id comparison: the server collapses
  // `effective_media_file_id` onto the requested id and publishes the concrete
  // candidate as `effective_virtual_uri` instead. There the substitution is
  // visible only by comparing the published candidate's path against the
  // requested row's own path. When the requested row carries no path (older
  // responses) the comparison says nothing, so the notice stays quiet rather
  // than guess.
  const plan = session.plan;
  const requestedVersion =
    plan && playbackVersions.find((v) => v.file_id === plan.requested_media_file_id);
  const virtualSubstitution =
    !!plan?.effective_virtual_uri &&
    requestedVersion?.file_path !== undefined &&
    requestedVersion.file_path !== plan.effective_virtual_uri;
  const versionWasSubstituted =
    !!plan &&
    (plan.requested_media_file_id !== plan.effective_media_file_id || virtualSubstitution);
  // Name the row the plan actually landed on when we can resolve it, so the
  // notice says what is playing instead of only that something changed. The
  // effective row is resolved through the plan's own ids/path, not the
  // session's requested id, because the plan's effective id is the authority.
  const effectiveVersionRow = plan
    ? resolveEffectiveVersion(playbackVersions, {
        // A published virtual URI is the sole identity of the effective
        // candidate; the collapsed id names the neutral row, so falling back
        // to it would label the wrong row. Only fall back to the id for
        // ordinary files and older plans that publish no URI.
        mediaFileId: plan.effective_virtual_uri ? null : plan.effective_media_file_id,
        effectiveVirtualUri: plan.effective_virtual_uri ?? null,
      })
    : undefined;
  const effectiveVersionLabel = effectiveVersionRow
    ? buildEffectiveVersionLabel(effectiveVersionRow)
    : null;
  const substitutionCopy = effectiveVersionLabel
    ? `The selected version wasn't available, so Vio is playing ${effectiveVersionLabel} instead.`
    : "Playing a different version than selected — the requested version isn't playable on this device.";
  const versionSwapNotice =
    versionWasSubstituted && !explicitFileSelection && !versionSwapNoticeDismissed ? (
      <div className="absolute top-[max(4.5rem,calc(env(safe-area-inset-top)+3.5rem))] left-1/2 z-50 -translate-x-1/2">
        <div className="flex items-center gap-2 rounded-full border border-white/15 bg-black/70 px-3 py-1.5 text-xs font-medium text-white/80 shadow-lg backdrop-blur">
          <span>{substitutionCopy}</span>
          <button
            type="button"
            aria-label="Dismiss version notice"
            onClick={() => setVersionSwapNoticeDismissed(true)}
            className="cursor-pointer rounded-full px-1 text-white/60 transition-colors hover:text-white"
          >
            ✕
          </button>
        </div>
      </div>
    ) : null;

  // Find the duration of the selected file so the player knows the total
  // length even when the stream is chunked (no Content-Length header).
  const selectedDuration =
    session.durationSeconds ??
    playbackVersions.find((v) => v.file_id === session.mediaFileId)?.duration ??
    playbackVersions[0]?.duration;
  const selectedVersion = resolveEffectiveVersion(playbackVersions, session) ?? playbackVersions[0];
  const activeChapters =
    (playbackVersions.find((v) => v.file_id === session.mediaFileId) ?? selectedVersion)
      ?.chapters ?? [];
  const activeMarkers = resolveActiveVersionMarkers(selectedVersion);

  return (
    <>
      {switchingIndicator}
      {versionSwapNotice}
      <VideoPlayer
        title={title}
        year={year}
        streamUrl={session.streamUrl}
        plan={session.plan}
        planRevision={session.planRevision}
        transportRevision={session.transportRevision}
        shouldAutoPlay={session.shouldAutoPlay}
        replanning={session.replanning}
        replanningQuality={session.replanningQuality}
        pendingSwitchFileId={session.pendingSwitchFileId}
        replanError={fallingBack ? null : session.error}
        replanErrorTitle={session.errorTitle}
        sessionId={session.sessionId}
        selectedVersion={selectedVersion}
        versions={playbackVersions}
        activeFileId={session.mediaFileId}
        chapters={activeChapters}
        onSwitchVersion={watchTogetherRoomId ? undefined : handleSwitchVersion}
        onRefreshVersions={handleRefreshVersions}
        subtitleUrls={playableSubtitles}
        initialPosition={session.initialPosition}
        onQualitySelect={session.changeQuality}
        onSubtitleTrackChange={session.changeSubtitleTrack}
        onPlanFailure={session.recoverFromFailure}
        onPlanInvalidated={session.invalidatePlan}
        onReanchorSeek={session.reanchorSeek}
        onApplySubtitleTrack={session.applySubtitleTrack}
        preferredSubtitleLanguage={preferredSubtitleLanguage}
        preferredSubtitleTrackSignature={preferredSubtitleTrackSignature}
        subtitleMode={session.initialSubtitleError ? "off" : subtitleMode}
        showForcedSubtitles={session.initialSubtitleError ? false : showForcedSubtitles}
        profileLanguage={profileLanguage}
        intro={activeMarkers.intro}
        introSkipMode={introSkipMode}
        credits={activeMarkers.credits}
        recap={activeMarkers.recap}
        autoSkipRecap={autoSkipRecap}
        preview={activeMarkers.preview}
        markerSegments={selectedVersion?.marker_segments}
        autoPlayNextPreview={autoPlayNextPreview}
        canEditMarkers={canEditMarkers}
        onMarkersEdited={(fileId, markers) =>
          setPlaybackVersions((current) =>
            patchVersionMarkers(
              current,
              fileId,
              markers.intro,
              markers.credits,
              markers.recap,
              markers.preview,
            ),
          )
        }
        duration={selectedDuration}
        // The session's preference, not the caller's: the server normalizes what
        // was requested and the menu has to light up whatever it settled on.
        qualityPreference={session.qualityPreference}
        seriesContext={seriesContext}
        onNavigateEpisode={onNavigateEpisode}
        displayMode={displayMode}
        onPictureInPictureChange={onPictureInPictureChange}
        autoEnterPictureInPicture={autoEnterPictureInPicture}
        onPlaybackStateChange={handlePlaybackStateChange}
        onPlaybackTransportReady={onPlaybackTransportReady}
        onRealtimeEvent={handleRealtimeEvent}
        onRealtimeConnectionStateChange={setRealtimeConnectionState}
        onExit={onExit}
        onMinimize={onMinimize}
        onEnded={handleEnded}
        onRefreshSubtitles={session.refreshSubtitles}
        audioTracks={audioTracks}
        activeAudioIndex={session.audioTrackIndex}
        onAudioSelect={handleSwitchAudio}
        onSubtitleChanged={handleSubtitleChanged}
        onReturnFromPostRoll={onReturnFromPostRoll}
        watchTogetherRoomId={watchTogetherRoomId}
        watchTogetherConnection={watchTogetherConnection}
      />
    </>
  );
}
