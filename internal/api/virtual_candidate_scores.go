package api

import (
	"context"
	"net/url"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// virtualCandidateLister is the slice of *virtuallibrary.Service the score
// adapter uses. It is an interface so the mapping is testable without a live
// resolver.
type virtualCandidateLister interface {
	ListStreams(ctx context.Context, virtualPath string) ([]virtuallibrary.PlaybackStream, error)
}

// virtualCandidateScoreSource adapts a virtual library's ranked candidate list
// to the catalog's score port, so a watch response can show each virtual
// version's custom-format score. It lives on the api side because the virtual
// library service imports catalog, so catalog cannot import it back.
//
// Scores come from the same ranking the playback picker uses: ListStreams
// re-lists (usually from the resolver's bounded candidate cache) and applies
// the active profile's custom formats. A provider miss returns no scores rather
// than an error, so the watch response still serves.
func virtualCandidateScoreSource(service virtualCandidateLister) catalog.VirtualCandidateScoreSource {
	if service == nil {
		return nil
	}
	return func(ctx context.Context, _ string, files []*models.MediaFile) map[int]int {
		neutral := virtualNeutralSourcePath(files)
		if neutral == "" {
			return nil
		}
		streams, err := service.ListStreams(ctx, neutral)
		if err != nil || len(streams) == 0 {
			return nil
		}
		scoreByURI := make(map[string]int, len(streams))
		for _, stream := range streams {
			// Zero is the unscored/neutral sum; the wire field is omitted for
			// it, so it must not be recorded as a real score.
			if stream.QualityScore != 0 {
				scoreByURI[stream.URI] = stream.QualityScore
			}
		}
		if len(scoreByURI) == 0 {
			return nil
		}
		scores := make(map[int]int, len(files))
		for _, file := range files {
			if file == nil {
				continue
			}
			if score, ok := scoreByURI[file.FilePath]; ok {
				scores[file.ID] = score
			}
		}
		if len(scores) == 0 {
			return nil
		}
		return scores
	}
}

// virtualNeutralSourcePath returns the shared virtual listing path of a
// content's candidate rows: the candidate URIs with their per-file result
// selector stripped, keeping any profile selector so the ranking uses the same
// profile the candidate list did. It returns "" when the files are not virtual.
func virtualNeutralSourcePath(files []*models.MediaFile) string {
	for _, file := range files {
		if file == nil || !strings.HasPrefix(file.FilePath, "virtual://") {
			continue
		}
		parsed, err := url.Parse(file.FilePath)
		if err != nil {
			continue
		}
		query := parsed.Query()
		query.Del("result")
		query.Del("results")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return ""
}
