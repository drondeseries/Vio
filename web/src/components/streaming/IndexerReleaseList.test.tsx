// @vitest-environment jsdom
import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { WatchIndexerRelease } from "@/api/types";
import type {
  IndexerReleaseRequests,
  IndexerReleaseRequestStatus,
} from "@/hooks/useIndexerReleases";
import { IndexerReleaseList } from "./IndexerReleaseList";

function makeRelease(overrides: Partial<WatchIndexerRelease> = {}): WatchIndexerRelease {
  return {
    release_id: overrides.release_id ?? "rel-1",
    title: overrides.title ?? "Movie.2026.2160p.WEB-DL-GRP",
    resolution: overrides.resolution ?? "2160p",
    ...overrides,
    download_state: overrides.download_state ?? "not_downloaded",
  };
}

function makeRequests(
  overrides: {
    statusFor?: (release: WatchIndexerRelease) => IndexerReleaseRequestStatus;
    errorFor?: (release: WatchIndexerRelease) => string | null;
    request?: IndexerReleaseRequests["request"];
  } = {},
): IndexerReleaseRequests {
  return {
    statusFor: overrides.statusFor ?? (() => "idle"),
    errorFor: overrides.errorFor ?? (() => null),
    request: overrides.request ?? vi.fn(),
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("IndexerReleaseList", () => {
  it("renders a row with the Not downloaded badge and a Request action", () => {
    render(
      <IndexerReleaseList
        releases={[makeRelease({ resolution: "2160p", codec_video: "hevc" })]}
        requests={makeRequests()}
      />,
    );

    const row = screen.getByRole("button", { name: /Request Movie/ });
    expect(row).toHaveAttribute("data-indexer-release", "rel-1");
    expect(row).toHaveAttribute("data-status", "idle");
    expect(within(row).getByText("Not downloaded")).toBeInTheDocument();
    expect(within(row).getByText("Request")).toBeInTheDocument();
    // No play affordance: the row is a request trigger, not a play target.
    expect(row.querySelector(".lucide-play")).toBeNull();
  });

  it("renders a queued row as Requested and disables it", () => {
    render(
      <IndexerReleaseList
        releases={[makeRelease()]}
        requests={makeRequests({ statusFor: () => "queued" })}
      />,
    );

    const row = screen.getByRole("button", { name: /Requested Movie/ });
    expect(row).toBeDisabled();
    expect(row).toHaveAttribute("data-status", "queued");
    expect(within(row).getByText("Requested")).toBeInTheDocument();
    // The visible row no longer reads as "Not downloaded".
    expect(within(row).queryByText("Not downloaded")).not.toBeInTheDocument();
  });

  it("shows the retryable failure copy and stays clickable", () => {
    render(
      <IndexerReleaseList
        releases={[makeRelease()]}
        requests={makeRequests({
          statusFor: () => "failed",
          errorFor: () => "Couldn't request. Try again.",
        })}
      />,
    );

    const row = screen.getByRole("button", { name: /Request Movie/ });
    expect(row).not.toBeDisabled();
    expect(row).toHaveAttribute("data-status", "failed");
    expect(within(row).getByText("Couldn't request. Try again.")).toBeInTheDocument();
    expect(within(row).getByText("Retry")).toBeInTheDocument();
  });

  it("fires the request for the clicked row's release id", () => {
    const request = vi.fn();
    render(
      <IndexerReleaseList
        releases={[makeRelease({ release_id: "rel-1" }), makeRelease({ release_id: "rel-2" })]}
        requests={makeRequests({ request })}
      />,
    );

    const rows = screen.getAllByRole("button", { name: /Request Movie/ });
    expect(rows).toHaveLength(2);

    fireEvent.click(rows[1]!);
    // The second row's identity is carried into the request, not the first's.
    expect(request).toHaveBeenCalledTimes(1);
    expect(request.mock.calls[0]?.[0]).toMatchObject({ release_id: "rel-2" });
  });

  it("renders nothing for an empty release list", () => {
    const { container } = render(<IndexerReleaseList releases={[]} requests={makeRequests()} />);

    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByText("Not downloaded")).not.toBeInTheDocument();
  });

  it("exposes each row only as a request row, never a play target", () => {
    render(<IndexerReleaseList releases={[makeRelease()]} requests={makeRequests()} />);

    const rows = document.querySelectorAll("[data-indexer-release]");
    expect(rows).toHaveLength(1);
    // The row's accessible name is a Request action, not a play/switch action.
    expect(screen.getByRole("button", { name: /Request Movie/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Play/i })).not.toBeInTheDocument();
  });
});
