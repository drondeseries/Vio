import { Check, Loader2 } from "lucide-react";

import type { WatchIndexerRelease } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { formatFileSize } from "@/lib/mediaFormat";
import { cn } from "@/lib/utils";
import {
  type IndexerReleaseRequests,
  type IndexerReleaseRequestStatus,
} from "@/hooks/useIndexerReleases";
import {
  indexerReleaseMeta,
  indexerReleaseTitle,
} from "@/pages/ItemDetail/components/indexerReleaseUtils";

interface IndexerReleaseListProps {
  releases: WatchIndexerRelease[];
  requests: IndexerReleaseRequests;
  /**
   * The surface the rows sit on: the player's always-dark overlay uses
   * white/alpha tokens, the item-page dropdown uses the app theme's tokens.
   */
  tone?: "dark" | "surface";
  /**
   * Set to `menuitem` inside the player's `role="menu"` so the rows join the
   * menu's keyboard navigation. Omitted in the item-page popover, which is not
   * a menu.
   */
  rowRole?: "menuitem";
  /**
   * Registers a row with the parent menu's roving-focus list so Arrow Up/Down
   * reach the indexer rows too. `index` is an absolute slot in the menu's item
   * list; `rowIndexStart` is the first slot the first row should take. Both are
   * omitted where the surface is not a keyboard-navigable menu.
   */
  registerRow?: (index: number, el: HTMLButtonElement | null) => void;
  rowIndexStart?: number;
  className?: string;
}

export function indexerReleaseStatusLabel(status: IndexerReleaseRequestStatus): string | null {
  if (status === "queued") return "Requested";
  if (status === "failed") return "Request failed";
  return null;
}

/**
 * The indexer-release rows shared by the item-page picker and the in-player
 * version menu: releases that exist on the indexers but are not downloaded on
 * the provider. Rows are never playable; each carries a "Not downloaded" badge
 * and is itself the Request action that asks the server to fetch it.
 */
export function IndexerReleaseList({
  releases,
  requests,
  tone = "surface",
  rowRole,
  registerRow,
  rowIndexStart,
  className,
}: IndexerReleaseListProps) {
  if (releases.length === 0) return null;

  const dark = tone === "dark";

  return (
    <div className={className}>
      {releases.map((release, rowIndex) => {
        const status = requests.statusFor(release);
        const error = requests.errorFor(release);
        const meta = indexerReleaseMeta(release);
        const requesting = status === "requesting";
        const queued = status === "queued";
        const sizeLabel = formatFileSize(release.size_bytes);
        // Stable accessible name across the failed/queued transitions so the
        // row keeps one identity; the visible label carries Retry/queued state.
        const accessibleName = queued ? `Requested ${release.title}` : `Request ${release.title}`;

        return (
          <button
            key={release.release_id}
            ref={
              registerRow && rowIndexStart !== undefined
                ? (el) => registerRow(rowIndexStart + rowIndex, el)
                : undefined
            }
            type="button"
            role={rowRole}
            data-indexer-release={release.release_id}
            data-status={status}
            disabled={requesting || queued}
            aria-busy={requesting || undefined}
            aria-label={accessibleName}
            title={sizeLabel ? `${release.title} · ${sizeLabel}` : release.title}
            onClick={() => requests.request(release)}
            className={cn(
              "flex w-full items-start gap-3 rounded-lg px-3 py-2.5 text-left transition-colors",
              "disabled:cursor-not-allowed disabled:opacity-60",
              // Secondary to the playable rows: muted, and a request trigger
              // rather than a play affordance.
              dark
                ? "text-white/60 hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none disabled:hover:bg-transparent"
                : "text-muted-foreground hover:bg-accent/50 disabled:hover:bg-transparent",
            )}
          >
            <span className="min-w-0 flex-1">
              <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span
                  className={cn(
                    "truncate text-sm font-medium",
                    dark ? "text-white/80" : "text-foreground",
                  )}
                >
                  {indexerReleaseTitle(release)}
                </span>
                <Badge
                  variant="outline"
                  data-state={status}
                  className={cn(
                    "shrink-0 px-1.5 py-0 text-[10px] font-medium",
                    queued
                      ? dark
                        ? "border-emerald-500/30 bg-emerald-500/15 text-emerald-300"
                        : "border-emerald-500/30 bg-emerald-500/15 text-emerald-600 dark:text-emerald-300"
                      : dark
                        ? "border-white/15 bg-white/10 text-white/60"
                        : "border-border bg-muted/60 text-muted-foreground",
                  )}
                >
                  {queued ? "Requested" : "Not downloaded"}
                </Badge>
              </span>
              {meta ? (
                <span
                  className={cn(
                    "mt-1 block text-xs",
                    dark ? "text-white/50" : "text-muted-foreground",
                  )}
                >
                  {meta}
                </span>
              ) : null}
              {error ? (
                <span
                  className={cn(
                    "mt-1 block text-[10px] leading-tight",
                    dark ? "text-red-400" : "text-destructive",
                  )}
                >
                  {error}
                </span>
              ) : null}
            </span>
            <span
              className={cn(
                "mt-0.5 flex shrink-0 items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs font-medium",
                dark ? "border-white/15 bg-white/10 text-white/80" : "border-border bg-background",
              )}
            >
              {requesting ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
              {queued ? null : (
                <span className={cn("text-foreground", dark && "text-white/80")}>
                  {status === "failed" ? "Retry" : "Request"}
                </span>
              )}
              {queued ? <Check className="size-3.5" aria-hidden="true" /> : null}
            </span>
          </button>
        );
      })}
    </div>
  );
}
