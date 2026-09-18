import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { userEvent } from "@testing-library/user-event";
import { createElement, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { MetadataProvidersSection } from "./MetadataProvidersSection";

const useAdminPluginsMock = vi.fn();
const mutateAsyncMock = vi.fn();

vi.mock("@/hooks/queries/admin/plugins", () => ({
  useAdminPlugins: (...args: unknown[]) => useAdminPluginsMock(...args),
  useInstallPlugin: () => ({ mutateAsync: mutateAsyncMock }),
}));

vi.mock("@/hooks/queries/keys", () => ({
  adminKeys: {
    pluginCatalog: () => ["admin", "plugins", "catalog"],
    pluginInstallations: () => ["admin", "plugins", "installations"],
  },
}));

function catalogEntry(plugin_id: string) {
  return { repository_id: 1, plugin_id, version: "1.0.0" };
}

function installation(plugin_id: string) {
  return { id: 7, plugin_id };
}

function fixture() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children),
  };
}

describe("MetadataProvidersSection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAdminPluginsMock.mockReturnValue({ catalog: [], installations: [], isLoading: true });
  });

  it("installs missing providers from the catalog once on load", async () => {
    useAdminPluginsMock.mockReturnValue({
      catalog: [catalogEntry("silo.tmdb"), catalogEntry("silo.tvdb")],
      installations: [],
      isLoading: false,
    });
    mutateAsyncMock.mockResolvedValue({});

    render(<MetadataProvidersSection />, fixture());

    await waitFor(() => {
      expect(mutateAsyncMock).toHaveBeenCalledWith({
        repository_id: 1,
        plugin_id: "silo.tmdb",
        version: "1.0.0",
      });
      expect(mutateAsyncMock).toHaveBeenCalledWith({
        repository_id: 1,
        plugin_id: "silo.tvdb",
        version: "1.0.0",
      });
    });
    expect(mutateAsyncMock).toHaveBeenCalledTimes(2);
  });

  it("skips providers that are already installed", async () => {
    useAdminPluginsMock.mockReturnValue({
      catalog: [catalogEntry("silo.tmdb"), catalogEntry("silo.tvdb")],
      installations: [installation("silo.tmdb"), installation("silo.tvdb")],
      isLoading: false,
    });

    render(<MetadataProvidersSection />, fixture());

    await waitFor(() => {
      expect(screen.getAllByText("Installed")).toHaveLength(2);
    });
    expect(mutateAsyncMock).not.toHaveBeenCalled();
  });

  it("reports a failed install with a retry action", async () => {
    useAdminPluginsMock.mockReturnValue({
      catalog: [catalogEntry("silo.tmdb"), catalogEntry("silo.tvdb")],
      installations: [installation("silo.tvdb")],
      isLoading: false,
    });
    mutateAsyncMock.mockRejectedValueOnce(new Error("offline"));

    render(<MetadataProvidersSection />, fixture());

    await waitFor(() => {
      expect(screen.getByText("Install failed")).toBeInTheDocument();
    });
    expect(mutateAsyncMock).toHaveBeenCalledTimes(1);

    mutateAsyncMock.mockResolvedValueOnce({});
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(mutateAsyncMock).toHaveBeenCalledTimes(2);
    });
  });

  it("reports providers missing from the catalog without installing", async () => {
    useAdminPluginsMock.mockReturnValue({ catalog: [], installations: [], isLoading: false });

    render(<MetadataProvidersSection />, fixture());

    await waitFor(() => {
      expect(screen.getAllByText("Not in plugin catalog")).toHaveLength(2);
    });
    expect(mutateAsyncMock).not.toHaveBeenCalled();
  });
});
