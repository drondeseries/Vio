/**
 * MSE buffer policy for the HLS transport and the buffer-first seek path.
 *
 * HLS delivery here is a growing manifest: the server remuxes or transcodes
 * just ahead of the playhead, and the client's fetch position drives that
 * production. The element's SourceBuffer therefore balances three concerns at
 * once:
 *
 *  - Backward retention. A short skip back must land on bytes the element
 *    already holds, so it stays a local seek instead of a seek-reanchor
 *    replan. The window behind the playhead is bounded on both ends: at least
 *    `MIN_BACK_BUFFER_SECONDS` so a backward skip stays local, and at most
 *    `MAX_BACK_BUFFER_SECONDS` so a long session cannot grow the SourceBuffer
 *    without limit.
 *  - Forward prefetch. While the element is playing and not seeking, hls.js
 *    keeps the next `HLS_PREFETCH_SECONDS` past the produced head in flight,
 *    so a forward skip that lands just beyond the buffer starts from media
 *    already loading rather than stalling on a cold request.
 *  - Throttler cooperation. The server pauses FFmpeg when the client's fetch
 *    position falls a configured gap behind the produced head and resumes when
 *    the client catches up. Prefetching toward the produced head feeds that
 *    loop; pulling far past it only wastes memory, so the forward target stays
 *    in the same 60-120s band as the retention window.
 *
 * hls.js evicts the back buffer itself with a SourceBuffer `remove()` once
 * playback crosses a segment boundary (`backBufferLength`), so eviction never
 * tears down the MediaSource or reloads the element.
 */

/** Sources at or above this bitrate get the shorter forward buffer target. */
export const HIGH_BITRATE_KBPS = 25_000;
/** Forward buffer target for high-bitrate sources. */
export const HIGH_BITRATE_FORWARD_BUFFER_SECONDS = 60;
/** Forward buffer target for everything else. */
export const DEFAULT_FORWARD_BUFFER_SECONDS = 120;
/** Back-buffer retention floor: a backward skip inside it stays a local seek. */
export const MIN_BACK_BUFFER_SECONDS = 60;
/** Back-buffer retention ceiling: bounds SourceBuffer growth on a long session. */
export const MAX_BACK_BUFFER_SECONDS = 120;
/** Seconds past the buffered edge prefetch keeps in flight while playing. */
export const HLS_PREFETCH_SECONDS = 30;
/** hls.js's default `maxBufferSize`, in bytes. */
export const HLS_DEFAULT_MAX_BUFFER_SIZE_BYTES = 60 * 1000 * 1000;

export interface HlsBufferPolicy {
  /** `maxBufferLength`/`maxMaxBufferLength`: forward buffer target in seconds. */
  forwardBufferSeconds: number;
  /** `backBufferLength`: retained seconds behind the playhead, in [60, 120]. */
  backBufferSeconds: number;
  /** Seconds past the buffered edge prefetch keeps in flight while stable. */
  prefetchSeconds: number;
  /** Source bitrate the byte target is sized from; 0 when unknown. */
  bitrateKbps: number;
}

/** Clamps an hls.js back-buffer request into the retention window. */
export function clampBackBufferSeconds(seconds: number): number {
  if (!Number.isFinite(seconds)) return MAX_BACK_BUFFER_SECONDS;
  return Math.min(MAX_BACK_BUFFER_SECONDS, Math.max(MIN_BACK_BUFFER_SECONDS, seconds));
}

/** Resolves the hls.js buffer settings for a source's bitrate. */
export function hlsBufferPolicy(bitrateKbps: number): HlsBufferPolicy {
  const safeBitrateKbps = Number.isFinite(bitrateKbps) && bitrateKbps > 0 ? bitrateKbps : 0;
  const forwardBufferSeconds =
    safeBitrateKbps >= HIGH_BITRATE_KBPS
      ? HIGH_BITRATE_FORWARD_BUFFER_SECONDS
      : DEFAULT_FORWARD_BUFFER_SECONDS;
  return {
    forwardBufferSeconds,
    backBufferSeconds: clampBackBufferSeconds(forwardBufferSeconds),
    prefetchSeconds: Math.min(HLS_PREFETCH_SECONDS, forwardBufferSeconds),
    bitrateKbps: safeBitrateKbps,
  };
}

/**
 * The `maxBufferSize` byte ceiling that leaves `seconds` in control of hls.js's
 * forward buffer. hls.js bounds loading at
 *
 *   min(max((8 * maxBufferSize) / levelBitrate, maxBufferLength), maxMaxBufferLength)
 *
 * so its default 60 MB byte target lets the buffer grow to roughly 60s at
 * 8 Mbps even when `maxBufferLength` asks for a shorter paused/seeking window.
 * Sizing the byte target to the window at the source bitrate stops the byte
 * target from raising the limit past `seconds`. A zero or unknown bitrate
 * returns 0, which cannot raise the limit and leaves the time target governing.
 */
export function maxBufferSizeBytes(seconds: number, bitrateKbps: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return 0;
  if (!Number.isFinite(bitrateKbps) || bitrateKbps <= 0) return 0;
  return Math.ceil((seconds * bitrateKbps * 1000) / 8);
}

/**
 * The lowest media time still retained behind `playheadSeconds`. Buffered bytes
 * below this floor are evicted by hls.js's back-buffer trim, so a target under
 * it is no longer in the element and cannot be a local seek.
 */
export function bufferRetentionFloor(playheadSeconds: number, backBufferSeconds: number): number {
  if (!Number.isFinite(playheadSeconds) || playheadSeconds <= 0) return 0;
  return Math.max(0, playheadSeconds - clampBackBufferSeconds(backBufferSeconds));
}

/**
 * The forward buffer target hls.js should hold for the element's transport
 * state. While playing and not seeking the playhead is heading somewhere, so
 * the client fills toward the produced head; a paused or seeking element is
 * not, so the target drops to the prefetch window and the server's throttler
 * is left alone until playback is stable again.
 */
export function prefetchBufferTargetSeconds(state: {
  playing: boolean;
  seeking: boolean;
  forwardBufferSeconds: number;
  prefetchSeconds: number;
}): number {
  if (state.playing && !state.seeking) return state.forwardBufferSeconds;
  return Math.min(state.prefetchSeconds, state.forwardBufferSeconds);
}

/**
 * Whether `nativeSeconds` lies inside a buffered range: any target inside a
 * buffered range seeks locally.
 *
 * The plan's timeline says what the server *can* serve; the element's buffer
 * says what it already has. Buffered bytes are playable without any server
 * interaction, so a target that is already buffered — at any distance from the
 * current playhead — must not be handed to the reanchor path just because the
 * plan reports `can_seek_anywhere=false` or the growing manifest has not
 * published the target yet. The range is half-open: `buffered.start(i)` is
 * inside and `buffered.end(i)` is not, so landing exactly on a buffered edge
 * still reanchors rather than stalling on the next missing chunk.
 */
export function isTimeBuffered(video: HTMLVideoElement, nativeSeconds: number): boolean {
  const buffered = video.buffered;
  for (let i = 0; i < buffered.length; i++) {
    if (nativeSeconds >= buffered.start(i) && buffered.end(i) > nativeSeconds) {
      return true;
    }
  }
  return false;
}
