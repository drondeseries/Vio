package virtuallibrary_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// trustWindowProvider serves one live release so an absent pin can be
// distinguished from a dead provider.
func trustWindowProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"streams": [
			{"name": "1080p", "title": "Live sibling", "url": "http://192.168.1.100:8080/stream1.mkv",
			 "behaviorHints": {"videoHash": "hash-live"}}
		]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func trustWindowService(t *testing.T, manifestURL string) *virtuallibrary.Service {
	t.Helper()
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:           true,
		ManifestURL:       manifestURL,
		AllowInsecureHTTP: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	return svc
}

// TestResolveDetailedTrustWindowRefusesToSubstitute proves that a session-bound
// pin that the provider no longer lists is refused with ErrPersistedCandidateTrusted
// when the caller declared the persisted same-identity row as trusted. It is
// deliberately not ErrSessionBoundCandidateAbsent, so the serve layer and the
// rehydration retry do not rotate to a sibling under the viewer's selection.
func TestResolveDetailedTrustWindowRefusesToSubstitute(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateIdentity(context.Background(), virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "hash-persisted",
		ReleaseName: "Movie.2024.1080p",
	})
	ctx = virtuallibrary.WithPersistedCandidateTrust(ctx, true)

	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatalf("err = %v, want ErrPersistedCandidateTrusted", err)
	}
	if errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatal("trusted candidate must not carry the rotation sentinel")
	}
}

// TestResolveDetailedTrustWindowRequiresIdentity proves the trusted flag alone
// never changes behavior: without a durable identity the row cannot be proven
// to be the requested release, so today's rotation sentinel is preserved.
func TestResolveDetailedTrustWindowRequiresIdentity(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateTrust(context.Background(), true)
	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want ErrSessionBoundCandidateAbsent", err)
	}
}

// TestResolveDetailedTrustWindowBeyondWindowUnchanged proves that without the
// trusted flag the pre-window refusal is unchanged: a session-bound absent pin
// still carries ErrSessionBoundCandidateAbsent so callers can rotate.
func TestResolveDetailedTrustWindowBeyondWindowUnchanged(t *testing.T) {
	server := trustWindowProvider(t)
	svc := trustWindowService(t, server.URL+"/manifest.json")

	ctx := virtuallibrary.WithPersistedCandidateIdentity(context.Background(), virtuallibrary.PersistedCandidateIdentity{
		VideoHash:   "hash-persisted",
		ReleaseName: "Movie.2024.1080p",
	})
	_, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result=absentrelease", false, nil, "", true, false)
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want ErrSessionBoundCandidateAbsent", err)
	}
	if errors.Is(err, virtuallibrary.ErrPersistedCandidateTrusted) {
		t.Fatal("untrusted candidate must not carry the trust sentinel")
	}
}
