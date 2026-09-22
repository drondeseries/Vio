// @vitest-environment jsdom
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import { REFRESH_VERSIONS_ERROR } from "@/hooks/useVersionListRefresh";
import VersionDropdown from "./VersionDropdown";

// The sort preference reads/writes the canonical settings endpoints; the
// menu tests only exercise the display re-order, so it is mocked inert.
const versionSortMock = vi.hoisted(() => ({
  criteria: [] as Array<{ attribute: string; direction: string }>,
  apply: vi.fn(),
  reset: vi.fn(),
}));
vi.mock("@/hooks/useVersionSortPreference", () => ({
  useVersionSortPreference: () => ({
    criteria: versionSortMock.criteria,
    apply: versionSortMock.apply,
    reset: versionSortMock.reset,
    loading: false,
  }),
}));

beforeEach(() => {
  versionSortMock.criteria = [];
  versionSortMock.apply.mockReset();
  versionSortMock.reset.mockReset();
});

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

function openPicker(
  versions: FileVersion[],
  props: Partial<Parameters<typeof VersionDropdown>[0]> = {},
) {
  render(
    <VersionDropdown
      versions={versions}
      selectedVersion={versions[0]!}
      onSelectVersion={vi.fn()}
      {...props}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Version/ }));
  return within(screen.getByRole("dialog"));
}

describe("VersionDropdown Refresh List", () => {
  const versions = [
    makeVersion({ file_id: 1, resolution: "2160p" }),
    makeVersion({ file_id: 2, resolution: "1080p" }),
  ];

  it("renders Refresh List as the last row and triggers exactly one refresh", async () => {
    const onRefreshVersions = vi.fn().mockResolvedValue(undefined);
    const dialog = openPicker(versions, { onRefreshVersions });

    const buttons = dialog.getAllByRole("button");
    expect(buttons[buttons.length - 1]).toHaveTextContent("Refresh List");

    await act(async () => {
      fireEvent.click(dialog.getByRole("button", { name: /Refresh List/ }));
      await Promise.resolve();
    });
    expect(onRefreshVersions).toHaveBeenCalledTimes(1);
  });

  it("disables the row while refreshing and keeps the list on failure", async () => {
    let reject: (error: unknown) => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((_resolve, rej) => {
          reject = rej;
        }),
    );
    const dialog = openPicker(versions, { onRefreshVersions });

    fireEvent.click(dialog.getByRole("button", { name: /Refresh List/ }));
    const busy = dialog.getByRole("button", { name: /Refresh List/ });
    expect(busy).toBeDisabled();
    expect(busy).toHaveAttribute("aria-busy", "true");

    await act(async () => {
      reject(new Error("network"));
      await Promise.resolve();
    });

    expect(await screen.findByText(REFRESH_VERSIONS_ERROR)).toBeInTheDocument();
    // The known versions stay on screen next to the failure.
    expect(dialog.getByRole("button", { name: /2160p/ })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /Refresh List/ })).not.toBeDisabled();
  });

  it("omits the row when no refresh handler is wired", () => {
    const dialog = openPicker(versions);
    expect(dialog.queryByRole("button", { name: /Refresh List/ })).not.toBeInTheDocument();
  });
});

describe("VersionDropdown score and size", () => {
  it("shows the format score badge on scored rows only", () => {
    const dialog = openPicker([
      makeVersion({ file_id: 1, resolution: "2160p", format_score: 850 }),
      makeVersion({ file_id: 2, resolution: "1080p", format_score: 0 }),
    ]);

    expect(dialog.getByText("★ 850")).toBeInTheDocument();
    expect(dialog.queryByText(/★ 0/)).not.toBeInTheDocument();
  });

  it("drops the label's embedded size when the structured size is known and shows the year once", () => {
    const dialog = openPicker([
      makeVersion({
        file_id: 1,
        resolution: "2160p",
        file_size: 50_570_000_000,
        edition_raw: "Movie.2026.2160p.WEB-DL.50.53GB",
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    expect(dialog.getByText(/47\.1 GB/)).toBeInTheDocument();
    expect(dialog.queryByText(/50 53 GB/)).not.toBeInTheDocument();
    // The release label carries the year once; the row title does not repeat it.
    expect(dialog.getAllByText(/2026/)).toHaveLength(1);
  });
});

describe("VersionDropdown version sort preference", () => {
  const versions = [
    makeVersion({ file_id: 1, resolution: "2160p", file_size: 100 }),
    makeVersion({ file_id: 2, resolution: "1080p", file_size: 300 }),
  ];

  function rowOrder(dialog: ReturnType<typeof openPicker>) {
    return dialog
      .getAllByRole("button")
      .filter((button) => /2160p|1080p/.test(button.textContent ?? ""))
      .map((button) => (button.textContent?.includes("2160p") ? "2160p" : "1080p"));
  }

  it("re-orders the displayed list for the viewer without touching selection", () => {
    versionSortMock.criteria = [{ attribute: "size", direction: "desc" }];
    const onSelectVersion = vi.fn();
    const dialog = openPicker(versions, { onSelectVersion });

    expect(rowOrder(dialog)).toEqual(["1080p", "2160p"]);
    // The reorder is display-only: nothing was selected or started.
    expect(onSelectVersion).not.toHaveBeenCalled();
  });

  it("keeps the server's incoming order with no override", () => {
    const dialog = openPicker(versions);
    expect(rowOrder(dialog)).toEqual(["2160p", "1080p"]);
  });
});

describe("VersionDropdown ranking indicator", () => {
  it("names the profile the ranked candidates carry", () => {
    const dialog = openPicker([
      makeVersion({
        file_id: 1,
        resolution: "2160p",
        file_path: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
      }),
      makeVersion({
        file_id: 2,
        resolution: "1080p",
        file_path: "virtual://movie/tt1?profile=4K%2BHDR&result=def",
      }),
    ]);

    expect(dialog.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(dialog.getByText(/4K\+HDR/)).toBeInTheDocument();
  });
});
