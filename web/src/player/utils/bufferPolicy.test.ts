import { describe, expect, it } from "vitest";

import {
  DEFAULT_FORWARD_BUFFER_SECONDS,
  HIGH_BITRATE_FORWARD_BUFFER_SECONDS,
  HLS_PREFETCH_SECONDS,
  MAX_BACK_BUFFER_SECONDS,
  MIN_BACK_BUFFER_SECONDS,
  bufferRetentionFloor,
  clampBackBufferSeconds,
  hlsBufferPolicy,
  isTimeBuffered,
  prefetchBufferTargetSeconds,
} from "./bufferPolicy";

/** A minimal `TimeRanges` view over half-open [start, end) bounds. */
function timeRanges(bounds: Array<[number, number]>): TimeRanges {
  return {
    length: bounds.length,
    start: (i: number) => bounds[i]?.[0] ?? 0,
    end: (i: number) => bounds[i]?.[1] ?? 0,
  } as unknown as TimeRanges;
}

function videoWithBuffered(bounds: Array<[number, number]>): HTMLVideoElement {
  return { buffered: timeRanges(bounds) } as unknown as HTMLVideoElement;
}

describe("clampBackBufferSeconds", () => {
  it("keeps a request inside the retention window", () => {
    expect(clampBackBufferSeconds(90)).toBe(90);
  });

  it("raises a request below the floor so a backward skip stays local", () => {
    expect(clampBackBufferSeconds(0)).toBe(MIN_BACK_BUFFER_SECONDS);
    expect(clampBackBufferSeconds(30)).toBe(MIN_BACK_BUFFER_SECONDS);
  });

  it("caps a request above the ceiling so the SourceBuffer stays bounded", () => {
    expect(clampBackBufferSeconds(600)).toBe(MAX_BACK_BUFFER_SECONDS);
  });

  it("treats an unbounded request as the ceiling", () => {
    expect(clampBackBufferSeconds(Infinity)).toBe(MAX_BACK_BUFFER_SECONDS);
    expect(clampBackBufferSeconds(Number.NaN)).toBe(MAX_BACK_BUFFER_SECONDS);
  });
});

describe("hlsBufferPolicy", () => {
  it("uses the shorter forward target for high-bitrate sources", () => {
    expect(hlsBufferPolicy(25_000)).toEqual({
      forwardBufferSeconds: HIGH_BITRATE_FORWARD_BUFFER_SECONDS,
      backBufferSeconds: HIGH_BITRATE_FORWARD_BUFFER_SECONDS,
      prefetchSeconds: HLS_PREFETCH_SECONDS,
    });
  });

  it("uses the longer forward target below the high-bitrate threshold", () => {
    expect(hlsBufferPolicy(8_000)).toEqual({
      forwardBufferSeconds: DEFAULT_FORWARD_BUFFER_SECONDS,
      backBufferSeconds: DEFAULT_FORWARD_BUFFER_SECONDS,
      prefetchSeconds: HLS_PREFETCH_SECONDS,
    });
  });

  it("keeps the back buffer inside the retention window and the prefetch inside the forward target", () => {
    for (const bitrate of [0, 1_500, 8_000, 25_000, 80_000]) {
      const policy = hlsBufferPolicy(bitrate);
      expect(policy.backBufferSeconds).toBeGreaterThanOrEqual(MIN_BACK_BUFFER_SECONDS);
      expect(policy.backBufferSeconds).toBeLessThanOrEqual(MAX_BACK_BUFFER_SECONDS);
      expect(policy.prefetchSeconds).toBeLessThanOrEqual(policy.forwardBufferSeconds);
    }
  });
});

describe("bufferRetentionFloor", () => {
  it("sits the retention window behind the playhead", () => {
    expect(bufferRetentionFloor(300, 90)).toBe(210);
  });

  it("never goes below the start of the media", () => {
    expect(bufferRetentionFloor(30, 90)).toBe(0);
    expect(bufferRetentionFloor(0, 90)).toBe(0);
  });

  it("uses the clamped window, so the floor never trails by less than the floor", () => {
    // A configured 10s window would evict a 30s backward skip; the clamp keeps
    // the floor at 60s behind the playhead instead.
    expect(bufferRetentionFloor(300, 10)).toBe(300 - MIN_BACK_BUFFER_SECONDS);
  });

  it("treats a non-finite playhead as the start of the media", () => {
    expect(bufferRetentionFloor(Number.NaN, 90)).toBe(0);
    expect(bufferRetentionFloor(-5, 90)).toBe(0);
  });
});

describe("prefetchBufferTargetSeconds", () => {
  const state = {
    forwardBufferSeconds: 90,
    prefetchSeconds: 30,
  };

  it("targets the full forward buffer while playing and not seeking", () => {
    expect(prefetchBufferTargetSeconds({ ...state, playing: true, seeking: false })).toBe(90);
  });

  it("drops to the prefetch window while seeking", () => {
    expect(prefetchBufferTargetSeconds({ ...state, playing: true, seeking: true })).toBe(30);
  });

  it("drops to the prefetch window while paused", () => {
    expect(prefetchBufferTargetSeconds({ ...state, playing: false, seeking: false })).toBe(30);
  });

  it("never exceeds the forward target", () => {
    expect(
      prefetchBufferTargetSeconds({
        playing: false,
        seeking: true,
        forwardBufferSeconds: 20,
        prefetchSeconds: 30,
      }),
    ).toBe(20);
  });
});

describe("isTimeBuffered", () => {
  it("accepts a target inside a buffered range", () => {
    expect(isTimeBuffered(videoWithBuffered([[0, 40]]), 39.9)).toBe(true);
  });

  it("treats the buffered end as outside so an edge seek still reanchors", () => {
    expect(isTimeBuffered(videoWithBuffered([[0, 40]]), 40)).toBe(false);
  });

  it("accepts the buffered start", () => {
    expect(isTimeBuffered(videoWithBuffered([[10, 40]]), 10)).toBe(true);
  });

  it("checks every buffered range", () => {
    const video = videoWithBuffered([
      [0, 10],
      [30, 40],
    ]);
    expect(isTimeBuffered(video, 35)).toBe(true);
    expect(isTimeBuffered(video, 20)).toBe(false);
  });

  it("rejects everything when nothing is buffered", () => {
    expect(isTimeBuffered(videoWithBuffered([]), 0)).toBe(false);
  });
});
