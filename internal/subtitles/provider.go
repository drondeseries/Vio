// internal/subtitles/provider.go
package subtitles

import "context"

// Provider defines the interface that each subtitle source must implement.
type Provider interface {
	// Name returns the provider identifier (e.g., "opensubtitles", "subdl", "subsource").
	Name() string

	// Search queries the provider for subtitles matching the request.
	Search(ctx context.Context, req SearchRequest) ([]SubtitleResult, error)

	// Download fetches the subtitle file content by provider-specific ID.
	Download(ctx context.Context, id string) ([]byte, SubtitleFormat, error)
}

// ConnectionTester is implemented by providers whose Search does not validate
// their credentials. OpenSubtitles searches with a shared default API key, so
// a search proves nothing about the configured username/password; the admin
// connection test must call this instead to verify the account can actually
// authenticate.
type ConnectionTester interface {
	// TestConnection verifies the provider's configured credentials against
	// the upstream. Returns nil when they authenticate, an error otherwise.
	TestConnection(ctx context.Context) error
}

// S3Client is the interface for S3 operations needed by the subtitle system.
// Defined here for testability — the concrete implementation is s3client.Client.
type S3Client interface {
	PutObject(ctx context.Context, bucket, key string, data []byte) error
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}
