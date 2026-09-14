package mdblist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/collectionutil"
)

// ErrUnauthorized reports a missing, invalid, or rejected apikey (HTTP 401 or
// 403). Callers surface it so an expired key is visible instead of silently
// degrading to the unauthenticated /json feed.
var ErrUnauthorized = errors.New("mdblist rejected apikey")

// ErrListNotFound reports that the requested user/list does not exist (404).
var ErrListNotFound = errors.New("mdblist list not found")

// ListItem is one movie or show in an authenticated list-items page. The
// authenticated API uses different field names than the public /json feed
// (tvdb_id / release_date), so the catalog maps these separately.
type ListItem struct {
	TMDBID      int
	TVDBID      *int
	IMDbID      string
	MediaType   string
	Title       string
	ReleaseYear int
	ReleaseDate string
	Rank        int
}

// listItemsPageSize is the maximum limit MDBList accepts for the items
// endpoint.
const listItemsPageSize = 1000

// listPagination mirrors the pagination object on a list-items response.
type listPagination struct {
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	Total      int    `json:"total"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// listItemsResponse is the movies+shows page shape. seasons and episodes are
// intentionally absent: Silo models only movie/series, and the JSON decoder
// ignores unknown fields.
type listItemsResponse struct {
	Movies     []listItem     `json:"movies"`
	Shows      []listItem     `json:"shows"`
	Pagination listPagination `json:"pagination"`
}

// listItem is a single API item. tvdb_id and release_date differ from the
// public /json feed's tvdbid and (absent) release date field.
type listItem struct {
	ID          int    `json:"id"`
	MediaType   string `json:"mediatype"`
	IMDbID      string `json:"imdb_id"`
	TVDBID      *int   `json:"tvdb_id"`
	Title       string `json:"title"`
	ReleaseYear int    `json:"release_year"`
	ReleaseDate string `json:"release_date"`
	Rank        int    `json:"rank"`
}

// ListItems fetches a user's list through the authenticated, cursor-paginated
// items endpoint. maxItems > 0 bounds the result (and stops pagination early);
// otherwise the hard MaxExplicitItemLimit cap applies. movies and shows are
// separate buckets, so this merges them by ascending rank (stable) to
// reproduce the public /json single-array rank order. seasons/episodes are
// ignored.
func (c *Client) ListItems(ctx context.Context, user, list string, maxItems int) ([]ListItem, error) {
	user = strings.TrimSpace(user)
	list = strings.TrimSpace(list)
	if user == "" || list == "" {
		return nil, fmt.Errorf("mdblist list user and slug are required")
	}
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	hardCap := maxItems
	if hardCap <= 0 {
		hardCap = collectionutil.MaxExplicitItemLimit
	}

	var movies, shows []listItem
	seenCursors := map[string]struct{}{}
	cursor := ""
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("apikey", c.currentAPIKey())
		q.Set("limit", strconv.Itoa(listItemsPageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := c.fetchListItemsPage(ctx, user, list, q)
		if err != nil {
			return nil, err
		}
		movies = append(movies, resp.Movies...)
		shows = append(shows, resp.Shows...)

		if len(movies)+len(shows) >= hardCap {
			break
		}
		if !resp.Pagination.HasMore {
			break
		}
		next := resp.Pagination.NextCursor
		if next == "" || next == cursor {
			return nil, fmt.Errorf("mdblist pagination did not advance (cursor %q repeated after page %d)", next, page+1)
		}
		if _, ok := seenCursors[next]; ok {
			return nil, fmt.Errorf("mdblist pagination repeated cursor %q after page %d", next, page+1)
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}

	items := make([]ListItem, 0, len(movies)+len(shows))
	for _, it := range movies {
		items = append(items, it.toListItem())
	}
	for _, it := range shows {
		items = append(items, it.toListItem())
	}
	// movies precede shows in the source order, so a stable sort keeps that
	// order for equal/zero ranks.
	sort.SliceStable(items, func(i, j int) bool { return items[i].Rank < items[j].Rank })
	if len(items) > hardCap {
		items = items[:hardCap]
	}
	return items, nil
}

func (it listItem) toListItem() ListItem {
	return ListItem{
		TMDBID:      it.ID,
		TVDBID:      it.TVDBID,
		IMDbID:      it.IMDbID,
		MediaType:   it.MediaType,
		Title:       it.Title,
		ReleaseYear: it.ReleaseYear,
		ReleaseDate: it.ReleaseDate,
		Rank:        it.Rank,
	}
}

func (c *Client) fetchListItemsPage(ctx context.Context, user, list string, q url.Values) (*listItemsResponse, error) {
	u := c.baseURL + "/lists/" + url.PathEscape(user) + "/" + url.PathEscape(list) + "/items?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating mdblist request: %w", err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling mdblist: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w (status %d): %s", ErrUnauthorized, res.StatusCode, readSnippet(res.Body))
	}
	if res.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w (status %d): %s", ErrListNotFound, res.StatusCode, readSnippet(res.Body))
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("mdblist rate limit exceeded")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("mdblist request failed with status %d: %s", res.StatusCode, readSnippet(res.Body))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading mdblist response: %w", err)
	}
	var resp listItemsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing mdblist response: %w", err)
	}
	return &resp, nil
}

// readSnippet returns a bounded, non-secret fragment of an error body for
// diagnostics.
func readSnippet(r io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
