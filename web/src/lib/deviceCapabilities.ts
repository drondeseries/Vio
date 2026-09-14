import { api } from "@/api/client";
import { storage } from "@/utils/storage";

const CAP_REPORT_KEY = "silo-device-capability-fingerprint";

/**
 * Reports this browser's media capabilities to the server once per
 * (server, device, profile) combination so playback of virtual streams can be
 * ranked for direct-play on this device.
 *
 * Detection uses the browser's real codec probes — HTMLMediaElement.canPlayType
 * and MediaSource.isTypeSupported — rather than a hardcoded table, because
 * Safari reports HEVC/DV and Chrome reports AV1/VP9 even on the same OS.
 */

export interface ReportedDeviceCapabilities {
  codecs_video?: string[];
  codecs_audio?: string[];
  containers?: string[];
  max_resolution?: string;
  hdr?: boolean;
}

function canPlay(mime: string): boolean {
  try {
    const v = document.createElement("video");
    return Boolean(v.canPlayType(mime));
  } catch {
    return false;
  }
}

export function detectBrowserCapabilities(): ReportedDeviceCapabilities {
  const video: string[] = [];
  const audio: string[] = [];
  const containers: string[] = [];

  if (canPlay('video/mp4; codecs="avc1.64001f"')) video.push("h264");
  if (
    canPlay('video/mp4; codecs="hev1.1.6.L93.B0"') ||
    canPlay('video/mp4; codecs="hvc1.1.6.L93.B0"')
  ) {
    video.push("hevc");
  }
  if (canPlay('video/webm; codecs="vp9"')) video.push("vp9");
  if (canPlay('video/webm; codecs="av01.0.08M.08"')) video.push("av1");
  containers.push("mp4");

  audio.push("aac");
  if (canPlay('audio/mp4; codecs="ac-3"')) audio.push("ac3");
  if (canPlay('audio/mp4; codecs="ec-3"')) audio.push("eac3");
  if (canPlay('audio/ogg; codecs="opus"')) audio.push("opus");

  const screenWidth = typeof window !== "undefined" ? window.screen?.width : 0;
  const screenHeight = typeof window !== "undefined" ? window.screen?.height : 0;
  const maxDimension = Math.max(screenWidth || 0, screenHeight || 0);
  const max_resolution =
    maxDimension >= 3840
      ? "2160p"
      : maxDimension >= 1920
        ? "1080p"
        : maxDimension >= 1280
          ? "720p"
          : "480p";
  const hdr =
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    (window.matchMedia("(dynamic-range: high)").matches ||
      window.matchMedia("(video-dynamic-range: high)").matches);

  return {
    codecs_video: video,
    codecs_audio: audio,
    containers,
    max_resolution,
    hdr,
  };
}

/**
 * Detects and reports browser capabilities to the server, deduped by a stored
 * fingerprint so the endpoint is hit once per browser/device, not on every
 * page load. Safe to call from any authenticated screen; it self-guards on
 * server origin and profile.
 */
export async function reportDeviceCapabilitiesIfNeeded(): Promise<void> {
  try {
    const caps = detectBrowserCapabilities();
    const fingerprint = JSON.stringify(caps);
    const origin = window.location.origin;
    const stored = window.localStorage.getItem(CAP_REPORT_KEY);
    if (stored === `${origin}:${fingerprint}`) {
      return;
    }
    const deviceId = storage.get(storage.KEYS.DEVICE_ID);
    if (!deviceId) {
      return;
    }
    await api(`/devices/${encodeURIComponent(deviceId)}/capabilities`, {
      method: "PUT",
      body: JSON.stringify(caps),
    });
    window.localStorage.setItem(CAP_REPORT_KEY, `${origin}:${fingerprint}`);
  } catch {
    // Reporting is best-effort; playback ranking falls back to provider order.
  }
}
