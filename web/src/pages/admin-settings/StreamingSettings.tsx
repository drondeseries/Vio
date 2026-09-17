import { useMemo, useState } from "react";
import { Link } from "react-router";
import { ArrowRight, Loader2, Sparkles } from "lucide-react";

import {
  ConnectionCheckAction,
  useConnectionCheck,
} from "@/components/admin/ConnectionCheckAction";
import { SettingsPageHeader } from "@/components/settings/SettingsPageHeader";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useAdminLibraries, useCreateLibrary } from "@/hooks/queries/admin/libraries";
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

const QUALITY_PRESET_OPTIONS = [
  { value: "custom", label: "Custom" },
  { value: "balanced", label: "Balanced (1080p)" },
  { value: "4k-hdr", label: "4K HDR" },
  { value: "4k-dolby-vision", label: "4K Dolby Vision" },
  { value: "no-dolby-vision", label: "4K HDR10 (no Dolby Vision)" },
  { value: "no-hdr", label: "4K SDR (no HDR)" },
  { value: "compatibility", label: "Compatibility (H.264/AAC)" },
  { value: "anime", label: "Anime" },
];

const CUSTOM_FORMAT_PRESET_OPTIONS = [
  { value: "custom", label: "Custom" },
  { value: "trash-recommended", label: "TRaSH Recommended (legacy)" },
  { value: "altmount-recommended", label: "AltMount TRaSH Recommended" },
  { value: "altmount-remux", label: "AltMount 4K Remux Enthusiast" },
  { value: "altmount-compatibility", label: "AltMount Compatibility" },
  { value: "english-original", label: "English Original" },
  { value: "english-strict", label: "English Strict" },
  { value: "original-or-english", label: "Original or English" },
  { value: "clean-quality", label: "Clean Quality (no CAM/3D/extras)" },
  { value: "audio-hd", label: "HD Audio (Atmos/DTS-HD)" },
  { value: "repack-proper", label: "Repack / Proper" },
  { value: "top-web-sources", label: "Top WEB Sources" },
  { value: "anime-enhanced", label: "Anime Enhanced" },
  { value: "web-tier-01", label: "WEB Tier 1 Groups" },
  { value: "web-tier-02", label: "WEB Tier 2 Groups" },
  { value: "remux-tier-01", label: "Remux Tier 1 Groups" },
  { value: "remux-tier-02", label: "Remux Tier 2 Groups" },
];

const AUTOMATION_KEYS = [
  "virtual_library.indexer_rss_url",
  "virtual_library.indexer_api_key",
  "virtual_library.indexer_rss_check_minutes",
  "virtual_library.altmount_url",
  "virtual_library.altmount_api_key",
  "virtual_library.altmount_check_minutes",
];

const ALL_KEYS = [...PROVIDER_KEYS, ...LIBRARY_KEYS, ...QUALITY_KEYS, ...AUTOMATION_KEYS];

export default function StreamingSettings() {
  const form = useSettingsForm({ keys: useMemo(() => ALL_KEYS, []) });
  const restartKeys = useRestartKeys();
  const { data: libraries } = useAdminLibraries();
  const createLibrary = useCreateLibrary();
  const [creatingLibs, setCreatingLibs] = useState(false);

  const providerCheck = useConnectionCheck("virtual_library", form, PROVIDER_KEYS);

  const anyDirty = (keys: string[]) => keys.some((key) => form.isDirty(key));

  const movieOptions = useMemo(() => {
    if (!libraries || libraries.length === 0) return [];
    return libraries
      .filter((l) => l.type === "movies" || l.type === "mixed")
      .map((l) => ({ value: String(l.id), label: `${l.name} (ID: ${l.id})` }));
  }, [libraries]);

  const seriesOptions = useMemo(() => {
    if (!libraries || libraries.length === 0) return [];
    return libraries
      .filter((l) => l.type === "series" || l.type === "mixed")
      .map((l) => ({ value: String(l.id), label: `${l.name} (ID: ${l.id})` }));
  }, [libraries]);

  const currentMovieID = form.getValue("virtual_library.movie_library_id");
  const movieSelectOptions = useMemo(() => {
    const opts = [...movieOptions];
    if (currentMovieID && !opts.some((o) => o.value === currentMovieID)) {
      opts.unshift({ value: currentMovieID, label: `Library #${currentMovieID}` });
    }
    return opts;
  }, [movieOptions, currentMovieID]);

  const currentSeriesID = form.getValue("virtual_library.series_library_id");
  const seriesSelectOptions = useMemo(() => {
    const opts = [...seriesOptions];
    if (currentSeriesID && !opts.some((o) => o.value === currentSeriesID)) {
      opts.unshift({ value: currentSeriesID, label: `Library #${currentSeriesID}` });
    }
    return opts;
  }, [seriesOptions, currentSeriesID]);

  const handleCreateVirtualLibraries = async () => {
    setCreatingLibs(true);
    try {
      const hasMovies = libraries?.some(
        (l) =>
          (l.type === "movies" || l.type === "mixed") &&
          l.paths.some((p) => p.startsWith("virtual://")),
      );
      const hasSeries = libraries?.some(
        (l) =>
          (l.type === "series" || l.type === "mixed") &&
          l.paths.some((p) => p.startsWith("virtual://")),
      );

      if (!hasMovies) {
        const mov = await createLibrary.mutateAsync({
          name: "Virtual Movies",
          type: "movies",
          paths: ["virtual://movies"],
        });
        form.setValue("virtual_library.movie_library_id", String(mov.id));
      }
      if (!hasSeries) {
        const ser = await createLibrary.mutateAsync({
          name: "Virtual Series",
          type: "series",
          paths: ["virtual://series"],
        });
        form.setValue("virtual_library.series_library_id", String(ser.id));
      }
    } finally {
      setCreatingLibs(false);
    }
  };

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

      <div className="flex-1 space-y-6">
        <div className="border-border/70 bg-card/60 flex items-center justify-between rounded-xl border p-4 backdrop-blur-sm">
          <div className="space-y-0.5">
            <div className="text-sm font-semibold">Virtual Release Desk</div>
            <p className="text-muted-foreground text-xs">
              Inspect zero-storage monitored items, candidate stream health, failed deliveries, and
              recent releases.
            </p>
          </div>
          <Button variant="outline" size="sm" asChild className="shrink-0">
            <Link to="/admin/virtual-library">
              Open Release Desk
              <ArrowRight className="ml-1.5 h-3.5 w-3.5" />
            </Link>
          </Button>
        </div>

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
          <ConnectionCheckAction
            onClick={providerCheck.run}
            result={providerCheck.result}
            isPending={providerCheck.isPending}
            disabled={!form.getValue("virtual_library.manifest_url")}
          />
        </FieldGroup>

        <FieldGroup label="Virtual libraries" dirty={anyDirty(LIBRARY_KEYS)}>
          <SettingField
            label="Movies library"
            settingKey="virtual_library.movie_library_id"
            type={movieSelectOptions.length > 0 ? "select" : "number"}
            options={movieSelectOptions.length > 0 ? movieSelectOptions : undefined}
            description="Virtual movies library (virtual://movies) holding provider titles."
            value={form.getValue("virtual_library.movie_library_id")}
            onChange={(v) => form.setValue("virtual_library.movie_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.movie_library_id")}
          />
          <SettingField
            label="Series library"
            settingKey="virtual_library.series_library_id"
            type={seriesSelectOptions.length > 0 ? "select" : "number"}
            options={seriesSelectOptions.length > 0 ? seriesSelectOptions : undefined}
            description="Virtual series library (virtual://series) holding provider episodes."
            value={form.getValue("virtual_library.series_library_id")}
            onChange={(v) => form.setValue("virtual_library.series_library_id", v)}
            restartRequired={restartKeys.has("virtual_library.series_library_id")}
          />
          <div className="border-border/60 flex flex-col gap-2 border-b pt-1 pb-3 sm:flex-row sm:items-center sm:justify-between">
            <div>
              <p className="text-xs font-medium">Auto-create zero-storage libraries</p>
              <p className="text-muted-foreground text-xs">
                Quickly create Virtual Movies (virtual://movies) and Virtual Series
                (virtual://series) if you haven't set them up yet.
              </p>
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={creatingLibs || createLibrary.isPending}
              onClick={handleCreateVirtualLibraries}
              className="shrink-0"
            >
              {creatingLibs || createLibrary.isPending ? (
                <>
                  <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
                  Creating...
                </>
              ) : (
                <>
                  <Sparkles className="mr-1.5 h-3.5 w-3.5" />
                  Create Virtual Libraries
                </>
              )}
            </Button>
          </div>
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

        <FieldGroup
          label="Automation & Indexers (Prowlarr & AltMount)"
          dirty={anyDirty(AUTOMATION_KEYS)}
        >
          <SettingField
            label="Prowlarr RSS URL"
            description="Optional: Prowlarr RSS feed URL for automated release discovery."
            hint="http://prowlarr:9696/1/api/v1/search?t=movie"
            value={form.getValue("virtual_library.indexer_rss_url")}
            onChange={(v) => form.setValue("virtual_library.indexer_rss_url", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_rss_url")}
          />
          <SettingField
            label="Prowlarr API key"
            description="API key for authenticated Prowlarr indexer queries."
            value={form.getValue("virtual_library.indexer_api_key")}
            onChange={(v) => form.setValue("virtual_library.indexer_api_key", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_api_key")}
          />
          <SettingField
            label="Prowlarr check interval (minutes)"
            type="number"
            description="How often to poll the Prowlarr RSS feed for new releases. 1–10080."
            value={form.getValue("virtual_library.indexer_rss_check_minutes") || "15"}
            onChange={(v) => form.setValue("virtual_library.indexer_rss_check_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.indexer_rss_check_minutes")}
          />
          <SettingField
            label="AltMount URL"
            description="Optional: AltMount backend URL for authoritative completion status."
            hint="http://altmount:8080"
            value={form.getValue("virtual_library.altmount_url")}
            onChange={(v) => form.setValue("virtual_library.altmount_url", v)}
            restartRequired={restartKeys.has("virtual_library.altmount_url")}
          />
          <SettingField
            label="AltMount API key"
            description="API key for authenticated AltMount status queries."
            value={form.getValue("virtual_library.altmount_api_key")}
            onChange={(v) => form.setValue("virtual_library.altmount_api_key", v)}
            restartRequired={restartKeys.has("virtual_library.altmount_api_key")}
          />
          <SettingField
            label="AltMount check interval (minutes)"
            type="number"
            description="How often to poll AltMount for completion updates. 1–10080."
            value={form.getValue("virtual_library.altmount_check_minutes") || "15"}
            onChange={(v) => form.setValue("virtual_library.altmount_check_minutes", v)}
            restartRequired={restartKeys.has("virtual_library.altmount_check_minutes")}
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
            label="Quality preset"
            settingKey="virtual_library.quality_preset"
            type="select"
            options={QUALITY_PRESET_OPTIONS}
            description="Named resolution/codec profile set applied when profiles are enabled."
            value={form.getValue("virtual_library.quality_preset") || "custom"}
            onChange={(v) => form.setValue("virtual_library.quality_preset", v)}
            restartRequired={restartKeys.has("virtual_library.quality_preset")}
          />
          <SettingField
            label="Custom format preset"
            settingKey="virtual_library.custom_format_preset"
            type="select"
            options={CUSTOM_FORMAT_PRESET_OPTIONS}
            description="TRaSH-style release scoring weights. AltMount presets mirror the Stremio addon scorer."
            value={form.getValue("virtual_library.custom_format_preset") || "custom"}
            onChange={(v) => form.setValue("virtual_library.custom_format_preset", v)}
            restartRequired={restartKeys.has("virtual_library.custom_format_preset")}
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
