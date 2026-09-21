import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

import { formatSortCriterion, normalizeSortCriteria, type SortCriterion } from "./scoringPresets";

interface SortCriteriaSummaryProps {
  criteria?: SortCriterion[] | null;
  className?: string;
}

/**
 * Compact read-only view of a profile's sort criteria: one chip per criterion
 * in evaluation order, or the default-order hint when the profile has none.
 */
export function SortCriteriaSummary({ criteria, className }: SortCriteriaSummaryProps) {
  const list = normalizeSortCriteria(criteria);

  if (list.length === 0) {
    return (
      <span className={cn("text-muted-foreground text-[10px] italic", className)}>
        Default order
      </span>
    );
  }

  return (
    <div className={cn("flex flex-wrap items-center gap-1", className)}>
      {list.map((criterion, index) => (
        <Badge
          key={`${criterion.attribute}-${index}`}
          variant="outline"
          className="bg-muted/40 text-muted-foreground font-mono text-[10px] font-medium"
        >
          {formatSortCriterion(criterion)}
        </Badge>
      ))}
    </div>
  );
}
