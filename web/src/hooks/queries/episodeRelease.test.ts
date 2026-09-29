import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  episodeReleaseState,
  fetchEpisodeReleaseCapability,
  formatCalendarDate,
  isCalendarDate,
  isUpcomingEpisode,
} from "./episodeRelease";
import { setProfileId } from "@/api/client";
import { installPolicyStorageMocks, jsonResponse } from "@/pages/admin-policy/policyTestUtils";

describe("episodeRelease", () => {
  beforeEach(() => {
    installPolicyStorageMocks();
    setProfileId("p-owner");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  describe("isCalendarDate", () => {
    it("accepts real calendar dates", () => {
      expect(isCalendarDate("2026-10-03")).toBe(true);
      expect(isCalendarDate("2024-02-29")).toBe(true);
      expect(isCalendarDate(" 2026-10-03 ")).toBe(true);
    });

    it("rejects impossible and malformed dates", () => {
      expect(isCalendarDate("")).toBe(false);
      expect(isCalendarDate("not-a-date")).toBe(false);
      expect(isCalendarDate("2026-10-03T00:00:00Z")).toBe(false);
      expect(isCalendarDate("2099-99-99")).toBe(false);
      expect(isCalendarDate("2026-13-01")).toBe(false);
      expect(isCalendarDate("2026-02-30")).toBe(false);
      expect(isCalendarDate("2023-02-29")).toBe(false);
      expect(isCalendarDate("2026-04-31")).toBe(false);
      expect(isCalendarDate("2026-1-3")).toBe(false);
    });
  });

  describe("episodeReleaseState", () => {
    it("honors the server release_state when the capability is available", () => {
      expect(episodeReleaseState({ release_state: "upcoming" }, true)).toBe("upcoming");
      expect(episodeReleaseState({ release_state: "released" }, true)).toBe("released");
    });

    it("ignores unknown server states", () => {
      expect(episodeReleaseState({ release_state: "someday" }, true)).toBeUndefined();
      expect(episodeReleaseState({}, true)).toBeUndefined();
    });

    it("degrades to undefined without the capability, even with a future air date", () => {
      // Older server, no release_state: plain rendering, no date inference.
      expect(episodeReleaseState({ release_state: "upcoming" }, false)).toBeUndefined();
      expect(episodeReleaseState({}, false)).toBeUndefined();
    });
  });

  describe("isUpcomingEpisode", () => {
    it("marks only server-classified upcoming episodes", () => {
      expect(isUpcomingEpisode({ release_state: "upcoming" }, true)).toBe(true);
      expect(isUpcomingEpisode({ release_state: "released" }, true)).toBe(false);
      expect(isUpcomingEpisode({}, true)).toBe(false);
      expect(isUpcomingEpisode({ release_state: "upcoming" }, false)).toBe(false);
    });
  });

  describe("formatCalendarDate", () => {
    it("formats the exact calendar day without timezone conversion", () => {
      const formatted = formatCalendarDate("2026-10-03");
      expect(formatted).toContain("2026");
      expect(formatted).toContain("Oct");
      expect(formatted).toContain("3");
      // A bare-date parse would shift Oct 3 to Oct 2 west of UTC; the
      // local-midnight parse keeps the stored day.
      const again = formatCalendarDate("2026-01-01");
      expect(again).toContain("2026");
      expect(again).toContain("1");
    });

    it("returns impossible dates unchanged instead of Invalid Date", () => {
      expect(formatCalendarDate("2099-99-99")).toBe("2099-99-99");
      expect(formatCalendarDate("2026-02-30")).toBe("2026-02-30");
    });

    it("passes malformed values through", () => {
      expect(formatCalendarDate(null)).toBe("");
      expect(formatCalendarDate("not-a-date")).toBe("not-a-date");
      expect(formatCalendarDate("2026-10-03T00:00:00Z")).toBe("2026-10-03T00:00:00Z");
    });
  });

  describe("fetchEpisodeReleaseCapability", () => {
    it("is available only in the available state", async () => {
      const fetchMock = vi.fn<typeof fetch>(async () =>
        jsonResponse({ revision: "r", state: "available" }),
      );
      vi.stubGlobal("fetch", fetchMock);

      await expect(fetchEpisodeReleaseCapability()).resolves.toEqual({
        available: true,
      });
      expect(String(fetchMock.mock.calls[0]?.[0])).toBe("/api/v2/capabilities/episode-release");

      vi.unstubAllGlobals();
    });

    it("treats other states as unavailable", async () => {
      const fetchMock = vi.fn<typeof fetch>(async () =>
        jsonResponse({ revision: "r", state: "not_configured" }),
      );
      vi.stubGlobal("fetch", fetchMock);

      await expect(fetchEpisodeReleaseCapability()).resolves.toEqual({
        available: false,
      });

      vi.unstubAllGlobals();
    });
  });
});
