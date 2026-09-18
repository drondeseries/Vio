export type CategoryType = "source" | "hdr" | "audio" | "release_group" | "resolution" | "custom";

export interface CustomFormatRule {
  id: string;
  name: string;
  category: CategoryType;
  pattern: string;
  patternType: "regex" | "token";
  score: number;
  enabled: boolean;
  isCustom?: boolean;
  invert?: boolean;
  reject?: boolean;
}

export interface QualityProfileRule {
  label: string;
  resolution?: string;
  hdr?: string;
  exclude_hdr?: string;
  codec_video?: string;
  codec_audio?: string;
  include_regex?: string;
  exclude_regex?: string;
  preferred_order: number;
}

export const VIO_RECOMMENDED_FORMATS: CustomFormatRule[] = [
  {
    id: "remux_4k",
    name: "4K UHD Remux (Disc)",
    category: "source",
    pattern:
      "\\b(2160p|4k)\\b.*\\b(remux|bdremux|uhd\\.remux)\\b|\\b(remux|bdremux|uhd\\.remux)\\b.*\\b(2160p|4k)\\b",
    patternType: "regex",
    score: 500,
    enabled: true,
    isCustom: false,
  },
  {
    id: "remux_1080p",
    name: "1080p Remux (Disc)",
    category: "source",
    pattern: "\\b1080p\\b.*\\b(remux|bdremux)\\b|\\b(remux|bdremux)\\b.*\\b1080p\\b",
    patternType: "regex",
    score: 350,
    enabled: true,
    isCustom: false,
  },
  {
    id: "dv_hdr",
    name: "Dolby Vision (P7/P8)",
    category: "hdr",
    pattern: "\\b(dv|dovi|dolby[ ._-]?vision)\\b",
    patternType: "regex",
    score: 350,
    enabled: true,
    isCustom: false,
  },
  {
    id: "hdr10_plus",
    name: "HDR10+ / HDR10",
    category: "hdr",
    pattern: "\\b(hdr10\\+|hdr10|hdr)\\b",
    patternType: "regex",
    score: 200,
    enabled: true,
    isCustom: false,
  },
  {
    id: "lossless_atmos",
    name: "Lossless Atmos / TrueHD",
    category: "audio",
    pattern: "\\b(truehd[ ._-]?atmos|truehd|atmos)\\b",
    patternType: "regex",
    score: 250,
    enabled: true,
    isCustom: false,
  },
  {
    id: "dts_hd_ma",
    name: "DTS-HD MA / DTS:X",
    category: "audio",
    pattern: "\\b(dts[ ._-]hd([ ._-]ma)?|dts[ ._-]?x)\\b",
    patternType: "regex",
    score: 200,
    enabled: true,
    isCustom: false,
  },
  {
    id: "webdl_4k",
    name: "4K WEB-DL / WEBRip",
    category: "source",
    pattern: "\\b(2160p|4k)\\b.*\\b(web[ ._-]?dl|webrip)\\b",
    patternType: "regex",
    score: 180,
    enabled: true,
    isCustom: false,
  },
  {
    id: "tier1_groups",
    name: "Tier 1 High-Quality Release Groups",
    category: "release_group",
    pattern: "-(FLUX|FraMeSToR|EPSiLON|DON|playBD|CtrlHD|ZQ|TayTO|BHDStudio|SURCODE)\\b",
    patternType: "regex",
    score: 150,
    enabled: true,
    isCustom: false,
  },
  {
    id: "webdl_1080p",
    name: "1080p WEB-DL",
    category: "source",
    pattern: "\\b1080p\\b.*\\b(web[ ._-]?dl|webrip)\\b",
    patternType: "regex",
    score: 120,
    enabled: true,
    isCustom: false,
  },
  {
    id: "aac_stereo_demote",
    name: "Low Bitrate Stereo Audio",
    category: "audio",
    pattern: "\\b(aac[ ._-]?2\\.0|stereo|mp3)\\b",
    patternType: "regex",
    score: -100,
    enabled: true,
    isCustom: false,
  },
  {
    id: "cam_ts_discard",
    name: "CAM / TeleSync / Screener",
    category: "source",
    pattern: "\\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\\b",
    patternType: "regex",
    score: -2000,
    enabled: true,
    isCustom: false,
    reject: true,
  },
];

export const REMUX_ENTHUSIAST_FORMATS: CustomFormatRule[] = [
  {
    id: "remux_4k",
    name: "4K UHD Remux (Disc)",
    category: "source",
    pattern:
      "\\b(2160p|4k)\\b.*\\b(remux|bdremux|uhd\\.remux)\\b|\\b(remux|bdremux|uhd\\.remux)\\b.*\\b(2160p|4k)\\b",
    patternType: "regex",
    score: 800,
    enabled: true,
    isCustom: false,
  },
  {
    id: "remux_1080p",
    name: "1080p Remux (Disc)",
    category: "source",
    pattern: "\\b1080p\\b.*\\b(remux|bdremux)\\b|\\b(remux|bdremux)\\b.*\\b1080p\\b",
    patternType: "regex",
    score: 500,
    enabled: true,
    isCustom: false,
  },
  {
    id: "dv_hdr",
    name: "Dolby Vision (P7/P8)",
    category: "hdr",
    pattern: "\\b(dv|dovi|dolby[ ._-]?vision)\\b",
    patternType: "regex",
    score: 400,
    enabled: true,
    isCustom: false,
  },
  {
    id: "hdr10_plus",
    name: "HDR10+ / HDR10",
    category: "hdr",
    pattern: "\\b(hdr10\\+|hdr10|hdr)\\b",
    patternType: "regex",
    score: 250,
    enabled: true,
    isCustom: false,
  },
  {
    id: "lossless_atmos",
    name: "Lossless Atmos / TrueHD",
    category: "audio",
    pattern: "\\b(truehd[ ._-]?atmos|truehd|atmos)\\b",
    patternType: "regex",
    score: 350,
    enabled: true,
    isCustom: false,
  },
  {
    id: "dts_hd_ma",
    name: "DTS-HD MA / DTS:X",
    category: "audio",
    pattern: "\\b(dts[ ._-]hd([ ._-]ma)?|dts[ ._-]?x)\\b",
    patternType: "regex",
    score: 300,
    enabled: true,
    isCustom: false,
  },
  {
    id: "tier1_groups",
    name: "Tier 1 High-Quality Release Groups",
    category: "release_group",
    pattern: "-(FLUX|FraMeSToR|EPSiLON|DON|playBD|CtrlHD|ZQ|TayTO|BHDStudio|SURCODE)\\b",
    patternType: "regex",
    score: 200,
    enabled: true,
    isCustom: false,
  },
  {
    id: "webdl_demote",
    name: "Compressed WEB-DL Demotion",
    category: "source",
    pattern: "\\b(web[ ._-]?dl|webrip)\\b",
    patternType: "regex",
    score: -200,
    enabled: true,
    isCustom: false,
  },
  {
    id: "cam_ts_discard",
    name: "CAM / TeleSync / Screener",
    category: "source",
    pattern: "\\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\\b",
    patternType: "regex",
    score: -2000,
    enabled: true,
    isCustom: false,
    reject: true,
  },
];

export const COMPATIBILITY_FORMATS: CustomFormatRule[] = [
  {
    id: "webdl_1080p",
    name: "1080p WEB-DL (High Compatibility)",
    category: "source",
    pattern: "\\b1080p\\b.*\\b(web[ ._-]?dl|webrip)\\b",
    patternType: "regex",
    score: 400,
    enabled: true,
    isCustom: false,
  },
  {
    id: "webdl_4k",
    name: "4K WEB-DL (SDR / Standard HDR)",
    category: "source",
    pattern: "\\b(2160p|4k)\\b.*\\b(web[ ._-]?dl|webrip)\\b",
    patternType: "regex",
    score: 300,
    enabled: true,
    isCustom: false,
  },
  {
    id: "h264_avc",
    name: "H.264 / AVC (Universal Playback)",
    category: "source",
    pattern: "\\b(h[ ._-]?264|x264|avc)\\b",
    patternType: "regex",
    score: 250,
    enabled: true,
    isCustom: false,
  },
  {
    id: "eac3_ddp",
    name: "Dolby Digital Plus (E-AC-3 / DDP)",
    category: "audio",
    pattern: "\\b(e[ ._-]?ac[ ._-]?3|ddp|dd\\+)\\b",
    patternType: "regex",
    score: 200,
    enabled: true,
    isCustom: false,
  },
  {
    id: "aac_stereo",
    name: "AAC Audio",
    category: "audio",
    pattern: "\\baac\\b",
    patternType: "regex",
    score: 150,
    enabled: true,
    isCustom: false,
  },
  {
    id: "remux_demote",
    name: "Heavy High-Bitrate Remux Demotion",
    category: "source",
    pattern: "\\b(remux|bdremux)\\b",
    patternType: "regex",
    score: -100,
    enabled: true,
    isCustom: false,
  },
  {
    id: "cam_ts_discard",
    name: "CAM / TeleSync / Screener",
    category: "source",
    pattern: "\\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\\b",
    patternType: "regex",
    score: -2000,
    enabled: true,
    isCustom: false,
    reject: true,
  },
];

export const DEFAULT_QUALITY_PRESET_PROFILES: Record<string, QualityProfileRule[]> = {
  balanced: [{ label: "1080p", resolution: "1080p", preferred_order: 1 }],
  "4k-hdr": [
    { label: "4K HDR", resolution: "2160p", hdr: "hdr", preferred_order: 1 },
    { label: "1080p", resolution: "1080p", preferred_order: 2 },
  ],
  "4k-dolby-vision": [
    { label: "4K Dolby Vision", resolution: "2160p", hdr: "dv", preferred_order: 1 },
    { label: "1080p", resolution: "1080p", preferred_order: 2 },
  ],
  "no-dolby-vision": [
    {
      label: "4K HDR10",
      resolution: "2160p",
      exclude_hdr: "dv",
      exclude_regex: "(?i)(dolby[ ._-]*vision|\\bdv\\b|dovi)",
      preferred_order: 1,
    },
    {
      label: "1080p",
      resolution: "1080p",
      exclude_hdr: "dv",
      exclude_regex: "(?i)(dolby[ ._-]*vision|\\bdv\\b|dovi)",
      preferred_order: 2,
    },
  ],
  "no-hdr": [
    {
      label: "4K SDR",
      resolution: "2160p",
      exclude_hdr: "*",
      exclude_regex: "(?i)(hdr|dolby[ ._-]*vision|\\bdv\\b|dovi|hlg)",
      preferred_order: 1,
    },
    {
      label: "1080p SDR",
      resolution: "1080p",
      exclude_hdr: "*",
      exclude_regex: "(?i)(hdr|dolby[ ._-]*vision|\\bdv\\b|dovi|hlg)",
      preferred_order: 2,
    },
  ],
  compatibility: [
    {
      label: "1080p Compatible",
      resolution: "1080p",
      codec_video: "h264",
      codec_audio: "aac",
      preferred_order: 1,
    },
    {
      label: "720p Compatible",
      resolution: "720p",
      codec_video: "h264",
      codec_audio: "aac",
      preferred_order: 2,
    },
  ],
  anime: [
    {
      label: "Anime 1080p",
      resolution: "1080p",
      include_regex: "(?i)(anime|web-dl|web)",
      preferred_order: 1,
    },
    {
      label: "Anime 720p",
      resolution: "720p",
      include_regex: "(?i)(anime|web-dl|web)",
      preferred_order: 2,
    },
  ],
};

export const TRASH_RECOMMENDED_FORMATS = VIO_RECOMMENDED_FORMATS;

export function getDefaultFormatsForPreset(preset: string): CustomFormatRule[] {
  switch (preset) {
    case "altmount-remux":
    case "remux_enthusiast":
      return JSON.parse(JSON.stringify(REMUX_ENTHUSIAST_FORMATS));
    case "altmount-compatibility":
    case "compatibility":
      return JSON.parse(JSON.stringify(COMPATIBILITY_FORMATS));
    default:
      return JSON.parse(JSON.stringify(VIO_RECOMMENDED_FORMATS));
  }
}
