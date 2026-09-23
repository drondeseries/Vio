import { useCallback, useState } from "react";

/** Concise failure copy shared by every version-list refresh entry. */
export const REFRESH_VERSIONS_ERROR = "Couldn't refresh. Try again.";

export interface VersionListRefreshState {
  /** True while the refresh request is in flight; the caller disables the row. */
  refreshing: boolean;
  /** Inline error copy, or null. The known rows stay rendered either way. */
  error: string | null;
  /** Fires one refresh; ignored while one is already in flight. */
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
 * lifetime of that promise, so the control stays disabled until the job is
 * done rather than only until the acceptance request returns.
 */
export function useVersionListRefresh(onRefresh?: () => Promise<void>): VersionListRefreshState {
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    if (!onRefresh || refreshing) return;
    setRefreshing(true);
    setError(null);
    void onRefresh()
      .catch(() => setError(REFRESH_VERSIONS_ERROR))
      .finally(() => setRefreshing(false));
  }, [onRefresh, refreshing]);

  return { refreshing, error, refresh };
}
