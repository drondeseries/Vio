// @vitest-environment jsdom
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { VERSION_SORT_PRESETS, type ServerVersionRanking } from "@/lib/qualityRanking";
import { QualityRankingSummary } from "./QualityRankingSummary";

const serverRanking: ServerVersionRanking = {
  profileLabel: "4K+HDR",
  criteria: [],
  source: null,
  fromPayload: false,
};

function renderControl(overrides: Partial<Parameters<typeof QualityRankingSummary>[0]> = {}) {
  const onApply = vi.fn();
  const onReset = vi.fn();
  render(
    <QualityRankingSummary
      serverRanking={serverRanking}
      userCriteria={[]}
      effectiveCriteria={[]}
      onApply={onApply}
      onReset={onReset}
      {...overrides}
    />,
  );
  return { onApply, onReset };
}

function presetCriteria(id: string) {
  return VERSION_SORT_PRESETS.find((preset) => preset.id === id)?.criteria ?? [];
}

describe("QualityRankingSummary presets", () => {
  it("renders the presets with the profile default highlighted and no popover", () => {
    renderControl();

    const group = screen.getByRole("group", { name: "Version order" });
    expect(within(group).getByRole("button", { name: "Profile default" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    for (const label of ["Quality first", "Biggest first", "Bitrate first", "Custom…"]) {
      expect(within(group).getByRole("button", { name: label })).toBeInTheDocument();
    }
    expect(screen.getByText(/Ranking: Default ranking/)).toBeInTheDocument();
    expect(screen.getByText(/4K\+HDR/)).toBeInTheDocument();
    expect(
      screen.getByText("Applies to this list only — playback selection is unchanged."),
    ).toBeInTheDocument();
    // The access is inline; no blocky popup.
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders the same controls in the player's dark tone", () => {
    renderControl({ tone: "dark" });

    expect(screen.getByRole("group", { name: "Version order" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Profile default" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(
      screen.getByText("Applies to this list only — playback selection is unchanged."),
    ).toBeInTheDocument();
  });

  it("applies a named preset in one tap", () => {
    const { onApply } = renderControl();

    fireEvent.click(screen.getByRole("button", { name: "Biggest first" }));

    expect(onApply).toHaveBeenCalledWith([
      { attribute: "size", direction: "desc" },
      { attribute: "bitrate", direction: "desc" },
    ]);
  });

  it("keeps the profile default as the reset affordance", () => {
    const criteria = presetCriteria("biggest");
    const { onReset } = renderControl({ userCriteria: criteria, effectiveCriteria: criteria });

    fireEvent.click(screen.getByRole("button", { name: "Profile default" }));

    expect(onReset).toHaveBeenCalledTimes(1);
  });

  it("highlights the preset a stored order matches", () => {
    const quality = presetCriteria("quality");
    renderControl({ userCriteria: quality, effectiveCriteria: quality });

    expect(screen.getByRole("button", { name: "Quality first" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Profile default" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(screen.getByRole("button", { name: "Custom…" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("marks a non-preset order as Custom and reveals the fine-tuner inline", () => {
    const criteria = [{ attribute: "hdr" as const, direction: "asc" as const }];
    renderControl({ userCriteria: criteria, effectiveCriteria: criteria });

    const custom = screen.getByRole("button", { name: "Custom…" });
    expect(custom).toHaveAttribute("aria-pressed", "true");
    expect(custom).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(custom);
    expect(screen.getByRole("button", { name: "Custom…" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

describe("QualityRankingSummary custom fine-tuner", () => {
  it("lists only the payload-supported attributes", () => {
    renderControl();
    fireEvent.click(screen.getByRole("button", { name: "Custom…" }));

    for (const label of [
      "File Size",
      "Bitrate",
      "Resolution",
      "Audio Channels",
      "Bit Depth",
      "HDR",
      "Format Score",
    ]) {
      expect(screen.getByRole("button", { name: label })).toBeInTheDocument();
    }
    for (const excluded of ["Source", "Language", "Confirmed Source"]) {
      expect(screen.queryByRole("button", { name: excluded })).not.toBeInTheDocument();
    }
  });

  it("appends a picked attribute as a descending criterion", () => {
    const { onApply } = renderControl();
    fireEvent.click(screen.getByRole("button", { name: "Custom…" }));

    fireEvent.click(screen.getByRole("button", { name: "File Size" }));

    expect(onApply).toHaveBeenLastCalledWith([{ attribute: "size", direction: "desc" }]);
  });

  it("toggles the direction of an active criterion", () => {
    const criteria = [{ attribute: "size" as const, direction: "desc" as const }];
    const { onApply } = renderControl({ userCriteria: criteria, effectiveCriteria: criteria });
    fireEvent.click(screen.getByRole("button", { name: "Custom…" }));

    fireEvent.click(screen.getByRole("button", { name: "Direction for File Size" }));

    expect(onApply).toHaveBeenLastCalledWith([{ attribute: "size", direction: "asc" }]);
  });

  it("resets from inside the fine-tuner when a custom order is active", () => {
    const criteria = [{ attribute: "hdr" as const, direction: "asc" as const }];
    const { onReset } = renderControl({ userCriteria: criteria, effectiveCriteria: criteria });
    fireEvent.click(screen.getByRole("button", { name: "Custom…" }));

    fireEvent.click(screen.getByRole("button", { name: "Reset to profile default" }));

    expect(onReset).toHaveBeenCalledTimes(1);
  });
});
