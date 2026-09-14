import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useSubtitleFontPrefetch } from "./useSubtitleFontPrefetch";
import type { PlayerSubtitleInfo } from "../types";

const loadSubtitleFontBundle = vi.hoisted(() =>
  vi.fn((_url: string, _signal?: AbortSignal) => Promise.resolve([])),
);

vi.mock("../utils/subtitleFonts", () => ({
  loadSubtitleFontBundle: (url: string) => loadSubtitleFontBundle(url),
}));

function track(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "eng",
    label: "English",
    codec: "ass",
    url: "/stream/x/subtitles/0.ass",
    ...overrides,
  };
}

afterEach(() => {
  loadSubtitleFontBundle.mockClear();
});

describe("useSubtitleFontPrefetch", () => {
  it("prefetches one font bundle per unique font_bundle_url on mount", () => {
    const urls = [
      track({ index: 1, font_bundle_url: "/fonts/a" }),
      track({ index: 2, font_bundle_url: "/fonts/a" }),
      track({ index: 3, font_bundle_url: "/fonts/b" }),
      track({ index: 4 }),
    ];
    renderHook(() => useSubtitleFontPrefetch(urls));
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(2);
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/a");
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/b");
  });

  it("does not fetch when no track advertises a font bundle", () => {
    renderHook(() => useSubtitleFontPrefetch([track({ index: 1 }), track({ index: 2 })]));
    expect(loadSubtitleFontBundle).not.toHaveBeenCalled();
  });

  it("does not refire on unrelated re-renders with the same subtitleUrls", () => {
    const urls = [track({ font_bundle_url: "/fonts/a" })];
    const { rerender } = renderHook(
      ({ subtitleUrls }: { subtitleUrls: PlayerSubtitleInfo[] }) =>
        useSubtitleFontPrefetch(subtitleUrls),
      { initialProps: { subtitleUrls: urls } },
    );
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
    rerender({ subtitleUrls: urls });
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
  });

  it("swallows a rejected font bundle fetch", async () => {
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown) => {
      unhandled.push(reason);
    };
    process.on("unhandledRejection", onUnhandled);
    try {
      loadSubtitleFontBundle.mockRejectedValueOnce(new Error("font extraction failed"));
      renderHook(() => useSubtitleFontPrefetch([track({ font_bundle_url: "/fonts/a" })]));
      await act(async () => {
        await Promise.resolve();
      });
      expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
      expect(unhandled).toHaveLength(0);
    } finally {
      process.removeListener("unhandledRejection", onUnhandled);
    }
  });
});
