// @vitest-environment jsdom
import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import MediaInfoDialog from "./MediaInfoDialog";

function makeVersion(overrides: Partial<FileVersion> = {}): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    resolution: overrides.resolution ?? "1080p",
    codec_video: overrides.codec_video ?? "h264",
    codec_audio: overrides.codec_audio ?? "aac",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "mkv",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    ...overrides,
  };
}

// A version with no quality summary forces the file-name fallback title, which
// is where the duplication lived.
function withoutQualitySummary(overrides: Partial<FileVersion> = {}): FileVersion {
  return makeVersion({
    resolution: "",
    codec_video: "",
    codec_audio: "",
    container: "",
    ...overrides,
  });
}

function renderDialog(versions: FileVersion[]) {
  render(
    <MediaInfoDialog
      open
      onOpenChange={vi.fn()}
      versions={versions}
      title="Sinners"
      initialFileId={undefined}
    />,
  );
  return within(screen.getByRole("dialog"));
}

// The version row (accordion trigger) is found by its title span, so assertions
// stay scoped to the row and not the expanded spec sheet's "File Size" row.
function triggerFor(title: string) {
  return screen.getByText(title).closest("button")!;
}

describe("MediaInfoDialog version row dedupe", () => {
  it("shows a structured size once when the file name also embeds it", () => {
    renderDialog([
      withoutQualitySummary({
        file_id: 1,
        file_name: "Sinners.2026.2160p.9.31GB.mkv",
        file_size: 10_000_000_000,
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    const trigger = triggerFor("Sinners 2026 2160p mkv");
    expect(trigger.textContent).toContain("9.3 GB");
    expect(trigger.textContent).not.toMatch(/9[ .]?31\s?GB/i);
  });

  it("shows a year once when both the title and the release label carry it", () => {
    renderDialog([
      withoutQualitySummary({
        file_id: 1,
        file_name: "Sinners.2026.1080p.x264.mkv",
        edition_raw: "Sinners.2026.1080p.x264-GRP",
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    const dialog = screen.getByRole("dialog");
    // The version row's title dropped its year; the detail/release line kept it.
    expect(triggerFor("Sinners 1080p x264 mkv").textContent).toContain("2026");
    expect(within(dialog).getAllByText(/2026/)).toHaveLength(1);
  });

  it("keeps the file name's size when no structured size exists", () => {
    renderDialog([
      withoutQualitySummary({
        file_id: 1,
        file_name: "Sinners.2160p.9.31GB.mkv",
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    expect(triggerFor("Sinners.2160p.9.31GB.mkv")).toBeTruthy();
  });

  it("leaves a non-duplicated row unchanged", () => {
    renderDialog([
      makeVersion({
        file_id: 1,
        resolution: "2160p",
        codec_video: "hevc",
        edition_raw: "Movie.2024.2160p.WEB-DL",
        file_size: 10_000_000_000,
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    // The quality summary still wins; the detail line carries the year and size.
    const trigger = triggerFor("2160p · WEB-DL · HEVC · AAC");
    expect(trigger.textContent).toContain("Movie 2024 2160p WEB-DL · 9.3 GB · WEB-DL");
    expect(trigger.textContent).not.toMatch(/2024.*2024/);
  });
});
