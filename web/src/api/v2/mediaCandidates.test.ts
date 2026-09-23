import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchWithSession: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  fetchWithSession: mocks.fetchWithSession,
}));

import {
  adminJobIdFromLocation,
  awaitVirtualCandidatesRefresh,
  DEFAULT_REFRESH_RETRY_AFTER_MS,
  requestVirtualRelease,
  startVirtualCandidatesRefresh,
  virtualCandidatesRefreshPath,
  virtualReleaseRequestPath,
  VIRTUAL_CANDIDATES_REFRESH_PATH,
  VIRTUAL_RELEASE_REQUEST_PATH,
} from "./mediaCandidates";

function sessionResult(res: Response) {
  return { res, requestProfileId: null, requestProfileToken: null };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  mocks.fetchWithSession.mockReset();
});

describe("virtualCandidatesRefreshPath", () => {
  it("fills the one path parameter and encodes it", () => {
    expect(virtualCandidatesRefreshPath("content 1/2")).toBe(
      "/api/v2/media/content%201%2F2/virtual-candidates:refresh",
    );
    expect(VIRTUAL_CANDIDATES_REFRESH_PATH).toBe(
      "/api/v2/media/{media_id}/virtual-candidates:refresh",
    );
  });
});

describe("startVirtualCandidatesRefresh", () => {
  it("POSTs the refresh and reads the accepted job from the Location header", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(
        new Response("", {
          status: 202,
          headers: {
            Location: "/api/v2/admin/jobs/job-7",
            "Retry-After": "5",
          },
        }),
      ),
    );

    const accepted = await startVirtualCandidatesRefresh("content-1");

    expect(mocks.fetchWithSession).toHaveBeenCalledTimes(1);
    const [url, init] = mocks.fetchWithSession.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v2/media/content-1/virtual-candidates:refresh");
    expect(init.method).toBe("POST");
    expect(accepted).toEqual({ jobId: "job-7", retryAfterMs: 5000 });
  });

  it("falls back to the documented retry delay when the header is absent", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(
        new Response("", { status: 202, headers: { Location: "/api/v2/admin/jobs/job-7" } }),
      ),
    );

    await expect(startVirtualCandidatesRefresh("content-1")).resolves.toEqual({
      jobId: "job-7",
      retryAfterMs: DEFAULT_REFRESH_RETRY_AFTER_MS,
    });
  });

  it("rejects a non-2xx answer so the caller keeps its list", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(new Response("", { status: 503 })));

    await expect(startVirtualCandidatesRefresh("content-1")).rejects.toThrow("503");
  });

  it("rejects an accepted answer without a usable job location", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(new Response("", { status: 202, headers: { Location: "/somewhere/else" } })),
    );

    await expect(startVirtualCandidatesRefresh("content-1")).rejects.toThrow(
      "not acknowledged",
    );
  });
});

describe("adminJobIdFromLocation", () => {
  it("accepts an absolute URL and strips any query or fragment", () => {
    expect(
      adminJobIdFromLocation("https://silo.example/api/v2/admin/jobs/job-1?x=1#frag"),
    ).toBe("job-1");
  });

  it("rejects a location that is not an admin job", () => {
    expect(adminJobIdFromLocation("/api/v2/media/movie:heat-1995")).toBeNull();
    expect(adminJobIdFromLocation(null)).toBeNull();
    expect(adminJobIdFromLocation("")).toBeNull();
  });
});

describe("awaitVirtualCandidatesRefresh", () => {
  it("waits on the accepted job before resolving", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(
        new Response("", { status: 202, headers: { Location: "/api/v2/admin/jobs/job-7" } }),
      ),
    );
    const awaitAdminJob = vi.fn().mockResolvedValue({ id: "job-7", status: "completed" });

    await awaitVirtualCandidatesRefresh("content-1", awaitAdminJob);

    expect(awaitAdminJob).toHaveBeenCalledWith("job-7");
  });

  it("does not wait when the acceptance request fails", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(new Response("", { status: 503 })));
    const awaitAdminJob = vi.fn();

    await expect(awaitVirtualCandidatesRefresh("content-1", awaitAdminJob)).rejects.toThrow("503");
    expect(awaitAdminJob).not.toHaveBeenCalled();
  });
});

describe("virtualReleaseRequestPath", () => {
  it("fills and encodes both path parameters", () => {
    expect(virtualReleaseRequestPath("content 1", "rel/2")).toBe(
      "/api/v2/media/content%201/virtual-releases/rel%2F2:request",
    );
    expect(VIRTUAL_RELEASE_REQUEST_PATH).toBe(
      "/api/v2/media/{media_id}/virtual-releases/{release_id}:request",
    );
  });
});

describe("requestVirtualRelease", () => {
  it("POSTs the request and returns the queued state", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(jsonResponse({ release_id: "rel-1", state: "queued" })),
    );

    const result = await requestVirtualRelease("content-1", "rel-1");

    const [url, init] = mocks.fetchWithSession.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v2/media/content-1/virtual-releases/rel-1:request");
    expect(init.method).toBe("POST");
    expect(result).toEqual({ release_id: "rel-1", state: "queued", message: undefined });
  });

  it("carries a failure message through", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(jsonResponse({ release_id: "rel-1", state: "failed", message: "no provider" })),
    );

    await expect(requestVirtualRelease("content-1", "rel-1")).resolves.toEqual({
      release_id: "rel-1",
      state: "failed",
      message: "no provider",
    });
  });

  it("treats an unknown state as a failure so the row stays retryable", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(jsonResponse({ release_id: "rel-1", state: "mystery" })),
    );

    await expect(requestVirtualRelease("content-1", "rel-1")).resolves.toEqual(
      expect.objectContaining({ state: "failed" }),
    );
  });

  it("rejects on a non-2xx answer", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(new Response("", { status: 500 })));

    await expect(requestVirtualRelease("content-1", "rel-1")).rejects.toThrow("500");
  });
});
