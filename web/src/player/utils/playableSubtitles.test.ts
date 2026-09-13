import { describe, expect, it } from "vitest";
import type { PlayerSubtitleInfo } from "../types";
import {
  hasSelectableSessionSubtitles,
  isSameSubtitleTrack,
  pendingServerSubtitleSelection,
  resolvePlayableSubtitles,
  subtitleTrackIdentityKey,
  type SubtitleTrackIdentity,
} from "./playableSubtitles";

function makeSubtitle(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "eng",
    label: "English",
    url: "",
    ...overrides,
  };
}

describe("resolvePlayableSubtitles", () => {
  it("prefers playback-session subtitle tracks when they include stream urls", () => {
    const sessionTrack = makeSubtitle({
      index: 2,
      source: "embedded",
      url: "/stream/session/subtitles/2",
    });
    const detailTrack = makeSubtitle({
      index: 0,
      source: "embedded",
      url: "",
    });

    expect(resolvePlayableSubtitles([sessionTrack], [detailTrack])).toEqual([sessionTrack]);
  });

  it("drops watch-detail subtitle tracks that have no playable url", () => {
    const detailTrack = makeSubtitle({
      index: 0,
      source: "embedded",
      codec: "hdmv_pgs_subtitle",
      url: "",
    });

    expect(resolvePlayableSubtitles([], [detailTrack])).toEqual([]);
  });

  it("keeps burn-in-only session tracks even though they have no url", () => {
    const burnInTrack = makeSubtitle({
      index: 3,
      source: "embedded",
      codec: "hdmv_pgs_subtitle",
      burn_in_only: true,
      url: "",
    });
    const sidecarTrack = makeSubtitle({
      index: 4,
      source: "embedded",
      url: "/stream/session/subtitles/4",
    });

    expect(resolvePlayableSubtitles([burnInTrack, sidecarTrack], [])).toEqual([
      burnInTrack,
      sidecarTrack,
    ]);
  });

  it("keeps fallback tracks that already have playable urls", () => {
    const fallbackTrack = makeSubtitle({
      index: 1,
      source: "downloaded",
      url: "/stream/fallback/subtitles/1",
    });

    expect(resolvePlayableSubtitles([], [fallbackTrack])).toEqual([fallbackTrack]);
  });
});

describe("hasSelectableSessionSubtitles", () => {
  it("is true when a session track carries a stream url", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ url: "/stream/session/0" })])).toBe(true);
  });

  it("is true for a burn-in-only track without a url", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ burn_in_only: true, url: "" })])).toBe(
      true,
    );
  });

  it("is false for a published track that is neither fetchable nor burn-in-only", () => {
    expect(hasSelectableSessionSubtitles([makeSubtitle({ url: "" })])).toBe(false);
  });

  it("is false for an empty inventory", () => {
    expect(hasSelectableSessionSubtitles([])).toBe(false);
  });
});

describe("subtitle track identity", () => {
  function identity(overrides: Partial<SubtitleTrackIdentity> = {}): SubtitleTrackIdentity {
    return { index: 0, ...overrides };
  }

  it("matches two descriptors that name the same server track_id", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 3, trackId: "file:7:subtitle:2" }),
        identity({ index: 9, trackId: "file:7:subtitle:2" }),
      ),
    ).toBe(true);
  });

  it("does not match different server track_ids even with equal descriptors", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 3, trackId: "file:7:subtitle:2", language: "en", codec: "srt" }),
        identity({ index: 4, trackId: "file:7:subtitle:3", language: "en", codec: "srt" }),
      ),
    ).toBe(false);
  });

  it("falls back to language/codec/forced/hearing-impaired when no track_id is present", () => {
    expect(
      isSameSubtitleTrack(
        identity({ index: 1, language: "English", codec: "SRT", forced: false }),
        identity({ index: 5, language: "english", codec: "srt", forced: false }),
      ),
    ).toBe(true);
    expect(
      isSameSubtitleTrack(
        identity({ index: 1, language: "en", codec: "srt", forced: true }),
        identity({ index: 5, language: "en", codec: "srt", forced: false }),
      ),
    ).toBe(false);
  });

  it("keys the stable identity on track_id when available", () => {
    expect(subtitleTrackIdentityKey(identity({ index: 3, trackId: "file:7:subtitle:2" }))).toBe(
      "id:file:7:subtitle:2",
    );
    expect(subtitleTrackIdentityKey(identity({ index: 3, language: "en", codec: "srt" }))).toBe(
      "desc:en|srt||",
    );
  });
});

describe("pendingServerSubtitleSelection", () => {
  function identity(overrides: Partial<SubtitleTrackIdentity> = {}): SubtitleTrackIdentity {
    return { index: 0, ...overrides };
  }

  it("settles an already-selected burn-in plan without another replan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
      ),
    ).toBeUndefined();
  });

  it("does not re-request a sidecar artifact selected by a burn-in plan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
      ),
    ).toBeUndefined();
  });

  it("preserves a sidecar selection while replacing burn-in", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
      ),
    ).toBe(0);
  });

  it("turns burn-in off explicitly rather than looping", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
        null,
      ),
    ).toBeNull();
  });

  it("requests a burn-in track from a sidecar plan", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 2, trackId: "file:7:subtitle:2", burnInOnly: true }),
      ),
    ).toBe(2);
  });

  it("persists a newly selected sidecar track when the plan has no selection", () => {
    expect(pendingServerSubtitleSelection(null, identity({ index: 0 }))).toBe(0);
  });

  it("persists a switch between sidecar tracks", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 0, trackId: "file:7:subtitle:0" }),
        identity({ index: 1, trackId: "file:7:subtitle:1" }),
      ),
    ).toBe(1);
  });

  it("persists turning a sidecar track off", () => {
    expect(
      pendingServerSubtitleSelection(identity({ index: 0, trackId: "file:7:subtitle:0" }), null),
    ).toBeNull();
  });

  it("treats a re-minted ordinal for the same track_id as settled", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 7, trackId: "file:7:subtitle:2" }),
        identity({ index: 2, trackId: "file:7:subtitle:2" }),
      ),
    ).toBeUndefined();
  });

  it("treats a reordered descriptor match without track_id as settled", () => {
    expect(
      pendingServerSubtitleSelection(
        identity({ index: 7, language: "en", codec: "srt", forced: false }),
        identity({ index: 2, language: "en", codec: "srt", forced: false }),
      ),
    ).toBeUndefined();
  });

  it("offers the selection when the server resolved it to off (caller guard dedupes)", () => {
    expect(
      pendingServerSubtitleSelection(null, identity({ index: 2, trackId: "file:7:subtitle:2" })),
    ).toBe(2);
  });
});
