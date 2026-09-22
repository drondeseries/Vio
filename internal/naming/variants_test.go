package naming

import "testing"

func TestParseVariantHints_PlexFolderEditionTag(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/Movie (1982) {edition-Final Cut}/Movie (1982) - 2160p.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.EditionKey, "final_cut"; got != want {
		t.Fatalf("EditionKey = %q, want %q", got, want)
	}
	if got, want := hints.EditionSource, "plex_tag_folder"; got != want {
		t.Fatalf("EditionSource = %q, want %q", got, want)
	}
}

func TestParseVariantHints_PlexFilenameEditionTag(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/Movie (1982) - 2160p {edition-Director's Cut}.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.EditionKey, "directors_cut"; got != want {
		t.Fatalf("EditionKey = %q, want %q", got, want)
	}
	if got, want := hints.EditionSource, "plex_tag_file"; got != want {
		t.Fatalf("EditionSource = %q, want %q", got, want)
	}
}

func TestParseVariantHints_BracketEditionTag(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/Movie [Director's Cut]/Movie 2023.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.EditionKey, "director_cut"; got != want {
		t.Fatalf("EditionKey = %q, want %q", got, want)
	}
}

func TestParseVariantHints_MultiEpisodeRangeDottedSeparator(t *testing.T) {
	for _, tt := range []struct {
		name  string
		path  string
		start int
		end   int
	}{
		{"two digits", "/tv/Show Name/Season 01/Show.Name.s01.e01-e02.mkv", 1, 2},
		{"four digits", "/tv/Show Name/Season 23/Show.Name.s23.e1162-e1163.mkv", 1162, 1163},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hints := ParseVariantHints(tt.path, "series")
			if hints == nil {
				t.Fatal("expected hints")
			}
			if got, want := hints.PresentationKind, "multi_episode"; got != want {
				t.Fatalf("PresentationKind = %q, want %q", got, want)
			}
			if got, want := hints.MultiEpisodeStart, tt.start; got != want {
				t.Fatalf("MultiEpisodeStart = %d, want %d", got, want)
			}
			if got, want := hints.MultiEpisodeEnd, tt.end; got != want {
				t.Fatalf("MultiEpisodeEnd = %d, want %d", got, want)
			}
		})
	}
}

func TestParseVariantHints_ReleaseNameAndGroup(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/Mission.Impossible.2023.2160p.Multi-AltMount.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.ReleaseName, "Mission.Impossible.2023.2160p.Multi-AltMount"; got != want {
		t.Fatalf("ReleaseName = %q, want %q", got, want)
	}
	if got, want := hints.ReleaseGroup, "AltMount"; got != want {
		t.Fatalf("ReleaseGroup = %q, want %q", got, want)
	}
}

func TestParseVariantHints_DoesNotParseChristmasEditionTitleAsEdition(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/The Christmas Edition (1941)/The Christmas Edition (1941) 720p HDTV x264.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if hints.EditionKey != "" {
		t.Fatalf("EditionKey = %q, want empty", hints.EditionKey)
	}
}

func TestParseVariantHints_NoReleaseGroupWithoutTrailingGroupTag(t *testing.T) {
	hints := ParseVariantHints(
		"/movies/Movie.2023.1080p.mkv",
		"movies",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.ReleaseName, "Movie.2023.1080p"; got != want {
		t.Fatalf("ReleaseName = %q, want %q", got, want)
	}
	if got, want := hints.ReleaseGroup, ""; got != want {
		t.Fatalf("ReleaseGroup = %q, want %q", got, want)
	}
}

func TestParseVariantHints_MultiEpisodeCarriesReleaseFields(t *testing.T) {
	hints := ParseVariantHints(
		"/tv/Show/Season 1/Show.S01E01-E02.1080p.WEB-DL.x264-GROUP.mkv",
		"series",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.PresentationKind, "multi_episode"; got != want {
		t.Fatalf("PresentationKind = %q, want %q", got, want)
	}
	if got, want := hints.ReleaseName, "Show.S01E01-E02.1080p.WEB-DL.x264-GROUP"; got != want {
		t.Fatalf("ReleaseName = %q, want %q", got, want)
	}
	if got, want := hints.ReleaseGroup, "GROUP"; got != want {
		t.Fatalf("ReleaseGroup = %q, want %q", got, want)
	}
}

func TestParseVariantHints_FourDigitMultiEpisodeRange(t *testing.T) {
	hints := ParseVariantHints(
		"/tv/One Piece/Season 23/One Piece S23E1162-E1163 - Wano Country.mkv",
		"series",
	)
	if hints == nil {
		t.Fatal("expected hints")
	}
	if got, want := hints.PresentationKind, "multi_episode"; got != want {
		t.Fatalf("PresentationKind = %q, want %q", got, want)
	}
	if got, want := hints.MultiEpisodeStart, 1162; got != want {
		t.Fatalf("EpisodeStart = %d, want %d", got, want)
	}
	if got, want := hints.MultiEpisodeEnd, 1163; got != want {
		t.Fatalf("EpisodeEnd = %d, want %d", got, want)
	}
}
