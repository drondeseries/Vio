import { useCallback, useEffect, useRef, useState } from "react";

import { v2 } from "@/api/v2/request";
import { normalizeSortCriteria, type SortCriterion } from "@/components/streaming/scoringPresets";
import { useOptionalAuth } from "@/hooks/useAuth";

/**
 * The profile-scoped setting a viewer's display order lives at. The canonical
 * settings contract owns this key; until the manifest declares
 * `playback.version_sort`, reads/writes go to the same non-admin settings
 * endpoints as other profile settings and degrade safely (the reorder still
 * works for the session; only persistence is inert).
 */
export const VERSION_SORT_SETTING_KEY = "playback.version_sort";

const PROFILE_SCOPE = { scope: "profile" } as const;

async function readStoredCriteria(): Promise<SortCriterion[]> {
  const result = await v2("GET /api/v2/settings/values/effective", {
    query: { keys: [VERSION_SORT_SETTING_KEY] },
  });
  const item = result.items.find((entry) => entry.key === VERSION_SORT_SETTING_KEY);
  return normalizeSortCriteria(item?.value);
}

/** Fire-and-forget persistence; a server that does not know the key is not an error here. */
function persistCriteria(criteria: SortCriterion[]) {
  return v2("PUT /api/v2/settings/values/{key}", {
    path: { key: VERSION_SORT_SETTING_KEY },
    query: PROFILE_SCOPE,
    body: { value: criteria },
  }).catch(() => undefined);
}

function clearStoredCriteria() {
  return v2("DELETE /api/v2/settings/values/{key}", {
    path: { key: VERSION_SORT_SETTING_KEY },
    query: PROFILE_SCOPE,
  }).catch(() => undefined);
}

export interface VersionSortPreference {
  /** The viewer's override; empty means "use the server/profile ranking". */
  criteria: SortCriterion[];
  /** Replace the override (a copy, ordered top-down). */
  apply: (criteria: SortCriterion[]) => void;
  /** Clear the override so the server/profile ranking shows again. */
  reset: () => void;
  /** True until the stored value has been read for a signed-in profile. */
  loading: boolean;
}

/**
 * Reads and writes the acting profile's version-list display order through the
 * canonical settings endpoints. Deliberately not a TanStack query: it must
 * render (and no-op) without a QueryClient, and it must never fail loudly when
 * the connected server predates the `playback.version_sort` definition.
 */
export function useVersionSortPreference(): VersionSortPreference {
  const auth = useOptionalAuth();
  const authReady = auth !== null && !auth.loading && !auth.setupLoading && auth.user !== null;
  const [criteria, setCriteria] = useState<SortCriterion[]>([]);
  const [loaded, setLoaded] = useState(false);
  // A viewer edit must win over the initial read if it resolves afterwards.
  const editedRef = useRef(false);

  useEffect(() => {
    if (!authReady) return;
    let cancelled = false;
    readStoredCriteria()
      .catch(() => [])
      .then((stored) => {
        if (cancelled) return;
        if (!editedRef.current) setCriteria(stored);
        setLoaded(true);
      });
    return () => {
      cancelled = true;
    };
  }, [authReady]);

  const apply = useCallback((next: SortCriterion[]) => {
    editedRef.current = true;
    setCriteria(next);
    void persistCriteria(next);
  }, []);

  const reset = useCallback(() => {
    editedRef.current = true;
    setCriteria([]);
    void clearStoredCriteria();
  }, []);

  return { criteria, apply, reset, loading: authReady && !loaded };
}
