import { useMemo } from "react";

import { SettingsPageHeader } from "@/components/settings/SettingsPageHeader";
import { Skeleton } from "@/components/ui/skeleton";
import { useRestartKeys } from "@/hooks/useRestartKeys";
import { useSettingsForm } from "@/hooks/useSettingsForm";

import { FieldGroup } from "./FieldGroup";
import { SaveBar } from "./SaveBar";
import { SettingField } from "./SettingField";

const PROVIDER_KEYS = [
  "virtual_library.enabled",
  "virtual_library.manifest_url",
  "virtual_library.tmdb_api_key",
  "virtual_library.allow_insecure_http",
  "virtual_library.cache_ttl_minutes",
];

const LIBRARY_KEYS = [
  "virtual_library.movie_library_id",
  "virtual_library.series_library_id",
  "virtual_library.monitor_file",
  "virtual_library.schedule_refresh_minutes",
];

const QUALITY_KEYS = [
  "virtual_library.enable_quality_profiles",
  "virtual_library.quality_preset",
  "virtual_library.custom_format_preset",
  "virtual_library.single_stream_with_failover",
  "virtual_library.fallback_to_any_stream",
];

const ALL_KEYS = [...PROVIDER_KEYS, ...LIBRARY_KEYS, ...QUALITY_KEYS];

export default function StreamingSettings() {
  const form = useSettingsForm({ keys: useMemo(() => ALL_KEYS, []) });
  const restartKeys = useRestartKeys();

  const anyDirty = (keys: string[]) => keys.some((key) => form.isDirty(key));

  if (form.isLoading)
    return (
      <div className="space-y-6" role="status" aria-label="Loading settings">
        <Skeleton className="h-8 w-40" />
        <div className="space-y-4">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </div>
        <span className="sr-only">Loading settings</span>
      </div>
    );

  return (
    <div className="flex h-full flex-col">
      <SettingsPageHeader title="Streaming" className="mb-8" />

      <div className="flex-1 space-y-5">
        <FieldGroup label="Stremio provider" dirty={anyDirty(PROVIDER_KEYS)}>
          <SettingField
            label="Enable streaming"
            type="toggle"
            description="Fetch stream candidates from the configured Stremio provider."
            value={form.getValue("virtual_library.enabled") || "true"}
            onChange={(v) => form.setValue("virtual_library.enabled", v)}
            restartRequired={restartKeys.has("virtual_library.enabled")}
          />
          <SettingField
            label="Manifest URL"
            description="Tokenized provider URL ending in /manifest.json. HTTPS required; allow local HTTP only for private hosts."
            hint="https://…/manifest.json"
            value={form.getValue("virtual_library.manifest_url")}
            onChange={(v) => form.setValue("virtual_library.manifest_url", v)}
            restartRequired={restartKeys.has("virtual_library.manifest_url")}
          />
          <SettingField
            label="TMDB API key"
            type="password"
            description="Optional: resolves TMDB IDs to provider IDs for broader coverage."
            value={form.getValue("virtual_library.tmdb_api_key")}
            onChange={(v) => form.setValue("virtual_library.tmdb_api_key", v)}
            restartRequired={restartKeys.has("virtual_library.tmdb_api_key")}
          />
          <SettingField
            label="Allow local HTTP"
            type="toggle"
            description="Permit http:// manifests on localhost and private networks."
            value={form.getValue("virtual_library.allow_insecure_http") || "false"}
            onChange={(v) => form.setValue("virtual_library.allow_insecure_http", v)}
            restartRequired={restartKeys.has("virtual_library.allow_insecure_http")}
          />
          <SettingField
            label="Cache TTL (minutes)"
            type="number"
            description="How long provider candidates are reused before re-fetching. 1–10080."
            value={form.getValue("virtual_library.cache_ttl_minutes")}
            onChange={(v) => form.setValue("virtual_library.cache_ttl_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.cache_ttl_minutes")}
          />
        </FieldGroup>
        <FieldGroup label="Virtual libraries" dirty={anyDirty(LIBRARY_KEYS)}>
          <SettingField
            label="Movies library ID"
            type="number"
            description="Virtual movies library (virtual://movies) holding provider titles."
            value={form.getValue("virtual_library.movie_library_id")}
            onChange={(v) => form.setValue("virtual_library.movie_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.movie_library_id")}
          />
          <SettingField
            label="Series library ID"
            type="number"
            description="Virtual series library (virtual://series) holding provider episodes."
            value={form.getValue("virtual_library.series_library_id")}
            onChange={(v) => form.setValue("virtual_library.series_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.series_library_id")}
          />
          <SettingField
            label="Monitor file"
            description="Filename tracking monitored titles inside each virtual library."
            value={form.getValue("virtual_library.monitor_file")}
            onChange={(v) => form.setValue("virtual_library.monitor_file", v)}
            restartRequired={restartKeys.has("virtual_library.monitor_file")}
          />
          <SettingField
            label="Schedule refresh (minutes)"
            type="number"
            description="How often monitored titles re-check the provider. 30–10080."
            value={form.getValue("virtual_library.schedule_refresh_minutes")}
            onChange={(v) => form.setValue("virtual_library.schedule_refresh_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.schedule_refresh_minutes")}
          />
        </FieldGroup>
        <FieldGroup label="Quality" dirty={anyDirty(QUALITY_KEYS)}>
          <SettingField
            label="Enable quality profiles"
            type="toggle"
            description="Rank candidates by named resolution/codec profiles instead of provider order."
            value={form.getValue("virtual_library.enable_quality_profiles") || "false"}
            onChange={(v) => form.setValue("virtual_library.enable_quality_profiles", v)}
            restartRequired={restartKeys.has("virtual_library.enable_quality_profiles")}
          />
          <SettingField
            label="Single stream with failover"
            type="toggle"
            description="Register one winning stream per title; fall over to the next on failure."
            value={form.getValue("virtual_library.single_stream_with_failover") || "true"}
            onChange={(v) => form.setValue("virtual_library.single_stream_with_failover", v)}
            restartRequired={restartKeys.has("virtual_library.single_stream_with_failover")}
          />
          <SettingField
            label="Fallback to any stream"
            type="toggle"
            description="When no candidate matches the profiles, use the best available anyway."
            value={form.getValue("virtual_library.fallback_to_any_stream") || "false"}
            onChange={(v) => form.setValue("virtual_library.fallback_to_any_stream", v)}
            restartRequired={restartKeys.has("virtual_library.fallback_to_any_stream")}
          />
        </FieldGroup>
      </div>

      <SaveBar
        dirtyCount={form.dirtyCount}
        onSave={form.save}
        onDiscard={form.discard}
        isSaving={form.isSaving}
      />
    </div>
  );
}
