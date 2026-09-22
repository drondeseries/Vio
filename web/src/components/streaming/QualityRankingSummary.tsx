import { useState } from "react";

import {
  matchVersionSortPreset,
  VERSION_SORT_ATTRIBUTES,
  VERSION_SORT_PRESETS,
  type ServerVersionRanking,
} from "@/lib/qualityRanking";
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
  /**
   * The surface the control sits on: the player's always-dark overlay uses
   * white/alpha tokens, the item-page dropdown uses the app theme's tokens.
   */
  tone?: "dark" | "surface";
  className?: string;
}

function chipClass(tone: "dark" | "surface", active: boolean) {
  const base =
    "min-h-8 shrink-0 rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors focus-visible:ring-2 focus-visible:outline-none";
  if (tone === "dark") {
    return cn(
      base,
      "focus-visible:ring-white/70",
      active
        ? "border-transparent bg-white text-black"
        : "border-white/15 text-white/70 hover:bg-white/10 hover:text-white",
    );
  }
  return cn(
    base,
    "focus-visible:ring-ring",
    active
      ? "border-transparent bg-primary text-primary-foreground"
      : "border-border/70 text-muted-foreground hover:bg-muted hover:text-foreground",
  );
}

function rowClass(tone: "dark" | "surface", active: boolean) {
  if (tone === "dark") {
    return cn(
      "min-h-8 flex-1 rounded px-2 py-1 text-left text-xs transition-colors",
      active ? "bg-white/15 font-medium text-white" : "text-white/70 hover:bg-white/10",
    );
  }
  return cn(
    "min-h-8 flex-1 rounded px-2 py-1 text-left text-xs transition-colors",
    active
      ? "bg-accent text-accent-foreground font-medium"
      : "text-muted-foreground hover:bg-accent/50",
  );
}

/**
 * The version list's ordering control: one-tap named presets in the list
 * header, with the active one highlighted. "Custom…" reveals the attribute
 * fine-tuner inline — no popover — and the profile-default preset is the reset.
 * The "Ranking: …" line stays as the compact summary. Ordering is display-only:
 * it re-orders the list the viewer sees and never changes what plays.
 */
export function QualityRankingSummary({
  serverRanking,
  userCriteria,
  effectiveCriteria,
  onApply,
  onReset,
  tone = "surface",
  className,
}: QualityRankingSummaryProps) {
  const [customOpen, setCustomOpen] = useState(false);
  const activePreset = matchVersionSortPreset(userCriteria);
  const summary = formatSortCriteriaSummary(effectiveCriteria);
  const custom = userCriteria.length > 0;
  const secondaryText = tone === "dark" ? "text-white/60" : "text-muted-foreground";
  const helperText = tone === "dark" ? "text-white/50" : "text-muted-foreground/90";

  const toggleAttribute = (attribute: SortAttribute) => {
    const isActive = effectiveCriteria.some((criterion) => criterion.attribute === attribute);
    const next = effectiveCriteria.filter((criterion) => criterion.attribute !== attribute);
    if (!isActive) next.push({ attribute, direction: "desc" });
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
    <div className={cn("space-y-1", className)}>
      <div role="group" aria-label="Version order" className="flex flex-wrap items-center gap-1">
        {VERSION_SORT_PRESETS.map((preset) => (
          <button
            key={preset.id}
            type="button"
            aria-pressed={activePreset === preset.id}
            title={preset.description}
            onClick={() => (preset.id === "profile" ? onReset() : onApply(preset.criteria))}
            className={chipClass(tone, activePreset === preset.id)}
          >
            {preset.label}
          </button>
        ))}
        <button
          type="button"
          aria-pressed={activePreset === "custom"}
          aria-expanded={customOpen}
          title="Fine-tune the order"
          onClick={() => setCustomOpen((open) => !open)}
          className={chipClass(tone, activePreset === "custom")}
        >
          Custom…
        </button>
      </div>

      <p className={cn("truncate text-[11px]", secondaryText)}>
        Ranking: {summary || "Default ranking"}
        {serverRanking.profileLabel ? (
          <span className="opacity-70"> · {serverRanking.profileLabel}</span>
        ) : null}
      </p>
      <p className={cn("text-[10px] leading-tight", helperText)}>
        Applies to this list only — playback selection is unchanged.
      </p>

      {customOpen && (
        <div className="space-y-1 pt-0.5">
          {VERSION_SORT_ATTRIBUTES.map((attribute) => {
            const active = effectiveCriteria.find((criterion) => criterion.attribute === attribute);
            return (
              <div key={attribute} className="flex items-center justify-between gap-2">
                <button
                  type="button"
                  aria-pressed={Boolean(active)}
                  onClick={() => toggleAttribute(attribute)}
                  className={rowClass(tone, Boolean(active))}
                >
                  {SORT_ATTRIBUTE_LABELS[attribute]}
                </button>
                {active ? (
                  <button
                    type="button"
                    onClick={() => toggleDirection(attribute)}
                    aria-label={`Direction for ${SORT_ATTRIBUTE_LABELS[attribute]}`}
                    className={cn(
                      "flex size-8 items-center justify-center rounded text-xs focus-visible:ring-2 focus-visible:outline-none",
                      tone === "dark"
                        ? "hover:bg-white/10 focus-visible:ring-white/70"
                        : "hover:bg-accent focus-visible:ring-ring",
                    )}
                  >
                    {SORT_DIRECTION_SYMBOLS[active.direction]}
                  </button>
                ) : (
                  <span className="text-muted-foreground/40 w-8 text-center text-xs">·</span>
                )}
              </div>
            );
          })}
          <button
            type="button"
            onClick={onReset}
            disabled={!custom}
            className={cn(
              "w-full rounded px-2 py-1 text-left text-[11px] transition-colors focus-visible:ring-2 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-40",
              tone === "dark"
                ? "text-white/60 hover:text-white focus-visible:ring-white/70"
                : "text-muted-foreground hover:text-foreground focus-visible:ring-ring",
            )}
          >
            Reset to profile default
          </button>
        </div>
      )}
    </div>
  );
}
