import type { WatchIndexerRelease } from "@/api/types";

/**
 * Normalizes the additive `indexer_releases` array the watch detail and the
 * refresh answer both carry.
 *
 * The array is absent or empty when there is nothing to request, and never
 * null. An unknown `download_state` is treated as `not_downloaded`; a
 * malformed element without an id or title is dropped rather than rendered as
 * a broken row.
 */
export function indexerReleasesFromWire(value: unknown): WatchIndexerRelease[] {
  if (!Array.isArray(value)) return [];
  const releases: WatchIndexerRelease[] = [];
  for (const entry of value) {
    if (!entry || typeof entry !== "object") continue;
    const raw = entry as Record<string, unknown>;
    const releaseId = optionalString(raw.release_id);
    const title = optionalString(raw.title);
    if (!releaseId || !title) continue;
    releases.push({
      release_id: releaseId,
      title,
      resolution: optionalString(raw.resolution),
      codec_video: optionalString(raw.codec_video),
      codec_audio: optionalString(raw.codec_audio),
      hdr: raw.hdr === true,
      size_bytes: typeof raw.size_bytes === "number" ? raw.size_bytes : undefined,
      indexer: optionalString(raw.indexer),
      published_at: optionalString(raw.published_at),
      format_score: typeof raw.format_score === "number" ? raw.format_score : undefined,
      protocol: optionalString(raw.protocol),
      download_state:
        raw.download_state === "queued" || raw.download_state === "failed"
          ? raw.download_state
          : "not_downloaded",
    });
  }
  return releases;
}

function optionalString(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed ? trimmed : undefined;
}
