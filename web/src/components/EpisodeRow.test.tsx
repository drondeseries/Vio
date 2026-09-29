import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import EpisodeRow from "./EpisodeRow";

vi.mock("@/components/overlays/CardOverlays", () => ({
  default: () => null,
}));

vi.mock("@/hooks/useOverlayPrefs", () => ({
  useOverlayPrefs: () => ({ prefs: null }),
}));

// Capability gating: renderToStaticMarkup runs without a QueryClient, so
// the capability stands in for the resolved answer per case.
const capabilityMock = vi.fn(() => ({ data: { available: true } }));
vi.mock("@/hooks/queries/episodeRelease", async (importOriginal) => {
  const original = (await importOriginal()) as typeof import("@/hooks/queries/episodeRelease");
  return {
    ...original,
    useEpisodeReleaseCapability: () => capabilityMock(),
  };
});

describe("EpisodeRow", () => {
  it("renders progress from inline episode user_data", () => {
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-001",
            season_number: 1,
            episode_number: 1,
            title: "Pilot",
            overview: "",
            air_date: "2024-01-01",
            runtime: 42,
            still_url: "",
            still_thumbhash: "",
            user_data: {
              played: false,
              is_in_progress: true,
              position_seconds: 600,
              duration_seconds: 2400,
            },
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).toContain("width:25%");
  });

  it("renders a watched indicator from inline episode user_data", () => {
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-002",
            season_number: 1,
            episode_number: 2,
            title: "Cat's in the Bag...",
            overview: "",
            air_date: null,
            runtime: 48,
            still_url: "",
            still_thumbhash: "",
            user_data: {
              played: true,
            },
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).toContain("text-success");
  });

  it("badges a server-classified upcoming episode with its air date", () => {
    capabilityMock.mockReturnValue({ data: { available: true } });
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-future",
            season_number: 2,
            episode_number: 3,
            title: "Rabbits Don't Swim",
            overview: "",
            air_date: "2099-10-03",
            release_state: "upcoming",
            runtime: 60,
            still_url: "https://stills.example/e3.jpg",
            still_thumbhash: "",
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).toContain("Upcoming");
    expect(markup).toContain("opacity-45");
    expect(markup).toContain("saturate-50");
  });

  it("renders a released episode without the upcoming treatment", () => {
    capabilityMock.mockReturnValue({ data: { available: true } });
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-aired",
            season_number: 2,
            episode_number: 1,
            title: "Remembrance Day",
            overview: "",
            air_date: "2022-02-18",
            release_state: "released",
            runtime: 60,
            still_url: "https://stills.example/e1.jpg",
            still_thumbhash: "",
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).not.toContain("Upcoming");
    expect(markup).not.toContain("opacity-45");
  });

  it("renders plainly without release_state, even for a future air date", () => {
    // Older server, no field: no date inference, today's plain rendering.
    capabilityMock.mockReturnValue({ data: { available: true } });
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-nofield",
            season_number: 2,
            episode_number: 3,
            title: "Rabbits Don't Swim",
            overview: "",
            air_date: "2099-10-03",
            runtime: 60,
            still_url: "https://stills.example/e3.jpg",
            still_thumbhash: "",
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).not.toContain("Upcoming");
    expect(markup).not.toContain("opacity-45");
  });

  it("renders plainly when the capability is unavailable", () => {
    capabilityMock.mockReturnValue({ data: { available: false } });
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-capped",
            season_number: 2,
            episode_number: 3,
            title: "Rabbits Don't Swim",
            overview: "",
            air_date: "2099-10-03",
            release_state: "upcoming",
            runtime: 60,
            still_url: "https://stills.example/e3.jpg",
            still_thumbhash: "",
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).not.toContain("Upcoming");
    expect(markup).not.toContain("opacity-45");
    capabilityMock.mockReturnValue({ data: { available: true } });
  });

  it("fails open on an unknown air date", () => {
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeRow
          episode={{
            content_id: "ep-unknown",
            season_number: 2,
            episode_number: 4,
            title: "Untitled",
            overview: "",
            air_date: null,
            runtime: 60,
            still_url: "https://stills.example/e4.jpg",
            still_thumbhash: "",
            files: [],
          }}
        />
      </MemoryRouter>,
    );

    expect(markup).not.toContain("Upcoming");
  });
});
