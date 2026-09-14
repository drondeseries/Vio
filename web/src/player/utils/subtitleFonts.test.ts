import { describe, expect, it, vi } from "vitest";
import { fontBundleCacheKey, loadSubtitleFontBundle } from "./subtitleFonts";

describe("fontBundleCacheKey", () => {
  it("strips only the embedded_stream_index query param", () => {
    const url = "/api/v1/stream/x/subtitles/8/fonts?embedded_stream_index=2&file_id=7&token=abc";
    const twin = "/api/v1/stream/x/subtitles/8/fonts?embedded_stream_index=3&file_id=7&token=abc";
    expect(fontBundleCacheKey(url)).toBe(fontBundleCacheKey(twin));
    expect(fontBundleCacheKey(url)).toBe("/api/v1/stream/x/subtitles/8/fonts?file_id=7&token=abc");
  });

  it("keeps a URL without the param unchanged", () => {
    const url = "/api/v1/stream/x/subtitles/8/fonts?file_id=7&token=abc";
    expect(fontBundleCacheKey(url)).toBe(url);
  });

  it("keeps a parameterless URL unchanged", () => {
    expect(fontBundleCacheKey("/api/v1/stream/x/subtitles/8/fonts")).toBe(
      "/api/v1/stream/x/subtitles/8/fonts",
    );
  });
});

describe("loadSubtitleFontBundle cache sharing", () => {
  it("shares one cache entry across URLs differing only in embedded_stream_index", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue({
      ok: true,
      status: 200,
      json: vi.fn().mockResolvedValue([{ name: "F.ttf", data: btoa("font-bytes") }]),
    } as unknown as Response);
    try {
      const urlA = "/fonts?embedded_stream_index=2&file_id=7&token=t";
      const urlB = "/fonts?embedded_stream_index=3&file_id=7&token=t";
      const [fontsA, fontsB] = await Promise.all([
        loadSubtitleFontBundle(urlA),
        loadSubtitleFontBundle(urlB),
      ]);
      expect(fontsA).toEqual(fontsB);
      expect(fontsA).toEqual([expect.any(Uint8Array)]);
      // The second URL hit the shared (normalized-key) cache entry: exactly one
      // network fetch happened.
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      fetchMock.mockRestore();
    }
  });
});
