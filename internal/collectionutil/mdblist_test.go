package collectionutil

import (
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestNormalizeMDBListURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"https://mdblist.com/lists/example-user/watchlist", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/json", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/watchlist/json/", "https://mdblist.com/lists/example-user/watchlist/json"},
		{"https://mdblist.com/lists/example-user/external/1234/json", "https://mdblist.com/lists/example-user/external/1234/json"},
		{"https://mdblist.com/lists/example-user/external/1234/json/json", "https://mdblist.com/lists/example-user/external/1234/json"},
		{"  https://mdblist.com/lists/example-user/watchlist  ", "https://mdblist.com/lists/example-user/watchlist/json"},
	}
	for _, tc := range cases {
		if got := NormalizeMDBListURL(tc.in); got != tc.want {
			t.Errorf("NormalizeMDBListURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMDBListURLCandidatesNormalizesAndDeduplicates(t *testing.T) {
	got := MDBListURLCandidates(
		"https://mdblist.com/lists/example-user/external/1234/json/json",
		"https://mdblist.com/lists/example-user/external/1234/json",
		"https://mdblist.com/lists/example-user/other",
	)
	want := []string{
		"https://mdblist.com/lists/example-user/external/1234/json",
		"https://mdblist.com/lists/example-user/other/json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MDBListURLCandidates = %#v, want %#v", got, want)
	}
}

func TestFetchMDBListWithFallback(t *testing.T) {
	errFetch := errors.New("fetch failed")

	t.Run("returns first success without trying later candidates", func(t *testing.T) {
		var tried []string
		got, err := FetchMDBListWithFallback([]string{"a", "b"}, func(url string) ([]string, error) {
			tried = append(tried, url)
			return []string{url + "-entry"}, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"a-entry"}) {
			t.Fatalf("entries = %#v, want first candidate's result", got)
		}
		if !reflect.DeepEqual(tried, []string{"a"}) {
			t.Fatalf("tried = %#v, want to stop after first success", tried)
		}
	})

	t.Run("falls back past a failing candidate", func(t *testing.T) {
		var tried []string
		got, err := FetchMDBListWithFallback([]string{"a", "b"}, func(url string) ([]string, error) {
			tried = append(tried, url)
			if url == "a" {
				return nil, errFetch
			}
			return []string{url + "-entry"}, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"b-entry"}) {
			t.Fatalf("entries = %#v, want fallback candidate's result", got)
		}
		if !reflect.DeepEqual(tried, []string{"a", "b"}) {
			t.Fatalf("tried = %#v, want both candidates attempted", tried)
		}
	})

	t.Run("returns the last error when every candidate fails", func(t *testing.T) {
		_, err := FetchMDBListWithFallback([]string{"a", "b"}, func(string) ([]string, error) {
			return nil, errFetch
		})
		if !errors.Is(err, errFetch) {
			t.Fatalf("err = %v, want %v", err, errFetch)
		}
	})

	t.Run("empty candidate list yields nil result and nil error", func(t *testing.T) {
		got, err := FetchMDBListWithFallback(nil, func(string) ([]string, error) {
			t.Fatal("fetch should not be called for an empty list")
			return nil, nil
		})
		if err != nil || got != nil {
			t.Fatalf("got %#v, %v; want nil, nil", got, err)
		}
	})
}

func TestValidateMDBListURL(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"https://mdblist.com/lists/example-user/watchlist",
		"https://mdblist.com/lists/example-user/watchlist/json",
		"https://www.mdblist.com/lists/example-user/watchlist/json",
		"http://mdblist.com/lists/example-user/watchlist/json",
		"https://mdblist.com./lists/example-user/watchlist/json",
		"https://mdblist.com:443/lists/example-user/watchlist/json",
	}
	for _, raw := range allowed {
		if err := ValidateMDBListURL(raw); err != nil {
			t.Errorf("ValidateMDBListURL(%q) = %v, want nil", raw, err)
		}
	}

	blocked := []string{
		"",
		"http://127.0.0.1:8096/",
		"http://127.0.0.1:8096/json",
		"http://127.0.0.1/lists/x/y/json",
		"http://localhost/lists/x/y/json",
		"http://[::1]/lists/x/y/json",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/lists/x/y/json",
		"http://192.168.1.1/lists/x/y/json",
		"http://172.16.0.1/lists/x/y/json",
		"https://mdblist.com.evil.example/lists/x/y/json",
		"https://evil.example/lists/x/y/json",
		"https://mdblist.com/",
		"https://mdblist.com/json",
		"https://mdblist.com/admin/json",
		"https://api.mdblist.com/lists/x/y/json",
		"https://mdblist.com:8080/lists/x/y/json",
		"https://mdblist.com@127.0.0.1/lists/x/y/json",
		"file:///etc/passwd",
		"ftp://mdblist.com/lists/x/y/json",
	}
	for _, raw := range blocked {
		if err := ValidateMDBListURL(raw); !errors.Is(err, ErrMDBListURL) {
			t.Errorf("ValidateMDBListURL(%q) = %v, want ErrMDBListURL", raw, err)
		}
	}
}

func TestCanonicalMDBListURLRejectsPrivateHosts(t *testing.T) {
	t.Parallel()

	if _, err := CanonicalMDBListURL("http://127.0.0.1:8096/"); !errors.Is(err, ErrMDBListURL) {
		t.Fatalf("CanonicalMDBListURL(loopback) = %v, want ErrMDBListURL", err)
	}

	got, err := CanonicalMDBListURL("https://mdblist.com/lists/example-user/watchlist")
	if err != nil {
		t.Fatalf("CanonicalMDBListURL(valid) = %v", err)
	}
	if got != "https://mdblist.com/lists/example-user/watchlist/json" {
		t.Fatalf("CanonicalMDBListURL = %q", got)
	}
}

func TestParseMDBListListURLReturnsUnescapedSegments(t *testing.T) {
	t.Parallel()

	// F10: the returned segments must be raw slugs so callers escape exactly
	// once. %2F/%25 must not come back percent-encoded.
	user, list, ok := ParseMDBListListURL("https://mdblist.com/lists/some%2Fuser/my%25list/json")
	if !ok {
		t.Fatal("ParseMDBListListURL rejected an escaped URL")
	}
	if user != "some/user" || list != "my%list" {
		t.Fatalf("segments = (%q, %q), want (some/user, my%%list)", user, list)
	}
}

func TestParseMDBListListURLEscapesExactlyOnce(t *testing.T) {
	t.Parallel()

	user, list, ok := ParseMDBListListURL("https://mdblist.com/lists/some%2Fuser/my%25list/json")
	if !ok {
		t.Fatal("ParseMDBListListURL rejected an escaped URL")
	}
	if got := url.PathEscape(user); got != "some%2Fuser" {
		t.Fatalf("re-escaping user = %q, want some%%2Fuser (single escape)", got)
	}
	if got := url.PathEscape(list); got != "my%25list" {
		t.Fatalf("re-escaping list = %q, want my%%25list (single escape)", got)
	}
}

func TestParseMDBListListURL(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		raw  string
		user string
		list string
	}{
		{"https://mdblist.com/lists/alice/horror", "alice", "horror"},
		{"https://mdblist.com/lists/alice/horror/json", "alice", "horror"},
		{"https://mdblist.com/lists/alice/horror/", "alice", "horror"},
		{"http://www.mdblist.com/lists/bob/my-list/json", "bob", "my-list"},
		{"https://mdblist.com:443/lists/carol/top_100/json", "carol", "top_100"},
		{"  https://mdblist.com/lists/dave/sci-fi  ", "dave", "sci-fi"},
		{"https://mdblist.com/lists/12345/horror", "12345", "horror"},
	}
	for _, tc := range accepted {
		user, list, ok := ParseMDBListListURL(tc.raw)
		if !ok {
			t.Errorf("ParseMDBListListURL(%q) rejected, want accept", tc.raw)
			continue
		}
		if user != tc.user || list != tc.list {
			t.Errorf("ParseMDBListListURL(%q) = (%q, %q), want (%q, %q)", tc.raw, user, list, tc.user, tc.list)
		}
	}

	rejected := []string{
		"",
		"https://mdblist.com/",
		"https://mdblist.com/lists/alice",
		"https://mdblist.com/lists/alice/horror/extra",
		"https://mdblist.com/lists/alice/horror/extra/json",
		"https://mdblist.com/lists/alice/12345", // numeric list id, not a slug
		"https://mdblist.com/lists/alice/12345/json",
		"https://evil.example/lists/alice/horror",
		"https://mdblist.com.evil.example/lists/alice/horror",
		"ftp://mdblist.com/lists/alice/horror",
		"https://mdblist.com:8080/lists/alice/horror",
		"https://mdblist.com@127.0.0.1/lists/alice/horror",
		"https://mdblist.com/lists//horror",
		"https://mdblist.com/lists/alice/",
		"https://mdblist.com/lists/alice/horror?x=1",
		"https://mdblist.com/lists/alice/horror#frag",
		"https://mdblist.com/admin/alice/horror",
	}
	for _, raw := range rejected {
		if _, _, ok := ParseMDBListListURL(raw); ok {
			t.Errorf("ParseMDBListListURL(%q) accepted, want reject", raw)
		}
	}
}

func TestMDBListHTTPClientRejectsPrivateRedirect(t *testing.T) {
	t.Parallel()

	client := MDBListHTTPClient(nil)
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8096/json", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := client.CheckRedirect(req, []*http.Request{req}); !errors.Is(err, ErrMDBListURL) {
		t.Fatalf("CheckRedirect = %v, want ErrMDBListURL", err)
	}
}

func TestMDBListHTTPClientAppliesDefaultTimeout(t *testing.T) {
	t.Parallel()

	base := &http.Client{}
	client := MDBListHTTPClient(base)
	if client.Timeout != 30*time.Second {
		t.Fatalf("clone timeout = %v, want 30s default", client.Timeout)
	}
	if base.Timeout != 0 {
		t.Fatalf("base client mutated: timeout = %v, want 0", base.Timeout)
	}
}

func TestMDBListHTTPClientPreservesExplicitTimeout(t *testing.T) {
	t.Parallel()

	base := &http.Client{Timeout: 7 * time.Second}
	client := MDBListHTTPClient(base)
	if client.Timeout != 7*time.Second {
		t.Fatalf("clone timeout = %v, want explicit 7s preserved", client.Timeout)
	}
}

func TestMDBListHTTPClientNilBaseGetsDefaultTimeout(t *testing.T) {
	t.Parallel()

	client := MDBListHTTPClient(nil)
	if client.Timeout != 30*time.Second {
		t.Fatalf("nil-base clone timeout = %v, want 30s default", client.Timeout)
	}
	if http.DefaultClient.Timeout != 0 {
		t.Fatalf("http.DefaultClient mutated: timeout = %v", http.DefaultClient.Timeout)
	}
}
