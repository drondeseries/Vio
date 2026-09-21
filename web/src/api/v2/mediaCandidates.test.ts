import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchWithSession: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  fetchWithSession: mocks.fetchWithSession,
}));

import {
  refreshVirtualCandidates,
  virtualCandidatesRefreshPath,
  VIRTUAL_CANDIDATES_REFRESH_PATH,
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

const candidate = {
  file_id: "9",
  resolution: "720p",
  codec_video: "h264",
  codec_audio: "aac",
  hdr: false,
  container: "mp4",
  file_size: 1234,
  bitrate: 5_000_000,
  duration_seconds: 120,
  added_at: "2026-01-02T03:04:05.000Z",
};

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

describe("refreshVirtualCandidates", () => {
  it("POSTs the refresh and returns the server's candidate list", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(jsonResponse({ versions: [candidate] })),
    );

    const versions = await refreshVirtualCandidates("content-1");

    expect(mocks.fetchWithSession).toHaveBeenCalledTimes(1);
    const [url, init] = mocks.fetchWithSession.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v2/media/content-1/virtual-candidates:refresh");
    expect(init.method).toBe("POST");
    expect(versions).toEqual([
      expect.objectContaining({
        file_id: 9,
        resolution: "720p",
        duration: 120,
        file_size: 1234,
      }),
    ]);
  });

  it("throws on a non-2xx answer so the caller keeps its list", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(new Response("", { status: 503 })));

    await expect(refreshVirtualCandidates("content-1")).rejects.toThrow("503");
  });

  it("throws when the payload is not a version list", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(jsonResponse({})));

    await expect(refreshVirtualCandidates("content-1")).rejects.toThrow(
      "not in the expected shape",
    );
  });
});
