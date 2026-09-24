package altmount

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func enqueueTestClient(t *testing.T, baseURL, apiKey string) *altmountStateClient {
	t.Helper()
	c := New(nil)
	c.Configure(baseURL, apiKey, 15)
	return c
}

func TestEnqueueSuccessReturnsNzoID(t *testing.T) {
	var gotMode, gotName, gotNZBName, gotOutput, gotQueryKey, gotHeaderKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotMode = r.Form.Get("mode")
		gotName = r.Form.Get("name")
		gotNZBName = r.Form.Get("nzbname")
		gotOutput = r.Form.Get("output")
		gotQueryKey = r.Form.Get("apikey")
		gotHeaderKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":true,"nzo_ids":["nzo_abc123"]}`)
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	nzoID, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "The Release")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if nzoID != "nzo_abc123" {
		t.Errorf("nzoID = %q, want nzo_abc123", nzoID)
	}
	if gotMode != "addurl" || gotOutput != "json" {
		t.Errorf("mode=%q output=%q, want addurl/json", gotMode, gotOutput)
	}
	if gotName != "https://indexer.example/download/abc" {
		t.Errorf("name = %q, want the download URL", gotName)
	}
	if gotNZBName != "The Release" {
		t.Errorf("nzbname = %q, want The Release", gotNZBName)
	}
	if gotQueryKey != "secret-key" || gotHeaderKey != "secret-key" {
		t.Errorf("apikey query=%q header=%q, want the configured key in both", gotQueryKey, gotHeaderKey)
	}
}

func TestEnqueueStatusFalseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":false,"error":"Invalid NZB"}`)
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	_, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "name")
	if err == nil {
		t.Fatal("expected an error for status:false")
	}
	if !strings.Contains(err.Error(), "Invalid NZB") {
		t.Errorf("error %q should carry the provider reason", err)
	}
}

func TestEnqueueEmptyNzoIDsIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":true,"nzo_ids":[]}`)
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	if _, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "name"); err == nil {
		t.Fatal("expected an error for an empty nzo_ids list")
	}
}

func TestEnqueueNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, "upstream exploded")
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	_, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "name")
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should name the status", err)
	}
}

func TestEnqueueTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold until the request context is canceled by the client's deadline.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Enqueue(ctx, "https://indexer.example/download/abc", "name")
	if err == nil {
		t.Fatal("expected an error when the caller context expires")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Enqueue took %s; it should honor the caller context, not the internal cap", elapsed)
	}
	if strings.Contains(err.Error(), "indexer.example") || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("timeout error leaked a secret: %q", err)
	}
}

func TestEnqueueErrorsNeverLogURLOrKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":false,"error":"refused secret-key for https://indexer.example/download/abc"}`)
	}))
	defer srv.Close()

	c := enqueueTestClient(t, srv.URL, "secret-key")
	_, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "name")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-key") {
		t.Errorf("error leaked the API key: %q", msg)
	}
	if strings.Contains(msg, "https://indexer.example/download/abc") || strings.Contains(msg, "indexer.example") {
		t.Errorf("error leaked the download URL: %q", msg)
	}
}

func TestEnqueueEmptyURLIsError(t *testing.T) {
	c := enqueueTestClient(t, "http://host", "secret-key")
	if _, err := c.Enqueue(context.Background(), "  ", "name"); err == nil {
		t.Fatal("expected an error for an empty download URL")
	}
}

func TestEnqueueUnconfiguredIsError(t *testing.T) {
	c := New(nil)
	if _, err := c.Enqueue(context.Background(), "https://indexer.example/download/abc", "name"); err == nil {
		t.Fatal("expected an error when AltMount is not configured")
	}
}
