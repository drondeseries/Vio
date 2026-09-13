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

// Explicitly bound each ASS fetch to this many source-time seconds, matching
// the VTT window (`useSubtitleTracks`). The server extracts the whole track
// otherwise — ~100s for an embedded ASS track over a relay — long enough to
// trip the stall watchdog and loop. Windowed extraction is seconds.
const ASS_WINDOW_DURATION_SECONDS = 600;
// Start a window this many seconds before the playhead so the cue currently
// on screen is inside the requested interval rather than cut off at its start.
const ASS_WINDOW_LEAD_SECONDS = 20;
// Fetch the next window once playback comes this close to the current one's
// end. The replacement window restarts `ASS_WINDOW_LEAD_SECONDS` behind the
// playhead, so it overlaps the outgoing window and the swap drops no cues.
const ASS_WINDOW_PREFETCH_LEAD_SECONDS = 60;
// A windowed extraction is seconds of work, not a whole-track 100s: allow this
// long without a chunk before abandoning the attempt. Covers a cold ffmpeg
// start plus a slow-but-progressing extraction.
const ASS_STALL_TIMEOUT_MS = 60_000;
// Consecutive windowed failures tolerated before falling back to the
// param-less whole-track URL once.
const ASS_WINDOW_MAX_ATTEMPTS = 3;
// Retry backoff after a failed windowed attempt.
const ASS_RETRY_BACKOFF_MS = 5_000;

/** `url` with the bounded window query the VTT path also uses. */
function appendSubtitleWindow(url: string, position: number): string {
  const sep = url.includes("?") ? "&" : "?";
  return `${url}${sep}position=${Math.max(0, Math.floor(position))}&duration=${ASS_WINDOW_DURATION_SECONDS}`;
}

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
  // The effective subtitle source generation. It survives plan swaps and only
  // changes when the underlying source identity changes, so a subtitle-only
  // replan does not tear down JASSUB and immediately refetch a possibly-still-
  // stale URL. A genuine source change still rebuilds.
  sourceGeneration = 0,
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
  // Raw stream origin (without the user sync delay). The player clock plus
  // this is the source time a window request must be anchored to; the delay
  // only shifts rendering and must not shift the requested source interval.
  const sourceOriginRef = useRef(streamOriginSeconds);
  sourceOriginRef.current = streamOriginSeconds;

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

    // Sliding-window state. The first window is anchored just behind the
    // playhead so the cue already on screen is included. `windowEnd` is
    // Infinity once the whole-track fallback is in use, so boundary refresh
    // never schedules.
    let usingWholeTrack = false;
    let windowStart = Math.max(
      0,
      (video.readyState > 0 ? video.currentTime : 0) +
        sourceOriginRef.current -
        ASS_WINDOW_LEAD_SECONDS,
    );
    let windowEnd = windowStart + ASS_WINDOW_DURATION_SECONDS;
    let windowFailures = 0;
    let refreshing = false;
    let refreshRetryAt = 0;
    let refreshController: AbortController | null = null;
    let pendingRefreshStart: number | null = null;

    // Fonts belong to the track, not the window: resolve them once (after the
    // first window's text lands) and reuse them for every later window.
    let fontState: {
      attached: Uint8Array[];
      fallbackFont: ReturnType<typeof fallbackFontForSubtitle>;
      fallbackFontData: Uint8Array[] | null;
    } | null = null;

    /** Source-time position of the playhead (player clock + stream origin). */
    function sourcePosition(): number {
      return (video!.readyState > 0 ? video!.currentTime : 0) + sourceOriginRef.current;
    }

    /** Resolve the per-track font state once, reusing it across windows. */
    async function resolveFontState(signal: AbortSignal, subContent: string) {
      if (fontState) return fontState;
      let attachedFontData: Uint8Array[] = [];
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
      // libass renders missing glyphs with its *default* font — it does not
      // search other loaded fonts for coverage. JASSUB's built-in default
      // (Liberation Sans) lacks many non-Latin glyphs, so for those scripts we
      // point `defaultFont` at a font that covers them, chosen by track metadata
      // first and subtitle text as a fallback.
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
      fontState = { attached: attachedFontData, fallbackFont, fallbackFontData };
      return fontState;
    }

    async function initJASSUB(
      signal: AbortSignal,
      progress: () => void,
      start: number,
      wholeTrack: boolean,
    ) {
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

      const url = wholeTrack ? activeUrl! : appendSubtitleWindow(activeUrl!, start);

      let subContent: string;
      let fonts: Uint8Array[];
      let renderedSubContent: string;
      try {
        subContent = await fetch(url, { signal }).then(async (response) => {
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

        const resolvedFonts = await resolveFontState(signal, subContent);
        renderedSubContent =
          resolvedFonts.fallbackFont && resolvedFonts.fallbackFontData
            ? forceASSFontFamily(subContent, resolvedFonts.fallbackFont.family)
            : subContent;
        fonts = [...resolvedFonts.attached, ...(resolvedFonts.fallbackFontData ?? [])];
      } catch (err) {
        if (!cancelled && (err as Error).name !== "AbortError") {
          console.error(`[useASSSubtitles] Failed to fetch ${url}:`, err);
        }
        throw err;
      }

      if (cancelled || signal.aborted) return;

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
              ...(fontState?.fallbackFont && { defaultFont: fontState.fallbackFont.family }),
            }
          : {}),
      });

      // Guard against the effect being cleaned up while the constructor ran.
      if (cancelled || signal.aborted) {
        instance.destroy();
        return;
      }

      const previous = jassubRef.current;
      if (!previous) {
        // Initial load: publish the instance before its renderer is ready so
        // an offset/delay change arriving while WASM initializes still lands on
        // the live instance (the offset effect repaints once it readies).
        jassubRef.current = instance;
        try {
          await instance.ready;
        } catch (err) {
          if (jassubRef.current === instance) jassubRef.current = null;
          instance.destroy();
          throw err;
        }
        if (cancelled || signal.aborted) {
          if (jassubRef.current === instance) jassubRef.current = null;
          instance.destroy();
          return;
        }
      } else {
        // Window swap: keep the outgoing script rendering until the
        // replacement has readied, then swap atomically so the boundary shows
        // no empty gap. Successive windows overlap at the playhead, so no cue
        // currently on screen is dropped.
        try {
          await instance.ready;
        } catch (err) {
          instance.destroy();
          throw err;
        }
        if (cancelled || signal.aborted) {
          instance.destroy();
          return;
        }
        // Pick up any delay/origin change made while the window loaded.
        instance.timeOffset = streamOriginRef.current;
        jassubRef.current = instance;
        if (previous !== instance) previous.destroy();
      }
      usingWholeTrack = wholeTrack;
      windowStart = start;
      windowEnd = wholeTrack ? Number.POSITIVE_INFINITY : start + ASS_WINDOW_DURATION_SECONDS;
      windowFailures = 0;
      onLoadStateRef.current?.("ready");
    }

    async function load() {
      controller = new AbortController();
      const attemptController = controller;
      const start = windowStart;
      const wholeTrack = usingWholeTrack;
      let progress = () => {};
      const stalled = new Promise<never>((_, reject) => {
        progress = () => {
          if (cancelled || attemptController.signal.aborted) return;
          if (timeout !== null) clearTimeout(timeout);
          timeout = setTimeout(() => {
            attemptController.abort();
            reject(new Error("Subtitle loading stalled"));
          }, ASS_STALL_TIMEOUT_MS);
        };
        progress();
      });
      try {
        await Promise.race([initJASSUB(controller.signal, progress, start, wholeTrack), stalled]);
      } catch (err) {
        if (cancelled) return;
        attemptController.abort();
        console.error("[useASSSubtitles] Unable to load subtitles:", err);
        onLoadStateRef.current?.("error");
        // A signaled source change must not re-run the pipeline against the
        // same stale URL; the rebuilt JASSUB comes from the main effect
        // re-running when the refresh adopts a new plan and activeUrl changes.
        if (sourceChangedSignaled) return;
        if (!usingWholeTrack && windowFailures + 1 >= ASS_WINDOW_MAX_ATTEMPTS) {
          // Bounded windowed retries exhausted: request the param-less
          // whole-track URL once instead of hammering one failing window.
          usingWholeTrack = true;
        }
        windowFailures += 1;
        retryTimer = setTimeout(() => void load(), ASS_RETRY_BACKOFF_MS);
      } finally {
        if (timeout !== null) clearTimeout(timeout);
      }
    }

    async function refreshWindow(start: number) {
      refreshing = true;
      refreshController = new AbortController();
      const refresh = refreshController;
      let timer: ReturnType<typeof setTimeout> | null = null;
      const progress = () => {
        if (timer !== null) clearTimeout(timer);
        timer = setTimeout(() => refresh.abort(), ASS_STALL_TIMEOUT_MS);
      };
      try {
        await initJASSUB(refresh.signal, progress, start, false);
      } catch (err) {
        if (!cancelled && (err as Error).name !== "AbortError") {
          console.error("[useASSSubtitles] Unable to refresh subtitle window:", err);
          // Keep what is already on screen; retry the boundary after a
          // backoff so a failing extraction cannot storm the server.
          onLoadStateRef.current?.("error");
          refreshRetryAt = Date.now() + ASS_RETRY_BACKOFF_MS;
        }
      } finally {
        if (timer !== null) clearTimeout(timer);
        refreshing = false;
        if (refreshController === refresh) refreshController = null;
        const pending = pendingRefreshStart;
        pendingRefreshStart = null;
        if (pending !== null && !cancelled) void refreshWindow(pending);
      }
    }

    function maybeRefreshWindow() {
      if (cancelled || usingWholeTrack || !jassubRef.current) return;
      const source = sourcePosition();
      const outside = source < windowStart - 1 || source > windowEnd + 1;
      const nearEnd = !outside && source > windowEnd - ASS_WINDOW_PREFETCH_LEAD_SECONDS;
      if (!outside && !nearEnd) return;
      if (refreshing) {
        // A seek while a window is in flight must win: remember the newest
        // target and start it once the current load settles.
        if (outside) pendingRefreshStart = Math.max(0, source - ASS_WINDOW_LEAD_SECONDS);
        return;
      }
      if (Date.now() < refreshRetryAt) return;
      // Restart a full lead behind the playhead so the replacement window
      // overlaps the cue currently on screen regardless of why it fired.
      void refreshWindow(Math.max(0, source - ASS_WINDOW_LEAD_SECONDS));
    }

    void load();
    video.addEventListener("timeupdate", maybeRefreshWindow);
    video.addEventListener("seeking", maybeRefreshWindow);
    video.addEventListener("seeked", maybeRefreshWindow);

    return () => {
      cancelled = true;
      if (retryTimer !== null) clearTimeout(retryTimer);
      if (timeout !== null) clearTimeout(timeout);
      controller.abort();
      refreshController?.abort();
      video.removeEventListener("timeupdate", maybeRefreshWindow);
      video.removeEventListener("seeking", maybeRefreshWindow);
      video.removeEventListener("seeked", maybeRefreshWindow);
      // Destroy the current instance if the effect is being torn down
      // (e.g. track switch or unmount). This covers the common case where
      // initJASSUB has already completed and stored the instance.
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
    };
    // videoRef is a stable ref object. streamOriginSeconds is read from
    // sourceOriginRef inside the async function to always get the latest value.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeUrl, activeLanguage, activeFontBundleUrl, isDetached, sourceGeneration]);

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
