// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { QualityProfileModal } from "./QualityProfileModal";
import { MAX_SORT_CRITERIA, type QualityProfileRule, type SortCriterion } from "./scoringPresets";

// Radix Select reads element sizes through ResizeObserver and opens through
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

function makeProfile(overrides: Partial<QualityProfileRule> = {}): QualityProfileRule {
  return { label: "Test profile", preferred_order: 1, ...overrides };
}

function renderModal(editingProfile: QualityProfileRule) {
  const onSave = vi.fn();
  const onClose = vi.fn();
  render(
    <QualityProfileModal
      isOpen
      onClose={onClose}
      onSave={onSave}
      editingProfile={editingProfile}
      defaultOrder={1}
    />,
  );
  return { onSave, onClose };
}

async function save(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Save Profile" }));
}

const sizeDesc: SortCriterion = { attribute: "size", direction: "desc" };
const bitrateAsc: SortCriterion = { attribute: "bitrate", direction: "asc" };

describe("QualityProfileModal sort criteria", () => {
  it("renders the existing criteria in order", () => {
    renderModal(makeProfile({ sort: [sizeDesc, bitrateAsc] }));

    expect(screen.getByRole("combobox", { name: "Criterion 1 attribute" })).toHaveTextContent(
      "File Size",
    );
    expect(screen.getByRole("combobox", { name: "Criterion 1 direction" })).toHaveTextContent(
      "Descending",
    );
    expect(screen.getByRole("combobox", { name: "Criterion 2 attribute" })).toHaveTextContent(
      "Bitrate",
    );
    expect(screen.getByRole("combobox", { name: "Criterion 2 direction" })).toHaveTextContent(
      "Ascending",
    );
  });

  it("appends a new criterion with size/descending defaults", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(
      makeProfile({ sort: [{ attribute: "score", direction: "desc" }] }),
    );

    await user.click(screen.getByRole("button", { name: "Add criterion" }));
    await save(user);

    expect(onSave).toHaveBeenCalledWith(
      expect.objectContaining({
        sort: [
          { attribute: "score", direction: "desc" },
          { attribute: "size", direction: "desc" },
        ],
      }),
    );
  });

  it("moves a criterion up", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile({ sort: [sizeDesc, bitrateAsc] }));

    await user.click(screen.getByRole("button", { name: "Move bitrate ↑ up" }));
    await save(user);

    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ sort: [bitrateAsc, sizeDesc] }));
  });

  it("moves a criterion down", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile({ sort: [sizeDesc, bitrateAsc] }));

    await user.click(screen.getByRole("button", { name: "Move size ↓ down" }));
    await save(user);

    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ sort: [bitrateAsc, sizeDesc] }));
  });

  it("removes a criterion", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile({ sort: [sizeDesc, bitrateAsc] }));

    await user.click(screen.getByRole("button", { name: "Remove size ↓" }));
    await save(user);

    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ sort: [bitrateAsc] }));
  });

  it("changes an attribute and a direction through the selects", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile({ sort: [sizeDesc] }));

    await user.click(screen.getByRole("combobox", { name: "Criterion 1 attribute" }));
    await user.click(screen.getByRole("option", { name: "Bit Depth" }));
    await user.click(screen.getByRole("combobox", { name: "Criterion 1 direction" }));
    await user.click(screen.getByRole("option", { name: "Ascending ↑" }));
    await save(user);

    expect(onSave).toHaveBeenCalledWith(
      expect.objectContaining({ sort: [{ attribute: "bit_depth", direction: "asc" }] }),
    );
  });

  it("omits sort entirely when the profile has no criteria", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile());

    await save(user);

    const payload = onSave.mock.calls[0]?.[0] as QualityProfileRule;
    expect(payload).not.toHaveProperty("sort");
  });

  it("omits sort when the criteria list is explicitly empty", async () => {
    const user = userEvent.setup();
    const { onSave } = renderModal(makeProfile({ sort: [] }));

    await save(user);

    const payload = onSave.mock.calls[0]?.[0] as QualityProfileRule;
    expect(payload).not.toHaveProperty("sort");
  });

  it("round-trips a loaded criteria list unchanged", async () => {
    const user = userEvent.setup();
    const sort: SortCriterion[] = [
      { attribute: "hdr", direction: "asc" },
      { attribute: "audio_channels", direction: "desc" },
      { attribute: "size", direction: "desc" },
    ];
    const { onSave } = renderModal(makeProfile({ sort }));

    await save(user);

    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ sort }));
  });

  it("stops at the cap and explains why", async () => {
    const user = userEvent.setup();
    renderModal(
      makeProfile({ sort: Array.from({ length: MAX_SORT_CRITERIA }, () => ({ ...sizeDesc })) }),
    );

    const addButton = screen.getByRole("button", { name: "Add criterion" });
    expect(addButton).toBeDisabled();
    expect(
      screen.getByText(`Maximum of ${MAX_SORT_CRITERIA} sort criteria per profile.`),
    ).toBeInTheDocument();

    // A direct click cannot push past the cap either.
    await user.click(addButton);
    expect(screen.getAllByRole("combobox", { name: /Criterion \d+ attribute/ })).toHaveLength(
      MAX_SORT_CRITERIA,
    );
  });
});
