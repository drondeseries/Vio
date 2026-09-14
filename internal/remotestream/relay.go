package remotestream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	relayEntryLifetime        = 24 * time.Hour
	relayMaxEntries           = 512
	maxPlaylistBytes          = 4 << 20
	maxRewrittenPlaylistBytes = 8 << 20
	maxPlaylistRefs           = 8192
	remoteFirstByteTimeout    = 30 * time.Second
	// remoteBodyIdleTimeout bounds how long a read from the upstream remote
	// body may pause before the relay gives up on it. Set generously enough
	// to tolerate transient Usenet provider latency and Altmount reader timeouts.
	remoteBodyIdleTimeout = 90 * time.Second
	remoteBodyChunkSize   = 256 << 10
	// remoteBodyBufferChunks bounds the per-connection read-ahead between the
	// upstream pump and the client writer. The channel already provides full
	// backpressure (the producer blocks when the client stalls, closing the
	// upstream TCP window), so this buffer only absorbs throughput jitter.
	// 64 chunks ≈ 16 MiB per active stream — enough smoothing without letting
	// many slow clients pin hundreds of megabytes of resident memory.
	remoteBodyBufferChunks = 64
)

// Relay exposes validated remote streams only on loopback. It gives FFmpeg a
// credential-free input URL while retaining Range support and applying the
// same SSRF policy to the initial request and every redirect.
type Relay struct {
	once           sync.Once
	mu             sync.Mutex
	server         *http.Server
	baseURL        string
	startErr       error
	closed         bool
	entries        map[string]*relayEntry
	client         *http.Client
	insecureClient *http.Client
	sealKey        [32]byte
}

type relayEntry struct {
	source    *url.URL
	baseName  string
	createdAt time.Time
	insecure  bool // private/local destinations explicitly allowed by admin
	headers   map[string]string
}

// ProxyError reports whether an upstream failure happened before any response
// bytes were committed. Callers may safely try another provider candidate only
// when Started is false.
type ProxyError struct {
	Started bool
	Err     error
}

func (e *ProxyError) Error() string { return e.Err.Error() }
func (e *ProxyError) Unwrap() error { return e.Err }

func RetryableBeforeResponse(err error) bool {
	var proxyErr *ProxyError
	return errors.As(err, &proxyErr) && !proxyErr.Started
}

// NewRelay creates a lazy loopback relay. The listener is opened on the first
// Register call so installations that never play virtual media consume no
// socket or goroutine.
func NewRelay() *Relay {
	transport := NewSafeTransport()
	relay := &Relay{
		entries: make(map[string]*relayEntry),
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
	}
	if _, err := rand.Read(relay.sealKey[:]); err != nil {
		relay.startErr = errors.New("initialize remote stream relay key")
	}
	return relay
}

func (r *Relay) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.startErr = errors.New("remote stream relay is closed")
		return
	}
	if r.startErr != nil {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.startErr = fmt.Errorf("start remote stream relay: %w", err)
		return
	}
	r.baseURL = "http://" + listener.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/source/", r.handle)
	r.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		_ = r.server.Serve(listener)
	}()
}

// Close revokes every registered source and gracefully stops the loopback
// listener. It is safe to call more than once.
func (r *Relay) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	clear(r.entries)
	server := r.server
	client := r.client
	insecureClient := r.insecureClient
	r.mu.Unlock()

	for _, candidate := range []*http.Client{client, insecureClient} {
		if candidate != nil {
			if transport, ok := candidate.Transport.(interface{ CloseIdleConnections() }); ok {
				transport.CloseIdleConnections()
			}
		}
	}
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

// Register validates source and returns a short-lived loopback URL plus an
// idempotent release function. The provider URL never appears in the returned
// value, transcode recipe, or FFmpeg command line.
func (r *Relay) Register(ctx context.Context, source string) (string, func(), error) {
	return r.register(ctx, source, false, nil)
}

// RegisterInsecure registers a structurally valid source while allowing the
// owning plugin's explicit private-host opt-in. FFmpeg still receives only a
// loopback relay URL; the insecure transport is isolated to this entry.
func (r *Relay) RegisterInsecure(ctx context.Context, source string) (string, func(), error) {
	return r.register(ctx, source, true, nil)
}

// RegisterWithHeaders registers a source with optional upstream request headers.
func (r *Relay) RegisterWithHeaders(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, false, headers)
}

// RegisterInsecureWithHeaders registers an insecure source with optional upstream request headers.
func (r *Relay) RegisterInsecureWithHeaders(ctx context.Context, source string, headers map[string]string) (string, func(), error) {
	return r.register(ctx, source, true, headers)
}

func cloneHeaderMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *Relay) register(ctx context.Context, source string, insecure bool, headers map[string]string) (string, func(), error) {
	if r == nil {
		return "", nil, errors.New("remote stream relay is not configured")
	}
	var sourceURL *url.URL
	var err error
	if insecure {
		sourceURL, err = ValidateURLSyntaxAllowNonPublic(source)
	} else {
		var validated *ValidatedURL
		validated, err = ValidateURL(ctx, source)
		if validated != nil {
			sourceURL = validated.URL()
		}
	}
	if err != nil {
		return "", nil, err
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return "", nil, errors.New("remote stream relay is closed")
	}
	r.once.Do(r.start)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", nil, fmt.Errorf("create remote stream relay token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	baseName := safeRelayBaseName(sourceURL)

	now := time.Now()
	r.mu.Lock()
	if r.startErr != nil {
		r.mu.Unlock()
		return "", nil, r.startErr
	}
	if r.closed {
		r.mu.Unlock()
		return "", nil, errors.New("remote stream relay is closed")
	}
	r.evictLocked(now)
	if len(r.entries) >= relayMaxEntries {
		r.mu.Unlock()
		return "", nil, errors.New("remote stream relay is at capacity")
	}
	r.entries[token] = &relayEntry{
		source: sourceURL, baseName: baseName, createdAt: now, insecure: insecure, headers: cloneHeaderMap(headers),
	}
	baseURL := r.baseURL
	r.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			r.mu.Lock()
			r.deleteEntryLocked(token)
			r.mu.Unlock()
		})
	}
	return baseURL + "/source/" + token + "/" + url.PathEscape(baseName), release, nil
}

func (r *Relay) evictLocked(now time.Time) {
	for token, entry := range r.entries {
		if now.Sub(entry.createdAt) >= relayEntryLifetime {
			r.deleteEntryLocked(token)
		}
	}
}

func (r *Relay) deleteEntryLocked(token string) {
	if _, ok := r.entries[token]; !ok {
		return
	}
	delete(r.entries, token)
}

func (r *Relay) handle(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, "/source/")
	token, suffix, found := strings.Cut(rest, "/")
	if !found || token == "" {
		http.NotFound(w, request)
		return
	}
	r.mu.Lock()
	entry, ok := r.entries[token]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, request)
		return
	}
	decodedSuffix, err := url.PathUnescape(suffix)
	if err != nil {
		http.Error(w, "invalid relay path", http.StatusBadRequest)
		return
	}
	var target *url.URL
	switch {
	case decodedSuffix == entry.baseName:
		target = entry.source
	case strings.HasPrefix(decodedSuffix, "resource/"):
		opaque, requestedName, found := strings.Cut(strings.TrimPrefix(decodedSuffix, "resource/"), "/")
		if !found || opaque == "" || requestedName == "" {
			http.NotFound(w, request)
			return
		}
		rawTarget, err := r.openReference(token, opaque, time.Now())
		if err != nil {
			http.NotFound(w, request)
			return
		}
		target, err = url.Parse(rawTarget)
		if err != nil || safeRelayBaseName(target) != requestedName {
			http.NotFound(w, request)
			return
		}
	default:
		http.NotFound(w, request)
		return
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	var proxyErr error
	if entry.insecure {
		proxyErr = r.proxyWithClient(tracked, request, target.String(), token, r.insecureHTTPClient(), entry.headers)
	} else {
		proxyErr = r.proxyWithClient(tracked, request, target.String(), token, r.client, entry.headers)
	}
	if proxyErr != nil {
		if !tracked.wroteHeader {
			http.Error(w, "remote stream unavailable", http.StatusBadGateway)
			return
		}
		panic(http.ErrAbortHandler)
	}
}

// Proxy streams one validated provider response through Silo. Only media-safe
// request/response headers are forwarded; cookies and authorization headers
// from the Silo request are never sent upstream.
func (r *Relay) Proxy(w http.ResponseWriter, request *http.Request, source string) error {
	return r.ProxyWithHeaders(w, request, source, nil)
}

// ProxyWithHeaders streams one validated provider response with optional upstream request headers.
func (r *Relay) ProxyWithHeaders(w http.ResponseWriter, request *http.Request, source string, headers map[string]string) error {
	if r == nil {
		return errors.New("remote stream relay is not configured")
	}
	validated, err := ValidateURL(request.Context(), source)
	if err != nil {
		return err
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	if err := r.proxyWithClient(tracked, request, validated.String(), "", r.client, headers); err != nil {
		return &ProxyError{Started: tracked.wroteHeader, Err: err}
	}
	return nil
}

// ProxyInsecure is like Proxy but skips the public-address DNS validation.
// Use only when the admin has explicitly enabled allow_insecure_http for a
// plugin — otherwise local and private IP stream URLs are rejected by Proxy.
func (r *Relay) ProxyInsecure(w http.ResponseWriter, request *http.Request, source string) error {
	return r.ProxyInsecureWithHeaders(w, request, source, nil)
}

// ProxyInsecureWithHeaders is like ProxyWithHeaders but allows private/local hosts when opted in.
func (r *Relay) ProxyInsecureWithHeaders(w http.ResponseWriter, request *http.Request, source string, headers map[string]string) error {
	if r == nil {
		return errors.New("remote stream relay is not configured")
	}
	parsed, err := ValidateURLSyntaxAllowNonPublic(source)
	if err != nil {
		return err
	}
	tracked := &relayResponseWriter{ResponseWriter: w}
	if err := r.proxyWithClient(tracked, request, parsed.String(), "", r.insecureHTTPClient(), headers); err != nil {
		return &ProxyError{Started: tracked.wroteHeader, Err: err}
	}
	return nil
}

// insecureHTTPClient lazily builds an HTTP client whose transport and redirect
// handling allow private/local hosts. It is used only by ProxyInsecure, which
// requires an explicit allow_insecure_http opt-in.
func (r *Relay) insecureHTTPClient() *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insecureClient == nil {
		r.insecureClient = &http.Client{
			Transport:     NewInsecureTransport(),
			CheckRedirect: checkRedirectAllowNonPublic,
		}
	}
	return r.insecureClient
}

func (r *Relay) proxyWithClient(w http.ResponseWriter, request *http.Request, source, relayToken string, client *http.Client, extraHeaders map[string]string) error {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return errors.New("remote stream relay supports only GET and HEAD")
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, source, nil)
	if err != nil {
		return errors.New("prepare remote stream request")
	}
	for _, header := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match", "Accept", "User-Agent"} {
		if value := request.Header.Get(header); value != "" {
			upstream.Header.Set(header, value)
		}
	}
	if len(extraHeaders) > 0 {
		for k, v := range extraHeaders {
			kLower := strings.ToLower(k)
			if kLower == "referer" || kLower == "origin" || kLower == "user-agent" {
				upstream.Header.Set(k, v)
			}
		}
	}
	response, err := client.Do(upstream)
	if err != nil {
		return errors.New("remote stream request failed")
	}
	defer func() { _ = response.Body.Close() }()
	// Detect upstream sources that ignore Range headers: when we ask for a
	// byte range but get back 200 OK (full file), strip Accept-Ranges from
	// the response so clients don't assume range support and fail on seek.
	hadRange := upstream.Header.Get("Range") != ""
	if hadRange && response.StatusCode == http.StatusOK && relayToken != "" {
		response.Header.Del("Accept-Ranges")
	}
	if response.StatusCode >= 400 && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		drainCtx, drainCancel := context.WithTimeout(request.Context(), 1*time.Second)
		defer drainCancel()
		stopTimer := context.AfterFunc(drainCtx, func() {
			_ = response.Body.Close()
		})
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		stopTimer()
		return fmt.Errorf("remote stream returned HTTP %d", response.StatusCode)
	}
	if isDASHManifestResponse(response, nil) {
		return errors.New("remote DASH manifests are not supported")
	}
	if request.Method == http.MethodHead || response.StatusCode == http.StatusNotModified ||
		response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusRequestedRangeNotSatisfiable ||
		strings.TrimSpace(response.Header.Get("Content-Length")) == "0" {
		copyRemoteResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		return nil
	}

	// The per-read idle timer is intentionally independent of the request
	// context. Give the pump its own cancellation scope so a timeout or an
	// early response-writing error always releases a worker that is waiting to
	// publish its next chunk, even when Proxy is called with a context whose
	// lifetime extends beyond this request.
	pumpCtx, cancelPump := context.WithCancel(request.Context())
	defer cancelPump()
	bodyChunks := pumpRemoteBody(pumpCtx, response.Body)
	first, err := nextRemoteBodyChunk(request.Context(), bodyChunks, remoteFirstByteTimeout)
	if err != nil {
		return err
	}
	if len(first.data) == 0 {
		return errors.New("remote media stream returned no data")
	}
	if isDASHManifestResponse(response, first.data) {
		return errors.New("remote DASH manifests are not supported")
	}
	if isHLSPlaylistResponse(response) || looksLikeHLSPlaylist(first.data) {
		if response.StatusCode != http.StatusOK {
			return errors.New("remote HLS playlist returned an unsupported partial response")
		}
		if relayToken == "" {
			return errors.New("remote HLS playback requires a registered relay")
		}
		playlist := append([]byte(nil), first.data...)
		for first.err == nil {
			first, err = nextRemoteBodyChunk(request.Context(), bodyChunks, remoteBodyIdleTimeout)
			if err != nil {
				return err
			}
			if len(playlist)+len(first.data) > maxPlaylistBytes {
				return errors.New("remote HLS playlist exceeded size limit")
			}
			playlist = append(playlist, first.data...)
		}
		rewritten, err := r.rewritePlaylist(request.Context(), relayToken, response.Request.URL, playlist)
		if err != nil {
			return err
		}
		for _, header := range []string{"Content-Type", "Cache-Control", "Last-Modified"} {
			if value := response.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(rewritten)
		return err
	}
	copyRemoteResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	if _, err := w.Write(first.data); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	for first.err == nil {
		first, err = nextRemoteBodyChunk(request.Context(), bodyChunks, remoteBodyIdleTimeout)
		if err != nil {
			return err
		}
		if len(first.data) > 0 {
			if _, err := w.Write(first.data); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if errors.Is(first.err, io.EOF) {
		return nil
	}
	return errors.New("read remote media stream")
}

type remoteBodyChunk struct {
	data []byte
	err  error
}

func pumpRemoteBody(ctx context.Context, body io.Reader) <-chan remoteBodyChunk {
	chunks := make(chan remoteBodyChunk, remoteBodyBufferChunks)
	go func() {
		defer close(chunks)
		for {
			buffer := make([]byte, remoteBodyChunkSize)
			n, err := body.Read(buffer)
			chunk := remoteBodyChunk{data: buffer[:n], err: err}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}

func nextRemoteBodyChunk(ctx context.Context, chunks <-chan remoteBodyChunk, timeout time.Duration) (remoteBodyChunk, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return remoteBodyChunk{}, ctx.Err()
	case <-timer.C:
		return remoteBodyChunk{}, errors.New("remote media stream stalled")
	case chunk, ok := <-chunks:
		if !ok {
			return remoteBodyChunk{err: io.EOF}, nil
		}
		if len(chunk.data) == 0 && chunk.err != nil && !errors.Is(chunk.err, io.EOF) {
			return remoteBodyChunk{}, errors.New("read remote media stream")
		}
		return chunk, nil
	}
}

func copyRemoteResponseHeaders(destination, source http.Header) {
	for _, header := range []string{
		"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type",
		"ETag", "Last-Modified", "Cache-Control",
	} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

func looksLikeHLSPlaylist(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
}

func isDASHManifestResponse(response *http.Response, body []byte) bool {
	if response != nil {
		contentType := strings.ToLower(response.Header.Get("Content-Type"))
		if strings.Contains(contentType, "dash+xml") ||
			(response.Request != nil && response.Request.URL != nil && strings.HasSuffix(strings.ToLower(response.Request.URL.Path), ".mpd")) {
			return true
		}
	}
	probe := strings.ToLower(string(bytes.TrimSpace(body)))
	if len(probe) > 4096 {
		probe = probe[:4096]
	}
	return strings.Contains(probe, "<mpd") || strings.Contains(probe, "urn:mpeg:dash:schema:mpd")
}

func isHLSPlaylistResponse(response *http.Response) bool {
	if response == nil || response.Request == nil || response.Request.URL == nil {
		return false
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "mpegurl") || strings.Contains(contentType, "m3u8") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(response.Request.URL.Path), ".m3u8")
}

func (r *Relay) rewritePlaylist(ctx context.Context, parentToken string, base *url.URL, body []byte) ([]byte, error) {
	if base == nil {
		return nil, errors.New("remote HLS playlist has no base URL")
	}
	allowNonPublic := false
	if parentToken != "" {
		r.mu.Lock()
		if entry, ok := r.entries[parentToken]; ok {
			allowNonPublic = entry.insecure
		}
		r.mu.Unlock()
	}
	newline := "\n"
	if strings.Contains(string(body), "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	referenceCount := 0
	rewrittenSize := len(body)
	for index, line := range lines {
		originalLength := len(line)
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			rewritten, err := r.rewritePlaylistAttributes(ctx, parentToken, base, line, &referenceCount, allowNonPublic)
			if err != nil {
				return nil, err
			}
			lines[index] = rewritten
		default:
			rewritten, err := r.relayPlaylistReference(ctx, parentToken, base, trimmed, &referenceCount, allowNonPublic)
			if err != nil {
				return nil, err
			}
			lines[index] = strings.Replace(line, trimmed, rewritten, 1)
		}
		rewrittenSize += len(lines[index]) - originalLength
		if rewrittenSize > maxRewrittenPlaylistBytes {
			return nil, errors.New("rewritten remote HLS playlist exceeded size limit")
		}
	}
	rewritten := []byte(strings.Join(lines, newline))
	if len(rewritten) > maxRewrittenPlaylistBytes {
		return nil, errors.New("rewritten remote HLS playlist exceeded size limit")
	}
	return rewritten, nil
}

func (r *Relay) rewritePlaylistAttributes(ctx context.Context, parentToken string, base *url.URL, line string, referenceCount *int, allowNonPublic bool) (string, error) {
	const marker = `URI="`
	offset := 0
	for {
		start := strings.Index(line[offset:], marker)
		if start < 0 {
			return line, nil
		}
		start += offset + len(marker)
		end := strings.IndexByte(line[start:], '"')
		if end < 0 {
			return "", errors.New("remote HLS playlist contains a malformed URI attribute")
		}
		end += start
		rewritten, err := r.relayPlaylistReference(ctx, parentToken, base, line[start:end], referenceCount, allowNonPublic)
		if err != nil {
			return "", err
		}
		line = line[:start] + rewritten + line[end:]
		offset = start + len(rewritten) + 1
	}
}

func (r *Relay) relayPlaylistReference(ctx context.Context, parentToken string, base *url.URL, reference string, referenceCount *int, allowNonPublic bool) (string, error) {
	if *referenceCount >= maxPlaylistRefs {
		return "", errors.New("remote HLS playlist contains too many references")
	}
	(*referenceCount)++
	parsed, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", errors.New("remote HLS playlist contains an invalid URI")
	}
	target := base.ResolveReference(parsed)
	target.Fragment = ""
	validated, err := validateURLSyntax(target.String(), allowNonPublic)
	if err != nil {
		return "", errors.New("remote HLS playlist contains an unsafe URI")
	}
	opaque, err := r.sealReference(parentToken, validated.String(), time.Now())
	if err != nil {
		return "", err
	}
	baseName := safeRelayBaseName(validated)
	return "/source/" + parentToken + "/resource/" + opaque + "/" + url.PathEscape(baseName), nil
}

func safeRelayBaseName(source *url.URL) string {
	if source == nil {
		return "stream"
	}
	extension := strings.ToLower(path.Ext(source.Path))
	if len(extension) < 2 || len(extension) > 10 {
		return "stream"
	}
	for _, char := range extension[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return "stream"
		}
	}
	switch extension {
	case ".m3u8", ".mp4", ".m4v", ".mkv", ".webm", ".ts", ".m2ts",
		".avi", ".mov", ".flv", ".wmv", ".mp3", ".m4a", ".aac", ".ac3",
		".eac3", ".flac", ".ogg", ".opus", ".wav", ".bin", ".key":
		return "stream" + extension
	default:
		return "stream"
	}
}

func (r *Relay) sealReference(parentToken, source string, now time.Time) (string, error) {
	block, err := aes.NewCipher(r.sealKey[:])
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("create remote HLS reference token")
	}
	plain := make([]byte, 8+len(source))
	binary.BigEndian.PutUint64(plain[:8], uint64(now.Add(relayEntryLifetime).Unix()))
	copy(plain[8:], source)
	sealed := aead.Seal(nil, nonce, plain, []byte(parentToken))
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (r *Relay) openReference(parentToken, opaque string, now time.Time) (string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(opaque)
	if err != nil {
		return "", errors.New("invalid remote HLS reference token")
	}
	block, err := aes.NewCipher(r.sealKey[:])
	if err != nil {
		return "", errors.New("initialize remote HLS reference encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(payload) < aead.NonceSize() {
		return "", errors.New("invalid remote HLS reference token")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte(parentToken))
	if err != nil || len(plain) < 9 {
		return "", errors.New("invalid remote HLS reference token")
	}
	expiresAt := int64(binary.BigEndian.Uint64(plain[:8]))
	if now.Unix() >= expiresAt {
		return "", errors.New("expired remote HLS reference token")
	}
	return string(plain[8:]), nil
}

type relayResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *relayResponseWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *relayResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *relayResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *relayResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
