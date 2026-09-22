// @vitest-environment jsdom
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { ServerVersionRanking } from "@/lib/qualityRanking";
import { QualityRankingSummary } from "./QualityRankingSummary";

// Radix Popover reads element sizes via ResizeObserver and opens through
// pointer capture, neither of which jsdom implements.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
if (typeof globalThis.ResizeObserver === "undefined") {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver =
    ResizeObserverStub;
}
if (typeof window !== "undefined" && !window.HTMLElement.prototype.hasPointerCapture) {
  window.HTMLElement.prototype.hasPointerCapture = () => false;
  window.HTMLElement.prototype.scrollIntoView = () => {};
}

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

function openPopover() {
  fireEvent.click(screen.getByRole("button", { name: "Version ranking" }));
}

describe("QualityRankingSummary control", () => {
  it("shows the server ranking by default", () => {
    renderControl();

    const trigger = screen.getByRole("button", { name: "Version ranking" });
    expect(trigger).toHaveTextContent("Ranking: Default ranking");
    expect(trigger).toHaveTextContent("4K+HDR");
  });

  it("opens a popover that lists only the payload-supported attributes", () => {
    renderControl();
    openPopover();

    expect(
      screen.getByText("Applies to this list only — playback selection is unchanged."),
    ).toBeInTheDocument();
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
    // Internal ranking signals are not offered.
    for (const excluded of ["Source", "Language", "Confirmed Source"]) {
      expect(screen.queryByRole("button", { name: excluded })).not.toBeInTheDocument();
    }
  });

  it("applies an attribute as the sort criterion when picked", () => {
    const { onApply } = renderControl();
    openPopover();

    fireEvent.click(screen.getByRole("button", { name: "File Size" }));
    expect(onApply).toHaveBeenCalledWith([{ attribute: "size", direction: "desc" }]);
  });

  it("toggles the direction of an active criterion", () => {
    const criteria = [{ attribute: "size" as const, direction: "desc" as const }];
    const { onApply } = renderControl({ effectiveCriteria: criteria, userCriteria: criteria });
    openPopover();

    fireEvent.click(screen.getByRole("button", { name: "Direction for File Size" }));
    expect(onApply).toHaveBeenCalledWith([{ attribute: "size", direction: "asc" }]);
  });

  it("marks a user override and resets it back to the profile default", () => {
    const criteria = [{ attribute: "size" as const, direction: "desc" as const }];
    const { onReset } = renderControl({ effectiveCriteria: criteria, userCriteria: criteria });

    expect(screen.getByRole("button", { name: "Version ranking" })).toHaveTextContent("Custom");
    openPopover();
    fireEvent.click(screen.getByRole("button", { name: "Reset to profile default" }));
    expect(onReset).toHaveBeenCalledTimes(1);
  });

  it("disables reset when there is no override", () => {
    renderControl();
    openPopover();

    expect(screen.getByRole("button", { name: "Reset to profile default" })).toBeDisabled();
  });
});
