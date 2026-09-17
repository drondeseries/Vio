import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Library } from "@/api/types";
import { StreamingStep } from "./StreamingStep";

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver;

const useSettingsFormMock = vi.fn();
const useWizardContextMock = vi.fn();
const useAdminLibrariesMock = vi.fn();
const mutateAsyncMock = vi.fn();

vi.mock("@/hooks/useSettingsForm", () => ({
  useSettingsForm: (...args: unknown[]) => useSettingsFormMock(...args),
}));

vi.mock("../WizardContext", () => ({
  useWizardContext: (...args: unknown[]) => useWizardContextMock(...args),
}));

vi.mock("@/hooks/queries/admin/libraries", () => ({
  useAdminLibraries: () => useAdminLibrariesMock(),
  useCreateLibrary: () => ({
    mutateAsync: mutateAsyncMock,
    isPending: false,
  }),
}));

vi.mock("@/components/admin/ConnectionCheckAction", () => ({
  ConnectionCheckAction: ({ label = "Check Connection", onClick, disabled }: any) => (
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

const defaultValues: Record<string, string> = {
  "virtual_library.enabled": "true",
  "virtual_library.manifest_url": "https://example.com/manifest.json",
  "virtual_library.movie_library_id": "1",
  "virtual_library.series_library_id": "2",
  "virtual_library.allow_insecure_http": "false",
  "virtual_library.cache_ttl_minutes": "10",
  "virtual_library.tmdb_api_key": "",
};

function mockStep(values: Record<string, string> = {}) {
  const formValues = { ...defaultValues, ...values };
  useSettingsFormMock.mockReturnValue({
    isPending: false,
    getValue: (key: string) => formValues[key] ?? "",
    setValue: vi.fn((key: string, val: string) => {
      formValues[key] = val;
    }),
    isDirty: () => false,
    dirtyCount: 0,
    save: vi.fn(),
  });
  useWizardContextMock.mockReturnValue({
    state: { currentStep: "streaming", completedSteps: [] },
    advance: vi.fn(),
    skip: vi.fn(),
    setSummary: vi.fn(),
  });
}

const mockLibraries = [
  {
    id: 1,
    name: "Movies",
    type: "movies",
    paths: ["virtual://movies"],
    enabled: true,
  },
  {
    id: 2,
    name: "TV Shows",
    type: "series",
    paths: ["virtual://series"],
    enabled: true,
  },
] as unknown as Library[];

describe("StreamingStep", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockStep();
    useAdminLibrariesMock.mockReturnValue({
      data: mockLibraries,
      isLoading: false,
    });
  });

  it("renders streaming fields and library dropdowns when enabled", () => {
    render(<StreamingStep />);

    expect(screen.getByText("Streaming")).toBeInTheDocument();
    expect(screen.getByLabelText("Enable streaming")).toBeInTheDocument();
    expect(screen.getByLabelText("Manifest URL")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Check Connection/i })).toBeInTheDocument();
    expect(screen.getAllByText("Movies (ID: 1)")[0]).toBeInTheDocument();
    expect(screen.getAllByText("TV Shows (ID: 2)")[0]).toBeInTheDocument();
  });

  it("triggers 1-click virtual library creation when libraries are absent", async () => {
    useAdminLibrariesMock.mockReturnValue({
      data: [],
      isLoading: false,
    });
    mutateAsyncMock
      .mockResolvedValueOnce({ id: 21, name: "Virtual Movies", type: "movies" })
      .mockResolvedValueOnce({ id: 22, name: "Virtual Series", type: "series" });

    render(<StreamingStep />);

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
  });
});
