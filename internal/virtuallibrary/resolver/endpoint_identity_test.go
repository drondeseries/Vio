package resolver

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// TestStreamEndpointWithPolicy pins the stream endpoint derivation for the
// movie and series shapes, including the colon-joined series identifier and
// ids that carry a provider prefix. url.PathEscape leaves ":" alone, so the
// season/episode form and prefixed ids must round-trip unchanged.
func TestStreamEndpointWithPolicy(t *testing.T) {
	cases := []struct {
		name          string
		manifestURL   string
		mediaType     string
		mediaID       string
		allowInsecure bool
		want          string
		wantErr       bool
	}{
		{
			name:        "movie",
			manifestURL: "https://example.com/manifest.json",
			mediaType:   "movie",
			mediaID:     "tt123",
			want:        "https://example.com/stream/movie/tt123.json",
		},
		{
			name:        "series colon joined season and episode",
			manifestURL: "https://example.com/manifest.json",
			mediaType:   "series",
			mediaID:     "tt123:1:2",
			want:        "https://example.com/stream/series/tt123:1:2.json",
		},
		{
			name:        "base path preserved",
			manifestURL: "https://example.com/base/manifest.json",
			mediaType:   "movie",
			mediaID:     "tt123",
			want:        "https://example.com/base/stream/movie/tt123.json",
		},
		{
			name:        "tvdb prefixed series id",
			manifestURL: "https://example.com/manifest.json",
			mediaType:   "series",
			mediaID:     "tvdb:123:1:2",
			want:        "https://example.com/stream/series/tvdb:123:1:2.json",
		},
		{
			name:        "tmdb prefixed movie id",
			manifestURL: "https://example.com/manifest.json",
			mediaType:   "movie",
			mediaID:     "tmdb:456",
			want:        "https://example.com/stream/movie/tmdb:456.json",
		},
		{
			name:        "manifest query is stripped from the endpoint",
			manifestURL: "https://example.com/base/manifest.json?token=secret",
			mediaType:   "movie",
			mediaID:     "tt123",
			want:        "https://example.com/base/stream/movie/tt123.json",
		},
		{
			name:          "private http allowed when insecure enabled",
			manifestURL:   "http://192.168.1.10/manifest.json",
			mediaType:     "movie",
			mediaID:       "tt123",
			allowInsecure: true,
			want:          "http://192.168.1.10/stream/movie/tt123.json",
		},
		{
			name:          "public http refused",
			manifestURL:   "http://example.com/manifest.json",
			mediaType:     "movie",
			mediaID:       "tt123",
			allowInsecure: true,
			wantErr:       true,
		},
		{
			name:        "http requires allow insecure",
			manifestURL: "http://192.168.1.10/manifest.json",
			mediaType:   "movie",
			mediaID:     "tt123",
			wantErr:     true,
		},
		{
			name:        "manifest suffix required",
			manifestURL: "https://example.com/manifest",
			mediaType:   "movie",
			mediaID:     "tt123",
			wantErr:     true,
		},
		{
			name:        "empty manifest URL",
			manifestURL: "",
			mediaType:   "movie",
			mediaID:     "tt123",
			wantErr:     true,
		},
		{
			name:        "unsupported scheme",
			manifestURL: "ftp://example.com/manifest.json",
			mediaType:   "movie",
			mediaID:     "tt123",
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := streamEndpointWithPolicy(tc.manifestURL, tc.mediaType, tc.mediaID, tc.allowInsecure)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("streamEndpointWithPolicy(...) = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("streamEndpointWithPolicy(...) = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("streamEndpointWithPolicy(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseVirtualPath(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantType  string
		wantID    string
		wantError bool
	}{
		{"movie", "virtual://movie/tt123", "movie", "tt123", false},
		{"series colon joined", "virtual://series/tt123:1:2", "series", "tt123:1:2", false},
		{"anime accepted", "virtual://anime/tt123", "anime", "tt123", false},
		{"query stripped from id", "virtual://movie/tt123?result=abc&x=1", "movie", "tt123", false},
		{"missing prefix", "movie/tt123", "", "", true},
		{"missing identifier", "virtual://movie", "", "", true},
		{"unsupported type", "virtual://channel/tt123", "", "", true},
		{"traversal rejected", "virtual://movie/tt123/../etc", "", "", true},
		{"fragment rejected", "virtual://movie/tt123#frag", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mediaType, mediaID, err := parseVirtualPath(tc.path)
			if tc.wantError {
				if err == nil {
					t.Fatalf("parseVirtualPath(%q) = (%q, %q, nil), want error", tc.path, mediaType, mediaID)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVirtualPath(%q) = %v, want nil", tc.path, err)
			}
			if mediaType != tc.wantType || mediaID != tc.wantID {
				t.Fatalf("parseVirtualPath(%q) = (%q, %q), want (%q, %q)", tc.path, mediaType, mediaID, tc.wantType, tc.wantID)
			}
		})
	}
}

// TestCandidateVariantIDStableAndDistinct pins candidate identity: identical
// inputs hash identically, and every identity-bearing field change produces a
// different id. The twin case is the collision guard: two offers that the
// coarse name+size dedup key deliberately merges must still have distinct
// per-variant ids so a pin can name the offer it wants.
func TestCandidateVariantIDStableAndDistinct(t *testing.T) {
	base := StreamCandidate{
		URL:        "https://cdn.example/one.mkv",
		Name:       "Movie 2024 1080p",
		Title:      "Movie.2024.1080p.WEB-DL.x264",
		Resolution: "1080p",
		CodecVideo: "h264",
		CodecAudio: "aac",
		FileSize:   5_000_000_000,
	}

	id := stream.CandidateVariantID(base)
	if len(id) != 24 {
		t.Fatalf("CandidateVariantID length = %d, want 24", len(id))
	}
	if again := stream.CandidateVariantID(base); again != id {
		t.Fatalf("CandidateVariantID not stable: %q != %q", id, again)
	}

	// A different file offer of the same release: the coarse dedup key agrees,
	// the per-variant identity must not.
	twin := base
	twin.URL = "https://cdn.example/two.mkv"
	if stream.CandidateVariantID(twin) == id {
		t.Fatalf("CandidateVariantID collided for a differing URL: %q", id)
	}
	if candidateDedupKey(base) != candidateDedupKey(twin) {
		t.Fatalf("setup: expected name+size dedup keys to match, got %q and %q",
			candidateDedupKey(base), candidateDedupKey(twin))
	}

	sizeChanged := base
	sizeChanged.FileSize++
	if stream.CandidateVariantID(sizeChanged) == id {
		t.Fatalf("CandidateVariantID collided for a differing FileSize: %q", id)
	}

	resolutionChanged := base
	resolutionChanged.Resolution = "2160p"
	if stream.CandidateVariantID(resolutionChanged) == id {
		t.Fatalf("CandidateVariantID collided for a differing Resolution: %q", id)
	}

	hashChanged := base
	hashChanged.BehaviorHints.VideoHash = "abcdef"
	if stream.CandidateVariantID(hashChanged) == id {
		t.Fatalf("CandidateVariantID collided for a differing VideoHash: %q", id)
	}
}
