// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { SortCriteriaSummary } from "./SortCriteriaSummary";
import { VioScoringProfilesCard } from "./VioScoringProfilesCard";
import type { QualityProfileRule } from "./scoringPresets";

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
if (typeof globalThis.ResizeObserver === "undefined") {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver =
    ResizeObserverStub;
}

function makeForm(values: Record<string, string>) {
  return {
    getValue: (key: string) => values[key],
    setValue: (key: string, value: string) => {
      values[key] = value;
    },
  };
}

describe("SortCriteriaSummary", () => {
  it("renders one chip per criterion in order", () => {
    render(
      <SortCriteriaSummary
        criteria={[
          { attribute: "size", direction: "desc" },
          { attribute: "bitrate", direction: "desc" },
        ]}
      />,
    );

    expect(screen.getByText("size ↓")).toBeInTheDocument();
    expect(screen.getByText("bitrate ↓")).toBeInTheDocument();
  });

  it("shows the default-order hint when the profile has no criteria", () => {
    const { rerender } = render(<SortCriteriaSummary criteria={[]} />);
    expect(screen.getByText("Default order")).toBeInTheDocument();

    rerender(<SortCriteriaSummary criteria={undefined} />);
    expect(screen.getByText("Default order")).toBeInTheDocument();
  });
});

describe("VioScoringProfilesCard sort summary", () => {
  it("shows chips for a configured profile and the hint for a default one", async () => {
    const user = userEvent.setup();
    const profiles: QualityProfileRule[] = [
      {
        label: "Big first",
        preferred_order: 1,
        sort: [
          { attribute: "size", direction: "desc" },
          { attribute: "bitrate", direction: "asc" },
        ],
      },
      { label: "Defaulted", preferred_order: 2 },
    ];
    const form = makeForm({
      "virtual_library.quality_preset": "custom",
      "virtual_library.quality_profiles": JSON.stringify(profiles),
      "virtual_library.enable_quality_profiles": "true",
    });

    render(<VioScoringProfilesCard form={form} defaultExpanded />);
    await user.click(screen.getByRole("tab", { name: "Quality Profiles & Resolution" }));

    expect(screen.getByText("size ↓")).toBeInTheDocument();
    expect(screen.getByText("bitrate ↑")).toBeInTheDocument();
    expect(screen.getByText("Default order")).toBeInTheDocument();
  });
});
