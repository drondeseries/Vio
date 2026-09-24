package remotestream

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	addresses, ok := r[host]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]netip.Addr(nil), addresses...), nil
}

type recordingDialer struct {
	address string
	err     error
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.address = address
	return nil, d.err
}

func TestValidateURLRejectsNonPublicAndReservedAddresses(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/stream",
		"http://10.0.0.7/stream",
		"http://100.64.0.1/stream",
		"http://169.254.169.254/latest/meta-data",
		"http://192.0.2.1/stream",
		"http://[::1]/stream",
		"http://[2001:db8::1]/stream",
		"http://localhost/stream",
		"file:///tmp/media",
	} {
		if _, err := ValidateURL(context.Background(), raw); err == nil {
			t.Fatalf("ValidateURL(%q) succeeded, want rejection", raw)
		}
	}
}

func TestValidateURLRejectsMixedPublicAndPrivateDNSAnswers(t *testing.T) {
	resolver := staticResolver{
		"provider.example": {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("127.0.0.1")},
	}
	if _, err := validateURL(context.Background(), "https://provider.example/stream", resolver); err == nil {
		t.Fatal("validateURL succeeded with a private DNS answer")
	}
}

func TestValidateURLAcceptsPublicAddress(t *testing.T) {
	got, err := ValidateURL(context.Background(), "https://1.1.1.1/stream?token=x")
	if err != nil {
		t.Fatalf("ValidateURL returned error: %v", err)
	}
	if got.String() != "https://1.1.1.1/stream?token=x" {
		t.Fatalf("validated URL = %q", got.String())
	}
}

func TestValidateURLSyntaxRejectsUnsafeStructureWithoutDNS(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"https://user:secret@1.1.1.1/stream",
		"https://1.1.1.1/stream\ninjected",
		"http://127.0.0.1/admin",
	} {
		if _, err := ValidateURLSyntax(raw); err == nil {
			t.Fatalf("ValidateURLSyntax(%q) succeeded, want rejection", raw)
		}
	}
	parsed, err := ValidateURLSyntax("https://provider.invalid/stream?token=x")
	if err != nil || parsed.String() != "https://provider.invalid/stream?token=x" {
		t.Fatalf("ValidateURLSyntax deferred DNS incorrectly: %v, %v", parsed, err)
	}
}

func TestValidateURLSyntaxAllowNonPublic(t *testing.T) {
	// The allow_private_streams opt-in must accept private/local/numeric hosts.
	for _, raw := range []string{
		"http://10.0.0.7/stream",
		"http://192.168.1.5/video.mp4",
		"http://127.0.0.1/admin",
		"http://[::1]/stream",
		"http://provider.internal/stream",
	} {
		if _, err := ValidateURLSyntaxAllowNonPublic(raw); err != nil {
			t.Fatalf("ValidateURLSyntaxAllowNonPublic(%q) rejected a private host: %v", raw, err)
		}
	}
	// Structural safety still applies.
	for _, raw := range []string{
		"file:///etc/passwd",
		"https://user:secret@10.0.0.7/stream",
		"https://10.0.0.7/stream\ninjected",
		"not a url",
		"ftp://10.0.0.7/stream",
		"",
	} {
		if _, err := ValidateURLSyntaxAllowNonPublic(raw); err == nil {
			t.Fatalf("ValidateURLSyntaxAllowNonPublic(%q) succeeded, want rejection", raw)
		}
	}
}

func TestSafeTransportDialsResolvedIPAddress(t *testing.T) {
	resolver := staticResolver{"provider.example": {netip.MustParseAddr("1.1.1.1")}}
	dialer := &recordingDialer{err: errors.New("stop after recording")}
	transport := newSafeTransport(resolver, dialer)

	_, err := transport.DialContext(context.Background(), "tcp", "provider.example:443")
	if err == nil {
		t.Fatal("DialContext succeeded, want recording dialer error")
	}
	if dialer.address != "1.1.1.1:443" {
		t.Fatalf("dialed %q, want pinned public address", dialer.address)
	}
}

func TestSafeTransportRejectsReboundPrivateAddress(t *testing.T) {
	resolver := staticResolver{"provider.example": {netip.MustParseAddr("127.0.0.1")}}
	dialer := &recordingDialer{}
	transport := newSafeTransport(resolver, dialer)

	if _, err := transport.DialContext(context.Background(), "tcp", "provider.example:443"); err == nil {
		t.Fatal("DialContext succeeded after DNS rebinding to loopback")
	}
	if dialer.address != "" {
		t.Fatalf("unsafe address reached dialer: %q", dialer.address)
	}
}

func TestInsecureTransportDialsPrivateAddress(t *testing.T) {
	resolver := staticResolver{"provider.example": {netip.MustParseAddr("127.0.0.1")}}
	dialer := &recordingDialer{err: errors.New("stop after recording")}
	transport := newInsecureTransport(resolver, dialer)

	if _, err := transport.DialContext(context.Background(), "tcp", "provider.example:443"); err == nil {
		t.Fatal("DialContext succeeded, want recording dialer error")
	}
	if dialer.address != "127.0.0.1:443" {
		t.Fatalf("dialed %q, want pinned private address", dialer.address)
	}
}

func TestSafeTransportBoundsResponseHeadersWithoutClientBodyTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	resolver := staticResolver{"provider.example": {netip.MustParseAddr("1.1.1.1")}}
	transport := newSafeTransport(resolver, contextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}))
	if transport.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", transport.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	transport.ResponseHeaderTimeout = 20 * time.Millisecond

	go func() {
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				return
			}
		}
	}()
	request, err := http.NewRequest(http.MethodGet, "http://provider.example/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip succeeded without response headers")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("response header timeout took %s", elapsed)
	}

	client := NewSafeClient()
	if client.Timeout != 0 {
		t.Fatalf("client body timeout = %s, want no whole-response timeout", client.Timeout)
	}
	if closer, ok := client.Transport.(io.Closer); ok {
		_ = closer.Close()
	}
}

func TestNewSafeTransportPinsConnectionsThroughSafeDialer(t *testing.T) {
	transport := NewSafeTransport()
	if transport == nil {
		t.Fatal("NewSafeTransport returned nil")
	}
	if transport.DialContext == nil {
		t.Fatal("NewSafeTransport has no pinned DialContext")
	}
	if transport.Proxy != nil {
		t.Fatal("NewSafeTransport uses an HTTP proxy")
	}
	if transport.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", transport.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("NewSafeTransport does not enforce TLS 1.2 minimum")
	}
}

func TestNewInsecureTransportStillPinsAddresses(t *testing.T) {
	resolver := staticResolver{"private.example": {netip.MustParseAddr("10.0.0.7")}}
	dialer := &recordingDialer{err: errors.New("stop after recording")}
	transport := newInsecureTransport(resolver, dialer)

	if _, err := transport.DialContext(context.Background(), "tcp", "private.example:80"); err == nil {
		t.Fatal("DialContext succeeded, want recording dialer error")
	}
	if dialer.address != "10.0.0.7:80" {
		t.Fatalf("dialed %q, want pinned private address", dialer.address)
	}
}

func TestNewSafeClientWiresSafeTransportAndRedirectGuard(t *testing.T) {
	client := NewSafeClient()
	if client == nil {
		t.Fatal("NewSafeClient returned nil")
	}
	if client.Timeout != 0 {
		t.Fatalf("client body timeout = %s, want no whole-response timeout", client.Timeout)
	}
	if client.CheckRedirect == nil {
		t.Fatal("NewSafeClient has no redirect guard")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatal("NewSafeClient does not use the pinned-dial transport")
	}
	if transport.Proxy != nil {
		t.Fatal("NewSafeClient transport uses an HTTP proxy")
	}
}

func TestCheckRedirectRejectsDowngradeAndExcessiveRedirects(t *testing.T) {
	previous := &http.Request{URL: mustURL(t, "https://1.1.1.1/start")}
	next := &http.Request{URL: mustURL(t, "http://1.1.1.1/next")}
	if err := checkRedirect(next, []*http.Request{previous}); err == nil {
		t.Fatal("HTTPS downgrade redirect was accepted")
	}
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = previous
	}
	if err := checkRedirect(previous, via); err == nil {
		t.Fatal("excessive redirects were accepted")
	}
	private := &http.Request{URL: mustURL(t, "https://127.0.0.1/private")}
	if err := checkRedirect(private, []*http.Request{previous}); err == nil {
		t.Fatal("redirect to a private address was accepted")
	}
}

func TestCheckRedirectAllowNonPublicAllowsPrivate(t *testing.T) {
	previous := &http.Request{URL: mustURL(t, "https://10.0.0.7/start")}
	private := &http.Request{URL: mustURL(t, "https://127.0.0.1/private")}
	if err := checkRedirectAllowNonPublic(private, []*http.Request{previous}); err != nil {
		t.Fatalf("insecure redirect to private address rejected: %v", err)
	}
	downgrade := &http.Request{URL: mustURL(t, "http://10.0.0.7/next")}
	if err := checkRedirectAllowNonPublic(downgrade, []*http.Request{previous}); err == nil {
		t.Fatal("insecure path accepted an HTTPS downgrade")
	}
}

type contextDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f contextDialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestRedactURLRemovesCredentialsPathQueryAndFragment(t *testing.T) {
	raw := "https://user:pass@provider.example/stremio/secret-token/manifest.json?api_key=secret#fragment"
	got := RedactURL(raw)
	for _, secret := range []string{"user", "pass", "secret-token", "api_key", "secret", "fragment"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RedactURL leaked %q in %q", secret, got)
		}
	}
	if got != "https://provider.example/%3Credacted%3E" {
		t.Fatalf("RedactURL = %q", got)
	}
}

type mutableResolver struct {
	addresses []netip.Addr
	err       error
	calls     int
}

func (r *mutableResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return append([]netip.Addr(nil), r.addresses...), nil
}

func TestValidateURLWithCacheReusesFreshResolution(t *testing.T) {
	resolver := &mutableResolver{addresses: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}
	cache := newResolutionCache(time.Minute, 8)
	const raw = "https://provider.example/stream?token=x"
	for call := 0; call < 2; call++ {
		got, err := validateURLWithCache(context.Background(), raw, resolver, cache)
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if got.String() != raw {
			t.Fatalf("call %d: validated URL = %q, want %q", call, got.String(), raw)
		}
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1: the second validation must be served from cache", resolver.calls)
	}
}

func TestValidateURLWithCacheDoesNotCacheFailures(t *testing.T) {
	resolver := &mutableResolver{err: errors.New("lookup failed")}
	cache := newResolutionCache(time.Minute, 8)
	for call := 0; call < 2; call++ {
		if _, err := validateURLWithCache(context.Background(), "https://provider.example/stream", resolver, cache); err == nil {
			t.Fatalf("call %d succeeded, want a resolution failure", call)
		}
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2: a failed resolution must not be cached", resolver.calls)
	}
	if cache.len() != 0 {
		t.Fatalf("cache retained %d entries after failures, want 0", cache.len())
	}
}

func TestValidateURLWithCacheRejectsPrivateAfterExpiry(t *testing.T) {
	resolver := &mutableResolver{addresses: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}
	cache := newResolutionCache(time.Minute, 8)
	base := time.Now()
	cache.now = func() time.Time { return base }
	const raw = "https://provider.example/stream"

	if _, err := validateURLWithCache(context.Background(), raw, resolver, cache); err != nil {
		t.Fatalf("initial validation: %v", err)
	}
	// The host rebinds to loopback. The fresh cache still serves the earlier
	// public answer for the TTL, which is the accepted cost of the cache.
	resolver.addresses = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if _, err := validateURLWithCache(context.Background(), raw, resolver, cache); err != nil {
		t.Fatalf("fresh cached answer was rejected: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 while the cache is fresh", resolver.calls)
	}

	// Once the entry expires the rebound private answer must be rejected.
	base = base.Add(2 * time.Minute)
	if _, err := validateURLWithCache(context.Background(), raw, resolver, cache); err == nil {
		t.Fatal("private rebound address was accepted after the cache expired")
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 after expiry", resolver.calls)
	}

	// A bypassed cache (fresh instance) rejects the private answer immediately.
	if _, err := validateURLWithCache(context.Background(), raw, resolver, newResolutionCache(time.Minute, 8)); err == nil {
		t.Fatal("private address was accepted with a bypassed cache")
	}
}

func TestValidateURLWithCacheRejectsForbiddenCachedAddress(t *testing.T) {
	cache := newResolutionCache(time.Minute, 8)
	// A poisoned cache entry must still be checked on read; the rejection rule
	// is never skipped just because an address came from the cache.
	cache.store("provider.example", []netip.Addr{netip.MustParseAddr("10.0.0.7")})
	resolver := &mutableResolver{addresses: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}
	if _, err := validateURLWithCache(context.Background(), "https://provider.example/stream", resolver, cache); err == nil {
		t.Fatal("a cached non-public address was served")
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 on a cache hit", resolver.calls)
	}
}

func TestResolutionCacheEvictsExpiredAndCapsSize(t *testing.T) {
	cache := newResolutionCache(time.Minute, 4)
	base := time.Now()
	cache.now = func() time.Time { return base }
	cache.store("a.example", []netip.Addr{netip.MustParseAddr("1.1.1.1")})
	cache.store("b.example", []netip.Addr{netip.MustParseAddr("1.0.0.1")})
	if got := cache.len(); got != 2 {
		t.Fatalf("cache len = %d, want 2", got)
	}
	base = base.Add(2 * time.Minute)
	cache.store("c.example", []netip.Addr{netip.MustParseAddr("8.8.8.8")})
	if got := cache.len(); got != 1 {
		t.Fatalf("cache len after expiry sweep = %d, want 1", got)
	}

	capped := newResolutionCache(time.Hour, 2)
	capped.now = func() time.Time { return base }
	capped.store("a.example", []netip.Addr{netip.MustParseAddr("1.1.1.1")})
	capped.store("b.example", []netip.Addr{netip.MustParseAddr("1.0.0.1")})
	capped.store("c.example", []netip.Addr{netip.MustParseAddr("8.8.8.8")})
	if got := capped.len(); got != 2 {
		t.Fatalf("capped cache len = %d, want 2", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
