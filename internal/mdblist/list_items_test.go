package mdblist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// metaOne is a single-list metadata response for GET /lists/{user}/{slug}.
func metaOne(user, slug string, id int) string {
	return fmt.Sprintf(`[{"id":%d,"user_name":%q,"slug":%q,"items":10}]`, id, user, slug)
}

// listItemsServer serves the two-call sequence: GET /lists/{user}/{slug}
// returns metaBody, and GET /lists/{id}/items returns pages keyed by cursor
// ("" for the first page). It records every request's query and path.
func listItemsServer(t *testing.T, metaBody string, pages map[string]string) (*httptest.Server, *[]url.Values, *[]string) {
	t.Helper()
	var queries []url.Values
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/items") {
			page, ok := pages[r.URL.Query().Get("cursor")]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(page))
			return
		}
		_, _ = w.Write([]byte(metaBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries, &paths
}

// itemPaths returns only the /items request paths.
func itemPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if strings.HasSuffix(p, "/items") {
			out = append(out, p)
		}
	}
	return out
}

func TestListItemsResolvesIDThenPagesByID(t *testing.T) {
	pages := map[string]string{
		"": `{
			"movies":[{"id":100,"mediatype":"movie","imdb_id":"tt100","title":"Movie A","release_year":2001,"release_date":"2001-01-01","rank":1000}],
			"shows":[],
			"pagination":{"limit":1000,"offset":0,"total":2,"has_more":true,"next_cursor":"c1"}
		}`,
		"c1": `{
			"movies":[],
			"shows":[{"id":200,"mediatype":"show","imdb_id":"tt200","tvdb_id":200,"title":"Show B","release_year":2002,"release_date":"2002-02-02","rank":2000}],
			"pagination":{"limit":1000,"offset":1,"total":2,"has_more":false,"next_cursor":""}
		}`,
	}
	srv, queries, paths := listItemsServer(t, metaOne("alice", "watchlist", 2194), pages)

	c := NewClient("secret-key", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "alice", "watchlist", 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Call sequence: metadata by slug first, then items by numeric id.
	if (*paths)[0] != "/lists/alice/watchlist" {
		t.Fatalf("first path = %q, want /lists/alice/watchlist", (*paths)[0])
	}
	ips := itemPaths(*paths)
	if len(ips) != 2 {
		t.Fatalf("item requests = %d, want 2", len(ips))
	}
	for _, p := range ips {
		if p != "/lists/2194/items" {
			t.Fatalf("items path = %q, want /lists/2194/items (by numeric id)", p)
		}
	}
	// The metadata request carries the key but no limit/cursor.
	if (*queries)[0].Get("apikey") != "secret-key" {
		t.Fatalf("metadata apikey = %q, want secret-key", (*queries)[0].Get("apikey"))
	}
	if (*queries)[0].Get("limit") != "" {
		t.Fatalf("metadata limit = %q, want empty", (*queries)[0].Get("limit"))
	}
	// Item pages: limit, cursor, and key.
	for i, q := range (*queries)[1:] {
		if q.Get("apikey") != "secret-key" {
			t.Fatalf("item request %d apikey = %q, want secret-key", i, q.Get("apikey"))
		}
		if q.Get("limit") != "1000" {
			t.Fatalf("item request %d limit = %q, want 1000", i, q.Get("limit"))
		}
	}
	itemQueries := (*queries)[1:]
	if itemQueries[0].Get("cursor") != "" {
		t.Fatalf("first item request sent cursor %q, want empty", itemQueries[0].Get("cursor"))
	}
	if itemQueries[1].Get("cursor") != "c1" {
		t.Fatalf("second item request cursor = %q, want c1", itemQueries[1].Get("cursor"))
	}
	// Fields map from the API names (tvdb_id/release_date).
	if items[0].TMDBID != 100 || items[0].ReleaseDate != "2001-01-01" || items[0].Rank != 1000 {
		t.Fatalf("movie item = %+v", items[0])
	}
	if items[1].MediaType != "show" || items[1].TVDBID == nil || *items[1].TVDBID != 200 || items[1].ReleaseDate != "2002-02-02" {
		t.Fatalf("show item = %+v", items[1])
	}
}

func TestResolveListIDSelectsMatchingEntry(t *testing.T) {
	meta := `[
		{"id":1,"user_name":"someone","slug":"other"},
		{"id":3082,"user_name":"garycrawfordgc","slug":"netflix-shows"},
		{"id":3,"user_name":"garycrawfordgc","slug":"netflix-movies"}
	]`
	srv, _, paths := listItemsServer(t, meta, nil)
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	id, err := c.ResolveListID(context.Background(), "garycrawfordgc", "netflix-shows")
	if err != nil {
		t.Fatalf("ResolveListID: %v", err)
	}
	if id != 3082 {
		t.Fatalf("id = %d, want 3082", id)
	}
	if (*paths)[0] != "/lists/garycrawfordgc/netflix-shows" {
		t.Fatalf("path = %q", (*paths)[0])
	}
}

func TestResolveListIDSingleEntryPartialMatch(t *testing.T) {
	// F5: a single entry is accepted only when at least one component matches.
	t.Run("slug matches", func(t *testing.T) {
		meta := `[{"id":42,"user_name":"Alice","slug":"watchlist"}]`
		srv, _, _ := listItemsServer(t, meta, nil)
		c := NewClient("k", srv.Client())
		c.baseURL = srv.URL
		id, err := c.ResolveListID(context.Background(), "alice", "watchlist")
		if err != nil || id != 42 {
			t.Fatalf("ResolveListID = %d, %v; want 42, nil (case-insensitive match)", id, err)
		}
	})

	t.Run("wholly unrelated single entry rejected", func(t *testing.T) {
		meta := `[{"id":99,"user_name":"someone-else","slug":"different-list"}]`
		srv, _, _ := listItemsServer(t, meta, nil)
		c := NewClient("k", srv.Client())
		c.baseURL = srv.URL
		if _, err := c.ResolveListID(context.Background(), "alice", "watchlist"); err == nil {
			t.Fatal("a wholly unrelated single entry must not resolve")
		}
	})

	t.Run("only user matches accepted", func(t *testing.T) {
		meta := `[{"id":7,"user_name":"alice","slug":"renamed"}]`
		srv, _, _ := listItemsServer(t, meta, nil)
		c := NewClient("k", srv.Client())
		c.baseURL = srv.URL
		id, err := c.ResolveListID(context.Background(), "alice", "watchlist")
		if err != nil || id != 7 {
			t.Fatalf("ResolveListID = %d, %v; want 7, nil (user matches)", id, err)
		}
	})
}

func TestResolveListIDMismatchErrors(t *testing.T) {
	meta := `[
		{"id":1,"user_name":"someone","slug":"other"},
		{"id":2,"user_name":"someone","slug":"another"}
	]`
	srv, _, _ := listItemsServer(t, meta, nil)
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	if _, err := c.ResolveListID(context.Background(), "alice", "watchlist"); err == nil {
		t.Fatal("expected an ambiguity error, got nil")
	}
}

func TestResolveListIDTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"not found", http.StatusNotFound, ErrListNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			}))
			defer srv.Close()
			c := NewClient("k", srv.Client())
			c.baseURL = srv.URL
			if _, err := c.ResolveListID(context.Background(), "u", "l"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestListItemsMergesMoviesAndShowsByRank(t *testing.T) {
	page := `{
		"movies":[
			{"id":1,"mediatype":"movie","title":"M rank 1000","rank":1000},
			{"id":2,"mediatype":"movie","title":"M rank 3000","rank":3000},
			{"id":3,"mediatype":"movie","title":"M rank 5000","rank":5000}
		],
		"shows":[
			{"id":10,"mediatype":"show","title":"S rank 2000","rank":2000},
			{"id":11,"mediatype":"show","title":"S rank 4000","rank":4000}
		],
		"pagination":{"limit":1000,"offset":0,"total":5,"has_more":false}
	}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 77), map[string]string{"": page})

	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	wantOrder := []int{1, 10, 2, 11, 3} // ranks 1000,2000,3000,4000,5000
	if len(items) != len(wantOrder) {
		t.Fatalf("items = %d, want %d", len(items), len(wantOrder))
	}
	for i, want := range wantOrder {
		if items[i].TMDBID != want {
			t.Fatalf("items[%d].TMDBID = %d, want %d (order %+v)", i, items[i].TMDBID, want, items)
		}
	}
}

func TestListItemsStableForZeroRanks(t *testing.T) {
	// With equal ranks, movies (source order first) must stay before shows.
	page := `{
		"movies":[{"id":1,"mediatype":"movie","title":"M1","rank":0},{"id":2,"mediatype":"movie","title":"M2","rank":0}],
		"shows":[{"id":3,"mediatype":"show","title":"S1","rank":0}],
		"pagination":{"limit":1000,"offset":0,"total":3,"has_more":false}
	}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 7), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	want := []int{1, 2, 3}
	for i, id := range want {
		if items[i].TMDBID != id {
			t.Fatalf("items = %+v, want stable order %v", items, want)
		}
	}
}

func TestListItemsIgnoresSeasonsAndEpisodes(t *testing.T) {
	page := `{
		"movies":[{"id":1,"mediatype":"movie","title":"M","rank":1000}],
		"shows":[{"id":2,"mediatype":"show","title":"S","rank":2000}],
		"seasons":[{"id":900,"mediatype":"season","title":"Season","rank":500}],
		"episodes":[{"id":901,"mediatype":"episode","title":"Episode","rank":600}],
		"pagination":{"limit":1000,"offset":0,"total":2,"has_more":false}
	}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 5), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (seasons/episodes ignored): %+v", len(items), items)
	}
	for _, item := range items {
		if item.MediaType == "season" || item.MediaType == "episode" {
			t.Fatalf("leaked non movie/show item: %+v", item)
		}
	}
}

func TestListItemsStopsEarlyAtMaxItems(t *testing.T) {
	var itemRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/items") {
			_, _ = w.Write([]byte(metaOne("u", "l", 9)))
			return
		}
		itemRequests++
		_, _ = w.Write([]byte(`{
			"movies":[{"id":1,"rank":1000},{"id":2,"rank":2000},{"id":3,"rank":3000},{"id":4,"rank":4000}],
			"shows":[],
			"pagination":{"limit":1000,"offset":0,"total":100,"has_more":true,"next_cursor":"c1"}
		}`))
	}))
	defer srv.Close()

	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 2)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if itemRequests != 1 {
		t.Fatalf("item requests = %d, want 1 (stop after first page exceeds maxItems)", itemRequests)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
}

func TestListItemsTrimsToMaxItemsAfterMerge(t *testing.T) {
	// maxItems=2 with 1 movie rank 1000 and 3 shows; the merged top 2 are the
	// movie + the first show.
	page := `{
		"movies":[{"id":1,"rank":1000}],
		"shows":[{"id":10,"rank":2000},{"id":11,"rank":3000},{"id":12,"rank":4000}],
		"pagination":{"limit":1000,"offset":0,"total":4,"has_more":false}
	}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 11), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 2)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 || items[0].TMDBID != 1 || items[1].TMDBID != 10 {
		t.Fatalf("items = %+v, want [1,10]", items)
	}
}

func TestListItemsEmptyItemsWithTotalErrors(t *testing.T) {
	// MDBList's slug endpoint returns this shape for many non-empty lists: the
	// buckets are empty while total is positive. It must not read as "empty".
	page := `{"movies":[],"shows":[],"pagination":{"offset":0,"limit":1000,"total":300,"has_more":false}}`
	srv, _, _ := listItemsServer(t, metaOne("garycrawfordgc", "netflix-shows", 3082), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	_, err := c.ListItems(context.Background(), "garycrawfordgc", "netflix-shows", 0)
	if !errors.Is(err, ErrEmptyItemsWithTotal) {
		t.Fatalf("err = %v, want ErrEmptyItemsWithTotal", err)
	}
}

func TestListItemsEmptyWithoutTotalErrors(t *testing.T) {
	// F1: an empty 200 with total absent/0 must still be a sentinel, never
	// (nil, nil) — otherwise the catalog would wipe membership.
	cases := map[string]string{
		"total zero":    `{"movies":[],"shows":[],"pagination":{"offset":0,"limit":1000,"total":0,"has_more":false}}`,
		"total omitted": `{"movies":[],"shows":[],"pagination":{"has_more":false}}`,
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := listItemsServer(t, metaOne("u", "l", 1), map[string]string{"": page})
			c := NewClient("k", srv.Client())
			c.baseURL = srv.URL
			items, err := c.ListItems(context.Background(), "u", "l", 0)
			if !errors.Is(err, ErrEmptyItemsWithTotal) {
				t.Fatalf("err = %v, want ErrEmptyItemsWithTotal", err)
			}
			if items != nil {
				t.Fatalf("items = %+v, want nil", items)
			}
		})
	}
}

func TestListItemsShortPageBelowTotalErrors(t *testing.T) {
	// F2: pagination ends with fewer items than total, beyond the cap.
	page := `{"movies":[{"id":1,"rank":1000},{"id":2,"rank":2000}],"shows":[],"pagination":{"total":5,"has_more":false}}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 2), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	_, err := c.ListItems(context.Background(), "u", "l", 0)
	if !errors.Is(err, ErrIncompleteItems) {
		t.Fatalf("err = %v, want ErrIncompleteItems", err)
	}
}

func TestListItemsMaxItemsCapIsNotIncomplete(t *testing.T) {
	// F2: the intentional maxItems cap must NOT be flagged incomplete.
	page := `{"movies":[{"id":1,"rank":1000},{"id":2,"rank":2000},{"id":3,"rank":3000}],"shows":[],"pagination":{"total":100,"has_more":true,"next_cursor":"c1"}}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 2), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 2)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
}

func TestListItemsGenuinelyEmptyErrorsNotSilent(t *testing.T) {
	// A genuinely empty list still yields the sentinel; the catalog falls back
	// to /json, and only an empty /json result may empty the collection.
	page := `{"movies":[],"shows":[],"pagination":{"offset":0,"limit":1000,"total":0,"has_more":false}}`
	srv, _, _ := listItemsServer(t, metaOne("u", "empty", 3), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	if _, err := c.ListItems(context.Background(), "u", "empty", 0); !errors.Is(err, ErrEmptyItemsWithTotal) {
		t.Fatalf("err = %v, want ErrEmptyItemsWithTotal (catalog then checks /json)", err)
	}
}

func TestListItemsCompletePageBelowCap(t *testing.T) {
	// A complete result equal to total at the cap is fine.
	page := `{"movies":[{"id":1,"rank":1000},{"id":2,"rank":2000}],"shows":[],"pagination":{"total":2,"has_more":false}}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 2), map[string]string{"": page})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "u", "l", 10)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
}

func TestListItemsNonAdvancingCursorGuard(t *testing.T) {
	cases := map[string]string{
		"empty": `{
			"movies":[{"id":1,"rank":1000}],
			"shows":[],
			"pagination":{"has_more":true,"next_cursor":""}
		}`,
		"unchanged": `{
			"movies":[{"id":1,"rank":1000}],
			"shows":[],
			"pagination":{"has_more":true,"next_cursor":"same"}
		}`,
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, paths := listItemsServer(t, metaOne("u", "l", 1), map[string]string{"": page, "same": page})
			c := NewClient("k", srv.Client())
			c.baseURL = srv.URL

			_, err := c.ListItems(context.Background(), "u", "l", 0)
			if err == nil {
				t.Fatal("expected non-advancing cursor error, got nil")
			}
			if len(itemPaths(*paths)) > 2 {
				t.Fatalf("guard did not stop pagination: %d item requests", len(itemPaths(*paths)))
			}
		})
	}
}

func TestListItemsRepeatedCursorGuard(t *testing.T) {
	// Page 1 advances to c1; page c1 claims has_more and points back at c1.
	first := `{
		"movies":[{"id":1,"rank":1000}],
		"shows":[],
		"pagination":{"has_more":true,"next_cursor":"c1"}
	}`
	second := `{
		"movies":[{"id":2,"rank":2000}],
		"shows":[],
		"pagination":{"has_more":true,"next_cursor":"c1"}
	}`
	srv, _, _ := listItemsServer(t, metaOne("u", "l", 2), map[string]string{"": first, "c1": second})
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL

	_, err := c.ListItems(context.Background(), "u", "l", 0)
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("err = %v, want a repeated-cursor error", err)
	}
}

func TestListItemsTypedErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantSubstr string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"Invalid API key"}`, ErrUnauthorized, "Invalid API key"},
		{"forbidden", http.StatusForbidden, `{"error":"nope"}`, ErrUnauthorized, ""},
		{"not found", http.StatusNotFound, `{"error":"List Not found"}`, ErrListNotFound, "List Not found"},
		{"rate limited", http.StatusTooManyRequests, `{"error":"Too many requests"}`, ErrRateLimit, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The metadata call fails first, so the typed error comes from
			// ResolveListID.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient("bad", srv.Client())
			c.baseURL = srv.URL

			_, err := c.ListItems(context.Background(), "u", "l", 0)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %v, want body snippet %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestListItemsWithoutKeyReturnsErrNotConfigured(t *testing.T) {
	c := NewClient("", nil)
	_, err := c.ListItems(context.Background(), "u", "l", 0)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestListItemsRejectsEmptyUserOrList(t *testing.T) {
	c := NewClient("k", nil)
	if _, err := c.ListItems(context.Background(), "", "l", 0); err == nil {
		t.Fatal("expected error for empty user")
	}
	if _, err := c.ListItems(context.Background(), "u", "  ", 0); err == nil {
		t.Fatal("expected error for empty list")
	}
}

func TestListItemsRespectsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.ListItems(ctx, "u", "l", 0)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
}

func TestListItemsEscapesUserAndListPathSegments(t *testing.T) {
	var requestURIs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURIs = append(requestURIs, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/items") {
			_, _ = w.Write([]byte(`{"movies":[{"id":1,"rank":1000}],"shows":[],"pagination":{"total":1,"has_more":false}}`))
			return
		}
		_, _ = w.Write([]byte(metaOne("a b", "c/d", 55)))
	}))
	defer srv.Close()
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL
	if _, err := c.ListItems(context.Background(), "a b", "c/d", 0); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if !strings.HasPrefix(requestURIs[0], "/lists/a%20b/c%2Fd?") {
		t.Fatalf("metadata URI = %q, want escaped path segments", requestURIs[0])
	}
	if !strings.HasPrefix(requestURIs[1], "/lists/55/items?") {
		t.Fatalf("items URI = %q, want /lists/55/items", requestURIs[1])
	}
}

func TestListItemsBoundedBodyOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()
	c := NewClient("bad", srv.Client())
	c.baseURL = srv.URL

	_, err := c.ListItems(context.Background(), "u", "l", 0)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error message not bounded (%d chars)", len(err.Error()))
	}
}
