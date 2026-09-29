import { useQuery } from "@tanstack/react-query";

import { v2 } from "@/api/v2/request";

export type EpisodeReleaseState = "upcoming" | "released";

export interface EpisodeReleaseCapability {
  /** Whether the server exposes release timing on episode rows. */
  available: boolean;
}

/**
 * Whether the server exposes release timing on episode rows
 * (`GET /api/v2/capabilities/episode-release`). Views gate the upcoming
 * treatment on this so an older server that omits release_state keeps
 * today's plain rendering.
 */
export async function fetchEpisodeReleaseCapability(
  options?: Pick<RequestInit, "signal">,
): Promise<EpisodeReleaseCapability> {
  const capability = await v2("GET /api/v2/capabilities/episode-release", {
    signal: options?.signal ?? undefined,
  });
  return { available: capability.state === "available" };
}

export function useEpisodeReleaseCapability(enabled = true) {
  return useQuery({
    queryKey: ["episode-release", "capability"],
    queryFn: ({ signal }) => fetchEpisodeReleaseCapability({ signal }),
    staleTime: 5 * 60 * 1000,
    enabled,
  });
}

/**
 * Strict YYYY-MM-DD calendar-date check: shape plus real month/day ranges
 * and day-of-month validity. Parsing alone is not enough — `new Date` with
 * an impossible date such as February 30 normalizes to March instead of
 * failing.
 */
export function isCalendarDate(value: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value.trim());
  if (!match) {
    return false;
  }
  const month = Number(match[2]);
  const day = Number(match[3]);
  if (month < 1 || month > 12 || day < 1 || day > 31) {
    return false;
  }
  const probe = new Date(Date.UTC(Number(match[1]), month - 1, day));
  return (
    probe.getUTCFullYear() === Number(match[1]) &&
    probe.getUTCMonth() === month - 1 &&
    probe.getUTCDate() === day
  );
}

/**
 * The episode's release timing under the documented contract: the server's
 * release_state when the capability is available, otherwise undefined (old
 * rendering). Unknown server values and unknown/malformed dates are
 * undefined (fail open): an episode with no usable classification renders
 * without the upcoming treatment. There is deliberately no air_date
 * inference — a client must not invent timing the server did not classify.
 */
export function episodeReleaseState(
  episode: { release_state?: string },
  capabilityAvailable = true,
): EpisodeReleaseState | undefined {
  if (!capabilityAvailable) {
    return undefined;
  }
  return episode.release_state === "upcoming" || episode.release_state === "released"
    ? episode.release_state
    : undefined;
}

/**
 * True when the server classifies the episode as airing after today: dim it
 * and badge it. Pass the capability answer; when it is unavailable (older
 * server) or the state is absent/unknown, this is false and the episode
 * renders exactly as before.
 */
export function isUpcomingEpisode(
  episode: { release_state?: string },
  capabilityAvailable = true,
): boolean {
  return episodeReleaseState(episode, capabilityAvailable) === "upcoming";
}

/**
 * Format a YYYY-MM-DD calendar date as local-midnight (no UTC shift: parsing
 * as a bare date would move it a day west of UTC). Returns the raw value for
 * anything that is not a real calendar date.
 */
export function formatCalendarDate(value: string | null | undefined): string {
  const date = (value ?? "").trim();
  if (!isCalendarDate(date)) {
    return date;
  }
  return new Date(`${date}T00:00:00`).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}
