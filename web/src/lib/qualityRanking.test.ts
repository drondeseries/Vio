import { describe, expect, it } from "vitest";

import { serverRankingFromVersions } from "@/pages/ItemDetail/components/versionFormatUtils";
import {
  matchVersionSortPreset,
  sortVersionsByCriteria,
  VERSION_SORT_ATTRIBUTES,
  VERSION_SORT_PRESETS,
  versionSortableFromFile,
  type VersionSortSource,
} from "./qualityRanking";

function version(
  overrides: Partial<VersionSortSource & { file_id: number }> = {},
): VersionSortSource & { file_id: number } {
  return {
    file_id: 1,
    file_size: 0,
    bitrate: 0,
    resolution: "",
    hdr: false,
    ...overrides,
  };
}

describe("VERSION_SORT_ATTRIBUTES", () => {
  it("offers only the payload-supported attributes", () => {
    expect(VERSION_SORT_ATTRIBUTES).toEqual([
      "size",
      "bitrate",
      "resolution",
      "audio_channels",
      "bit_depth",
      "hdr",
      "score",
    ]);
    // Internal-only signals never reach the wire.
    expect(VERSION_SORT_ATTRIBUTES).not.toContain("source");
    expect(VERSION_SORT_ATTRIBUTES).not.toContain("language");
    expect(VERSION_SORT_ATTRIBUTES).not.toContain("confirmed");
  });
});

describe("VERSION_SORT_PRESETS", () => {
  it("offers the named presets plus the profile-default reset", () => {
    expect(VERSION_SORT_PRESETS.map((preset) => preset.id)).toEqual([
      "profile",
      "quality",
      "biggest",
      "bitrate",
    ]);
    // The profile default is the reset: no override.
    expect(VERSION_SORT_PRESETS.find((preset) => preset.id === "profile")?.criteria).toEqual([]);
    // Every preset only uses offered attributes.
    for (const preset of VERSION_SORT_PRESETS) {
      for (const criterion of preset.criteria) {
        expect(VERSION_SORT_ATTRIBUTES).toContain(criterion.attribute);
      }
    }
  });

  it("matches a stored order to its preset, or Custom", () => {
    expect(matchVersionSortPreset([])).toBe("profile");
    expect(
      matchVersionSortPreset([
        { attribute: "size", direction: "desc" },
        { attribute: "bitrate", direction: "desc" },
      ]),
    ).toBe("biggest");
    expect(matchVersionSortPreset([{ attribute: "hdr", direction: "asc" }])).toBe("custom");
  });
});

describe("versionSortableFromFile", () => {
  it("takes the richest audio channels and video bit depth", () => {
    const sortable = versionSortableFromFile(
      version({
        audio_tracks: [{ channels: 2 }, { channels: 6 }],
        video_tracks: [{ bit_depth: 8 }, { bit_depth: 10 }],
      }),
    );
    expect(sortable.audioChannels).toBe(6);
    expect(sortable.bitDepth).toBe(10);
  });
});

describe("sortVersionsByCriteria", () => {
  const versions = [
    version({ file_id: 1, file_size: 100 }),
    version({ file_id: 2, file_size: 300 }),
    version({ file_id: 3, file_size: 200 }),
  ];

  it("orders by size descending and ascending", () => {
    expect(
      sortVersionsByCriteria(versions, [{ attribute: "size", direction: "desc" }], (v) =>
        versionSortableFromFile(v),
      ).map((v) => v.file_id),
    ).toEqual([2, 3, 1]);
    expect(
      sortVersionsByCriteria(versions, [{ attribute: "size", direction: "asc" }], (v) =>
        versionSortableFromFile(v),
      ).map((v) => v.file_id),
    ).toEqual([1, 3, 2]);
  });

  it("sorts unknown values last within their criterion", () => {
    const mixed = [
      version({ file_id: 1, file_size: 100 }),
      version({ file_id: 2, file_size: 0 }),
      version({ file_id: 3, file_size: 300 }),
    ];
    expect(
      sortVersionsByCriteria(mixed, [{ attribute: "size", direction: "asc" }], (v) =>
        versionSortableFromFile(v),
      ).map((v) => v.file_id),
    ).toEqual([1, 3, 2]);
  });

  it("keeps the incoming order for ties", () => {
    const tied = [
      version({ file_id: 1, file_size: 0 }),
      version({ file_id: 2, file_size: 100 }),
      version({ file_id: 3, file_size: 0 }),
    ];
    expect(
      sortVersionsByCriteria(tied, [{ attribute: "size", direction: "desc" }], (v) =>
        versionSortableFromFile(v),
      ).map((v) => v.file_id),
    ).toEqual([2, 1, 3]);
  });

  it("falls through to the next criterion", () => {
    const same = [
      version({ file_id: 1, file_size: 100, bitrate: 5 }),
      version({ file_id: 2, file_size: 100, bitrate: 9 }),
    ];
    expect(
      sortVersionsByCriteria(
        same,
        [
          { attribute: "size", direction: "desc" },
          { attribute: "bitrate", direction: "desc" },
        ],
        (v) => versionSortableFromFile(v),
      ).map((v) => v.file_id),
    ).toEqual([2, 1]);
  });

  it("leaves the list untouched with no criteria", () => {
    expect(sortVersionsByCriteria(versions, [], (v) => versionSortableFromFile(v))).toEqual(
      versions,
    );
  });
});

describe("serverRankingFromVersions", () => {
  it("prefers the virtual_ranking payload when present", () => {
    const ranking = serverRankingFromVersions([
      {
        file_path: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
        virtual_ranking: {
          profile_label: "4K+HDR",
          source: "profile",
          criteria: [{ attribute: "score", direction: "desc" }],
        },
      },
    ]);
    expect(ranking).toEqual({
      profileLabel: "4K+HDR",
      criteria: [{ attribute: "score", direction: "desc" }],
      source: "profile",
      fromPayload: true,
    });
  });

  it("falls back to the ?profile= selector with the default order", () => {
    const ranking = serverRankingFromVersions([
      { file_path: "virtual://movie/tt1?profile=4K%2BHDR&result=abc" },
    ]);
    expect(ranking).toEqual({
      profileLabel: "4K+HDR",
      criteria: [],
      source: null,
      fromPayload: false,
    });
  });

  it("returns no profile when the candidates have none", () => {
    expect(serverRankingFromVersions([{ file_path: "" }])).toEqual({
      profileLabel: null,
      criteria: [],
      source: null,
      fromPayload: false,
    });
  });
});
