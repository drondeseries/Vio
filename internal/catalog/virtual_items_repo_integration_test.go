package catalog

import (
	"context"
	"testing"
)

// TestListVirtualItemSummariesAggregatesVirtualCandidates exercises the
// admin Release Desk read against a real database. It covers a movie with two
// virtual candidates (one failed, one delivered), a series whose episode
// candidate rolls up onto the series, and the exclusion of physical files.
func TestListVirtualItemSummariesAggregatesVirtualCandidates(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES
			(3907,'Release Movies','movies',true),
			(3908,'Release Series','series',true);
		INSERT INTO media_items(content_id,type,title,sort_title,status) VALUES
			('movie-tmdb-3907','movie','Movie 3907','Movie 3907','matched'),
			('series-tvdb-3908','series','Series 3908','Series 3908','matched'),
			('movie-tmdb-3909','movie','Movie 3909','Movie 3909','matched');
		INSERT INTO episodes(content_id,series_id,season_number,episode_number,title)
			VALUES('episode-tvdb-3908-1-1','series-tvdb-3908',1,1,'Pilot');
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,release_name,failed_at,last_delivered_at,virtual_owner_installation_id)
			VALUES
			('movie-tmdb-3907',3907,'virtual://movie/tmdb/3907?variant=hd',0,'virtual','virtual','movie.R1',NULL,NOW()-INTERVAL '2 hours',11),
			('movie-tmdb-3907',3907,'virtual://movie/tmdb/3907?variant=sd',0,'virtual','virtual','movie.R2',NOW()-INTERVAL '1 hour',NULL,11);
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,release_name,episode_id,virtual_owner_installation_id)
			VALUES('series-tvdb-3908',3908,'virtual://series/tvdb/3908/1/1',0,'virtual','virtual','show.R1','episode-tvdb-3908-1-1',11);
		-- A physical file on the movie must not count as a virtual candidate.
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
			VALUES('movie-tmdb-3907',3907,'/movies/3907.mkv',1234,'mkv');
		-- A physical-only item must not appear at all.
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
			VALUES('movie-tmdb-3909',3907,'/movies/3909.mkv',1234,'mkv');
	`); err != nil {
		t.Fatal(err)
	}

	summaries, err := NewItemRepository(pool).ListVirtualItemSummaries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2: %+v", len(summaries), summaries)
	}
	// The delivered movie sorts before the series, whose candidates have no
	// delivery timestamp.
	movie, series := summaries[0], summaries[1]
	if movie.ContentID != "movie-tmdb-3907" || series.ContentID != "series-tvdb-3908" {
		t.Fatalf("ordering = %q then %q", movie.ContentID, series.ContentID)
	}
	if movie.CandidateCount != 2 || movie.FailedCount != 1 || movie.LastDeliveredAt == nil {
		t.Fatalf("movie summary = %+v", movie)
	}
	if movie.LibraryID != 3907 || movie.LibraryName != "Release Movies" || movie.InstallationID != 11 {
		t.Fatalf("movie identity = %+v", movie)
	}
	if len(movie.ReleaseNames) != 2 || movie.ReleaseNames[0] != "movie.R1" || movie.ReleaseNames[1] != "movie.R2" {
		t.Fatalf("movie release names = %v", movie.ReleaseNames)
	}
	if series.CandidateCount != 1 || series.FailedCount != 0 || series.LastDeliveredAt != nil {
		t.Fatalf("series summary = %+v", series)
	}
	if series.Title != "Series 3908" || series.ItemType != "series" || series.LibraryID != 3908 {
		t.Fatalf("series identity = %+v", series)
	}
	for _, summary := range summaries {
		if summary.ContentID == "movie-tmdb-3909" {
			t.Fatalf("physical-only item leaked into summaries: %+v", summary)
		}
	}
}
