import type { PlayerSubtitleInfo } from "../types";

function hasPlayableUrl(track: PlayerSubtitleInfo): boolean {
  return track.url.trim().length > 0;
}

/**
 * A track from the plan's inventory is selectable when the client can fetch it
 * *or* when the server publishes it as `burn_in_only` — the latter has no URL
 * on purpose, because selecting it asks the server to composite it into the
 * video instead of handing over a sidecar.
 */
function isSelectableSessionTrack(track: PlayerSubtitleInfo): boolean {
  return hasPlayableUrl(track) || track.burn_in_only === true;
}

/**
 * Whether the plan's own inventory offers anything the menu can render. A
 * published-but-unselectable entry (no URL and not `burn_in_only`) does not
 * count, so callers that gate on "the menu is still empty" keep looking for
 * the probe's real inventory instead of treating the placeholder as complete.
 */
export function hasSelectableSessionSubtitles(sessionTracks: PlayerSubtitleInfo[]): boolean {
  return sessionTracks.some(isSelectableSessionTrack);
}

export function resolvePlayableSubtitles(
  sessionTracks: PlayerSubtitleInfo[],
  fallbackTracks: PlayerSubtitleInfo[],
): PlayerSubtitleInfo[] {
  const selectableSessionTracks = sessionTracks.filter(isSelectableSessionTrack);
  if (selectableSessionTracks.length > 0) {
    return selectableSessionTracks;
  }
  // The watch-detail fallback is not an inventory: it carries no delivery
  // information, so a track without a URL there is simply unplayable.
  return fallbackTracks.filter(hasPlayableUrl);
}

/**
 * The stable identity fields of one subtitle track, independent of the dense
 * combined ordinal the server assigns it. A replan that re-mints the inventory
 * (or the server resolving the selection to a different ordinal for the same
 * asset) changes `index` but leaves these fields alone.
 */
export interface SubtitleTrackIdentity {
  /** Combined ordinal the client echoes back on a track change. */
  index: number | null;
  /** Server-assigned asset identity (`track_id`). */
  trackId?: string | null;
  language?: string | null;
  codec?: string | null;
  forced?: boolean;
  hearingImpaired?: boolean;
  /** True when the server can only deliver this track by burning it in. */
  burnInOnly?: boolean;
}

function normalizeIdentityValue(value: string | null | undefined): string {
  return (value ?? "").trim().toLowerCase();
}

function subtitleDescriptorKey(identity: SubtitleTrackIdentity): string {
  return [
    normalizeIdentityValue(identity.language),
    normalizeIdentityValue(identity.codec),
    identity.forced ? "forced" : "",
    identity.hearingImpaired ? "hearing_impaired" : "",
  ].join("|");
}

/**
 * Stable key for a logical subtitle track, for deduping a pending selection
 * across plans. The server's `track_id` is the authoritative asset identity
 * when present; the descriptor tuple is the fallback for rows that do not
 * carry one.
 */
export function subtitleTrackIdentityKey(identity: SubtitleTrackIdentity): string {
  const trackId = normalizeIdentityValue(identity.trackId);
  if (trackId) return `id:${trackId}`;
  return `desc:${subtitleDescriptorKey(identity)}`;
}

/**
 * True when two descriptors name the same logical subtitle asset. Comparing
 * `track_id` first keeps a selection settled across a re-minted or reordered
 * inventory where the combined ordinal changed but the asset did not.
 */
export function isSameSubtitleTrack(a: SubtitleTrackIdentity, b: SubtitleTrackIdentity): boolean {
  const aTrackId = normalizeIdentityValue(a.trackId);
  const bTrackId = normalizeIdentityValue(b.trackId);
  if (aTrackId && bTrackId) return aTrackId === bTrackId;
  return subtitleDescriptorKey(a) === subtitleDescriptorKey(b);
}

/**
 * Returns the selection that must be sent to settle the plan's authoritative
 * selected_tracks state, or undefined when the current plan already matches
 * the UI. Sidecar selections are included: the server owns durable track
 * intent even when the browser renders the selected artifact itself.
 *
 * The comparison is by stable track identity, not by ordinal: a replan that
 * re-mints the inventory (or resolves the same asset to a different combined
 * index) must not restart the request. A plan that resolved the selection to
 * `off`/absent is treated as settled by the caller's per-identity request
 * guard, so one user action produces at most one request until the plan
 * acknowledges it.
 */
export function pendingServerSubtitleSelection(
  planSelected: SubtitleTrackIdentity | null,
  active: SubtitleTrackIdentity | null,
): number | null | undefined {
  // Nothing selected in the UI. Asking the server to clear a selection it does
  // not have would replan on every plan; only an outstanding server selection
  // needs an explicit `null`.
  if (active === null) {
    if (planSelected === null || planSelected.index === null) return undefined;
    return null;
  }

  // The plan already carries the requested logical track. Its ordinal may
  // differ from the UI index after a re-mint or reorder; that is not a reason
  // to replan again.
  if (planSelected !== null && isSameSubtitleTrack(planSelected, active)) {
    return undefined;
  }

  // The plan does not carry this asset yet: request it at its current ordinal.
  // For a bitmap `burn_in_only` track this is the only way to ask the server
  // to composite it; for a text sidecar the caller's identity guard keeps this
  // to a single request until the plan acknowledges the selection.
  return active.index;
}
