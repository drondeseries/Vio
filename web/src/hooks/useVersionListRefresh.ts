import { useCallback, useRef, useState } from "react";

/** Concise failure copy shared by every version-list refresh entry. */
export const REFRESH_VERSIONS_ERROR = "Couldn't refresh. Try again.";

export interface VersionListRefreshState {
  /** True while the refresh request is in flight. */
  refreshing: boolean;
  /** True when a second press can cancel the in-flight refresh. */
  cancelable: boolean;
  /** Inline error copy, or null. The known rows stay rendered either way. */
  error: string | null;
  /**
   * Fires one refresh when idle. While a refresh is in flight and `onCancel` is
   * wired it cancels the running job instead, so a second press of the control
   * stops the work it started rather than being ignored.
   */
  refresh: () => void;
}

/**
 * The idle / in-flight / error state behind a "Refresh List" entry, shared by
 * the item-page version picker and the in-player version menu so both behave
 * identically: one request at a time, the list is never cleared, and a failure
 * surfaces a concise inline message.
 *
 * `onRefresh` owns the whole refresh: it starts the asynchronous job, waits for
 * it to finish, and applies the new list. The hook holds `refreshing` for the
 * lifetime of that promise, so the control stays busy until the job is done
 * rather than only until the acceptance request returns. While it is busy a
 * second press calls `onCancel` to stop the job; a rejection the user caused by
 * canceling does not surface as an error. With no `onCancel` the control is not
 * cancelable and a second press is ignored.
 */
export function useVersionListRefresh(
  onRefresh?: () => Promise<void>,
  onCancel?: () => Promise<void> | void,
): VersionListRefreshState {
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cancelledRef = useRef(false);
  const cancelable = Boolean(onCancel);

  const refresh = useCallback(() => {
    if (!onRefresh) return;
    if (refreshing) {
      if (!onCancel) return;
      // Mark the in-flight refresh as user-canceled before the promise settles
      // so its cancellation rejection is not shown as a failure.
      cancelledRef.current = true;
      void Promise.resolve(onCancel()).catch(() => setError(REFRESH_VERSIONS_ERROR));
      return;
    }
    cancelledRef.current = false;
    setRefreshing(true);
    setError(null);
    void onRefresh()
      .catch(() => {
        if (!cancelledRef.current) setError(REFRESH_VERSIONS_ERROR);
      })
      .finally(() => setRefreshing(false));
  }, [onRefresh, onCancel, refreshing]);

  return { refreshing, cancelable, error, refresh };
}
