import { describe, expect, it } from "vitest";

import type { WatchIndexerRelease } from "@/api/types";
import { indexerReleasesFromWire } from "./indexerReleases";

describe("indexerReleasesFromWire", () => {
  it("maps every field of a full release", () => {
    const wire = [
      {
        release_id: "rel-1",
        title: "Movie.2026.2160p.WEB-DL.DDP5.1.Atmos.HDR.HEVC-GRP",
        resolution: "2160p",
        codec_video: "hevc",
        codec_audio: "eac3",
        hdr: true,
        size_bytes: 50_570_000_000,
        indexer: "Prowlarr",
        published_at: "2026-01-02T03:04:05.000Z",
        format_score: 850,
        protocol: "torrent",
        download_state: "not_downloaded",
      },
    ];

    expect(indexerReleasesFromWire(wire)).toEqual<WatchIndexerRelease[]>([
      {
        release_id: "rel-1",
        title: "Movie.2026.2160p.WEB-DL.DDP5.1.Atmos.HDR.HEVC-GRP",
        resolution: "2160p",
        codec_video: "hevc",
        codec_audio: "eac3",
        hdr: true,
        size_bytes: 50_570_000_000,
        indexer: "Prowlarr",
        published_at: "2026-01-02T03:04:05.000Z",
        format_score: 850,
        protocol: "torrent",
        download_state: "not_downloaded",
      },
    ]);
  });

  it("normalizes an absent array to an empty array, never null", () => {
    const result = indexerReleasesFromWire(undefined);
    expect(result).toEqual([]);
    expect(result).not.toBeNull();
  });

  it("normalizes a null payload to an empty array", () => {
    const result = indexerReleasesFromWire(null);
    expect(result).toEqual([]);
    expect(result).not.toBeNull();
  });

  it("normalizes a non-array payload to an empty array", () => {
    expect(indexerReleasesFromWire({ not: "an array" })).toEqual([]);
    expect(indexerReleasesFromWire("2160p")).toEqual([]);
  });

  it("treats an unknown download_state as not_downloaded", () => {
    const [release] = indexerReleasesFromWire([
      { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "something_new" },
    ]);

    expect(release?.download_state).toBe("not_downloaded");
  });

  it("preserves the known queued and failed download states", () => {
    const releases = indexerReleasesFromWire([
      { release_id: "rel-1", title: "Queued release", download_state: "queued" },
      { release_id: "rel-2", title: "Failed release", download_state: "failed" },
    ]);

    expect(releases.map((release) => release.download_state)).toEqual(["queued", "failed"]);
  });

  it("leaves absent optional fields absent rather than fabricating them", () => {
    const [release] = indexerReleasesFromWire([
      { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "not_downloaded" },
    ]);

    expect(release).toMatchObject({
      release_id: "rel-1",
      title: "Movie 2026 2160p",
      hdr: false,
      download_state: "not_downloaded",
    });
    // No fabricated values: each optional field stays `undefined`.
    expect(release?.resolution).toBeUndefined();
    expect(release?.codec_video).toBeUndefined();
    expect(release?.codec_audio).toBeUndefined();
    expect(release?.size_bytes).toBeUndefined();
    expect(release?.indexer).toBeUndefined();
    expect(release?.published_at).toBeUndefined();
    expect(release?.format_score).toBeUndefined();
    expect(release?.protocol).toBeUndefined();
  });

  it("drops a malformed element without an id or title", () => {
    const releases = indexerReleasesFromWire([
      { title: "No id", download_state: "not_downloaded" },
      { release_id: "rel-2", download_state: "not_downloaded" },
      { release_id: "rel-3", title: "  ", download_state: "not_downloaded" },
      null,
      "nope",
      { release_id: "rel-4", title: "Valid release" },
    ]);

    expect(releases.map((release) => release.release_id)).toEqual(["rel-4"]);
  });

  it("trims string fields and ignores a non-boolean hdr", () => {
    const [release] = indexerReleasesFromWire([
      {
        release_id: "  rel-1  ",
        title: "  Movie 2026 2160p  ",
        resolution: "  2160p  ",
        hdr: "yes",
      },
    ]);

    expect(release?.release_id).toBe("rel-1");
    expect(release?.title).toBe("Movie 2026 2160p");
    expect(release?.resolution).toBe("2160p");
    expect(release?.hdr).toBe(false);
  });
});
