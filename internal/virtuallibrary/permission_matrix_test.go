package virtuallibrary_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

func fakeProviderServer(privateStreamURL string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"streams": [{"name": "1080p", "title": "Matrix", "url": "` + privateStreamURL + `"}]}`))
	}))
}

func TestPermissionMatrixStreamDestinations(t *testing.T) {
	privateURL := "http://192.168.1.100:8080/stream1.mkv"
	server := fakeProviderServer(privateURL)
	defer server.Close()
	ctx := context.Background()

	t.Run("private streams off rejects private destination", func(t *testing.T) {
		if _, err := virtuallibrary.ValidateProviderStreamURL(ctx, privateURL, false); err == nil {
			t.Fatal("expected SSRF rejection with allowPrivateStreams=false")
		}
	})

	t.Run("private streams on permits private destination", func(t *testing.T) {
		got, err := virtuallibrary.ValidateProviderStreamURL(ctx, privateURL, true)
		if err != nil {
			t.Fatalf("ValidateProviderStreamURL: %v", err)
		}
		if got != privateURL {
			t.Fatalf("url = %q, want %q", got, privateURL)
		}
	})

	t.Run("private streams on still rejects control characters", func(t *testing.T) {
		if _, err := virtuallibrary.ValidateProviderStreamURL(ctx, "http://192.168.1.1/x\x00.mkv", true); err == nil {
			t.Fatal("expected control-character rejection even with opt-in")
		}
	})

	t.Run("private streams on still rejects credentials", func(t *testing.T) {
		if _, err := virtuallibrary.ValidateProviderStreamURL(ctx, "http://user:pass@192.168.1.1/x.mkv", true); err == nil {
			t.Fatal("expected credential rejection even with opt-in")
		}
	})

	t.Run("strict policy rejects loopback link-local multicast", func(t *testing.T) {
		for _, raw := range []string{
			"http://127.0.0.1/x.mkv",
			"http://169.254.169.254/latest/meta-data/",
			"http://224.0.0.1/x.mkv",
			"http://[::1]/x.mkv",
		} {
			if _, err := virtuallibrary.ValidateProviderStreamURL(ctx, raw, false); err == nil {
				t.Errorf("expected rejection of %q with opt-in off", raw)
			}
		}
	})

	t.Run("service resolve honors AllowPrivateStreams", func(t *testing.T) {
		strict := virtuallibrary.New(virtuallibrary.Config{
			Enabled:           true,
			ManifestURL:       server.URL + "/manifest.json",
			AllowInsecureHTTP: true,
		}, nil, nil)
		if _, err := strict.ResolveDetailed(ctx, "virtual://movie/tt100", false, nil, "", false); err == nil {
			t.Fatal("expected SSRF failure without AllowPrivateStreams")
		}
		permissive := virtuallibrary.New(virtuallibrary.Config{
			Enabled:             true,
			ManifestURL:         server.URL + "/manifest.json",
			AllowInsecureHTTP:   true,
			AllowPrivateStreams: true,
		}, nil, nil)
		res, err := permissive.ResolveDetailed(ctx, "virtual://movie/tt100", false, nil, "", false)
		if err != nil {
			t.Fatalf("ResolveDetailed: %v", err)
		}
		if res.URL != privateURL {
			t.Fatalf("url = %q, want %q", res.URL, privateURL)
		}
	})
}

func TestPermissionMatrixManifestIndependence(t *testing.T) {
	server := fakeProviderServer("http://192.168.1.100:8080/stream1.mkv")
	defer server.Close()
	ctx := context.Background()

	t.Run("manifest HTTP works without private streams", func(t *testing.T) {
		svc := virtuallibrary.New(virtuallibrary.Config{
			Enabled:           true,
			ManifestURL:       server.URL + "/manifest.json",
			AllowInsecureHTTP: true,
		}, nil, nil)
		if err := svc.Validate(ctx); err != nil {
			t.Fatalf("HTTP manifest rejected with manifest opt-in on: %v", err)
		}
		if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100", false, nil, "", false); err == nil {
			t.Fatal("private stream must still fail SSRF without AllowPrivateStreams")
		}
	})

	t.Run("private streams alone does not permit HTTP manifest", func(t *testing.T) {
		svc := virtuallibrary.New(virtuallibrary.Config{
			Enabled:             true,
			ManifestURL:         server.URL + "/manifest.json",
			AllowPrivateStreams: true,
		}, nil, nil)
		if err := svc.Validate(ctx); err == nil {
			t.Fatal("HTTP manifest accepted without manifest opt-in")
		}
	})

	t.Run("both off rejects HTTP manifest", func(t *testing.T) {
		svc := virtuallibrary.New(virtuallibrary.Config{
			Enabled:     true,
			ManifestURL: server.URL + "/manifest.json",
		}, nil, nil)
		if err := svc.Validate(ctx); err == nil {
			t.Fatal("HTTP manifest accepted with both flags off")
		} else if !strings.Contains(err.Error(), "insecure HTTP manifest") {
			t.Fatalf("err = %q, want insecure-manifest refusal", err)
		}
	})
}
