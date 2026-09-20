package resolver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateStremioManifest covers the manifest acceptance rules the
// connection validator depends on: an id, the stream resource (string or
// descriptor form), and at least one supported media type.
func TestValidateStremioManifest(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "string stream resource and movie type",
			payload: `{"id":"p","resources":["catalog","stream"],"types":["movie"]}`,
		},
		{
			name:    "descriptor stream resource and series type",
			payload: `{"id":"p","resources":[{"name":"stream","types":["series"]}],"types":["series"]}`,
		},
		{
			name:    "missing id",
			payload: `{"resources":["stream"],"types":["movie"]}`,
			wantErr: true,
		},
		{
			name:    "no stream resource",
			payload: `{"id":"p","resources":["catalog","meta"],"types":["movie"]}`,
			wantErr: true,
		},
		{
			name:    "only unsupported types",
			payload: `{"id":"p","resources":["stream"],"types":["channel","tv"]}`,
			wantErr: true,
		},
		{
			name:    "empty types",
			payload: `{"id":"p","resources":["stream"],"types":[]}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var manifest stremioManifest
			if err := json.Unmarshal([]byte(tc.payload), &manifest); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			err := validateStremioManifest(manifest)
			if tc.wantErr && err == nil {
				t.Fatal("validateStremioManifest() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateStremioManifest() = %v, want nil", err)
			}
		})
	}
}

// TestDecodeBoundedJSONEnforcesSizeBound pins the body bound used for both
// stream answers and manifests: a payload exactly at the limit decodes, one
// byte over is rejected before any parsing.
func TestDecodeBoundedJSONEnforcesSizeBound(t *testing.T) {
	const limit = int64(64)

	t.Run("at limit decodes", func(t *testing.T) {
		body := `{"a":1}`
		pad := strings.Repeat(" ", int(limit)-len(body))
		var dst map[string]int
		if err := decodeBoundedJSON(io.NopCloser(strings.NewReader(body+pad)), limit, &dst); err != nil {
			t.Fatalf("decodeBoundedJSON(at limit) = %v, want nil", err)
		}
		if dst["a"] != 1 {
			t.Fatalf("decoded value = %v, want a=1", dst)
		}
	})

	t.Run("over limit rejected", func(t *testing.T) {
		body := strings.Repeat("x", int(limit)+1)
		var dst map[string]int
		err := decodeBoundedJSON(io.NopCloser(strings.NewReader(body)), limit, &dst)
		if err == nil {
			t.Fatal("decodeBoundedJSON(over limit) = nil, want error")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want an exceeds-bound error", err)
		}
	})

	t.Run("malformed rejected", func(t *testing.T) {
		var dst map[string]int
		if err := decodeBoundedJSON(io.NopCloser(strings.NewReader(`{"a":`)), 64, &dst); err == nil {
			t.Fatal("decodeBoundedJSON(malformed) = nil, want error")
		}
	})
}

func TestValidateConnectionManifestPolicies(t *testing.T) {
	cases := []struct {
		name        string
		manifestURL string
		allowHTTP   bool
	}{
		{"insecure public host", "http://example.com/manifest.json", false},
		{"wrong path suffix", "https://example.com/notmanifest.json", false},
		{"relative URL", "/manifest.json", false},
		{"empty URL", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Config{ManifestURL: tc.manifestURL, AllowInsecure: tc.allowHTTP})
			if err := r.ValidateConnection(context.Background()); err == nil {
				t.Fatalf("ValidateConnection(%q) = nil, want URL-policy error", tc.manifestURL)
			}
		})
	}
}

func TestValidateConnectionFetchesManifest(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantErrSub string
	}{
		{
			name:   "valid manifest",
			status: http.StatusOK,
			body:   `{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`,
		},
		{
			name:       "malformed manifest",
			status:     http.StatusOK,
			body:       `{"id":`,
			wantErrSub: "decode streaming provider manifest",
		},
		{
			name:       "missing stream resource",
			status:     http.StatusOK,
			body:       `{"id":"p","resources":["catalog"],"types":["movie"]}`,
			wantErrSub: "stream resource",
		},
		{
			name:       "unsupported type",
			status:     http.StatusOK,
			body:       `{"id":"p","resources":["stream"],"types":["channel"]}`,
			wantErrSub: "movie or series",
		},
		{
			name:       "provider error status",
			status:     http.StatusBadGateway,
			body:       `{}`,
			wantErrSub: "status 502",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			r := New(Config{ManifestURL: server.URL + "/manifest.json", AllowInsecure: true})
			err := r.ValidateConnection(context.Background())
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("ValidateConnection() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateConnection() = nil, want error containing %q", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErrSub)
			}
		})
	}
}

// TestValidateConnectionRejectsOversizedManifest proves the manifest bound is
// enforced against a real response, not just decodeBoundedJSON in isolation.
func TestValidateConnectionRejectsOversizedManifest(t *testing.T) {
	oversized := strings.Repeat("x", maxManifestResponseBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, oversized)
	}))
	t.Cleanup(server.Close)

	r := New(Config{ManifestURL: server.URL + "/manifest.json", AllowInsecure: true})
	err := r.ValidateConnection(context.Background())
	if err == nil {
		t.Fatal("ValidateConnection(oversized manifest) = nil, want error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want it to mention the byte bound", err)
	}
}
