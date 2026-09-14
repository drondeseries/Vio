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

// listItemsServer returns an httptest server that serves canned pages keyed by
// cursor ("" for the first page) and records the query params of each request.
func listItemsServer(t *testing.T, pages map[string]string) (*httptest.Server, *[]url.Values, *[]string) {
	t.Helper()
	var queries []url.Values
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		paths = append(paths, r.URL.Path)
		page, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries, &paths
}

func TestListItemsTraversesCursorsAndSendsKey(t *testing.T) {
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
	srv, queries, paths := listItemsServer(t, pages)

	c := NewClient("secret-key", srv.Client())
	c.baseURL = srv.URL

	items, err := c.ListItems(context.Background(), "alice", "watchlist", 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if len(*queries) != 2 {
		t.Fatalf("requests = %d, want 2", len(*queries))
	}
	for i, q := range *queries {
		if q.Get("apikey") != "secret-key" {
			t.Fatalf("request %d apikey = %q, want secret-key", i, q.Get("apikey"))
		}
		if q.Get("limit") != "1000" {
			t.Fatalf("request %d limit = %q, want 1000", i, q.Get("limit"))
		}
	}
	if (*queries)[0].Get("cursor") != "" {
		t.Fatalf("first request sent cursor %q, want empty", (*queries)[0].Get("cursor"))
	}
	if (*queries)[1].Get("cursor") != "c1" {
		t.Fatalf("second request cursor = %q, want c1", (*queries)[1].Get("cursor"))
	}
	if (*paths)[0] != "/lists/alice/watchlist/items" {
		t.Fatalf("path = %q, want /lists/alice/watchlist/items", (*paths)[0])
	}
	// Fields map from the API names (tvdb_id/release_date).
	if items[0].TMDBID != 100 || items[0].ReleaseDate != "2001-01-01" || items[0].Rank != 1000 {
		t.Fatalf("movie item = %+v", items[0])
	}
	if items[1].MediaType != "show" || items[1].TVDBID == nil || *items[1].TVDBID != 200 || items[1].ReleaseDate != "2002-02-02" {
		t.Fatalf("show item = %+v", items[1])
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
	srv, _, _ := listItemsServer(t, map[string]string{"": page})

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
	srv, _, _ := listItemsServer(t, map[string]string{"": page})
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
	srv, _, _ := listItemsServer(t, map[string]string{"": page})
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
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
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
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (stop after first page exceeds maxItems)", requests)
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
	srv, _, _ := listItemsServer(t, map[string]string{"": page})
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
			srv, queries, _ := listItemsServer(t, map[string]string{"": page, "same": page})
			c := NewClient("k", srv.Client())
			c.baseURL = srv.URL

			_, err := c.ListItems(context.Background(), "u", "l", 0)
			if err == nil {
				t.Fatal("expected non-advancing cursor error, got nil")
			}
			if len(*queries) > 2 {
				t.Fatalf("guard did not stop pagination: %d requests", len(*queries))
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
	srv, _, _ := listItemsServer(t, map[string]string{"": first, "c1": second})
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
	var requestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		_, _ = w.Write([]byte(`{"movies":[],"shows":[],"pagination":{"has_more":false}}`))
	}))
	defer srv.Close()
	c := NewClient("k", srv.Client())
	c.baseURL = srv.URL
	if _, err := c.ListItems(context.Background(), "a b", "c/d", 0); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if !strings.HasPrefix(requestURI, "/lists/a%20b/c%2Fd/items?") {
		t.Fatalf("request URI = %q, want escaped path segments", requestURI)
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
