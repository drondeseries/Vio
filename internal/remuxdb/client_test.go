package remuxdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchProbe(t *testing.T) {
	season, episode := 1, 2
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/media/info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("imdb_id") != "tt1" || q.Get("season") != "1" || q.Get("episode") != "2" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if r.Header.Get("x-client-id") != "silo-server" {
			t.Errorf("client id = %q", r.Header.Get("x-client-id"))
		}
		_ = json.NewEncoder(w).Encode([]MediaInfo{{
			Size:      10,
			Container: "mkv",
			Tracks:    []TrackDetail{{Kind: "video", Codec: "h264"}},
		}})
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "")
	got, err := client.FetchProbe(context.Background(), "tt1", &season, &episode)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0].Size != 10 || got[0].Tracks[0].Codec != "h264" {
		t.Fatalf("versions = %+v", got)
	}
}

func TestFetchProbeNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	got, err := NewClient(ts.URL, "").FetchProbe(context.Background(), "tt9", nil, nil)
	if err != nil || got != nil {
		t.Fatalf("404 = %+v %v, want nil nil", got, err)
	}
}

func TestFetchProbeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	if _, err := NewClient(ts.URL, "").FetchProbe(context.Background(), "tt1", nil, nil); err == nil {
		t.Fatal("500 accepted")
	}
	if _, err := NewClient(ts.URL, "").FetchProbe(context.Background(), "", nil, nil); err != nil {
		t.Fatalf("empty id should short-circuit: %v", err)
	}
}

func TestSubmitProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mediainfo" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		var payload SubmissionPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload.Kind != "movie" || payload.TorrentInfoHash != "abc" {
			t.Errorf("payload = %+v", payload)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"uuid-123"}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "secret-token")
	err := client.SubmitProbe(context.Background(), SubmissionPayload{
		Kind:            "movie",
		Filename:        "test.mkv",
		TorrentInfoHash: "abc",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func TestSubmitProbeValidation(t *testing.T) {
	client := NewClient("http://localhost", "")
	if err := client.SubmitProbe(context.Background(), SubmissionPayload{}); err == nil {
		t.Fatal("empty payload should fail validation")
	}
	if err := client.SubmitProbe(context.Background(), SubmissionPayload{Kind: "movie", Filename: "a.mkv"}); err == nil {
		t.Fatal("payload without hash or nzb should fail validation")
	}
}
