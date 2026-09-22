import { ChevronDown } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { VERSION_SORT_ATTRIBUTES, type ServerVersionRanking } from "@/lib/qualityRanking";
import { cn } from "@/lib/utils";

import {
  formatSortCriteriaSummary,
  SORT_ATTRIBUTE_LABELS,
  SORT_DIRECTION_SYMBOLS,
  type SortAttribute,
  type SortCriterion,
} from "./scoringPresets";

interface QualityRankingSummaryProps {
  /** The server/profile ranking shown when the viewer has no override. */
  serverRanking: ServerVersionRanking;
  /** The viewer's override; empty means "use the server ranking". */
  userCriteria: SortCriterion[];
  /** `userCriteria` when set, otherwise the server criteria — what the menu applies. */
  effectiveCriteria: SortCriterion[];
  onApply: (criteria: SortCriterion[]) => void;
  onReset: () => void;
  className?: string;
}

/**
 * The "Ranking: …" line, now an interactive control. It shows the ordering in
 * effect for the version list and opens a small popover of the attributes the
 * delivered payload can sort by. This is display-only: it re-orders the list
 * the viewer sees and never changes what plays automatically.
 */
export function QualityRankingSummary({
  serverRanking,
  userCriteria,
  effectiveCriteria,
  onApply,
  onReset,
  className,
}: QualityRankingSummaryProps) {
  const summary = formatSortCriteriaSummary(effectiveCriteria);
  const custom = userCriteria.length > 0;

  const toggleAttribute = (attribute: SortAttribute) => {
    const active = effectiveCriteria.some((criterion) => criterion.attribute === attribute);
    const next = effectiveCriteria.filter((criterion) => criterion.attribute !== attribute);
    if (!active) next.push({ attribute, direction: "desc" });
    onApply(next);
  };

  const toggleDirection = (attribute: SortAttribute) => {
    onApply(
      effectiveCriteria.map((criterion) =>
        criterion.attribute === attribute
          ? { ...criterion, direction: criterion.direction === "desc" ? "asc" : "desc" }
          : criterion,
      ),
    );
  };

  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label="Version ranking"
          className={cn(
            "text-muted-foreground hover:text-foreground flex w-full items-center gap-1 text-left text-[11px]",
            className,
          )}
        >
          <span className="truncate">
            Ranking: {summary || "Default ranking"}
            {serverRanking.profileLabel ? (
              <span className="opacity-70"> · {serverRanking.profileLabel}</span>
            ) : null}
            {custom ? <span className="opacity-70"> · Custom</span> : null}
          </span>
          <ChevronDown className="size-3 shrink-0 opacity-60" aria-hidden="true" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-72 p-3">
        <p className="text-muted-foreground text-[11px]">
          Applies to this list only — playback selection is unchanged.
        </p>
        <div className="mt-2 space-y-1">
          {VERSION_SORT_ATTRIBUTES.map((attribute) => {
            const active = effectiveCriteria.find((criterion) => criterion.attribute === attribute);
            return (
              <div key={attribute} className="flex items-center justify-between gap-2">
                <button
                  type="button"
                  aria-pressed={Boolean(active)}
                  onClick={() => toggleAttribute(attribute)}
                  className={cn(
                    "flex-1 rounded px-2 py-1 text-left text-xs",
                    active
                      ? "bg-accent text-accent-foreground font-medium"
                      : "text-muted-foreground hover:bg-accent/50",
                  )}
                >
                  {SORT_ATTRIBUTE_LABELS[attribute]}
                </button>
                {active ? (
                  <button
                    type="button"
                    onClick={() => toggleDirection(attribute)}
                    aria-label={`Direction for ${SORT_ATTRIBUTE_LABELS[attribute]}`}
                    className="hover:bg-accent flex size-6 items-center justify-center rounded text-xs"
                  >
                    {SORT_DIRECTION_SYMBOLS[active.direction]}
                  </button>
                ) : (
                  <span className="text-muted-foreground/40 w-6 text-center text-xs">·</span>
                )}
              </div>
            );
          })}
        </div>
        <button
          type="button"
          onClick={onReset}
          disabled={!custom}
          className="text-muted-foreground hover:text-foreground mt-2 w-full rounded px-2 py-1 text-left text-[11px] transition-colors disabled:cursor-not-allowed disabled:opacity-40"
        >
          Reset to profile default
        </button>
      </PopoverContent>
    </Popover>
  );
}
