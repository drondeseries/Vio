import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Library } from "@/api/types";
import StreamingSettings from "./StreamingSettings";

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver;

const useSettingsFormMock = vi.fn();
const useAdminLibrariesMock = vi.fn();
const mutateAsyncMock = vi.fn();

vi.mock("@/hooks/useSettingsForm", () => ({
  useSettingsForm: (...args: unknown[]) => useSettingsFormMock(...args),
}));

vi.mock("@/hooks/useRestartKeys", () => ({
  useRestartKeys: () => new Set<string>(),
}));

vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: () => useAdminLibrariesMock(),
  useCreateLibrary: () => ({
    mutateAsync: mutateAsyncMock,
    isPending: false,
  }),
}));

vi.mock("@/components/admin/ConnectionCheckAction", () => ({
  ConnectionCheckAction: ({
    label = "Check Connection",
    onClick,
    disabled,
  }: {
    label?: string;
    onClick?: () => void;
    disabled?: boolean;
  }) => (
    <button type="button" onClick={onClick} disabled={disabled}>
      {label}
    </button>
  ),
  useConnectionCheck: () => ({
    run: vi.fn(),
    result: null,
    isPending: false,
  }),
}));

function makeForm(values: Record<string, string> = {}, sensitiveConfigured: string[] = []) {
  const formValues = { ...values };
  return {
    isLoading: false,
    getValue: (key: string) => formValues[key] ?? "",
    setValue: vi.fn((key: string, val: string) => {
      formValues[key] = val;
    }),
    resetValue: vi.fn(),
    isClearStaged: () => false,
    sensitiveConfigured,
    isDirty: () => false,
    dirtyCount: 0,
    save: vi.fn(),
    discard: vi.fn(),
    isSaving: false,
    restartRequired: false,
  };
}

const mockLibraries = [
  {
    id: 1,
    name: "My Movies",
    type: "movies",
    paths: ["/data/movies"],
    enabled: true,
  },
  {
    id: 2,
    name: "My Series",
    type: "series",
    paths: ["/data/series"],
    enabled: true,
  },
] as unknown as Library[];

function renderPage() {
  return render(
    <MemoryRouter>
      <StreamingSettings />
    </MemoryRouter>,
  );
}

describe("StreamingSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useSettingsFormMock.mockReturnValue(
      makeForm({
        "virtual_library.enabled": "true",
        "virtual_library.manifest_url": "https://example.com/manifest.json",
        "virtual_library.movie_library_id": "1",
        "virtual_library.series_library_id": "2",
      }),
    );
    useAdminLibrariesMock.mockReturnValue({
      data: mockLibraries,
      isLoading: false,
    });
  });

  it("renders the heading, release desk link, and all setting groups", () => {
    renderPage();

    expect(screen.getByRole("heading", { name: "Streaming" })).toBeInTheDocument();
    expect(screen.getByText("Virtual Release Desk")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Open Release Desk/i })).toHaveAttribute(
      "href",
      "/admin/virtual-library",
    );
    expect(screen.getByRole("group", { name: "Stremio provider" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Virtual libraries" })).toBeInTheDocument();
    expect(
      screen.getByRole("group", { name: "Automation & Indexers (Prowlarr & AltMount)" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Quality" })).toBeInTheDocument();
  });

  it("renders library dropdown selects with existing library names", () => {
    renderPage();

    expect(screen.getAllByText("My Movies (ID: 1)")[0]).toBeInTheDocument();
    expect(screen.getAllByText("My Series (ID: 2)")[0]).toBeInTheDocument();
  });

  it("triggers virtual library auto-creation when button clicked", async () => {
    const form = makeForm({
      "virtual_library.enabled": "true",
    });
    useSettingsFormMock.mockReturnValue(form);
    useAdminLibrariesMock.mockReturnValue({
      data: [],
      isLoading: false,
    });

    mutateAsyncMock
      .mockResolvedValueOnce({ id: 10, name: "Virtual Movies", type: "movies" })
      .mockResolvedValueOnce({ id: 11, name: "Virtual Series", type: "series" });

    renderPage();

    const createBtn = screen.getByRole("button", { name: /Create Virtual Libraries/i });
    expect(createBtn).toBeInTheDocument();
    fireEvent.click(createBtn);

    await waitFor(() => {
      expect(mutateAsyncMock).toHaveBeenCalledWith({
        name: "Virtual Movies",
        type: "movies",
        paths: ["virtual://movies"],
      });
      expect(mutateAsyncMock).toHaveBeenCalledWith({
        name: "Virtual Series",
        type: "series",
        paths: ["virtual://series"],
      });
    });

    expect(form.setValue).toHaveBeenCalledWith("virtual_library.movie_library_id", "10");
    expect(form.setValue("virtual_library.series_library_id", "11"));
  });

  it("exposes quality and custom format preset selects", () => {
    renderPage();

    expect(screen.getByLabelText("Quality preset")).toBeInTheDocument();
    expect(screen.getByLabelText("Custom format preset")).toBeInTheDocument();
  });

  it("exposes Prowlarr and AltMount inputs in the automation group", () => {
    renderPage();

    expect(screen.getByLabelText("Prowlarr URL")).toBeInTheDocument();
    expect(screen.getByLabelText("Prowlarr API key")).toBeInTheDocument();
    expect(screen.getByLabelText("Prowlarr check interval (minutes)")).toBeInTheDocument();
    expect(screen.getByLabelText("AltMount URL")).toBeInTheDocument();
    expect(screen.getByLabelText("AltMount API key")).toBeInTheDocument();
    expect(screen.getByLabelText("AltMount check interval (minutes)")).toBeInTheDocument();
  });

  it("shows API key fields as not configured without a clear action", () => {
    renderPage();

    expect(screen.getByLabelText("Prowlarr API key")).toHaveAttribute(
      "placeholder",
      "Not configured",
    );
    expect(screen.getByLabelText("AltMount API key")).toHaveAttribute(
      "placeholder",
      "Not configured",
    );
    expect(screen.queryByRole("button", { name: "Clear saved value" })).not.toBeInTheDocument();
  });

  it("stages clearing a saved API key through the clear action", () => {
    const form = makeForm({}, ["virtual_library.indexer_api_key"]);
    useSettingsFormMock.mockReturnValue(form);
    renderPage();

    expect(screen.getByLabelText("Prowlarr API key")).toHaveAttribute(
      "placeholder",
      "••••••••••••",
    );
    fireEvent.click(screen.getByRole("button", { name: "Clear saved value" }));
    expect(form.setValue).toHaveBeenCalledWith("virtual_library.indexer_api_key", "");
  });

  it("stages a typed API key replacement", () => {
    const form = makeForm({}, ["virtual_library.indexer_api_key"]);
    useSettingsFormMock.mockReturnValue(form);
    renderPage();

    fireEvent.change(screen.getByLabelText("Prowlarr API key"), {
      target: { value: "new-prowlarr-key" },
    });
    expect(form.setValue).toHaveBeenCalledWith(
      "virtual_library.indexer_api_key",
      "new-prowlarr-key",
    );
  });
});
