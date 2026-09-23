import { useCallback, useEffect, useState } from "react";

import type { WatchIndexerRelease } from "@/api/types";
import { requestVirtualRelease } from "@/api/v2/mediaCandidates";
import {
  fetchVirtualLibraryCapability,
  type VirtualLibraryCapability,
} from "@/api/v2/virtualLibrary";

/** Concise failure copy shared by every indexer request entry. */
export const REQUEST_RELEASE_ERROR = "Couldn't request. Try again.";

/** Row states the request action moves through. */
export type IndexerReleaseRequestStatus = "idle" | "requesting" | "queued" | "failed";

export interface IndexerReleaseRequestState {
  status: IndexerReleaseRequestStatus;
  /** Server-supplied failure copy, when it sent any. */
  message?: string;
}

/** Maps a release's seed `download_state` onto a row status. */
export function seedIndexerReleaseStatus(
  state: WatchIndexerRelease["download_state"],
): IndexerReleaseRequestStatus {
  if (state === "queued") return "queued";
  if (state === "failed") return "failed";
  return "idle";
}

export interface IndexerReleaseRequests {
  /** The effective row status: a local request overrides the seed state. */
  statusFor: (release: WatchIndexerRelease) => IndexerReleaseRequestStatus;
  /** The failure copy for a row, or null. */
  errorFor: (release: WatchIndexerRelease) => string | null;
  /** Fires one request; ignored while the same row is already requesting. */
  request: (release: WatchIndexerRelease) => void;
}

/**
 * Tracks the per-row request state for one item's indexer releases. One
 * request at a time per row; a queued answer reads as requested, and a failed
 * answer keeps the row retryable.
 */
export function useIndexerReleaseRequests(mediaId?: string): IndexerReleaseRequests {
  const [overrides, setOverrides] = useState<Record<string, IndexerReleaseRequestState>>({});

  const request = useCallback(
    (release: WatchIndexerRelease) => {
      const releaseId = release.release_id;
      if (!mediaId || overrides[releaseId]?.status === "requesting") return;
      setOverrides((current) => ({ ...current, [releaseId]: { status: "requesting" } }));
      void requestVirtualRelease(mediaId, releaseId)
        .then((result) => {
          setOverrides((current) => ({
            ...current,
            [releaseId]:
              result.state === "queued"
                ? { status: "queued" }
                : { status: "failed", message: result.message },
          }));
        })
        .catch(() => {
          // A transport failure surfaces the same concise, retryable copy as
          // every other row rather than an internal error string.
          setOverrides((current) => ({
            ...current,
            [releaseId]: { status: "failed" },
          }));
        });
    },
    [mediaId, overrides],
  );

  const statusFor = useCallback(
    (release: WatchIndexerRelease): IndexerReleaseRequestStatus =>
      overrides[release.release_id]?.status ?? seedIndexerReleaseStatus(release.download_state),
    [overrides],
  );

  const errorFor = useCallback(
    (release: WatchIndexerRelease): string | null => {
      const state = overrides[release.release_id];
      if (state?.status !== "failed") return null;
      return state.message || REQUEST_RELEASE_ERROR;
    },
    [overrides],
  );

  return { statusFor, errorFor, request };
}

export interface VirtualLibraryCapabilityOptions {
  /** Skip the read entirely (e.g. no item id to gate on). */
  enabled?: boolean;
}

export interface VirtualLibraryCapabilityState {
  /** True only when the server can request releases; absent/unknown fails closed. */
  indexerRequest: boolean;
  loading: boolean;
}

/**
 * Reads the virtual-library capability without a QueryClient so both the item
 * page and the player can use it. Fails closed: while the answer is unknown or
 * the server does not advertise `indexer_request`, the indexer UI is hidden.
 */
export function useVirtualLibraryCapability(
  options: VirtualLibraryCapabilityOptions = {},
): VirtualLibraryCapabilityState {
  const enabled = options.enabled ?? true;
  const [capability, setCapability] = useState<VirtualLibraryCapability | null>(null);
  const [loading, setLoading] = useState(enabled);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    fetchVirtualLibraryCapability()
      .then((next) => {
        if (!cancelled) setCapability(next);
      })
      .catch(() => {
        // An unreachable capability surface hides the UI rather than breaking the page.
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [enabled]);

  return { indexerRequest: enabled && capability?.indexer_request === true, loading };
}
