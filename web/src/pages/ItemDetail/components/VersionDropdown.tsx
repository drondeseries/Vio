import { memo, useMemo, useState } from "react";
import { Check, ChevronDown, Disc3, Layers3, RefreshCw } from "lucide-react";

import type { FileVersion, PlaybackVariant } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { QualityRankingSummary } from "@/components/streaming/QualityRankingSummary";
import { useVersionListRefresh } from "@/hooks/useVersionListRefresh";
import { useVersionSortPreference } from "@/hooks/useVersionSortPreference";
import { sortVersionsByCriteria, versionSortableFromFile } from "@/lib/qualityRanking";
import { videoRangeLabel } from "@/lib/videoRange";
import DetailPopover from "./DetailPopover";
import { sortPlaybackVariantsByEditionPreference } from "./versionRankingUtils";
import {
  collectLanguageLabels,
  profileLabelFromFilePath,
  serverRankingFromVersions,
} from "./versionFormatUtils";
import { buildDetailLine, buildQualitySummary, sortByResolution } from "./VersionFlyout";
import { isVersionUnavailable, useVersionVisibility } from "./versionAvailability";

interface VersionDropdownProps {
  versions: FileVersion[];
  playbackVariants?: PlaybackVariant[];
  selectedVersion: FileVersion | null;
  onSelectVersion: (version: FileVersion) => void;
  /** Fired whenever a picker popover opens or closes (open=true on open). */
  onOpenChange?: (open: boolean) => void;
  /**
   * Re-lists the title's video candidates for the version menu. Resolves once
   * the refreshed list has been applied to the page; rejecting keeps the rows
   * already on screen. Omitted when the page cannot refresh.
   */
  onRefreshVersions?: () => Promise<void>;
}

interface EditionOption {
  id: string;
  label: string;
  variant: PlaybackVariant;
  defaultVersion: FileVersion;
  versions: FileVersion[];
}

function VersionDropdown({
  versions,
  playbackVariants,
  selectedVersion,
  onSelectVersion,
  onOpenChange,
  onRefreshVersions,
}: VersionDropdownProps) {
  const [editionOpen, setEditionOpen] = useState(false);
  const [versionOpen, setVersionOpen] = useState(false);
  const {
    refreshing: refreshingVersions,
    error: refreshVersionsError,
    refresh: handleRefreshVersions,
  } = useVersionListRefresh(onRefreshVersions);

  const handleEditionOpenChange = (open: boolean) => {
    setEditionOpen(open);
    onOpenChange?.(open);
  };
  const handleVersionOpenChange = (open: boolean) => {
    setVersionOpen(open);
    onOpenChange?.(open);
  };

  const sorted = useMemo(() => sortByResolution(versions), [versions]);
  const editionOptions = useMemo(
    () => buildEditionOptions(playbackVariants, versions),
    [playbackVariants, versions],
  );

  const hasNamedEditions = editionOptions.some((option) => option.variant.edition_key);
  const showEditionDropdown =
    editionOptions.length > 1 &&
    new Set(editionOptions.map((option) => option.label.toLowerCase())).size > 1 &&
    hasNamedEditions;

  const selectedEdition = showEditionDropdown
    ? resolveSelectedEditionOption(editionOptions, selectedVersion)
    : null;
  const activeVersions = selectedEdition?.versions ?? sorted;
  const activeVersion =
    selectedVersion ?? selectedEdition?.defaultVersion ?? activeVersions[0] ?? null;
  const showVersionDropdown = activeVersions.length > 1;

  // The viewer's per-profile display order. It only re-orders the list below;
  // the server's auto-pick is untouched.
  const { criteria: userCriteria, apply: applySort, reset: resetSort } = useVersionSortPreference();
  // The catalog item detail this picker reads carries no virtual_ranking (the
  // server publishes it only on the v2 watch detail), so this falls back to the
  // candidates' `?profile=` label with the default order. The in-player menu
  // reads the real ranking from the watch detail.
  const serverRanking = useMemo(() => serverRankingFromVersions(activeVersions), [activeVersions]);
  const effectiveCriteria = userCriteria.length > 0 ? userCriteria : serverRanking.criteria;
  const orderedVersions = useMemo(
    () => sortVersionsByCriteria(activeVersions, effectiveCriteria, versionSortableFromFile),
    [activeVersions, effectiveCriteria],
  );

  const { visibleVersions, hiddenUnavailableCount, setShowUnavailable } = useVersionVisibility(
    orderedVersions,
    activeVersion?.file_id,
  );

  if (!showEditionDropdown && !showVersionDropdown) {
    return null;
  }

  return (
    <>
      {showEditionDropdown && selectedEdition ? (
        <DetailPopover
          open={editionOpen}
          onOpenChange={handleEditionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Layers3 className="size-3.5" />
              Edition
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {selectedEdition.label}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            {editionOptions.map((option) => {
              const isSelected = option.id === selectedEdition.id;
              const detail = buildEditionDetail(option);

              return (
                <button
                  key={option.id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(option.defaultVersion);
                    setEditionOpen(false);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  }`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium">{option.label}</div>
                    {detail && <div className="text-muted-foreground text-xs">{detail}</div>}
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
          </div>
        </DetailPopover>
      ) : null}

      {showVersionDropdown ? (
        <DetailPopover
          open={versionOpen}
          onOpenChange={handleVersionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Disc3 className="size-3.5" />
              Version
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {activeVersion ? buildVersionTriggerSummary(activeVersion) : ""}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            <QualityRankingSummary
              serverRanking={serverRanking}
              userCriteria={userCriteria}
              effectiveCriteria={effectiveCriteria}
              onApply={applySort}
              onReset={resetSort}
              className="px-3 pt-1.5 pb-0.5"
            />
            {visibleVersions.map((version) => {
              const isSelected = version.file_id === activeVersion?.file_id;
              const summary = buildQualitySummary(version);
              const detail = buildDetailLine(version);
              const rangeLabel = videoRangeLabel(version);
              const unavailable = isVersionUnavailable(version);
              const versionProfileLabel = profileLabelFromFilePath(version.file_path);

              return (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(version);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  } ${unavailable && !isSelected ? "opacity-80" : ""}`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium">
                          {summary || `Version ${version.file_id}`}
                        </span>
                        {rangeLabel ? (
                          <Badge variant="secondary" className="px-1.5 py-0 text-[10px] uppercase">
                            {rangeLabel}
                          </Badge>
                        ) : null}
                        {unavailable ? (
                          <Badge
                            variant="outline"
                            className="border-amber-500/30 bg-amber-500/15 px-1.5 py-0 text-[10px] font-medium text-amber-600 dark:text-amber-300"
                          >
                            Will retry on play
                          </Badge>
                        ) : null}
                        {typeof version.format_score === "number" && version.format_score !== 0 ? (
                          <Badge
                            variant="outline"
                            className="text-muted-foreground bg-muted/40 px-1.5 py-0 font-mono text-[10px] font-medium"
                            title={`Format score ${version.format_score}${
                              versionProfileLabel ? ` · ${versionProfileLabel}` : ""
                            }`}
                          >
                            ★ {version.format_score}
                          </Badge>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap gap-1">
                        {collectLanguageLabels(
                          version.audio_tracks?.map((t) => t.language) ?? [],
                        ).map((lang) => (
                          <Badge
                            key={lang}
                            variant="outline"
                            className="border-blue-500/20 bg-blue-500/10 px-1 py-0 text-[10px] font-medium text-blue-400"
                          >
                            <span className="mr-0.5 opacity-70">🔊</span>
                            {lang}
                          </Badge>
                        ))}
                      </div>
                    </div>
                    {detail && (
                      <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="text-muted-foreground text-xs">{detail}</span>
                        <div className="flex flex-wrap gap-1">
                          {collectLanguageLabels(
                            version.subtitle_tracks?.map((t) => t.language) ?? [],
                          ).map((lang) => (
                            <Badge
                              key={lang}
                              variant="outline"
                              className="border-amber-500/20 bg-amber-500/10 px-1 py-0 text-[10px] font-medium text-amber-400"
                            >
                              <span className="mr-0.5 opacity-70">CC</span>
                              {lang}
                            </Badge>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
            {hiddenUnavailableCount > 0 && (
              <button
                type="button"
                onClick={() => setShowUnavailable(true)}
                className="text-muted-foreground hover:bg-accent/50 hover:text-foreground w-full rounded-lg px-3 py-2 text-left text-xs font-medium transition-colors"
              >
                Show {hiddenUnavailableCount} unavailable{" "}
                {hiddenUnavailableCount === 1 ? "version" : "versions"}
              </button>
            )}
            {onRefreshVersions ? (
              <button
                type="button"
                disabled={refreshingVersions}
                aria-busy={refreshingVersions || undefined}
                onClick={() => handleRefreshVersions()}
                className="text-muted-foreground hover:bg-accent/50 hover:text-foreground flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-60"
              >
                <RefreshCw
                  className={`size-3.5 shrink-0 ${refreshingVersions ? "animate-spin" : ""}`}
                  aria-hidden="true"
                />
                <span className="flex min-w-0 flex-col">
                  <span>Refresh List</span>
                  {refreshVersionsError ? (
                    <span className="text-destructive text-[10px] leading-tight">
                      {refreshVersionsError}
                    </span>
                  ) : null}
                </span>
              </button>
            ) : null}
          </div>
        </DetailPopover>
      ) : null}
    </>
  );
}

export default memo(VersionDropdown);

function buildVersionTriggerSummary(version: FileVersion): string {
  return buildQualitySummary(version) || buildDetailLine(version) || `Version ${version.file_id}`;
}

function buildEditionOptions(
  playbackVariants: PlaybackVariant[] | undefined,
  versions: FileVersion[],
): EditionOption[] {
  if (!playbackVariants || playbackVariants.length === 0) {
    return [];
  }

  const orderedVariants = sortPlaybackVariantsByEditionPreference(playbackVariants);
  const hasNamedEditions = orderedVariants.some((variant) => variant.edition_key);

  return orderedVariants
    .map((variant) => {
      const firstPart = [...(variant.parts ?? [])].sort((a, b) => a.part_index - b.part_index)[0];
      if (!firstPart) {
        return null;
      }

      const partVersions = sortByResolution([...(firstPart.versions ?? [])]);
      const defaultVersion =
        (firstPart.default_file_id != null
          ? versions.find((version) => version.file_id === firstPart.default_file_id)
          : undefined) ?? partVersions[0];
      if (!defaultVersion) {
        return null;
      }

      return {
        id: variant.variant_id,
        label: buildEditionLabel(variant, hasNamedEditions),
        variant,
        defaultVersion,
        versions: partVersions,
      };
    })
    .filter((entry): entry is EditionOption => !!entry);
}

function resolveSelectedEditionOption(
  editionOptions: EditionOption[],
  selectedVersion: FileVersion | null,
): EditionOption | null {
  if (editionOptions.length === 0) {
    return null;
  }

  if (selectedVersion) {
    const matching = editionOptions.find((option) =>
      option.variant.parts.some((part) =>
        part.versions.some((candidate) => candidate.file_id === selectedVersion.file_id),
      ),
    );
    if (matching) {
      return matching;
    }
  }

  return editionOptions[0] ?? null;
}

function buildEditionLabel(variant: PlaybackVariant, hasNamedEditions: boolean): string {
  if (variant.edition_raw?.trim()) {
    return variant.edition_raw.trim();
  }
  if (variant.edition_key?.trim()) {
    return humanizeEditionKey(variant.edition_key);
  }
  return hasNamedEditions ? "Standard" : "Edition";
}

function humanizeEditionKey(value: string): string {
  return value
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => {
      if (part.toLowerCase() === "imax") {
        return "IMAX";
      }
      return `${part.charAt(0).toUpperCase()}${part.slice(1)}`;
    })
    .join(" ");
}

function buildEditionDetail(option: EditionOption): string {
  const parts: string[] = [];
  if (option.versions.length > 1) {
    parts.push(`${option.versions.length} versions`);
  }

  const defaultSummary = buildQualitySummary(option.defaultVersion);
  if (defaultSummary) {
    parts.push(defaultSummary);
  }

  return parts.join(" · ");
}
