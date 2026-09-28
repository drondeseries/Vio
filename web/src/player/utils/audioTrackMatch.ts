import { canonicalLanguageTag } from "@/lib/languageTags";
import type { TrackIdentityV3 } from "../protocol-v3";

/**
 * The audio fields shared by the plan's inventory (`AudioTrackV3`) and the
 * menu's track shape (`PlayerAudioTrack`). Both describe the same probed
 * streams; the plan's list is the server's authoritative inventory, and the
 * catalog row the poll resolved is what a probe repair can rewrite.
 */
export interface AudioInventoryTrack {
  language?: string;
  languages?: string[];
  title?: string;
  embedded_title?: string;
  codec?: string;
  layout?: string;
  channels?: number;
  /** Canonical per-track identity (`file:<id>:audio:<selection_index>`). */
  track_id?: string;
}

function normalize(value: string | undefined | null): string {
  return (value ?? "").trim().toLowerCase();
}

/**
 * The concrete languages a track names, canonicalized and sorted so two
 * spellings of the same language set compare equal. Undetermined/multiple
 * placeholders carry no language intent and are dropped.
 */
function languageSet(track: AudioInventoryTrack): string[] {
  const raw = track.languages?.length ? track.languages : track.language ? [track.language] : [];
  const set = new Set<string>();
  for (const code of raw) {
    const canonical = canonicalLanguageTag(code);
    if (!canonical || canonical === "und" || canonical === "mul") continue;
    set.add(canonical);
  }
  return [...set].sort();
}

function sameLanguages(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  const left = languageSet(a);
  const right = languageSet(b);
  if (left.length === 0 || left.length !== right.length) return false;
  return left.every((code, index) => code === right[index]);
}

function sameSignature(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  return (
    normalize(a.codec) === normalize(b.codec) &&
    normalize(a.layout) === normalize(b.layout) &&
    (a.channels ?? 0) === (b.channels ?? 0) &&
    normalize(a.title) === normalize(b.title) &&
    normalize(a.embedded_title) === normalize(b.embedded_title) &&
    sameLanguages(a, b)
  );
}

/** The sole index matching `predicate`, or null when none or several do. */
function uniqueMatch<T>(items: readonly T[], predicate: (item: T) => boolean): number | null {
  let found: number | null = null;
  for (let index = 0; index < items.length; index += 1) {
    if (!predicate(items[index]!)) continue;
    if (found !== null) return null;
    found = index;
  }
  return found;
}

function matchPlanTrack(
  planTracks: readonly AudioInventoryTrack[],
  picked: AudioInventoryTrack | undefined,
): AudioInventoryTrack | undefined {
  if (!picked) return undefined;
  const bySignature = uniqueMatch(planTracks, (track) => sameSignature(track, picked));
  if (bySignature !== null) return planTracks[bySignature];
  const byLanguage = uniqueMatch(planTracks, (track) => sameLanguages(track, picked));
  if (byLanguage !== null) return planTracks[byLanguage];
  return undefined;
}

/**
 * Resolves the identity to send for an explicit audio pick.
 *
 * The menu renders `displayed`, which `applyAudioInventory` may have replaced
 * with the probed catalog list. A probe repair can reorder the effective
 * file's tracks, so a bare position no longer names the picked language. Send
 * the plan inventory's canonical `track_id` for the picked track — matched by
 * stable signature, then by language family when the menu no longer shows the
 * plan's own list — and keep the client's own ordinal as the request's index
 * fallback for when the identity cannot be named.
 */
export function resolvePlanAudioIdentity(
  planTracks: readonly AudioInventoryTrack[],
  displayed: readonly AudioInventoryTrack[],
  clientIndex: number,
): TrackIdentityV3 {
  const planTrack =
    displayed === planTracks
      ? planTracks[clientIndex]
      : matchPlanTrack(planTracks, displayed[clientIndex]);
  return { id: planTrack?.track_id?.trim() ?? "", index: clientIndex };
}
