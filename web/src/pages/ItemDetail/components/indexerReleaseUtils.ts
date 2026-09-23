import type { WatchIndexerRelease } from "@/api/types";
import { formatFileSize, mapAudioLabel } from "@/lib/mediaFormat";
import { videoRangeLabel } from "@/lib/videoRange";
import { prettifyReleaseName, sanitizeVersionLabel } from "./versionFormatUtils";

/**
 * The display title for an indexer release: the release name with provider
 * plumbing stripped and release dots spaced out, matching how a playable
 * version's identity is sanitized. Falls back to a neutral label so a row is
 * never blank.
 */
export function indexerReleaseTitle(release: WatchIndexerRelease): string {
  const label = prettifyReleaseName(sanitizeVersionLabel(release.title));
  if (label) return label;
  return prettifyReleaseName(sanitizeVersionLabel(release.resolution)) || "Indexer release";
}

/**
 * The compact metadata line under an indexer row: resolution, codecs, dynamic
 * range, size and the indexer it came from, using the same formatters as the
 * playable version rows. Empty segments are dropped.
 */
export function indexerReleaseMeta(release: WatchIndexerRelease): string {
  const rangeLabel = videoRangeLabel({ hdr: release.hdr });
  return [
    release.resolution,
    release.codec_video ? release.codec_video.toUpperCase() : "",
    rangeLabel,
    release.codec_audio ? mapAudioLabel(release.codec_audio) : "",
    formatFileSize(release.size_bytes),
    release.indexer,
  ]
    .filter(Boolean)
    .join(" · ");
}
