import { useEffect, useRef } from "react";
import type JASSUB from "jassub";
import type { PlayerSubtitleInfo } from "../types";
import { isASSCodec } from "../utils/subtitleCodecs";
import { isSubtitleSourceChanged } from "../utils/subtitleSourceChanged";
import {
  fallbackFontForSubtitle,
  forceASSFontFamily,
  loadSubtitleFontBundle,
  loadSubtitleFallbackFontData,
} from "../utils/subtitleFonts";
// Liberation Sans (SIL OFL 1.1; license colocated as liberation-sans.LICENSE),
// the font JASSUB uses as its built-in Latin default, taken verbatim from
// jassub@2.4.2's dist/default.woff2. Vendored because jassub >= 2.5.4 still
// references that file but no longer ships it in the npm package, which would
// leave libass with no usable default font (queryFonts is disabled) and
// silently render nothing.
import liberationSansUrl from "../assets/liberation-sans.woff2?url";

// A font bundle extraction (up to 32MiB, server-side, over network-backed or
// virtual storage) is allowed this much time before JASSUB is constructed
// without it. Subtitles render with the fallback/default fonts meanwhile; the
// losing fetch keeps running in the background and lands in the shared
// fontBundleCache, so a later re-select or the prefetch path picks up the real
// fonts. The subtitle TEXT is never gated by this budget.
const FONT_BUNDLE_BUDGET_MS = 3000;

/**
 * Fetches an ASS font bundle but abandons it at a time budget. On a budget miss
 * the in-flight fetch continues in the background (warming the shared cache)
 * and this resolves with no attached fonts so subtitle appearance is never
 * delayed. Errors degrade to [] exactly like the old parallel Promise.all.
 */
async function loadSubtitleFontBundleWithinBudget(
  url: string,
  signal: AbortSignal,
  onSourceChanged?: () => void,
): Promise<Uint8Array[]> {
  const fontPromise = loadSubtitleFontBundle(url, signal, onSourceChanged);
  let budgetTimer: ReturnType<typeof setTimeout> | null = null;
  const budgetMiss = new Promise<Uint8Array[]>((resolve) => {
    budgetTimer = setTimeout(() => resolve([]), FONT_BUNDLE_BUDGET_MS);
  });
  try {
    const fonts = await Promise.race([fontPromise, budgetMiss]);
    // The race is settled on the font result; never leave the budget timer
    // dangling to fire into a settled pipeline.
    if (budgetTimer !== null) clearTimeout(budgetTimer);
    return fonts;
  } catch (err) {
    if (budgetTimer !== null) clearTimeout(budgetTimer);
    if ((err as Error).name !== "AbortError") {
      console.error(`[useASSSubtitles] Failed to load subtitle font bundle ${url}:`, err);
    }
    return [];
  }
}

/**
 * Manages client-side ASS/SSA subtitle rendering via JASSUB (libass WASM).
 *
 * When an ASS-codec subtitle track is active, this hook lazy-loads JASSUB,
 * creates an instance attached to the video element, and renders styled
 * subtitles onto a canvas overlay. When a non-ASS track is selected (or
 * subtitles are turned off), the JASSUB instance is destroyed.
 *
 * The existing VTT subtitle pipeline (useSubtitleTracks) handles SRT/VTT;
 * this hook handles ASS/SSA. The two are coordinated by the `isActive`
 * return value — when true, the VTT overlay should be suppressed.
 */
export function useASSSubtitles(
  videoRef: React.RefObject<HTMLVideoElement | null>,
  subtitleUrls: PlayerSubtitleInfo[],
  activeSubtitleIndex: number | null,
  isDetached: boolean,
  streamOriginSeconds: number,
  subtitleDelayMs: number,
  onLoadState?: (state: "idle" | "loading" | "ready" | "error") => void,
  // Fired when the server answers the subtitle fetch with
  // `subtitle_source_changed` (409): a virtual release rotated under this plan
  // and every URL for the active track is stale. The caller must refresh the
  // plan's subtitle inventory; retrying the same URL can never succeed.
  onSourceChanged?: () => void,
  // Plan identity: forces the ASS effect to re-run on a new plan even when
  // the subtitle URL is coincidentally identical (e.g. a rotation replan
  // that re-mints the same effective file/track URL).
  planId?: string,
): { isActive: boolean } {
  const onLoadStateRef = useRef(onLoadState);
  onLoadStateRef.current = onLoadState;
  const onSourceChangedRef = useRef(onSourceChanged);
  onSourceChangedRef.current = onSourceChanged;
  const jassubRef = useRef<JASSUB | null>(null);
  const jassubImportRef = useRef<Promise<typeof JASSUB> | null>(null);
  // Effective JASSUB time offset. JASSUB renders the ASS event matching
  // `video.currentTime + timeOffset`, so an event at source time S appears
  // at video time S - timeOffset. `streamOriginSeconds` accounts for HLS
  // PTS rebasing; the user-facing delay (ms → s) must be SUBTRACTED so that
  // positive delay = subtitles shown later, matching the VTT path's
  // `start - origin + delay` cue shift.
  const effectiveOffset = streamOriginSeconds - subtitleDelayMs / 1000;
  const streamOriginRef = useRef(effectiveOffset);
  streamOriginRef.current = effectiveOffset;

  // Resolve the active subtitle track.
  const activeSub =
    activeSubtitleIndex !== null
      ? (subtitleUrls.find((s) => s.index === activeSubtitleIndex) ?? null)
      : null;

  const isASS = activeSub !== null && isASSCodec(activeSub.codec);
  const activeUrl = isASS ? activeSub.url : null;
  const activeLanguage = isASS ? activeSub.language : "";
  const activeFontBundleUrl = isASS ? activeSub.font_bundle_url : undefined;

  // Main effect: create/destroy JASSUB based on active track.
  useEffect(() => {
    const video = videoRef.current;
    onLoadStateRef.current?.("idle");

    // Destroy JASSUB if the active track is not ASS, or player is detached,
    // or no video element is available.
    if (!activeUrl || !video || isDetached) {
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
      return;
    }

    let cancelled = false;
    let controller = new AbortController();
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let timeout: ReturnType<typeof setTimeout> | null = null;
    // Set when the subtitle fetch answers 409 subtitle_source_changed: the
    // source rotated under this plan, so the outer retry must not re-run the
    // whole pipeline against the same stale URL.
    let sourceChangedSignaled = false;

    async function initJASSUB(signal: AbortSignal, progress: () => void) {
      if (!video || cancelled) return;
      onLoadStateRef.current?.("loading");

      // Lazy-load JASSUB module (only once).
      if (!jassubImportRef.current) {
        jassubImportRef.current = import("jassub")
          .then((m) => m.default)
          .catch((err) => {
            jassubImportRef.current = null;
            throw err;
          });
      }

      const classPromise = jassubImportRef.current;
      void classPromise.catch(() => {});

      let subContent: string;
      let attachedFontData: Uint8Array[] = [];
      try {
        subContent = await fetch(activeUrl!, { signal }).then(async (response) => {
          if (!response.ok) {
            if (await isSubtitleSourceChanged(response)) {
              sourceChangedSignaled = true;
              onSourceChangedRef.current?.();
              throw new DOMException("Subtitle source changed", "AbortError"); // bypass the retry
            }
            throw new Error(`HTTP ${response.status}`);
          }
          progress();
          if (!response.body) return response.text();
          const reader = response.body.getReader();
          const decoder = new TextDecoder();
          let text = "";
          while (!signal.aborted && !cancelled) {
            const { value, done } = await reader.read();
            if (done) return text + decoder.decode();
            progress();
            text += decoder.decode(value, { stream: true });
          }
          throw new DOMException("Subtitle loading cancelled", "AbortError");
        });
        // The subtitle TEXT drives the pipeline (and the stall watchdog); the
        // font bundle is raced against a budget so a slow extraction never
        // delays subtitle appearance. JASSUB renders with fallback fonts
        // meanwhile, and the budget-losing fetch continues in the background.
        if (activeFontBundleUrl) {
          attachedFontData = await loadSubtitleFontBundleWithinBudget(
            activeFontBundleUrl,
            signal,
            onSourceChangedRef.current ?? undefined,
          );
        }
      } catch (err) {
        if (!cancelled && (err as Error).name !== "AbortError") {
          console.error(`[useASSSubtitles] Failed to fetch ${activeUrl}:`, err);
        }
        throw err;
      }

      if (cancelled || signal.aborted) return;

      // libass renders missing glyphs with its *default* font — it does not
      // search other loaded fonts for coverage. JASSUB's built-in default
      // (Liberation Sans) lacks many non-Latin glyphs, so for those scripts we
      // point `defaultFont` at a font that covers them, chosen by track metadata
      // first and subtitle text as a fallback. Each track switch destroys and
      // rebuilds the instance, so this stays in sync per track.
      const fallbackFont = fallbackFontForSubtitle(activeLanguage, subContent);

      let fallbackFontData: Uint8Array[] | null = null;
      if (fallbackFont) {
        try {
          fallbackFontData = await loadSubtitleFallbackFontData(fallbackFont);
        } catch (err) {
          if (!cancelled) {
            console.error(
              `[useASSSubtitles] Failed to load fallback font ${fallbackFont.family}:`,
              err,
            );
          }
        }
      }

      if (cancelled) return;

      const renderedSubContent =
        fallbackFont && fallbackFontData
          ? forceASSFontFamily(subContent, fallbackFont.family)
          : subContent;
      const fonts = [...attachedFontData, ...(fallbackFontData ?? [])];

      const JASSUBClass = await classPromise;
      if (cancelled || signal.aborted) return;
      const instance = new JASSUBClass({
        video,
        subContent: renderedSubContent,
        timeOffset: streamOriginRef.current,
        // The browser Local Font Access API is inconsistent and permissioned.
        // Letting JASSUB probe it produces noisy console warnings for common ASS
        // style fonts without making playback reliable across clients.
        queryFonts: false,
        availableFonts: { "liberation sans": liberationSansUrl },
        ...(fonts.length > 0
          ? {
              fonts,
              ...(fallbackFont && { defaultFont: fallbackFont.family }),
            }
          : {}),
      });

      // Guard against the effect being cleaned up while the constructor ran.
      if (cancelled) {
        instance.destroy();
        return;
      }

      jassubRef.current = instance;
      await instance.ready;
      if (!cancelled && !signal.aborted) onLoadStateRef.current?.("ready");
    }

    async function load() {
      controller = new AbortController();
      const attemptController = controller;
      let progress = () => {};
      const stalled = new Promise<never>((_, reject) => {
        progress = () => {
          if (cancelled || attemptController.signal.aborted) return;
          if (timeout !== null) clearTimeout(timeout);
          timeout = setTimeout(() => {
            attemptController.abort();
            reject(new Error("Subtitle loading stalled"));
          }, 30_000);
        };
        progress();
      });
      try {
        await Promise.race([initJASSUB(controller.signal, progress), stalled]);
      } catch (err) {
        if (cancelled) return;
        attemptController.abort();
        console.error("[useASSSubtitles] Unable to load subtitles:", err);
        jassubRef.current?.destroy();
        jassubRef.current = null;
        onLoadStateRef.current?.("error");
        // A signaled source change must not re-run the pipeline against the
        // same stale URL; the rebuilt JASSUB comes from the main effect
        // re-running when the refresh adopts a new plan and activeUrl changes.
        if (sourceChangedSignaled) return;
        retryTimer = setTimeout(() => void load(), 5_000);
      } finally {
        if (timeout !== null) clearTimeout(timeout);
      }
    }
    void load();

    return () => {
      cancelled = true;
      if (retryTimer !== null) clearTimeout(retryTimer);
      if (timeout !== null) clearTimeout(timeout);
      controller.abort();
      // Destroy the current instance if the effect is being torn down
      // (e.g. track switch or unmount). This covers the common case where
      // initJASSUB has already completed and stored the instance.
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
    };
    // videoRef is a stable ref object. streamOriginSeconds is read from
    // streamOriginRef inside the async function to always get the latest value.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeUrl, activeLanguage, activeFontBundleUrl, isDetached, planId]);

  // Update JASSUB's time offset when either the media timeline remaps or
  // the user nudges subtitle sync. Avoids destroying and recreating the
  // instance for offset-only changes.
  useEffect(() => {
    const instance = jassubRef.current;
    if (!instance || !activeUrl) return;

    instance.timeOffset = effectiveOffset;
    void instance.ready
      .then(() => {
        if (jassubRef.current === instance) return instance.resize(true);
      })
      .catch((err) => {
        if (jassubRef.current === instance) {
          console.error("[useASSSubtitles] Unable to repaint subtitles:", err);
        }
      });
  }, [effectiveOffset, activeUrl]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
    };
  }, []);

  return { isActive: isASS && !isDetached };
}
