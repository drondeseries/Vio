import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  useAdminVirtualItems: vi.fn(),
  useAdminLibraries: vi.fn(),
  useCreateLibrary: vi.fn(),
}));

vi.mock("@/hooks/queries/admin/virtualItems", () => ({
  useAdminVirtualItems: (...args: unknown[]) => mocks.useAdminVirtualItems(...args),
}));

vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: (...args: unknown[]) => mocks.useAdminLibraries(...args),
  useCreateLibrary: (...args: unknown[]) => mocks.useCreateLibrary(...args),
}));

import AdminVirtualLibrary from "./AdminVirtualLibrary";

interface ItemOverrides {
  id?: string;
  title?: string;
  type?: string;
  library_id?: string;
  library_name?: string;
  installation_id?: string;
  candidate_count?: number;
  failed_count?: number;
  last_delivered_at?: string;
  last_seen_at?: string;
  release_names?: string[];
}

function virtualItem(overrides: ItemOverrides = {}) {
  return {
    id: "movie:1",
    title: "Fixture Movie",
    type: "movie",
    library_id: "1",
    library_name: "Virtual Movies",
    installation_id: "0",
    candidate_count: 3,
    failed_count: 0,
    last_delivered_at: "2026-09-17T09:00:00Z",
    last_seen_at: "2026-09-17T10:00:00Z",
    release_names: ["1080p"],
    ...overrides,
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <AdminVirtualLibrary />
    </MemoryRouter>,
  );
}

function queryResult(data: unknown, overrides: Record<string, unknown> = {}) {
  return { data, isLoading: false, error: null, isFetching: false, refetch: vi.fn(), ...overrides };
}

describe("AdminVirtualLibrary", () => {
  beforeEach(() => {
    mocks.useAdminVirtualItems.mockReset();
    mocks.useAdminLibraries.mockReset();
    mocks.useCreateLibrary.mockReset();

    mocks.useAdminVirtualItems.mockReturnValue(queryResult([virtualItem()]));
    mocks.useAdminLibraries.mockReturnValue({
      data: [
        {
          id: 1,
          name: "Virtual Movies",
          paths: ["virtual://movies"],
          type: "movies",
        },
        {
          id: 2,
          name: "Virtual Series",
          paths: ["virtual://series"],
          type: "series",
        },
      ],
      isLoading: false,
    });
    mocks.useCreateLibrary.mockReturnValue({ mutate: vi.fn(), isPending: false });
  });

  it("renders a row and flags failed candidate deliveries", () => {
    mocks.useAdminVirtualItems.mockReturnValue(
      queryResult([virtualItem({ failed_count: 2, candidate_count: 5 })]),
    );

    renderPage();

    expect(screen.getByText("Fixture Movie")).toBeInTheDocument();
    expect(screen.getByText("Movie")).toBeInTheDocument();
    expect(screen.getByText("Host")).toBeInTheDocument();
    expect(screen.getByText("2 failed")).toBeInTheDocument();
  });

  it("renders Never when an item was never delivered or seen", () => {
    mocks.useAdminVirtualItems.mockReturnValue(
      queryResult([virtualItem({ last_delivered_at: undefined, last_seen_at: undefined })]),
    );

    renderPage();

    expect(screen.getAllByText("Never")).toHaveLength(2);
  });

  it("narrows rows by title or content id", async () => {
    const user = userEvent.setup();
    mocks.useAdminVirtualItems.mockReturnValue(
      queryResult([
        virtualItem({ id: "movie:alpha", title: "Alpha" }),
        virtualItem({ id: "movie:beta", title: "Beta" }),
      ]),
    );

    renderPage();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();

    await user.type(screen.getByLabelText("Filter virtual items"), "beta");

    expect(screen.queryByText("Alpha")).not.toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();
    expect(screen.getByText("Showing 1 of 2")).toBeInTheDocument();
  });

  it("hides healthy rows when Failed only is on", async () => {
    const user = userEvent.setup();
    mocks.useAdminVirtualItems.mockReturnValue(
      queryResult([
        virtualItem({ id: "movie:ok", title: "Healthy", failed_count: 0 }),
        virtualItem({ id: "movie:bad", title: "Broken", failed_count: 1 }),
      ]),
    );

    renderPage();
    expect(screen.getByText("Healthy")).toBeInTheDocument();

    await user.click(screen.getByRole("switch", { name: "Failed only" }));

    expect(screen.queryByText("Healthy")).not.toBeInTheDocument();
    expect(screen.getByText("Broken")).toBeInTheDocument();
  });

  it("does not show the empty state while loading", () => {
    mocks.useAdminVirtualItems.mockReturnValue(
      queryResult(undefined, { data: undefined, isLoading: true }),
    );

    renderPage();

    expect(screen.queryByText("No virtual items yet")).not.toBeInTheDocument();
  });

  it("offers one-click creation when no virtual library exists", async () => {
    const user = userEvent.setup();
    const mutate = vi.fn();
    mocks.useAdminLibraries.mockReturnValue({
      data: [{ id: 3, name: "Movies", paths: ["/media/movies"], type: "movies" }],
      isLoading: false,
    });
    mocks.useCreateLibrary.mockReturnValue({ mutate, isPending: false });

    renderPage();

    expect(screen.getByText("Virtual library setup")).toBeInTheDocument();
    expect(screen.getAllByText("Not set up")).toHaveLength(2);

    await user.click(screen.getByRole("button", { name: /Create Virtual Movies/ }));

    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate.mock.calls[0]?.[0]).toEqual({
      paths: ["virtual://movies"],
      type: "movies",
      name: "Virtual Movies",
    });
  });

  it("reports which virtual kind already exists when setup is partial", () => {
    mocks.useAdminVirtualItems.mockReturnValue(queryResult([]));
    mocks.useAdminLibraries.mockReturnValue({
      data: [{ id: 1, name: "Virtual Movies", paths: ["virtual://movies"], type: "movies" }],
      isLoading: false,
    });

    renderPage();

    expect(screen.getByText("Virtual library setup")).toBeInTheDocument();
    expect(screen.getByText("Virtual Movies")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Create Virtual Movies/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Create Virtual Series/ })).toBeInTheDocument();
  });

  it("hides the setup card once both kinds exist", () => {
    mocks.useAdminLibraries.mockReturnValue({
      data: [
        { id: 1, name: "Virtual Movies", paths: ["virtual://movies"], type: "movies" },
        { id: 2, name: "Virtual Series", paths: ["virtual://series"], type: "series" },
      ],
      isLoading: false,
    });

    renderPage();

    expect(screen.queryByText("Virtual library setup")).not.toBeInTheDocument();
  });

  it("renders the empty state when the list is empty and libraries exist", () => {
    mocks.useAdminVirtualItems.mockReturnValue(queryResult([]));

    renderPage();

    expect(screen.getByText("No virtual items yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Plugins page" })).toHaveAttribute(
      "href",
      "/admin/plugins",
    );
  });
});
