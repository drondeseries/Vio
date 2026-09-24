// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { WatchIndexerRelease } from "@/api/types";
import { REQUEST_RELEASE_ERROR, useIndexerReleaseRequests } from "./useIndexerReleases";

const mocks = vi.hoisted(() => ({
  requestVirtualRelease: vi.fn(),
}));

// The request action lives in the hand-rolled media-candidates client; the hook
// is exercised against the mocked boundary, following mediaCandidates.test.ts.
vi.mock("@/api/v2/mediaCandidates", () => ({
  requestVirtualRelease: mocks.requestVirtualRelease,
}));

function makeRelease(overrides: Partial<WatchIndexerRelease> = {}): WatchIndexerRelease {
  return {
    release_id: overrides.release_id ?? "rel-1",
    title: overrides.title ?? "Movie.2026.2160p.WEB-DL-GRP",
    download_state: overrides.download_state ?? "not_downloaded",
    ...overrides,
  };
}

beforeEach(() => {
  mocks.requestVirtualRelease.mockReset();
});

describe("useIndexerReleaseRequests", () => {
  it("posts the row's release id to the request endpoint", async () => {
    mocks.requestVirtualRelease.mockResolvedValue({ release_id: "rel-1", state: "queued" });
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    await act(async () => {
      result.current.request(makeRelease());
    });

    expect(mocks.requestVirtualRelease).toHaveBeenCalledTimes(1);
    expect(mocks.requestVirtualRelease).toHaveBeenCalledWith("content-1", "rel-1");
  });

  it("moves idle -> requesting -> queued on success", async () => {
    let resolveRequest: (value: { release_id: string; state: "queued" }) => void = () => {};
    mocks.requestVirtualRelease.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRequest = resolve;
        }),
    );
    const release = makeRelease();
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    expect(result.current.statusFor(release)).toBe("idle");

    act(() => {
      result.current.request(release);
    });
    expect(result.current.statusFor(release)).toBe("requesting");
    expect(result.current.errorFor(release)).toBeNull();

    await act(async () => {
      resolveRequest({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });

    expect(result.current.statusFor(release)).toBe("queued");
    expect(result.current.errorFor(release)).toBeNull();
  });

  it("surfaces the server's failure message on a failed result", async () => {
    mocks.requestVirtualRelease.mockResolvedValue({
      release_id: "rel-1",
      state: "failed",
      message: "No provider is configured for this release.",
    });
    const release = makeRelease();
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    await act(async () => {
      result.current.request(release);
    });

    expect(result.current.statusFor(release)).toBe("failed");
    expect(result.current.errorFor(release)).toBe("No provider is configured for this release.");
  });

  it("surfaces the concise retryable copy on a transport rejection", async () => {
    mocks.requestVirtualRelease.mockRejectedValue(new Error("network"));
    const release = makeRelease();
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    await act(async () => {
      result.current.request(release);
    });

    expect(result.current.statusFor(release)).toBe("failed");
    expect(result.current.errorFor(release)).toBe(REQUEST_RELEASE_ERROR);
  });

  it("starts a row seeded queued by download_state in the queued state", () => {
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    expect(result.current.statusFor(makeRelease({ download_state: "queued" }))).toBe("queued");
  });

  it("starts a row seeded failed by download_state in the failed state", () => {
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    expect(result.current.statusFor(makeRelease({ download_state: "failed" }))).toBe("failed");
  });

  it("ignores a second request while the same row is requesting", async () => {
    let resolveRequest: (value: { release_id: string; state: "queued" }) => void = () => {};
    mocks.requestVirtualRelease.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRequest = resolve;
        }),
    );
    const release = makeRelease();
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    act(() => {
      result.current.request(release);
    });
    // A re-render has flushed the requesting state, as the disabled row does in
    // the UI; a repeated call must not issue a duplicate POST.
    act(() => {
      result.current.request(release);
    });

    expect(mocks.requestVirtualRelease).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveRequest({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });
  });

  it("tracks requests to different rows independently", async () => {
    const requests: Array<(value: { release_id: string; state: "queued" }) => void> = [];
    mocks.requestVirtualRelease.mockImplementation(
      (_mediaId: string, releaseId: string) =>
        new Promise((resolve) => {
          requests.push((value) => resolve({ ...value, release_id: releaseId }));
        }),
    );
    const first = makeRelease({ release_id: "rel-1" });
    const second = makeRelease({ release_id: "rel-2" });
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    act(() => {
      result.current.request(first);
      result.current.request(second);
    });

    expect(mocks.requestVirtualRelease).toHaveBeenCalledTimes(2);
    // Both rows are independently in flight; neither blocks the other.
    expect(result.current.statusFor(first)).toBe("requesting");
    expect(result.current.statusFor(second)).toBe("requesting");

    await act(async () => {
      requests[0]?.({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });

    expect(result.current.statusFor(first)).toBe("queued");
    expect(result.current.statusFor(second)).toBe("requesting");
  });

  it("ignores a request when there is no media id to post to", () => {
    const { result } = renderHook(() => useIndexerReleaseRequests(undefined));

    act(() => {
      result.current.request(makeRelease());
    });

    expect(mocks.requestVirtualRelease).not.toHaveBeenCalled();
  });

  it("resolves statusFor for a release once its request settles without leaking to others", async () => {
    mocks.requestVirtualRelease.mockResolvedValue({ release_id: "rel-1", state: "queued" });
    const { result } = renderHook(() => useIndexerReleaseRequests("content-1"));

    await act(async () => {
      result.current.request(makeRelease({ release_id: "rel-1" }));
    });

    expect(result.current.statusFor(makeRelease({ release_id: "rel-1" }))).toBe("queued");
    expect(result.current.statusFor(makeRelease({ release_id: "rel-2" }))).toBe("idle");
    expect(result.current.errorFor(makeRelease({ release_id: "rel-2" }))).toBeNull();
  });
});
