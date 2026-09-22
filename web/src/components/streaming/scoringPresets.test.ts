import { describe, expect, it } from "vitest";

import {
  formatSortCriteriaSummary,
  formatSortCriterion,
  MAX_SORT_CRITERIA,
  normalizeSortCriteria,
  SORT_ATTRIBUTE_LABELS,
  SORT_ATTRIBUTE_SHORT_LABELS,
  SORT_ATTRIBUTES,
  SORT_DIRECTION_LABELS,
  SORT_DIRECTION_SYMBOLS,
  SORT_DIRECTIONS,
} from "./scoringPresets";

describe("sort criteria enums", () => {
  // These literals are the wire contract. Pinning them makes a backend spelling
  // change fail loudly here instead of silently dropping criteria at runtime.
  it("matches the backend's accepted attribute values exactly", () => {
    expect(SORT_ATTRIBUTES).toEqual([
      "size",
      "bitrate",
      "resolution",
      "audio_channels",
      "bit_depth",
      "hdr",
      "source",
      "score",
      "confirmed",
      "language",
    ]);
  });

  it("matches the backend's accepted direction values exactly", () => {
    expect(SORT_DIRECTIONS).toEqual(["desc", "asc"]);
  });

  it("labels every accepted attribute and direction", () => {
    for (const attribute of SORT_ATTRIBUTES) {
      expect(SORT_ATTRIBUTE_LABELS[attribute]).toBeTruthy();
      expect(SORT_ATTRIBUTE_SHORT_LABELS[attribute]).toBeTruthy();
    }
    for (const direction of SORT_DIRECTIONS) {
      expect(SORT_DIRECTION_LABELS[direction]).toBeTruthy();
      expect(SORT_DIRECTION_SYMBOLS[direction]).toBeTruthy();
    }
  });

  it("does not offer more than the cap in one profile", () => {
    expect(SORT_ATTRIBUTES.length).toBeLessThanOrEqual(MAX_SORT_CRITERIA);
  });
});

describe("normalizeSortCriteria", () => {
  it("treats an absent or malformed list as no criteria", () => {
    expect(normalizeSortCriteria(undefined)).toEqual([]);
    expect(normalizeSortCriteria(null)).toEqual([]);
    expect(normalizeSortCriteria("size")).toEqual([]);
    expect(normalizeSortCriteria({ attribute: "size" })).toEqual([]);
  });

  it("keeps recognized criteria in order and drops unknown attributes", () => {
    expect(
      normalizeSortCriteria([
        { attribute: "size", direction: "desc" },
        { attribute: "made_up", direction: "asc" },
        { attribute: "bitrate", direction: "asc" },
      ]),
    ).toEqual([
      { attribute: "size", direction: "desc" },
      { attribute: "bitrate", direction: "asc" },
    ]);
  });

  it("falls back to descending for an unknown direction", () => {
    expect(normalizeSortCriteria([{ attribute: "score", direction: "sideways" }])).toEqual([
      { attribute: "score", direction: "desc" },
    ]);
    expect(normalizeSortCriteria([{ attribute: "score" }])).toEqual([
      { attribute: "score", direction: "desc" },
    ]);
  });

  it("caps the list at the maximum", () => {
    const overflow = Array.from({ length: MAX_SORT_CRITERIA + 5 }, () => ({
      attribute: "size",
      direction: "desc",
    }));
    expect(normalizeSortCriteria(overflow)).toHaveLength(MAX_SORT_CRITERIA);
  });
});

describe("formatting helpers", () => {
  it("formats a single chip label", () => {
    expect(formatSortCriterion({ attribute: "size", direction: "desc" })).toBe("size ↓");
    expect(formatSortCriterion({ attribute: "bitrate", direction: "asc" })).toBe("bitrate ↑");
  });

  it("joins chips for a one-line summary", () => {
    expect(
      formatSortCriteriaSummary([
        { attribute: "size", direction: "desc" },
        { attribute: "bitrate", direction: "desc" },
      ]),
    ).toBe("size ↓ · bitrate ↓");
    expect(formatSortCriteriaSummary([])).toBe("");
  });
});
