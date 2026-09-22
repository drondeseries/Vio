import { normalizeSortCriteria, type SortCriterion } from "@/components/streaming/scoringPresets";

/**
 * Reads a quality profile's `sort` list out of the streaming settings payload
 * the scoring UI already edits. Returns [] for a missing/unparseable payload or
 * an unknown profile, which is exactly the server's default-order case.
 */
export function resolveProfileSortCriteria(
  settings: Record<string, string> | undefined,
  profileLabel?: string | null,
): SortCriterion[] {
  if (!settings || !profileLabel) return [];
  const raw = settings["virtual_library.quality_profiles"];
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const wanted = profileLabel.trim().toLowerCase();
    const match = parsed.find(
      (profile): profile is { label?: string; sort?: unknown } =>
        typeof profile === "object" &&
        profile !== null &&
        typeof (profile as { label?: unknown }).label === "string" &&
        (profile as { label: string }).label.trim().toLowerCase() === wanted,
    );
    return normalizeSortCriteria(match?.sort);
  } catch {
    return [];
  }
}
