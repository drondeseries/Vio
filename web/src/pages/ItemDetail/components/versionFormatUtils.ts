import type { VersionAudioTrack, VersionSubtitleTrack, VersionVideoTrack } from "@/api/types";
import { englishLanguageName, getLanguageName } from "@/lib/languageNames";
import {
  formatBitrate,
  formatChannels,
  formatFileSize,
  formatSampleRate,
  stripReleaseSizeToken,
} from "@/lib/mediaFormat";

/** Languages that carry no real identity and are skipped in summaries. */
const LANGUAGE_PLACEHOLDERS = new Set(["und", "unknown", "unk", ""]);

/**
 * Collects deduplicated, human-readable language labels from a version's
 * tracks. Release markers like MULTI/DUAL and ISO/BCP 47 codes both resolve
 * through formatLanguageName, so a virtual candidate advertising
 * ["MULTI", "FR", "eng"] summarizes to "Multi/French/English".
 */
export function collectLanguageLabels(
  languages: ReadonlyArray<string | undefined | null>,
): string[] {
  const seen = new Set<string>();
  const labels: string[] = [];
  for (const value of languages ?? []) {
    const language = value?.trim();
    if (!language || LANGUAGE_PLACEHOLDERS.has(language.toLowerCase())) continue;
    const label = formatLanguageName(language);
    // Deduplicate on the resolved display label: "eng", "en", and "English"
    // all render as "English", while distinct regions keep their qualifiers.
    const identity = label.toLowerCase();
    if (seen.has(identity)) continue;
    seen.add(identity);
    labels.push(label);
  }
  return labels;
}

/** Compact, deduplicated audio language list ("Multi/French") or null. */
export function audioLanguageSummary(tracks: VersionAudioTrack[] | undefined): string | null {
  const languages = tracks?.flatMap((track) =>
    track.languages?.length ? track.languages : [track.language],
  );
  const labels = collectLanguageLabels(languages ?? []);
  return labels.length > 0 ? labels.join("/") : null;
}

/** Compact, deduplicated subtitle language list ("English/French") or null. */
export function subtitleLanguageSummary(tracks: VersionSubtitleTrack[] | undefined): string | null {
  const labels = collectLanguageLabels(tracks?.map((track) => track.language) ?? []);
  return labels.length > 0 ? labels.join("/") : null;
}

/**
 * The quality-profile label a virtual candidate was ranked under, taken from
 * its `?profile=` selector. Null for local files and any URI without one (the
 * server then ranks under its default order).
 */
export function profileLabelFromFilePath(filePath?: string): string | null {
  if (!filePath) return null;
  try {
    const parsed = new URL(filePath, "http://silo.local");
    const label = parsed.searchParams.get("profile")?.trim();
    return label ? label : null;
  } catch {
    return null;
  }
}

/** True for zero-storage catalog entries backed by a virtual:// provider URI. */
export function isVirtualFileVersion(version: { container?: string; file_path?: string }): boolean {
  return (
    version.container === "virtual" ||
    Boolean(version.file_path?.toLowerCase().startsWith("virtual://"))
  );
}

/** Turns a release-style name ("Movie.2023.2160p.Remux-GRP") into a readable
 *  line ("Movie 2023 2160p Remux GRP"). */
export function prettifyReleaseName(releaseName?: string): string {
  if (!releaseName) return "";
  return releaseName.replace(/[._]+/g, " ").replace(/\s+/g, " ").trim();
}

/**
 * The release/size line a version row shows, shared by the item-page picker and
 * the in-player version menu so both carry the same information.
 *
 * `label` is the row's chosen release/file label; its embedded size is dropped
 * when the structured `fileSize` is known (the canonical value), and kept when
 * it is the only size evidence. `scanText` is scanned for a source hint
 * (Remux/WEB-DL/...). Bitrate and other attributes are untouched.
 */
export function formatVersionDetail({
  label,
  fileSize,
  scanText,
}: {
  label?: string;
  fileSize?: number;
  scanText?: string;
}): string {
  const parts: string[] = [];
  const size = formatFileSize(fileSize);
  const releaseName = prettifyReleaseName(
    size ? stripReleaseSizeToken(label ?? "") : (label ?? ""),
  );
  if (releaseName) parts.push(releaseName);
  if (size) parts.push(size);
  const hint = scanText ? extractSourceHint(scanText) : null;
  if (hint) parts.push(hint);
  return parts.join(" · ");
}

export function formatPageCount(pages?: number): string {
  if (!pages || pages <= 0) return "";
  return `${pages.toLocaleString()} ${pages === 1 ? "page" : "pages"}`;
}

export function formatAddedAt(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("en-US", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

export function formatLanguageName(language?: string): string {
  if (!language) return "";

  const trimmed = language.trim();
  const normalized = trimmed.toLowerCase();
  if (!normalized) return "";

  const standardizedName = englishLanguageName(trimmed);
  if (standardizedName) return standardizedName;

  if (normalized.length > 3) {
    return trimmed
      .split(/[\s_-]+/)
      .filter(Boolean)
      .map((part) => part[0]?.toUpperCase() + part.slice(1).toLowerCase())
      .join(" ");
  }
  return getLanguageName(trimmed);
}

const SOURCE_HINT_PATTERNS: Array<{ pattern: RegExp; canonical: string }> = [
  { pattern: /\bremux\b/i, canonical: "Remux" },
  { pattern: /\bweb-dl\b/i, canonical: "WEB-DL" },
  { pattern: /\bwebrip\b/i, canonical: "WEBRip" },
  { pattern: /\bbluray\b/i, canonical: "BluRay" },
  { pattern: /\bbdrip\b/i, canonical: "BDRip" },
  { pattern: /\bhdtv\b/i, canonical: "HDTV" },
  { pattern: /\bdvdrip\b/i, canonical: "DVDRip" },
];

export function extractSourceHint(fileName: string): string | null {
  for (const { pattern, canonical } of SOURCE_HINT_PATTERNS) {
    if (pattern.test(fileName)) return canonical;
  }
  return null;
}

export function metadataLine(parts: Array<string | undefined | false>): string {
  return parts.filter(Boolean).join(" \u00B7 ");
}

export function videoTitle(track: VersionVideoTrack): string {
  return (
    track.title ||
    [track.width && track.height ? `${track.width}x${track.height}` : "", track.codec]
      .filter(Boolean)
      .join(" ") ||
    "Video"
  );
}

export function audioTitle(track: VersionAudioTrack): string {
  return (
    track.title ||
    track.embedded_title ||
    [track.language, track.codec, formatChannels(track.channels)].filter(Boolean).join(" ") ||
    "Audio"
  );
}

export function subtitleTitle(track: VersionSubtitleTrack): string {
  const language = formatLanguageName(track.language);
  const title = track.title || track.embedded_title || track.codec || "Subtitle";

  const languageLower = language.toLowerCase();
  const titleLower = title.toLowerCase();

  if (language && title && !titleLower.includes(languageLower)) {
    return `${language} - ${title}`;
  }

  return title || language;
}

export function compactVideoMeta(track: VersionVideoTrack): string {
  return metadataLine([
    track.profile,
    track.aspect_ratio,
    track.frame_rate ? `${track.frame_rate} fps` : "",
    formatBitrate(track.bitrate),
    track.bit_depth ? `${track.bit_depth}-bit` : "",
    track.video_range,
    track.dolby_vision,
    track.color_space,
    track.interlaced ? "Interlaced" : "",
  ]);
}

export function compactAudioMeta(track: VersionAudioTrack): string {
  return metadataLine([
    track.layout,
    formatBitrate(track.bitrate),
    formatSampleRate(track.sample_rate),
    track.bit_depth ? `${track.bit_depth}-bit` : "",
  ]);
}

export function compactSubtitleMeta(track: VersionSubtitleTrack): string {
  return metadataLine([
    track.external ? "External" : "Embedded",
    track.forced ? "Forced" : "",
    track.hearing_impaired ? "HI" : "",
    track.default ? "Default" : "",
    track.resolution,
  ]);
}
