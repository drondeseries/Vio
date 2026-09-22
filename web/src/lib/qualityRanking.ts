import type { SortAttribute, SortCriterion } from "@/components/streaming/scoringPresets";
import { resolutionScore } from "@/pages/ItemDetail/components/versionRankingUtils";

/**
 * The sort attributes a viewer may pick, limited to what the delivered version
 * payload actually carries: format_score, file_size, bitrate, resolution, hdr,
 * audio_tracks[].channels and video_tracks[].bit_depth. `source`, `language`
 * and `confirmed` are internal ranking signals that never reach the client, so
 * they are deliberately not offered here.
 */
export const VERSION_SORT_ATTRIBUTES = [
  "size",
  "bitrate",
  "resolution",
  "audio_channels",
  "bit_depth",
  "hdr",
  "score",
] as const satisfies readonly SortAttribute[];

/** The server's ranking for a version list, as the payload describes it. */
export interface ServerVersionRanking {
  /** The quality-profile label the server ranked under, when it names one. */
  profileLabel: string | null;
  /** The profile's ordered sort criteria; empty means the server default order. */
  criteria: SortCriterion[];
  /** Where the ranking came from (e.g. "profile"); null when it was inferred. */
  source: string | null;
  /** True when the payload carried an explicit `virtual_ranking` block. */
  fromPayload: boolean;
}

/** The numeric view of a version used by the client-side display re-order. */
export interface VersionSortable {
  fileSize?: number;
  bitrate?: number;
  resolution?: string;
  hdr?: boolean;
  audioChannels?: number;
  bitDepth?: number;
  formatScore?: number;
}

/** The payload fields both FileVersion and PlayerFileVersion expose. */
export interface VersionSortSource {
  file_size?: number;
  bitrate?: number;
  resolution?: string;
  hdr?: boolean;
  audio_channels?: number;
  audio_tracks?: { channels?: number }[];
  video_tracks?: { bit_depth?: number }[];
  format_score?: number;
}

function maxNumber(values: readonly (number | undefined)[]): number | undefined {
  let max: number | undefined;
  for (const value of values) {
    if (value == null) continue;
    if (max == null || value > max) max = value;
  }
  return max;
}

/** Projects a catalog/watch version onto the values the sort control compares. */
export function versionSortableFromFile(version: VersionSortSource): VersionSortable {
  return {
    fileSize: version.file_size,
    bitrate: version.bitrate,
    resolution: version.resolution,
    hdr: version.hdr,
    audioChannels:
      version.audio_channels ?? maxNumber((version.audio_tracks ?? []).map((t) => t.channels)),
    bitDepth: maxNumber((version.video_tracks ?? []).map((t) => t.bit_depth)),
    formatScore: version.format_score,
  };
}

function positive(value?: number): number | null {
  return value != null && value > 0 ? value : null;
}

/**
 * The comparable value for one attribute, or null when the version does not
 * carry it. Unknown values sort last within their criterion regardless of
 * direction, matching the server's ranking semantics.
 */
export function versionSortAttributeValue(
  version: VersionSortable,
  attribute: SortAttribute,
): number | null {
  switch (attribute) {
    case "size":
      return positive(version.fileSize);
    case "bitrate":
      return positive(version.bitrate);
    case "resolution": {
      const score = resolutionScore(version.resolution ?? "");
      return score > 0 ? score : null;
    }
    case "audio_channels":
      return positive(version.audioChannels);
    case "bit_depth":
      return positive(version.bitDepth);
    case "hdr":
      return version.hdr == null ? null : version.hdr ? 1 : 0;
    case "score":
      return version.formatScore == null ? null : version.formatScore;
    default:
      return null;
  }
}

/**
 * Display-only ordering: criteria are applied top-down, unknown values fall
 * last, and equal versions keep their incoming order (stable). This never
 * touches what plays — the server's auto-pick is unchanged.
 */
export function sortVersionsByCriteria<T>(
  versions: readonly T[],
  criteria: readonly SortCriterion[],
  toSortable: (version: T) => VersionSortable,
): T[] {
  if (criteria.length === 0) return [...versions];
  return versions
    .map((version, index) => ({ version, index }))
    .sort((a, b) => {
      for (const criterion of criteria) {
        const left = versionSortAttributeValue(toSortable(a.version), criterion.attribute);
        const right = versionSortAttributeValue(toSortable(b.version), criterion.attribute);
        if (left === null && right === null) continue;
        if (left === null) return 1;
        if (right === null) return -1;
        if (left !== right) return criterion.direction === "asc" ? left - right : right - left;
      }
      return a.index - b.index;
    })
    .map((entry) => entry.version);
}
