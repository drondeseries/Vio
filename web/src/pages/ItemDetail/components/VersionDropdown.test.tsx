// @vitest-environment jsdom
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import { REFRESH_VERSIONS_ERROR } from "@/hooks/useVersionListRefresh";
import { REQUEST_RELEASE_ERROR } from "@/hooks/useIndexerReleases";
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

// The indexer UI is gated on the server capability and the Request action POSTs
// its own endpoint. Mock both so the tests drive the real hooks end to end.
const capabilityMock = vi.hoisted(() => vi.fn());
const requestReleaseMock = vi.hoisted(() => vi.fn());
vi.mock("@/api/v2/virtualLibrary", () => ({
  fetchVirtualLibraryCapability: capabilityMock,
}));
vi.mock("@/api/v2/mediaCandidates", () => ({
  requestVirtualRelease: requestReleaseMock,
}));

beforeEach(() => {
  versionSortMock.criteria = [];
  versionSortMock.apply.mockReset();
  versionSortMock.reset.mockReset();
  watchDetailMock.current = undefined;
  watchDetailMock.lastOptions = undefined;
  capabilityMock.mockReset().mockResolvedValue({
    state: "available",
    indexer_search: true,
    indexer_request: true,
  });
  requestReleaseMock.mockReset();
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

  it("holds the control disabled until the whole refresh job resolves", async () => {
    // The handler models the async flow: it resolves only once the job has
    // finished, so the control must stay locked the entire time.
    let finishJob: () => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finishJob = resolve;
        }),
    );
    const dialog = openPicker(versions, { onRefreshVersions });

    fireEvent.click(dialog.getByRole("button", { name: /Refresh List/ }));
    const busy = dialog.getByRole("button", { name: /Refresh List/ });
    expect(busy).toBeDisabled();
    expect(busy).toHaveAttribute("aria-busy", "true");

    // Still locked while the job runs.
    expect(dialog.getByRole("button", { name: /Refresh List/ })).toBeDisabled();

    await act(async () => {
      finishJob();
      await Promise.resolve();
    });
    expect(dialog.getByRole("button", { name: /Refresh List/ })).not.toBeDisabled();
  });

  it("cancels on a second press while running and unlocks the control", async () => {
    let finishJob: (() => void) | undefined;
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((_resolve, reject) => {
          finishJob = () => reject(new Error("Job cancelled"));
        }),
    );
    const onCancelRefresh = vi.fn().mockResolvedValue(undefined);
    const dialog = openPicker(versions, { onRefreshVersions, onCancelRefresh });

    fireEvent.click(dialog.getByRole("button", { name: /Refresh List/ }));
    // While running the row is not disabled when the surface can cancel; it
    // switches to the cancel affordance.
    const running = dialog.getByRole("button", { name: /Cancel refresh/ });
    expect(running).not.toBeDisabled();
    expect(running).toHaveAttribute("aria-busy", "true");

    fireEvent.click(running);
    expect(onCancelRefresh).toHaveBeenCalledTimes(1);
    // The first refresh rejects because the job was canceled; the cancel is not
    // shown as an error.
    await act(async () => {
      finishJob?.();
      await Promise.resolve();
    });
    expect(dialog.queryByText(REFRESH_VERSIONS_ERROR)).not.toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /Refresh List/ })).not.toBeDisabled();
  });
});

describe("VersionDropdown indexer releases", () => {
  const versions = [
    makeVersion({ file_id: 1, resolution: "2160p" }),
    makeVersion({ file_id: 2, resolution: "1080p" }),
  ];

  function withIndexerReleases(
    releases: Array<Record<string, unknown>>,
    props: Partial<Parameters<typeof VersionDropdown>[0]> = {},
  ) {
    watchDetailMock.current = { versions: [], indexer_releases: releases };
    return openPicker(versions, { contentId: "movie-1", ...props });
  }

  it("renders indexer rows below the playable versions with a Not downloaded badge", async () => {
    const dialog = withIndexerReleases([
      {
        release_id: "rel-1",
        title: "Movie.2026.2160p.WEB-DL.DDP5.1",
        resolution: "2160p",
        codec_video: "hevc",
        codec_audio: "eac3",
        size_bytes: 5_000_000_000,
        indexer: "Prowlarr",
        download_state: "not_downloaded",
      },
    ]);

    expect(await dialog.findByText("Not downloaded")).toBeInTheDocument();
    // The row carries the same metadata formatter as the playable rows.
    expect(dialog.getByText(/2160p · HEVC · EAC3/)).toBeInTheDocument();
    expect(dialog.getByText("Request")).toBeInTheDocument();

    // The indexer row sits after the playable rows.
    const buttons = dialog.getAllByRole("button");
    const playableIndex = buttons.findIndex((b) => /2160p/.test(b.textContent ?? ""));
    const requestIndex = buttons.findIndex((b) => /Request/.test(b.textContent ?? ""));
    expect(playableIndex).toBeGreaterThanOrEqual(0);
    expect(requestIndex).toBeGreaterThan(playableIndex);
  });

  it("posts the Request to the release endpoint and shows requesting then queued", async () => {
    let resolveRequest: (value: { release_id: string; state: "queued" }) => void = () => {};
    requestReleaseMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRequest = resolve;
        }),
    );
    const dialog = withIndexerReleases([
      { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "not_downloaded" },
    ]);

    fireEvent.click(await dialog.findByRole("button", { name: "Request Movie 2026 2160p" }));
    expect(requestReleaseMock).toHaveBeenCalledWith("movie-1", "rel-1");
    const busy = dialog.getByRole("button", { name: "Request Movie 2026 2160p" });
    expect(busy).toBeDisabled();
    expect(busy).toHaveAttribute("aria-busy", "true");

    await act(async () => {
      resolveRequest({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });

    const requested = dialog.getByRole("button", { name: "Requested Movie 2026 2160p" });
    expect(requested).toBeDisabled();
    // The row badge flips to Requested once the provider has accepted it.
    expect(dialog.getByText("Requested")).toBeInTheDocument();
  });

  it("keeps the row retryable when the request fails", async () => {
    requestReleaseMock.mockRejectedValueOnce(new Error("network"));
    const dialog = withIndexerReleases([
      { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "not_downloaded" },
    ]);

    fireEvent.click(await dialog.findByRole("button", { name: "Request Movie 2026 2160p" }));

    expect(await screen.findByText(REQUEST_RELEASE_ERROR)).toBeInTheDocument();
    const retry = dialog.getByRole("button", { name: "Request Movie 2026 2160p" });
    expect(retry).not.toBeDisabled();
    expect(retry).toHaveTextContent("Retry");
  });

  it("renders nothing new when the payload carries no indexer releases", async () => {
    watchDetailMock.current = { versions: [], indexer_releases: [] };
    const dialog = openPicker(versions, { contentId: "movie-1" });

    await act(async () => {
      await Promise.resolve();
    });
    expect(dialog.queryByText("Not downloaded")).not.toBeInTheDocument();
    expect(dialog.queryByText("Request")).not.toBeInTheDocument();
  });

  it("hides the indexer UI when the server capability is off", async () => {
    capabilityMock.mockResolvedValue({ state: "available", indexer_request: false });
    watchDetailMock.current = {
      versions: [],
      indexer_releases: [
        { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "not_downloaded" },
      ],
    };
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
        contentId="movie-1"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Version/ }));
    const dialog = within(screen.getByRole("dialog"));

    // The capability read resolves asynchronously; wait it out, then assert.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(dialog.queryByText("Not downloaded")).not.toBeInTheDocument();
    expect(dialog.queryByText("Request")).not.toBeInTheDocument();
  });

  it("treats an unknown download_state as not downloaded", async () => {
    const dialog = withIndexerReleases([
      { release_id: "rel-1", title: "Movie 2026 2160p", download_state: "something_new" },
    ]);

    expect(await dialog.findByText("Not downloaded")).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: "Request Movie 2026 2160p" })).toBeInTheDocument();
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
