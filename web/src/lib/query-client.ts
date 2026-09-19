import { QueryClient } from "@tanstack/react-query";

/**
 * True for a 4xx API failure: the request is invalid or the resource is gone,
 * so retrying it cannot succeed. Transient failures (5xx, network) have no
 * client status and stay retryable.
 */
export function isClientError(error: unknown): boolean {
  const status = (error as { status?: unknown } | null | undefined)?.status;
  return typeof status === "number" && status >= 400 && status < 500;
}

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 2 * 60_000,
      gcTime: 10 * 60_000,
      // One retry for transient failures; a 4xx (a dead catalog item's 404)
      // is permanent, so retrying it just doubles the request.
      retry: (failureCount, error) => !isClientError(error) && failureCount < 1,
      refetchOnWindowFocus: false,
      refetchOnReconnect: true,
      throwOnError: false,
    },
    mutations: {
      retry: 0,
    },
  },
});
