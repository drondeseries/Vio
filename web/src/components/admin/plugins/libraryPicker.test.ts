import { describe, expect, it } from "vitest";

import type { Library } from "@/api/types";

import {
  libraryKind,
  libraryMatchesPicker,
  libraryPickerForFormat,
  libraryPickerOptions,
} from "./libraryPicker";

function library(overrides: Partial<Library> & Pick<Library, "id" | "name" | "type">): Library {
  return {
    paths: [],
    enabled: true,
    metadata_language: "",
    auto_translate_metadata: false,
    chapter_thumbnails_enabled: false,
    chapter_thumbnails_supported: false,
    intro_detection_enabled: false,
    trailer_kinds: [],
    sort_order: 0,
    last_scanned_at: null,
    ...overrides,
  };
}

const LIBRARIES: Library[] = [
  library({ id: 1, name: "Movies", type: "movie" }),
  library({ id: 2, name: "Movies alt", type: "movies" }),
  library({ id: 3, name: "Shows", type: "series" }),
  library({ id: 4, name: "TV", type: "tvshows" }),
  library({ id: 5, name: "Mixed", type: "mixed" }),
  library({ id: 6, name: "Disabled movies", type: "movie", enabled: false }),
  library({ id: 7, name: "Music", type: "music" }),
];

describe("libraryPickerForFormat", () => {
  it("maps the three host-known formats to a picker kind", () => {
    expect(libraryPickerForFormat("silo-library")).toBe("any");
    expect(libraryPickerForFormat("silo-library-movie")).toBe("movie");
    expect(libraryPickerForFormat("silo-library-tv")).toBe("tv");
  });

  it("returns null for unknown, missing, and unrelated formats", () => {
    expect(libraryPickerForFormat("uri")).toBeNull();
    expect(libraryPickerForFormat("password")).toBeNull();
    expect(libraryPickerForFormat("silo-library-other")).toBeNull();
    expect(libraryPickerForFormat(undefined)).toBeNull();
  });
});

describe("libraryKind", () => {
  it("normalises the movie, series, and mixed spellings hosts use", () => {
    expect(libraryKind("Movie")).toBe("movie");
    expect(libraryKind("movies")).toBe("movie");
    expect(libraryKind("series")).toBe("tv");
    expect(libraryKind("show")).toBe("tv");
    expect(libraryKind("TvShows")).toBe("tv");
    expect(libraryKind("mixed")).toBe("mixed");
    expect(libraryKind("music")).toBeNull();
  });
});

describe("libraryPickerOptions", () => {
  it("offers every enabled library for an any picker, labelled Name (ID)", () => {
    expect(libraryPickerOptions("any", LIBRARIES)).toEqual([
      { value: "1", label: "Movies (1)" },
      { value: "2", label: "Movies alt (2)" },
      { value: "3", label: "Shows (3)" },
      { value: "4", label: "TV (4)" },
      { value: "5", label: "Mixed (5)" },
      { value: "7", label: "Music (7)" },
    ]);
  });

  it("offers only movie-capable libraries for a movie picker", () => {
    const options = libraryPickerOptions("movie", LIBRARIES);
    expect(options.map((option) => option.value)).toEqual(["1", "2", "5"]);
    expect(libraryMatchesPicker("movie", library({ id: 4, name: "TV", type: "tvshows" }))).toBe(
      false,
    );
    expect(
      libraryMatchesPicker(
        "movie",
        library({ id: 6, name: "Disabled movies", type: "movie", enabled: false }),
      ),
    ).toBe(false);
  });

  it("offers only TV-capable libraries for a tv picker", () => {
    expect(libraryPickerOptions("tv", LIBRARIES).map((option) => option.value)).toEqual([
      "3",
      "4",
      "5",
    ]);
  });
});
