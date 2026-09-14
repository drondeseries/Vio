package mdblist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// ErrRateLimit reports an HTTP 429 from MDBList.
var ErrRateLimit = errors.New("mdblist rate limit exceeded")

// ErrEmptyItemsWithTotal reports an items response whose movie/show buckets are
// empty. MDBList returns this for many lists (notably via its slug-based items
// endpoint), so an empty response must never be accepted as "the list is now
// empty": that would wipe collection membership. The catalog falls back to the
// public /json feed, and only a genuinely empty /json result may empty a
// collection.
var ErrEmptyItemsWithTotal = errors.New("mdblist items response was empty")

// ErrIncompleteItems reports that pagination ended with fewer items than the
// announced total, beyond what the requested cap explains. A truncated or
// dropped bucket must not silently replace full collection membership; the
// catalog falls back to the public /json feed.
var ErrIncompleteItems = errors.New("mdblist items response was incomplete")

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

// listMeta is one entry from GET /lists/{user}/{slug}, a JSON array. Only the
// fields needed to match the requested list and recover its numeric id.
type listMeta struct {
	ID       int    `json:"id"`
	UserName string `json:"user_name"`
	Slug     string `json:"slug"`
}

// listItem is a single API item. tvdb_id and release_date differ from the
// public /json feed's tvdbid and (absent) release date field. Item ID equals
// the item's ids.tmdb and the public feed's "id": it is the TMDB id.
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

// ResolveListID maps a user/slug pair to MDBList's numeric list id via
// GET /lists/{user}/{slug}. That endpoint returns an array of list objects;
// the entry whose user_name and slug both match (case-insensitive) wins. A
// single-entry response is accepted only when at least one of user_name/slug
// also matches, so a wholly unrelated list is never imported; an ambiguous
// multi-entry response errors rather than guessing.
func (c *Client) ResolveListID(ctx context.Context, user, slug string) (int, error) {
	user = strings.TrimSpace(user)
	slug = strings.TrimSpace(slug)
	if user == "" || slug == "" {
		return 0, fmt.Errorf("mdblist list user and slug are required")
	}
	if !c.Configured() {
		return 0, ErrNotConfigured
	}

	q := url.Values{}
	q.Set("apikey", c.currentAPIKey())
	u := c.baseURL + "/lists/" + url.PathEscape(user) + "/" + url.PathEscape(slug) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("creating mdblist request: %w", err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("calling mdblist: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if err := checkResponse(res); err != nil {
		return 0, err
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return 0, fmt.Errorf("reading mdblist response: %w", err)
	}
	var metas []listMeta
	if err := json.Unmarshal(body, &metas); err != nil {
		return 0, fmt.Errorf("parsing mdblist response: %w", err)
	}
	for _, meta := range metas {
		if strings.EqualFold(meta.UserName, user) && strings.EqualFold(meta.Slug, slug) && meta.ID > 0 {
			return meta.ID, nil
		}
	}
	if len(metas) == 1 && metas[0].ID > 0 {
		// A single result is only the requested list if at least one component
		// matches; a wholly unrelated single entry is a resolution failure, not
		// permission to import a different list.
		meta := metas[0]
		if strings.EqualFold(meta.UserName, user) || strings.EqualFold(meta.Slug, slug) {
			return meta.ID, nil
		}
	}
	return 0, fmt.Errorf("mdblist could not resolve list %s/%s to a unique id (%d matches)", user, slug, len(metas))
}

// ListItems fetches a user's list through the authenticated, cursor-paginated
// items endpoint. It first resolves the numeric list id (MDBList's slug-based
// items endpoint silently returns zero items for many lists), then pages
// /lists/{id}/items. maxItems > 0 bounds the result (and stops pagination
// early); otherwise the hard MaxExplicitItemLimit cap applies. movies and
// shows are separate buckets, so this merges them by ascending rank (stable)
// to reproduce the public /json single-array rank order. seasons/episodes are
// ignored.
//
// It refuses to return an empty or short result that would silently shrink a
// collection: ErrEmptyItemsWithTotal for empty buckets, ErrIncompleteItems
// when pagination ends below min(total, cap). Callers fall back to /json.
func (c *Client) ListItems(ctx context.Context, user, list string, maxItems int) ([]ListItem, error) {
	user = strings.TrimSpace(user)
	list = strings.TrimSpace(list)
	if user == "" || list == "" {
		return nil, fmt.Errorf("mdblist list user and slug are required")
	}
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	listID, err := c.ResolveListID(ctx, user, list)
	if err != nil {
		return nil, err
	}

	hardCap := maxItems
	if hardCap <= 0 {
		hardCap = collectionutil.MaxExplicitItemLimit
	}

	var movies, shows []listItem
	total := 0
	seenCursors := map[string]struct{}{}
	cursor := ""
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("apikey", c.currentAPIKey())
		q.Set("limit", strconv.Itoa(listItemsPageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := c.fetchListItemsPage(ctx, listID, q)
		if err != nil {
			return nil, err
		}
		movies = append(movies, resp.Movies...)
		shows = append(shows, resp.Shows...)
		if resp.Pagination.Total > 0 {
			total = resp.Pagination.Total
		}

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
	if len(items) == 0 {
		// An empty 200 must never be accepted as "the list is now empty": the
		// catalog would delete all membership. It falls back to /json, and only
		// a genuinely empty /json result may empty the collection.
		return nil, fmt.Errorf("%w: list %s/%s (id %d) total=%d", ErrEmptyItemsWithTotal, user, list, listID, total)
	}
	// A short page (has_more=false but fewer items than announced) or a dropped
	// bucket would silently shrink membership. The intentional maxItems cap is
	// not a shortfall: min(total, hardCap) is exactly what the cap allows.
	if total > 0 && len(items) < min(total, hardCap) {
		slog.WarnContext(ctx, "mdblist items response incomplete",
			"list_id", listID,
			"user", user,
			"list", list,
			"delivered", len(items),
			"total", total,
			"cap", hardCap,
		)
		return nil, fmt.Errorf("%w: list %s/%s (id %d) delivered %d of total %d", ErrIncompleteItems, user, list, listID, len(items), total)
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

func (c *Client) fetchListItemsPage(ctx context.Context, listID int, q url.Values) (*listItemsResponse, error) {
	u := c.baseURL + "/lists/" + strconv.Itoa(listID) + "/items?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating mdblist request: %w", err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling mdblist: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if err := checkResponse(res); err != nil {
		return nil, err
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

// checkResponse maps non-2xx statuses onto the package's typed errors. It only
// reads a bounded snippet of the body.
func checkResponse(res *http.Response) error {
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (status %d): %s", ErrUnauthorized, res.StatusCode, readSnippet(res.Body))
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w (status %d): %s", ErrListNotFound, res.StatusCode, readSnippet(res.Body))
	case res.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w (status %d): %s", ErrRateLimit, res.StatusCode, readSnippet(res.Body))
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return fmt.Errorf("mdblist request failed with status %d: %s", res.StatusCode, readSnippet(res.Body))
	}
	return nil
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
