package naming

// FilenameHints contains information parsed from file/folder names.
type FilenameHints struct {
	Title      string
	Year       int
	Type       string // movie, series
	SeasonNum  int
	EpisodeNum int
	AirDate    string // ISO date for daily/by-date episodes, e.g. 2026-04-24
}

// PathContext captures the classification and parsed hints for a media path
// after considering both naming heuristics and the declared library type.
type PathContext struct {
	Type                   string // movie, series
	RootPath               string
	Title                  string
	Year                   int
	SeasonNum              int
	EpisodeNum             int
	AirDate                string // ISO date for daily/by-date episodes, e.g. 2026-04-24
	HasEpisodePattern      bool
	HasAirDatePattern      bool
	HasSeasonStructure     bool
	HasMovieFolderEvidence bool
}

// FolderIDHints contains external IDs parsed from folder names.
type FolderIDHints struct {
	TmdbID string
	ImdbID string
	TvdbID string
}

// CanonicalRoot describes the deduplicated content root for a media file.
// For series content this is the show folder; for movies it is the containing
// movie folder when one can be inferred, or a synthetic root derived from the
// filename stem for true loose files.
type CanonicalRoot struct {
	RootPath string // Absolute path to the canonical root directory (or synthetic path for true loose files).
	Type     string // "series" or "movie".
}

// VariantHints captures edition and presentation metadata parsed from a path.
type VariantHints struct {
	EditionRaw            string
	EditionKey            string
	EditionSource         string
	EditionConfidence     *float64
	PresentationKind      string
	PresentationGroupKey  string
	PresentationPartIndex int
	MultiEpisodeStart     int
	MultiEpisodeEnd       int
	// ReleaseName is the file's stem (basename without extension), the closest
	// the server gets to the release's advertised name.
	ReleaseName string
	// ReleaseGroup is the trailing group tag on release-style names
	// ("Movie.2023.2160p.AltMount" → "AltMount"), "" when none is present.
	ReleaseGroup string
}
