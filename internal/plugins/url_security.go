package plugins

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// validateProviderStreamURL accepts only externally reachable HTTP(S) URLs.
// Plugin output is fetched by ffprobe/FFmpeg from the server, so accepting a
// loopback, link-local, private, or otherwise non-global destination would
// turn a compromised plugin into an SSRF primitive.
func validateProviderStreamURL(ctx context.Context, raw string) (string, error) {
	validated, err := remotestream.ValidateURL(ctx, raw)
	if err != nil {
		return "", err
	}
	return validated.String(), nil
}

// validateProviderStreamURLSyntax validates the structural safety of a
// provider stream URL without requiring it to resolve to a public address. It
// is used only when the plugin admin has explicitly enabled allow_insecure_http
// and the provider may legitimately return private/local stream URLs.
func validateProviderStreamURLSyntax(raw string) (string, error) {
	parsed, err := remotestream.ValidateURLSyntaxAllowNonPublic(raw)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}
