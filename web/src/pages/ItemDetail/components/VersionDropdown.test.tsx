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

// The picker reads the watch detail (lazily, when it opens) for the score and
// the server ranking. Mock it so the tests need no QueryClientProvider and can
// still drive the payload.
const watchDetailMock = vi.hoisted(() => ({
  current: undefined as unknown,
  lastOptions: undefined as unknown,
}));
vi.mock("@/hooks/queries/items", () => ({
  useWatchDetail: (
    _id: string | undefined,
    _fileId?: number,
    _libraryId?: number,
    options?: { enabled?: boolean },
  ) => {
    watchDetailMock.lastOptions = options;
    return { data: watchDetailMock.current, isLoading: false };
  },
}));

beforeEach(() => {
  versionSortMock.criteria = [];
  versionSortMock.apply.mockReset();
  versionSortMock.reset.mockReset();
  watchDetailMock.current = undefined;
  watchDetailMock.lastOptions = undefined;
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

describe("VersionDropdown rich row content", () => {
  it("renders the quality summary, languages, size and range like the player menu", () => {
    const dialog = openPicker([
      makeVersion({
        file_id: 1,
        resolution: "2160p",
        codec_video: "hevc",
        codec_audio: "eac3",
        hdr: true,
        edition_raw: "Movie.2026.2160p.WEB-DL.DDP5.1.Atmos.H.265-GRP",
        file_size: 50_570_000_000,
        audio_tracks: [{ language: "eng" }, { language: "fra" }],
        subtitle_tracks: [{ language: "deu" }],
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    expect(dialog.getByText(/2160p · WEB-DL · HEVC/)).toBeInTheDocument();
    expect(dialog.getByText("English")).toBeInTheDocument();
    expect(dialog.getByText("French")).toBeInTheDocument();
    expect(dialog.getByText("German")).toBeInTheDocument();
    expect(dialog.getByText(/47\.1 GB/)).toBeInTheDocument();
    expect(dialog.getByText("HDR")).toBeInTheDocument();
  });

  it("shows the custom-format score from the watch detail when the picker opens", () => {
    watchDetailMock.current = { versions: [{ file_id: 2, format_score: 850 }] };
    const dialog = openPicker(
      [
        makeVersion({ file_id: 1, resolution: "2160p" }),
        makeVersion({ file_id: 2, resolution: "1080p", file_size: 300 }),
      ],
      { contentId: "movie-1" },
    );

    expect(watchDetailMock.lastOptions).toEqual({ enabled: true });
    expect(dialog.getByText("★ 850")).toBeInTheDocument();
  });

  it("does not read the watch detail until the picker opens", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
        contentId="movie-1"
      />,
    );
    expect(watchDetailMock.lastOptions).toEqual({ enabled: false });

    fireEvent.click(screen.getByRole("button", { name: /Version/ }));
    expect(watchDetailMock.lastOptions).toEqual({ enabled: true });
  });

  it("takes the ranking from the watch payload's virtual_ranking", () => {
    watchDetailMock.current = {
      versions: [],
      virtual_ranking: {
        profile_label: "4K+HDR",
        source: "profile",
        criteria: [{ attribute: "score", direction: "desc" }],
      },
    };
    const dialog = openPicker(
      [
        makeVersion({ file_id: 1, resolution: "2160p" }),
        makeVersion({ file_id: 2, resolution: "1080p" }),
      ],
      { contentId: "movie-1" },
    );

    expect(dialog.getByText(/Ranking: score ↓/)).toBeInTheDocument();
    expect(dialog.getByText(/4K\+HDR/)).toBeInTheDocument();
  });
});

describe("VersionDropdown raw provider ids", () => {
  it("never renders a virtual URI, result token or tt id as a row label", () => {
    const dialog = openPicker([
      makeVersion({
        file_id: 1,
        resolution: "2160p",
        codec_video: "hevc",
        container: "virtual",
        file_path: "virtual://movie/tt123?result=abc",
        file_name: "tt123?result=abc",
        edition_raw: "virtual://movie/tt123?result=abc",
        file_size: 10_000_000_000,
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    const text = document.body.textContent ?? "";
    expect(text).not.toMatch(/tt\d{5,}/);
    expect(text).not.toMatch(/result=/);
    expect(text).not.toMatch(/virtual:\/\//);
    // The row's identity falls back to its quality summary.
    expect(dialog.getByText(/2160p · HEVC/)).toBeInTheDocument();
  });

  it("falls back to a neutral name when a version has no meaningful label", () => {
    const dialog = openPicker([
      makeVersion({
        file_id: 1,
        resolution: "",
        codec_video: "",
        codec_audio: "",
        container: "virtual",
        file_path: "virtual://movie/tt456?result=xyz",
        file_name: "tt456?result=xyz",
        edition_raw: "",
      }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ]);

    expect(dialog.queryByText(/tt456/)).not.toBeInTheDocument();
    expect(dialog.getByText("Video version")).toBeInTheDocument();
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
