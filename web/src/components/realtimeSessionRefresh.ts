import type { QueryClient } from "@tanstack/react-query";
import { isCapturedProfileAuthorityActive, type ProfileRequestContextSnapshot } from "@/api/client";
import { adminSessionsKey } from "@/api/v2/adminSessionsCache";
import { adminKeys } from "@/hooks/queries/keys";

const SESSION_REFRESH_WINDOW_MS = 5_000;

// Session frames omit v2 fields and contain at most 200 rows. Keep the HTTP
// reader authoritative, but combine bursts instead of walking every page for
// every worker update. Only incoming events schedule work; this is not a poll.
export function createSessionRefreshScheduler(
  queryClient: QueryClient,
  authority: ProfileRequestContextSnapshot,
  allowDashboardUpdates: () => boolean,
) {
  const queryKey = adminSessionsKey(authority);
  let timer: number | undefined;
  let refreshing = false;
  let pending = false;
  let cancelled = false;

  const flush = () => {
    if (cancelled || !authority.profileId || !isCapturedProfileAuthorityActive(authority)) return;
    const refetchType = allowDashboardUpdates() ? "active" : "none";
    // An initial read may already be in flight. Preserve it, then read again
    // once to include changes that arrived after that request began.
    pending =
      refetchType === "active" && queryClient.getQueryState(queryKey)?.fetchStatus === "fetching";
    refreshing = true;
    const settle = () => {
      refreshing = false;
      if (cancelled) return;
      timer = window.setTimeout(() => {
        timer = undefined;
        if (pending) flush();
      }, SESSION_REFRESH_WINDOW_MS);
    };
    void Promise.all([
      queryClient.invalidateQueries(
        { queryKey, exact: true, refetchType },
        { cancelRefetch: false },
      ),
      queryClient.invalidateQueries(
        { queryKey: adminKeys.stats(), refetchType },
        { cancelRefetch: false },
      ),
    ]).then(settle, settle);
  };

  return {
    schedule() {
      if (cancelled) return;
      pending = true;
      if (!refreshing && timer === undefined) flush();
    },
    cancel() {
      cancelled = true;
      pending = false;
      if (timer !== undefined) window.clearTimeout(timer);
    },
  };
}
