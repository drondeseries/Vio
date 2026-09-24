import { describe, expect, it } from "vitest";

import type { WatchIndexerRelease } from "@/api/types";
import { indexerReleaseMeta, indexerReleaseTitle } from "./indexerReleaseUtils";

function makeRelease(overrides: Partial<WatchIndexerRelease> = {}): WatchIndexerRelease {
  return {
    release_id: overrides.release_id ?? "rel-1",
    title: overrides.title ?? "Movie.2026.2160p.WEB-DL-GRP",
    download_state: overrides.download_state ?? "not_downloaded",
    ...overrides,
  };
}

describe("indexerReleaseTitle", () => {
  it("spaces out the release name's dots", () => {
    // `prettifyReleaseName` spaces out dots; the release-group hyphen is part
    // of the name and stays.
    expect(indexerReleaseTitle(makeRelease({ title: "Movie.2026.2160p.WEB-DL-GRP" }))).toBe(
      "Movie 2026 2160p WEB-DL-GRP",
    );
  });

  it("strips provider plumbing like a virtual URI, result token and tt id", () => {
    expect(
      indexerReleaseTitle(makeRelease({ title: "tt123456?result=abc virtual://movie/x" })),
    ).not.toMatch(/tt\d{5,}|result=|virtual:\/\//);
  });

  it("falls back to the resolution when the title sanitizes to nothing", () => {
    // A bare `virtual://…` URI is stripped to nothing, so the resolution stands in.
    expect(
      indexerReleaseTitle(
        makeRelease({ title: "virtual://movie/tt123?result=abc", resolution: "1080p" }),
      ),
    ).toBe("1080p");
  });

  it("falls back to a neutral label when neither title nor resolution is usable", () => {
    expect(indexerReleaseTitle(makeRelease({ title: "   ", resolution: undefined }))).toBe(
      "Indexer release",
    );
  });
});

describe("indexerReleaseMeta", () => {
  it("joins resolution, codecs, dynamic range, size and indexer in order", () => {
    const meta = indexerReleaseMeta(
      makeRelease({
        resolution: "2160p",
        codec_video: "hevc",
        codec_audio: "eac3",
        hdr: true,
        size_bytes: 50_570_000_000,
        indexer: "Prowlarr",
      }),
    );

    expect(meta).toBe("2160p · HEVC · HDR · EAC3 · 47.1 GB · Prowlarr");
  });

  it("drops absent segments without leaving empty separators", () => {
    const meta = indexerReleaseMeta(
      makeRelease({ resolution: "1080p", codec_video: "h264", codec_audio: undefined, hdr: false }),
    );

    expect(meta).toBe("1080p · H264");
  });

  it("uses the audio-family label for an Atmos track", () => {
    expect(indexerReleaseMeta(makeRelease({ codec_audio: "truehd atmos" }))).toContain("Atmos");
  });

  it("omits the dynamic range for an SDR release", () => {
    const meta = indexerReleaseMeta(
      makeRelease({ resolution: "1080p", codec_video: "h264", hdr: false }),
    );

    expect(meta).not.toContain("HDR");
  });

  it("returns an empty string when there is nothing to show", () => {
    expect(indexerReleaseMeta(makeRelease({ title: "Only a title" }))).toBe("");
  });
});
