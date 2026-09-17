import { useMemo } from "react";

import { useSettingsForm } from "@/hooks/useSettingsForm";
import { SettingField } from "@/pages/admin-settings/SettingField";

import { StepFrame, StepSection, StepSkeleton } from "../StepFrame";
import { useStepSubmit, useStepSummary } from "../useStep";

const STREAMING_KEYS = [
  "virtual_library.enabled",
  "virtual_library.manifest_url",
  "virtual_library.movie_library_id",
  "virtual_library.series_library_id",
  "virtual_library.allow_insecure_http",
  "virtual_library.cache_ttl_minutes",
  "virtual_library.tmdb_api_key",
];

export function StreamingStep() {
  const form = useSettingsForm({ keys: useMemo(() => STREAMING_KEYS, []) });
  const { handleSubmit, busy, skip } = useStepSubmit("streaming", form, "Failed to save");

  const enabled = form.getValue("virtual_library.enabled") === "true";
  const manifestUrl = form.getValue("virtual_library.manifest_url");
  useStepSummary("streaming", enabled && manifestUrl ? "Stremio provider set" : "Skipped");

  if (form.isPending) return <StepSkeleton rows={4} />;

  return (
    <StepFrame
      title="Streaming"
      lede="Vio streams from a Stremio addon provider. Paste your provider's manifest URL and Vio registers its catalog as virtual libraries — no files needed."
      onSubmit={handleSubmit}
      busy={busy}
      onSkip={skip}
      footnote="Create virtual://movies and virtual://series libraries under Libraries, then point the provider at them. Details live in Admin › Settings › Streaming."
    >
      <StepSection
        title="Stremio provider"
        caption="The tokenized addon URL ending in /manifest.json. HTTPS is required; allow local HTTP only for private hosts."
      >
        <SettingField
          label="Enable streaming"
          type="toggle"
          description="Fetch stream candidates from the configured Stremio provider."
          value={enabled ? "true" : "false"}
          onChange={(v) => form.setValue("virtual_library.enabled", v)}
        />
        {enabled && (
          <>
            <SettingField
              label="Manifest URL"
              type="text"
              description="Your streaming provider's manifest URL (ends in /manifest.json)."
              hint="https://…/manifest.json"
              value={manifestUrl}
              onChange={(v) => form.setValue("virtual_library.manifest_url", v)}
            />
            <SettingField
              label="Movies library ID"
              type="number"
              description="Virtual movies library (virtual://movies) that holds provider titles."
              value={form.getValue("virtual_library.movie_library_id")}
              onChange={(v) => form.setValue("virtual_library.movie_library_id", v)}
            />
            <SettingField
              label="Series library ID"
              type="number"
              description="Virtual series library (virtual://series) that holds provider episodes."
              value={form.getValue("virtual_library.series_library_id")}
              onChange={(v) => form.setValue("virtual_library.series_library_id", v)}
            />
            <SettingField
              label="TMDB API key"
              type="password"
              description="Optional: resolves TMDB IDs to provider IDs for broader coverage."
              value={form.getValue("virtual_library.tmdb_api_key")}
              onChange={(v) => form.setValue("virtual_library.tmdb_api_key", v)}
            />
            <SettingField
              label="Allow local HTTP"
              type="toggle"
              description="Permit http:// manifests on localhost and private networks."
              value={
                form.getValue("virtual_library.allow_insecure_http") === "true" ? "true" : "false"
              }
              onChange={(v) => form.setValue("virtual_library.allow_insecure_http", v)}
            />
          </>
        )}
      </StepSection>
    </StepFrame>
  );
}
