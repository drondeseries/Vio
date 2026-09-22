import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  type MutableRefObject,
  type RefObject,
} from "react";
import { toMediaTime } from "../utils/mediaTimeline";
import type { WatchTogetherRoomConnectionResult } from "./useWatchTogetherRoomConnection";

interface UseWatchTogetherPlaybackSyncOptions {
  roomConnection: WatchTogetherRoomConnectionResult;
  sessionId?: string | null;
  videoRef: RefObject<HTMLVideoElement | null>;
  streamOriginRef: MutableRefObject<number>;
  appliedCommandIdRef: RefObject<string | null>;
}

interface TransportRequestResult {
  ok: boolean;
}

interface UseWatchTogetherPlaybackSyncResult {
  attachedSessionId: string | null;
  requestTransport: (
    action: "play" | "pause" | "seek",
    positionSeconds: number,
    isPaused: boolean,
  ) => TransportRequestResult;
  reportReady: () => TransportRequestResult;
  reportBuffering: (positionSeconds?: number, isPaused?: boolean) => TransportRequestResult;
}

const stateReportIntervalMs = 1_500;
// Retry readiness until the server acknowledges this member, so a lost or
// rejected acknowledgement heals quickly while another member may still load.
const waitingReportIntervalMs = 500;
const pendingCommandQuietPeriodMs = 250;
const readySeekToleranceSeconds = 1;
// The host's real position becomes the room anchor, so a rebuilt stream that
// lands short of the target does not hold the room. Mirrors the server bound.
const hostReadySeekToleranceSeconds = 15;
const bufferingGraceMs = 500;

type ReadyCheck =
  | { ok: true; commandId: string; positionSeconds: number; isPaused: boolean }
  | { ok: false; reason: string };

export function useWatchTogetherPlaybackSync({
  roomConnection,
  sessionId,
  videoRef,
  streamOriginRef,
  appliedCommandIdRef,
}: UseWatchTogetherPlaybackSyncOptions): UseWatchTogetherPlaybackSyncResult {
  const connectionState = roomConnection.connectionState;
  const room = roomConnection.room;
  const transportCommand = roomConnection.transportCommand;
  const serverTimeOffsetMs = roomConnection.serverTimeOffsetMs;
  const attachedSessionId = room?.attached_session_id ?? null;
  const roomConnected = room !== null;
  const roomPlaybackState = room?.playback_state ?? null;
  const roomPhase = room?.phase ?? null;
  const roomSelectionRevision = room?.selection_revision;
  const isHost = room?.self_role === "host";
  // The server clears readiness in its snapshot before each waiting command.
  const readinessAcknowledged =
    room?.members?.some((member) => member.is_self && member.is_ready) === true;
  const lastReadyRejectReasonRef = useRef<string | null>(null);
  const sendRoomMessage = roomConnection.sendRoomMessage;
  const waitingStateRef = useRef<"idle" | "buffering" | "ready">("idle");
  const bufferingTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const cancelBuffering = useCallback(() => {
    if (bufferingTimerRef.current !== null) {
      clearTimeout(bufferingTimerRef.current);
      bufferingTimerRef.current = null;
    }
  }, []);
  useEffect(() => {
    const video = videoRef.current;
    const recovered = () => {
      if (video && !video.seeking && video.readyState >= HTMLMediaElement.HAVE_FUTURE_DATA) {
        cancelBuffering();
        if (roomPlaybackState !== "waiting" && waitingStateRef.current === "buffering") {
          waitingStateRef.current = "idle";
        }
      }
    };
    video?.addEventListener("canplay", recovered);
    video?.addEventListener("canplaythrough", recovered);
    video?.addEventListener("loadeddata", recovered);
    video?.addEventListener("playing", recovered);
    video?.addEventListener("timeupdate", recovered);
    video?.addEventListener("seeked", recovered);
    return () => {
      cancelBuffering();
      video?.removeEventListener("canplay", recovered);
      video?.removeEventListener("canplaythrough", recovered);
      video?.removeEventListener("loadeddata", recovered);
      video?.removeEventListener("playing", recovered);
      video?.removeEventListener("timeupdate", recovered);
      video?.removeEventListener("seeked", recovered);
    };
  }, [
    cancelBuffering,
    connectionState,
    attachedSessionId,
    roomPhase,
    roomPlaybackState,
    room?.room_id,
    room?.selection_revision,
    sessionId,
    transportCommand?.command_id,
    videoRef,
  ]);

  useEffect(() => {
    waitingStateRef.current = "idle";
  }, [
    attachedSessionId,
    connectionState,
    roomPhase,
    readinessAcknowledged,
    room?.room_id,
    room?.playback_state,
    room?.selection_revision,
    sessionId,
    transportCommand?.command_id,
  ]);

  useEffect(() => {
    if (!sessionId || connectionState !== "connected") {
      return;
    }

    sendRoomMessage({ type: "attach_session", session_id: sessionId });
  }, [connectionState, sendRoomMessage, sessionId]);

  // Level-triggered readiness: anything that can acknowledge the waiting
  // command evaluates the same guards, so no single media event is the one
  // chance to leave the barrier.
  const checkReady = useCallback((): ReadyCheck => {
    const video = videoRef.current;
    const command = transportCommand;
    if (connectionState !== "connected" || !roomConnected) {
      return { ok: false, reason: "room not connected" };
    }
    if (!sessionId || attachedSessionId !== sessionId) {
      return { ok: false, reason: "playback session not attached" };
    }
    if (roomPlaybackState !== "waiting") {
      return { ok: false, reason: "room is not waiting" };
    }
    if (!command || command.playback_state !== "waiting") {
      return { ok: false, reason: "no waiting command" };
    }
    if (command.selection_revision !== roomSelectionRevision) {
      return { ok: false, reason: "command belongs to a previous selection" };
    }
    if (command.session_id && command.session_id !== sessionId) {
      return { ok: false, reason: "command targets another session" };
    }
    if (appliedCommandIdRef.current !== command.command_id) {
      return { ok: false, reason: "command not yet executed locally" };
    }
    if (!video) {
      return { ok: false, reason: "no media element" };
    }
    if (video.seeking) {
      return { ok: false, reason: "element still seeking" };
    }
    if (video.readyState < HTMLMediaElement.HAVE_FUTURE_DATA) {
      return { ok: false, reason: `element readyState ${video.readyState} < HAVE_FUTURE_DATA` };
    }
    const positionSeconds = Math.max(0, toMediaTime(video.currentTime, streamOriginRef.current));
    // A canplay event can still belong to the stream a room seek replaces.
    if (command.action === "seek") {
      const delta = Math.abs(positionSeconds - command.position_seconds);
      const tolerance = isHost ? hostReadySeekToleranceSeconds : readySeekToleranceSeconds;
      if (delta > tolerance) {
        return {
          ok: false,
          reason: `position ${positionSeconds.toFixed(2)}s is ${delta.toFixed(2)}s from seek target ${command.position_seconds.toFixed(2)}s`,
        };
      }
    }
    return { ok: true, commandId: command.command_id, positionSeconds, isPaused: video.paused };
  }, [
    appliedCommandIdRef,
    attachedSessionId,
    connectionState,
    isHost,
    roomConnected,
    roomPlaybackState,
    roomSelectionRevision,
    sessionId,
    streamOriginRef,
    transportCommand,
    videoRef,
  ]);

  const noteReadyReject = useCallback(
    (reason: string) => {
      if (lastReadyRejectReasonRef.current === reason) return;
      lastReadyRejectReasonRef.current = reason;
      console.debug(
        `[watch-together] not ready for command ${transportCommand?.command_id ?? "?"}: ${reason}`,
      );
    },
    [transportCommand?.command_id],
  );

  useEffect(() => {
    lastReadyRejectReasonRef.current = null;
  }, [transportCommand?.command_id, roomPlaybackState]);

  useEffect(() => {
    if (!sessionId || connectionState !== "connected") {
      return;
    }

    const retryReadiness = roomPlaybackState === "waiting" && !readinessAcknowledged;
    const intervalId = window.setInterval(
      () => {
        const video = videoRef.current;
        if (!video || attachedSessionId !== sessionId) {
          return;
        }
        if (transportCommand?.session_id === sessionId) {
          const localExecuteAt = Date.parse(transportCommand.execute_at) - serverTimeOffsetMs;
          if (
            Number.isFinite(localExecuteAt) &&
            localExecuteAt + pendingCommandQuietPeriodMs > Date.now()
          ) {
            return;
          }
        }

        if (retryReadiness) {
          const check = checkReady();
          if (check.ok) {
            sendRoomMessage({
              type: "state_report",
              session_id: sessionId,
              command_id: check.commandId,
              position_seconds: check.positionSeconds,
              is_paused: check.isPaused,
              is_ready: true,
            });
            return;
          }
          noteReadyReject(check.reason);
        }

        sendRoomMessage({
          type: "state_report",
          session_id: sessionId,
          position_seconds: toMediaTime(video.currentTime, streamOriginRef.current),
          is_paused: video.paused,
        });
      },
      retryReadiness ? waitingReportIntervalMs : stateReportIntervalMs,
    );

    return () => {
      window.clearInterval(intervalId);
    };
  }, [
    attachedSessionId,
    checkReady,
    connectionState,
    noteReadyReject,
    readinessAcknowledged,
    roomPlaybackState,
    sendRoomMessage,
    serverTimeOffsetMs,
    sessionId,
    streamOriginRef,
    transportCommand,
    videoRef,
  ]);

  const requestTransport = useCallback(
    (action: "play" | "pause" | "seek", positionSeconds: number, isPaused: boolean) => {
      if (
        connectionState !== "connected" ||
        !roomConnected ||
        !sessionId ||
        attachedSessionId !== sessionId
      ) {
        return { ok: false };
      }
      return sendRoomMessage({
        type: "transport_request",
        action,
        position_seconds: positionSeconds,
        is_paused: isPaused,
      });
    },
    [attachedSessionId, connectionState, roomConnected, sendRoomMessage, sessionId],
  );

  const reportReady = useCallback(() => {
    cancelBuffering();
    if (readinessAcknowledged || waitingStateRef.current === "ready") {
      return { ok: false };
    }
    const check = checkReady();
    if (!check.ok) {
      noteReadyReject(check.reason);
      return { ok: false };
    }
    const result = sendRoomMessage({
      type: "ready",
      command_id: check.commandId,
      session_id: sessionId,
      position_seconds: check.positionSeconds,
      is_paused: check.isPaused,
    });
    if (result.ok) {
      waitingStateRef.current = "ready";
    }
    return result;
  }, [
    cancelBuffering,
    checkReady,
    noteReadyReject,
    readinessAcknowledged,
    sendRoomMessage,
    sessionId,
  ]);

  const reportBuffering = useCallback(
    (positionSeconds?: number, isPaused?: boolean) => {
      const video = videoRef.current;
      if (
        connectionState !== "connected" ||
        !roomConnected ||
        !sessionId ||
        attachedSessionId !== sessionId ||
        roomPhase !== "playing" ||
        roomPlaybackState !== "playing" ||
        waitingStateRef.current === "buffering" ||
        !video
      ) {
        return { ok: false };
      }

      if (bufferingTimerRef.current !== null) return { ok: false };
      bufferingTimerRef.current = setTimeout(() => {
        bufferingTimerRef.current = null;
        // A stalled download can leave plenty of playable media buffered.
        if (!video.seeking && video.readyState >= HTMLMediaElement.HAVE_FUTURE_DATA) return;
        const result = sendRoomMessage({
          type: "buffering",
          session_id: sessionId,
          position_seconds: Math.max(
            0,
            positionSeconds ?? toMediaTime(video.currentTime, streamOriginRef.current),
          ),
          is_paused: isPaused ?? video.paused,
        });
        if (result.ok) waitingStateRef.current = "buffering";
      }, bufferingGraceMs);
      return { ok: true };
    },
    [
      attachedSessionId,
      connectionState,
      roomConnected,
      roomPhase,
      roomPlaybackState,
      sendRoomMessage,
      sessionId,
      streamOriginRef,
      videoRef,
    ],
  );

  // Stable identity so consumers (e.g. VideoPlayer's video-event-listener
  // effect) don't re-run on every room snapshot.
  return useMemo(
    () => ({
      attachedSessionId,
      requestTransport,
      reportReady,
      reportBuffering,
    }),
    [attachedSessionId, requestTransport, reportReady, reportBuffering],
  );
}
