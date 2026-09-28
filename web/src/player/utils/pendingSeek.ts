export interface PendingSeekResolution {
  currentTime: number;
  pendingSeekTime: number | null;
}

const DEFAULT_SEEK_SETTLE_TOLERANCE_SECONDS = 1;

/**
 * How long the scrubber may hold a requested position the media has not
 * reached. A seek the server declines (or the element clamps) never lands at
 * its target, so without a bound `resolvePendingSeekTime` would pin the
 * scrubber to a position the media is not at while playback continues. When
 * this elapses the Player rolls the scrubber back onto the element's real
 * position and clears the hold.
 */
export const PENDING_SEEK_HOLD_TIMEOUT_MS = 5_000;

export function resolvePendingSeekTime(
  actualTime: number,
  pendingSeekTime: number | null,
  toleranceSeconds = DEFAULT_SEEK_SETTLE_TOLERANCE_SECONDS,
): PendingSeekResolution {
  if (pendingSeekTime == null) {
    return { currentTime: actualTime, pendingSeekTime: null };
  }

  if (Math.abs(actualTime - pendingSeekTime) <= toleranceSeconds) {
    return { currentTime: actualTime, pendingSeekTime: null };
  }

  return { currentTime: pendingSeekTime, pendingSeekTime };
}
