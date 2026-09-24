// @vitest-environment jsdom

import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { createElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildVersionStatusLabels,
  QualityMenu,
  REFRESH_VERSIONS_ERROR,
  type VersionInfo,
} from "./QualityMenu";
import type { PlayerIndexerRelease } from "../types";
import { REQUEST_RELEASE_ERROR } from "@/hooks/useIndexerReleases";

// The sort preference reads/writes the canonical settings endpoints; the menu
// tests only exercise the display re-order, so it is mocked inert.
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
  capabilityMock.mockReset().mockResolvedValue({
    state: "available",
    indexer_search: true,
    indexer_request: true,
  });
  requestReleaseMock.mockReset();
});

const qualityOptions = [
  {
    id: "original",
    label: "Original",
    sublabel: "25 Mbps",
    resolution: "2160p",
    bitrateKbps: 25_000,
    isOriginal: true,
  },
  {
    id: "1080p-medium",
    label: "1080p Medium",
    sublabel: "6 Mbps",
    resolution: "1080p",
    bitrateKbps: 6000,
    isOriginal: false,
  },
];

function renderVersionMenu(
  overrides: {
    onRefreshVersions?: () => Promise<void>;
    onCancelRefresh?: () => Promise<void> | void;
    onSwitchVersion?: (fileId: number) => void;
    versions?: VersionInfo[];
    indexerReleases?: PlayerIndexerRelease[];
    contentId?: string;
  } = {},
) {
  render(
    createElement(QualityMenu, {
      options: qualityOptions,
      activeId: "original",
      isTranscoding: false,
      error: null,
      onSelect: () => {},
      versions: overrides.versions ?? [
        makeVersionInfo({ fileId: 1, label: "1080p H264", isCurrentSource: true }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
      indexerReleases: overrides.indexerReleases,
      contentId: overrides.contentId ?? "content-1",
      onSwitchVersion: overrides.onSwitchVersion ?? (() => {}),
      onRefreshVersions: overrides.onRefreshVersions,
      onCancelRefresh: overrides.onCancelRefresh,
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Quality" }));
}

function makeVersionInfo(overrides: Partial<VersionInfo> = {}): VersionInfo {
  return {
    fileId: 1,
    label: "2160p HEVC HDR",
    isCurrentSource: false,
    isRequestedSource: false,
    ...overrides,
  };
}

describe("buildVersionStatusLabels", () => {
  it("shows only Playing when requested and current source match", () => {
    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          isCurrentSource: true,
          isRequestedSource: true,
        }),
      ),
    ).toEqual(["Playing"]);
  });

  it("shows Playing and Requested on different versions", () => {
    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          isCurrentSource: true,
        }),
      ),
    ).toEqual(["Playing"]);

    expect(
      buildVersionStatusLabels(
        makeVersionInfo({
          fileId: 2,
          isRequestedSource: true,
        }),
      ),
    ).toEqual(["Requested"]);
  });
});

describe("QualityMenu", () => {
  it("keeps quality adjustments available while the room locks version switching", () => {
    const select = vi.fn();
    const switchVersion = vi.fn();
    render(
      createElement(QualityMenu, {
        options: [
          {
            id: "original",
            label: "Original",
            sublabel: "",
            resolution: "1080p",
            bitrateKbps: 8000,
            isOriginal: true,
          },
        ],
        activeId: "original",
        isTranscoding: false,
        error: null,
        onSelect: select,
        onSwitchVersion: switchVersion,
        versionLocked: true,
        versions: [makeVersionInfo(), makeVersionInfo({ fileId: 2, label: "1080p H264" })],
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByText(/Watch Party keeps everyone on the same version/)).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /2160p HEVC HDR/ })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: /Original/ }));
    expect(select).toHaveBeenCalledWith("original");
    expect(switchVersion).not.toHaveBeenCalled();
  });

  it("shows the stored resolution preference as the selected bitrate rung", () => {
    render(
      createElement(QualityMenu, {
        options: [
          {
            id: "original",
            label: "Original",
            sublabel: "25 Mbps",
            resolution: "2160p",
            bitrateKbps: 25_000,
            isOriginal: true,
          },
          {
            id: "1080p-medium",
            label: "1080p Medium",
            sublabel: "6 Mbps",
            resolution: "1080p",
            bitrateKbps: 6000,
            isOriginal: false,
          },
        ],
        activeId: "1080p",
        isTranscoding: false,
        error: null,
        onSelect: () => {},
      }),
    );

    expect(screen.getByRole("button", { name: "Quality" })).toHaveTextContent("1080p Medium");
    fireEvent.click(screen.getByRole("button", { name: "Quality" }));
    expect(screen.getByRole("menu")).toHaveClass("z-30");
    expect(screen.getByRole("menuitem", { name: /1080p Medium.*Selected/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
  });
});

describe("QualityMenu version format score", () => {
  it("shows the score badge when the server ranked the candidate", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264", formatScore: 850 }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
    });

    expect(screen.getByText("★ 850")).toBeInTheDocument();
  });

  it("shows no badge for an unscored or zero-scored version", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264" }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC", formatScore: 0 }),
      ],
    });

    expect(screen.queryByText(/★/)).not.toBeInTheDocument();
  });

  it("keeps a negative score visible so a demoted candidate is not mistaken for unscored", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "1080p H264", formatScore: -200 }),
        makeVersionInfo({ fileId: 2, label: "2160p HEVC" }),
      ],
    });

    expect(screen.getByText("★ -200")).toBeInTheDocument();
  });
});

describe("QualityMenu version row parity with the item picker", () => {
  it("renders audio and subtitle language badges", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          audioLanguages: ["English", "French"],
          subtitleLanguages: ["German"],
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    const row = screen.getByRole("menuitem", { name: /2160p HEVC/ });
    expect(within(row).getByText("English")).toBeInTheDocument();
    expect(within(row).getByText("French")).toBeInTheDocument();
    expect(within(row).getByText("German")).toBeInTheDocument();
  });

  it("renders the shared release + size detail line", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          detail: "Movie 2026 2160p WEB-DL · 47.1 GB",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText("Movie 2026 2160p WEB-DL · 47.1 GB")).toBeInTheDocument();
  });

  it("renders the ranking indicator and names the profile", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          filePath: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
          profileLabel: "4K+HDR",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(screen.getByText(/4K\+HDR/)).toBeInTheDocument();
  });
});

describe("QualityMenu server virtual ranking", () => {
  it("renders the server payload's criteria and profile label when present", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          virtualRanking: {
            profile_label: "4K HDR",
            source: "profile",
            criteria: [
              { attribute: "score", direction: "desc" },
              { attribute: "resolution", direction: "desc" },
            ],
          },
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: score ↓ · resolution ↓/)).toBeInTheDocument();
    expect(screen.getByText(/4K HDR/)).toBeInTheDocument();
  });

  it("falls back to the profile label and default order without the block", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({
          fileId: 1,
          label: "2160p HEVC",
          filePath: "virtual://movie/tt1?profile=4K%2BHDR&result=abc",
          profileLabel: "4K+HDR",
        }),
        makeVersionInfo({ fileId: 2, label: "1080p H264" }),
      ],
    });

    expect(screen.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(screen.getByText(/4K\+HDR/)).toBeInTheDocument();
    expect(screen.queryByText(/Ranking: score/)).not.toBeInTheDocument();
  });
});

describe("QualityMenu version sort preference", () => {
  function versionRowOrder() {
    return screen
      .getAllByRole("menuitem")
      .filter((row) => /HEVC|H264/.test(row.textContent ?? ""))
      .map((row) => (row.textContent?.includes("HEVC") ? "HEVC" : "H264"));
  }

  it("re-orders the version rows for the viewer without starting playback", () => {
    versionSortMock.criteria = [{ attribute: "size", direction: "desc" }];
    const onSwitchVersion = vi.fn();
    renderVersionMenu({
      onSwitchVersion,
      versions: [
        makeVersionInfo({ fileId: 1, label: "2160p HEVC", sortable: { fileSize: 100 } }),
        makeVersionInfo({ fileId: 2, label: "1080p H264", sortable: { fileSize: 300 } }),
      ],
    });

    expect(versionRowOrder()).toEqual(["H264", "HEVC"]);
    // Display-only: the reorder starts no playback and switches no version.
    expect(onSwitchVersion).not.toHaveBeenCalled();
  });

  it("keeps the server's incoming order with no override", () => {
    renderVersionMenu({
      versions: [
        makeVersionInfo({ fileId: 1, label: "2160p HEVC", sortable: { fileSize: 100 } }),
        makeVersionInfo({ fileId: 2, label: "1080p H264", sortable: { fileSize: 300 } }),
      ],
    });

    expect(versionRowOrder()).toEqual(["HEVC", "H264"]);
  });
});

describe("QualityMenu version list refresh", () => {
  it("renders Refresh List as the last row of the version list", () => {
    renderVersionMenu({ onRefreshVersions: vi.fn().mockResolvedValue(undefined) });

    expect(screen.getByText("Version")).toBeInTheDocument();
    const rows = screen.getAllByRole("menuitem");
    // Two version rows, then the refresh action, then the quality options.
    expect(rows[0]).toHaveTextContent("1080p H264");
    expect(rows[1]).toHaveTextContent("2160p HEVC");
    expect(rows[2]).toHaveTextContent("Refresh List");
    expect(rows[3]).toHaveTextContent("Original");
  });

  it("does not render the refresh row when no refresh handler is wired", () => {
    renderVersionMenu();

    expect(screen.queryByRole("menuitem", { name: /Refresh List/ })).not.toBeInTheDocument();
  });

  it("triggers exactly one refresh and disables the row while in flight", async () => {
    let resolveRefresh: () => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          resolveRefresh = resolve;
        }),
    );
    renderVersionMenu({ onRefreshVersions });

    const refreshRow = screen.getByRole("menuitem", { name: /Refresh List/ });
    fireEvent.click(refreshRow);

    expect(onRefreshVersions).toHaveBeenCalledTimes(1);
    const busyRow = screen.getByRole("menuitem", { name: /Refresh List/ });
    expect(busyRow).toBeDisabled();
    expect(busyRow).toHaveAttribute("aria-busy", "true");

    // A second click while the request is in flight is ignored.
    fireEvent.click(busyRow);
    expect(onRefreshVersions).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolveRefresh();
      await Promise.resolve();
    });
    expect(screen.getByRole("menuitem", { name: /Refresh List/ })).not.toBeDisabled();
  });

  it("keeps the row locked for the whole async job, not just acceptance", async () => {
    let finishJob: () => void = () => {};
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finishJob = resolve;
        }),
    );
    renderVersionMenu({ onRefreshVersions });

    fireEvent.click(screen.getByRole("menuitem", { name: /Refresh List/ }));
    // Locked while the job runs; the control is never left enabled mid-job.
    expect(screen.getByRole("menuitem", { name: /Refresh List/ })).toBeDisabled();

    await act(async () => {
      finishJob();
      await Promise.resolve();
    });
    expect(screen.getByRole("menuitem", { name: /Refresh List/ })).not.toBeDisabled();
  });

  it("cancels on a second press while running and unlocks the row", async () => {
    let finishJob: (() => void) | undefined;
    const onRefreshVersions = vi.fn(
      () =>
        new Promise<void>((_resolve, reject) => {
          finishJob = () => reject(new Error("Job cancelled"));
        }),
    );
    const onCancelRefresh = vi.fn().mockResolvedValue(undefined);
    renderVersionMenu({ onRefreshVersions, onCancelRefresh });

    fireEvent.click(screen.getByRole("menuitem", { name: /Refresh List/ }));
    const running = screen.getByRole("menuitem", { name: /Cancel refresh/ });
    expect(running).not.toBeDisabled();
    expect(running).toHaveAttribute("aria-busy", "true");

    fireEvent.click(running);
    expect(onCancelRefresh).toHaveBeenCalledTimes(1);

    await act(async () => {
      finishJob?.();
      await Promise.resolve();
    });
    expect(screen.queryByText(REFRESH_VERSIONS_ERROR)).not.toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Refresh List/ })).not.toBeDisabled();
  });

  it("keeps the version rows and shows an inline message when a refresh fails", async () => {
    const onRefreshVersions = vi.fn().mockRejectedValue(new Error("network"));
    renderVersionMenu({ onRefreshVersions });

    fireEvent.click(screen.getByRole("menuitem", { name: /Refresh List/ }));

    expect(await screen.findByText(REFRESH_VERSIONS_ERROR)).toBeInTheDocument();
    // The known candidates stay on screen next to the failure.
    expect(screen.getByRole("menuitem", { name: /1080p H264/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /2160p HEVC/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Refresh List/ })).not.toBeDisabled();
  });
});

describe("QualityMenu indexer releases", () => {
  const release = (overrides: Partial<PlayerIndexerRelease> = {}): PlayerIndexerRelease => ({
    release_id: "rel-1",
    title: "Movie.2026.2160p.WEB-DL.DDP5.1",
    download_state: "not_downloaded",
    ...overrides,
  });

  it("renders indexer rows below the playable versions with a Not downloaded badge", async () => {
    renderVersionMenu({
      contentId: "content-1",
      indexerReleases: [
        release({ resolution: "2160p", codec_video: "hevc", size_bytes: 5_000_000_000 }),
      ],
    });

    expect(await screen.findByText("Not downloaded")).toBeInTheDocument();
    expect(screen.getByText(/2160p · HEVC/)).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /Request Movie/ })).toBeInTheDocument();

    // The indexer row comes after the playable version rows.
    const items = screen.getAllByRole("menuitem");
    const playable = items.findIndex((row) => /1080p H264|2160p HEVC/.test(row.textContent ?? ""));
    const indexer = items.findIndex((row) => /Not downloaded/.test(row.textContent ?? ""));
    expect(indexer).toBeGreaterThan(playable);
  });

  it("posts the Request to the release endpoint and flips the row to Requested", async () => {
    let resolveRequest: (value: { release_id: string; state: "queued" }) => void = () => {};
    requestReleaseMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRequest = resolve;
        }),
    );
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    fireEvent.click(await screen.findByRole("menuitem", { name: /Request Movie/ }));
    expect(requestReleaseMock).toHaveBeenCalledWith("content-1", "rel-1");
    expect(screen.getByRole("menuitem", { name: /Request Movie/ })).toBeDisabled();

    await act(async () => {
      resolveRequest({ release_id: "rel-1", state: "queued" });
      await Promise.resolve();
    });

    const row = screen.getByRole("menuitem", { name: /Requested Movie/ });
    expect(row).toBeDisabled();
    expect(within(row).getByText("Requested")).toBeInTheDocument();
  });

  it("keeps the row retryable when the request fails", async () => {
    requestReleaseMock.mockRejectedValueOnce(new Error("network"));
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    fireEvent.click(await screen.findByRole("menuitem", { name: /Request Movie/ }));

    expect(await screen.findByText(REQUEST_RELEASE_ERROR)).toBeInTheDocument();
    const retry = screen.getByRole("menuitem", { name: /Request Movie/ });
    expect(retry).not.toBeDisabled();
    expect(retry).toHaveTextContent("Retry");
  });

  it("renders nothing new when no indexer releases are present", async () => {
    renderVersionMenu({ indexerReleases: [] });

    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByText("Not downloaded")).not.toBeInTheDocument();
  });

  it("hides the indexer UI when the server capability is off", async () => {
    capabilityMock.mockResolvedValue({ state: "available", indexer_request: false });
    renderVersionMenu({ contentId: "content-1", indexerReleases: [release()] });

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.queryByText("Not downloaded")).not.toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /Request Movie/ })).not.toBeInTheDocument();
  });
});
