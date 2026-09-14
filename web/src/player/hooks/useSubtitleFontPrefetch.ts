import { useEffect, useMemo } from "react";
import type { PlayerSubtitleInfo } from "../types";
import { loadSubtitleFontBundle } from "../utils/subtitleFonts";

/**
 * Prefetches ASS font bundles when the plan is adopted or refreshed. Selection
 * then hits the in-memory font cache instead of waiting on a cold server
 * extraction. Purely a warm-up: errors are swallowed and never affect playback.
 */
export function useSubtitleFontPrefetch(subtitleUrls: PlayerSubtitleInfo[]) {
  const fontUrls = useMemo(
    () => [
      ...new Set(subtitleUrls.filter((s) => s.font_bundle_url).map((s) => s.font_bundle_url!)),
    ],
    [subtitleUrls],
  );
  const key = fontUrls.join("|");
  useEffect(() => {
    if (!key) return;
    for (const url of fontUrls) {
      void loadSubtitleFontBundle(url).catch(() => {});
    }
  }, [key, fontUrls]);
}
