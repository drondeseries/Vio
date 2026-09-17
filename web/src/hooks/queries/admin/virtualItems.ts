import { useQuery } from "@tanstack/react-query";

import { v2 } from "@/api/v2/request";
import type { components } from "@/api/v2/schema";
import { adminKeys } from "@/hooks/queries/keys";

/** One zero-storage catalog item as the admin virtual-items listing reports it. */
export type AdminVirtualItem = components["schemas"]["AdminVirtualItem"];

const ADMIN_VIRTUAL_ITEMS_STALE_TIME = 30_000;

export function fetchAdminVirtualItems(
  limit: number,
  signal?: AbortSignal,
): Promise<AdminVirtualItem[]> {
  return v2("GET /api/v2/admin/virtual-items", { query: { limit }, signal }).then(
    (page) => page.items,
  );
}

/**
 * Lists the zero-storage items plugins have registered into virtual libraries.
 * The listing is bounded rather than cursor paginated, so callers pass the
 * page size they want and refresh on demand instead of polling.
 */
export function useAdminVirtualItems(limit = 100) {
  return useQuery({
    queryKey: adminKeys.virtualItems(limit),
    queryFn: ({ signal }) => fetchAdminVirtualItems(limit, signal),
    staleTime: ADMIN_VIRTUAL_ITEMS_STALE_TIME,
  });
}
