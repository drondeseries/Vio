import { fetchWithSession } from "@/api/client";
import type { FileVersion } from "@/api/types";
import { watchFileVersionsFromV2, type WatchFileVersionV2 } from "./watch";

/**
 * Asks the server to re-list a title's video candidates.
 *
 * The version-candidates menu reads its rows from the watch detail. This
 * endpoint is the explicit "re-list now" trigger: the server preserves any
 * candidate already known to be working, so a successful answer never drops a
 * row the viewer could still play. The client must not use a failed or
 * in-flight refresh to shrink the list it already has.
 *
 * The endpoint is not in the committed v2 OpenAPI document yet, so the request
 * is hand-rolled through the shared session client instead of `v2(...)`. The
 * path lives in this one template so it is easy to change when the backend
 * endpoint lands.
 */
export const VIRTUAL_CANDIDATES_REFRESH_PATH =
  "/api/v2/media/{media_id}/virtual-candidates:refresh";

export function virtualCandidatesRefreshPath(mediaId: string): string {
  return VIRTUAL_CANDIDATES_REFRESH_PATH.replace("{media_id}", encodeURIComponent(mediaId));
}

/**
 * The refreshed list, in the same wire shape as `WatchDetail.versions`. Kept
 * local because the endpoint is not contract-generated yet; if the backend
 * settles on a different shape this adapter is the only thing to change.
 */
interface VirtualCandidatesRefreshPayload {
  versions?: WatchFileVersionV2[];
}

export async function refreshVirtualCandidates(mediaId: string): Promise<FileVersion[]> {
  const { res } = await fetchWithSession(virtualCandidatesRefreshPath(mediaId), {
    method: "POST",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`Refreshing the version list failed (${res.status}).`);
  }
  const body = (await res.json()) as VirtualCandidatesRefreshPayload;
  if (!Array.isArray(body?.versions)) {
    throw new Error("The refreshed version list was not in the expected shape.");
  }
  return watchFileVersionsFromV2(body.versions);
}
