// @vitest-environment jsdom

import { act, fireEvent, render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import {
  buildVersionStatusLabels,
  QualityMenu,
  REFRESH_VERSIONS_ERROR,
  type VersionInfo,
} from "./QualityMenu";

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
    versions?: VersionInfo[];
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
      onSwitchVersion: () => {},
      onRefreshVersions: overrides.onRefreshVersions,
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Quality" }));
}

function makeVersionInfo(overrides: Partial<VersionInfo> = {}): VersionInfo {
  return {
    fileId: overrides.fileId ?? 1,
    label: overrides.label ?? "2160p HEVC HDR",
    formatScore: overrides.formatScore,
    isCurrentSource: overrides.isCurrentSource ?? false,
    isRequestedSource: overrides.isRequestedSource ?? false,
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
