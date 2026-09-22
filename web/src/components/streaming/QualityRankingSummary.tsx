import { useMemo } from "react";

import { useAdminServerSettings } from "@/hooks/queries/admin/settings";
import { useOptionalAuth } from "@/hooks/useAuth";
import { isActingAdmin } from "@/lib/permissions";
import { resolveProfileSortCriteria } from "@/lib/qualityRanking";
import { cn } from "@/lib/utils";

import { formatSortCriteriaSummary, type SortCriterion } from "./scoringPresets";

interface QualityRankingSummaryProps {
  /** The `?profile=` label the ranked candidates carry, when there is one. */
  profileLabel?: string | null;
  className?: string;
}

function RankingLine({
  criteria,
  profileLabel,
  className,
}: {
  criteria: SortCriterion[];
  profileLabel?: string | null;
  className?: string;
}) {
  const summary = formatSortCriteriaSummary(criteria);
  return (
    <p
      className={cn("text-muted-foreground text-[11px]", className)}
      title={profileLabel ? `Quality profile: ${profileLabel}` : undefined}
    >
      Ranking: {summary || "Default ranking"}
      {profileLabel ? <span className="opacity-70"> · {profileLabel}</span> : null}
    </p>
  );
}

function AdminRankingSummary({
  profileLabel,
  className,
}: {
  profileLabel?: string | null;
  className?: string;
}) {
  const { data: settings } = useAdminServerSettings();
  const criteria = useMemo(
    () => resolveProfileSortCriteria(settings, profileLabel),
    [settings, profileLabel],
  );
  return <RankingLine criteria={criteria} profileLabel={profileLabel} className={className} />;
}

/**
 * One-line indicator of the ordering currently in effect for a version list,
 * derived from the active quality profile's sort criteria. The profile JSON is
 * only readable by an admin account, so a non-admin (or a title with no
 * `?profile=` selector) shows the default-order hint rather than a guessed
 * order.
 */
export function QualityRankingSummary({ profileLabel, className }: QualityRankingSummaryProps) {
  // Read the auth context directly (not useIsActingAdmin) so this can render
  // with no providers — the player and picker tests mount it bare — and so the
  // admin-settings query is never issued for a non-admin.
  const auth = useOptionalAuth();
  if (!isActingAdmin(auth?.user, auth?.profile)) {
    return <RankingLine criteria={[]} profileLabel={profileLabel} className={className} />;
  }
  return <AdminRankingSummary profileLabel={profileLabel} className={className} />;
}
