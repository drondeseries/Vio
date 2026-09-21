package prowlarr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func configureTestClient(t *testing.T, baseURL, apiKey string) *prowlarrSearchClient {
	t.Helper()
	c := NewSearchClient(nil)
	c.Configure(baseURL, apiKey, 15)
	return c
}

// errorBodyServer answers every request with the given status and body.
func errorBodyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=movie)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search", "limit=1000", "query=The+Show"} {
		if !strings.Contains(msg, want) {
			t.Errorf("search error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("search error leaked the API key: %q", msg)
	}
}

func TestRefreshErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=tvsearch)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	err := c.refresh(context.Background())
	if err == nil {
		t.Fatal("refresh: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refresh error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("refresh error leaked the API key: %q", msg)
	}
	c.mu.Lock()
	lastErr := c.lastErr
	c.mu.Unlock()
	if lastErr == nil {
		t.Error("refresh did not record lastErr")
	}
}

func TestValidateErrorIncludesStatusBodyAndURL(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"No such function (t=movie)"}`)
	c := configureTestClient(t, srv.URL, "an-api-key")

	_, err := c.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate: expected an error for HTTP 400")
	}
	msg := err.Error()
	for _, want := range []string{"400", "No such function", "/api/v1/search"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Validate error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "an-api-key") {
		t.Errorf("Validate error leaked the API key: %q", msg)
	}
}

func TestErrorBodyIsTruncatedSafely(t *testing.T) {
	srv := errorBodyServer(t, http.StatusBadRequest, strings.Repeat("A", maxErrorBodyBytes*4)+"TAIL-MARKER")
	c := configureTestClient(t, srv.URL, "")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	if !strings.Contains(msg, "(truncated)") {
		t.Errorf("error %q missing truncation marker", msg)
	}
	if strings.Contains(msg, "TAIL-MARKER") {
		t.Errorf("error quoted body past the %d-byte cap", maxErrorBodyBytes)
	}
	if len(msg) > maxErrorBodyBytes+512 {
		t.Errorf("error length %d exceeds the bounded snippet", len(msg))
	}
}

func TestErrorBodyRedactsAPIKey(t *testing.T) {
	const apiKey = "sup3r-secret-key"
	srv := errorBodyServer(t, http.StatusBadRequest, `{"message":"rejected key `+apiKey+`"}`)
	c := configureTestClient(t, srv.URL, apiKey)

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 400")
	}
	msg := err.Error()
	if strings.Contains(msg, apiKey) {
		t.Errorf("error leaked the API key: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("error %q missing redaction marker", msg)
	}
}

func TestUnauthorizedStaysDistinguishable(t *testing.T) {
	srv := errorBodyServer(t, http.StatusUnauthorized, `{"message":"unauthorized"}`)
	c := configureTestClient(t, srv.URL, "bad-key")

	_, err := c.search(context.Background(), monitoredMedia{Title: "The Show"})
	if err == nil {
		t.Fatal("search: expected an error for HTTP 401")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error %q does not name HTTP 401", msg)
	}
	if strings.Contains(msg, "400") {
		t.Errorf("error %q is indistinguishable from a 400", msg)
	}
	if !strings.Contains(msg, "unauthorized") {
		t.Errorf("error %q missing the response body", msg)
	}
}

func TestSearchURLKeepsBaseAndAppendsEndpoint(t *testing.T) {
	c := configureTestClient(t, "http://prowlarr:9696", "key")

	got, err := c.searchURLForQuery("")
	if err != nil {
		t.Fatalf("searchURLForQuery: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/api/v1/search" {
		t.Errorf("path = %q, want /api/v1/search", u.Path)
	}
	if q := u.Query(); q.Get("t") != "" || q.Get("query") != "" {
		t.Errorf("unexpected query parameters: %q", u.RawQuery)
	}
	if cats := u.Query()["categories"]; len(cats) != 2 || cats[0] != "2000" || cats[1] != "5000" {
		t.Errorf("categories = %v, want [2000 5000]", cats)
	}

	got, err = c.searchURLForQuery("The Show")
	if err != nil {
		t.Fatalf("searchURLForQuery(query): %v", err)
	}
	u, err = url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("query") != "The Show" {
		t.Errorf("query = %q, want The Show", u.Query().Get("query"))
	}
}

func TestSearchURLKeepsReverseProxyBasePath(t *testing.T) {
	c := configureTestClient(t, "http://host/prowlarr", "key")

	got, err := c.searchURLForQuery("")
	if err != nil {
		t.Fatalf("searchURLForQuery: %v", err)
	}
	if !strings.HasPrefix(got, "http://host/prowlarr/api/v1/search?") {
		t.Errorf("URL = %q, want a /prowlarr/api/v1/search path", got)
	}
}

func TestSearchURLRejectsIndexerPathOrQuery(t *testing.T) {
	cases := map[string]string{
		"hint URL":          "http://prowlarr:9696/1/api/v1/search?t=movie",
		"bare indexer id":   "http://prowlarr:9696/1",
		"trailing slash id": "http://prowlarr:9696/1/",
		"search path":       "http://prowlarr:9696/api/v1/search",
		"newznab path":      "http://prowlarr:9696/newznab",
		"query only":        "http://prowlarr:9696?t=movie",
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			c := configureTestClient(t, base, "key")
			_, err := c.searchURLForQuery("")
			if err == nil {
				t.Fatalf("searchURLForQuery(%q): expected a configuration error", base)
			}
			if !strings.Contains(err.Error(), "base URL") {
				t.Errorf("error %q does not name the expected base URL shape", err)
			}
		})
	}
}

func TestSearchURLRejectsMissingScheme(t *testing.T) {
	for _, base := range []string{"prowlarr:9696", "localhost:9696", "//prowlarr:9696"} {
		t.Run(base, func(t *testing.T) {
			c := configureTestClient(t, base, "key")
			_, err := c.searchURLForQuery("")
			if err == nil {
				t.Fatalf("searchURLForQuery(%q): expected a configuration error", base)
			}
			if !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error %q does not name the missing scheme", err)
			}
		})
	}
}
