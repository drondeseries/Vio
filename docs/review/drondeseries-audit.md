# drondeseries Code Audit — Virtual Playback, Security, and Ownership

Audit of the `drondeseries` contribution set (139 commits grouped in
`docs/review/drondeseries-commits.md`), focused on the highest-risk clusters:
plugin SSRF/security, resolved-URL memoization, virtual ownership isolation and
purge, and the loopback-relay removal.

## Verification run

- `go test ./internal/plugins ./internal/remotestream -count=1` — pass.
- Virtual playback unit tests: 20 cases in `internal/plugins/virtual_playback_test.go` — pass.
- Remote stream URL tests: `internal/remotestream/url_test.go` — pass.
- Catalog virtual media tests (36) require `SILO_TEST_DATABASE_URL` and skip cleanly when unset, per repo convention.

## What is good

- **Defense in depth against SSRF.** `ValidateURL` resolves DNS at validation
  time (`internal/remotestream/url.go:80`), `newSafeTransport` re-validates and
  dials the resolved IP directly to close the DNS-rebinding window
  (`internal/remotestream/client.go:39`), and `checkRedirect` re-validates every
  redirect and rejects HTTPS downgrades (`internal/remotestream/client.go:75`).
  The forbidden-prefix list is comprehensive, including IPv4-mapped IPv6 ranges.
- **`allow_insecure_http` is a per-installation opt-in with fail-closed
  defaults.** `InstallationAllowsInsecure` returns false on nil service, missing
  configs, errors, or invalid installation IDs (`internal/plugins/virtual_playback.go:1211`),
  and tolerates multiple value encodings (`bool`, `"true"` string, `enabled`/`value` keys).
- **Reconciliation cannot delete what it does not own.** Source-scoped advisory
  locks, ownership-scoped file deletes, a guard that refuses an empty keep list
  when claims exist, protection for collection-linked files, and physical media
  staying authoritative (`internal/catalog/virtual_media.go:414`).
- **Server-computed checksums.** Binary SHA-256 is computed from the downloaded
  bytes and compared against the repository checksum; the manifest's self-reported
  value is overwritten (`internal/plugins/installer.go:197`). Real supply-chain hardening.
- **Bounded, lazy memoization.** Resolved-URL memo is 30 s TTL, max 256 entries,
  owner-scoped key, lazily initialized (`internal/plugins/virtual_playback.go:1244`).
  The lazy-init fix (`42e8e356`) closes a nil-map crash on cold start.
- **Relay removal simplifies the hot path.** Provider URLs feed ffmpeg directly
  after validation instead of a loopback relay, removing a whole class of
  relay-lifecycle bugs; dead scaffolding was deleted (`52be0603`).

## Findings

### 1. `allow_insecure_http` could not reach private hosts (root-cause bug — fixed)

Investigation showed the insecure opt-in was a no-op on the direct-play path:
- `ValidateURLSyntax` rejected literal private IPs, so `validateProviderStreamURLSyntax`
  and the candidate-list validation dropped private candidates before they could
  ever be selected.
- `ProxyInsecure` validated the initial URL with the strict syntax check and
  proxied through the safe transport, whose `DialContext` re-applies
  `resolvePublicAddresses` at connect time — private hosts were rejected again
  at dial, and redirects went through strict `checkRedirect`.

Fix (implemented in this branch):
- `remotestream.ValidateURLSyntaxAllowNonPublic` — structural checks only,
  private/loopback/link-local hosts allowed (`internal/remotestream/url.go`).
- `remotestream.NewInsecureTransport` + `newInsecureTransport` — pins the
  resolved IP without the public-address filter (`internal/remotestream/client.go`).
- `checkRedirectAllowNonPublic` — keeps the redirect bound and HTTPS-downgrade
  protection while allowing non-public targets (`internal/remotestream/client.go`).
- `Relay.ProxyInsecure` now uses a lazily-built insecure client
  (`internal/remotestream/relay.go`); the strict `Proxy`/`Register` paths are
  unchanged.
- Candidate listings are structurally screened only; enforcement of the public-
  only rule happens at resolved-URL and fetch time
  (`internal/plugins/virtual_playback.go`, `internal/plugins/url_security.go`).
- The Jellyfin-compat surface had the same half-wired gap: its resolver already
  opts into insecure, but `serveVirtualDirect` always used strict `Proxy`. Added
  `ProxyInsecure` to the compat `RemoteStreamRelay` interface and an
  `AllowInsecureVirtual` callback threaded through `jellycompat.Dependencies` →
  `PlaybackHandler` → `internal/jellycompat/router.go` → `cmd/silo/main.go`
  (`internal/jellycompat/virtual_playback.go`, `internal/jellycompat/server.go`).

Tradeoff: a private-host candidate can now appear in the stream picker without
the opt-in, but playback of it is refused unless `allow_insecure_http` is
enabled for the owning installation. Enforcement is at fetch, which is where it
must be.

### 2. Missing direct test coverage for the insecure path (mostly fixed)

Added tests:
- `TestValidateURLSyntaxAllowNonPublic` — private hosts accepted, structural
  violations still rejected.
- `TestInsecureTransportDialsPrivateAddress` — private IP pinned and dialed.
- `TestCheckRedirectAllowNonPublicAllowsPrivate` — private redirect accepted,
  HTTPS downgrade still rejected.
- `TestProxyRejectsPrivateSourceAndProxyInsecureAttemptsIt` and
  `TestProxyInsecureRejectsStructurallyUnsafeSource`.
- `TestInstallationAllowsInsecure` (value encodings) and
  `TestInstallationAllowsInsecureFailClosed` (nil/error/invalid ID).
- `TestVirtualPlaybackRejectsPrivateCandidateWithoutInsecureOptIn` and
  `TestVirtualPlaybackResolvesPrivateHostWithInsecureOptIn`.
- `TestServeVirtualDirectRoutesInsecureOptInThroughProxyInsecure` (compat relay
  route) and `TestServeVirtualDirectResolvesBoundSourceThroughRelay` (strict route).

Still uncovered: `NewSafeTransport`/`NewSafeClient` themselves (the URL suite
covers the underlying `newSafeTransport` and `checkRedirect`).

The indirect coverage that exists is good (unsafe-candidate rejection,
DNS-deferral, fallback), but the explicit admin-bypass path deserves its own
table test for both `bool` and `string` encodings plus the fail-closed cases.

### 3. Memo sweep runs O(n) under lock on every store

`storeResolvedURL` walks all 256 entries to evict expired keys on every write
(`internal/plugins/virtual_playback.go:1273`). Bounded and correct, but at
steady state the sweep is wasted work; a periodic sweep (e.g., on a timer or
every N writes) would be cheaper. Low priority.

### 4. Multiple TTL constants to reconcile

`resolvedURLMemoTTL` (30 s), the virtual stream cache TTL (1 min,
`6df2f503`), and provider TTL clamping all live separately. Consider one
documented TTL policy per stage (resolution memo < stream cache < provider
rotation) so future tuning has a single source of truth.

### 5. Error strings lack context

`virtual playback provider returned no matching stream`
(`internal/plugins/virtual_playback.go:193`) does not include `virtualPath`,
the selection, or the candidate count. URLs are correctly redacted elsewhere;
adding the virtual path and candidate count would make prod debugging faster
without leaking provider credentials.

## Verdict

The contribution set is coherent, well-tested where it counts, and security
conscious: SSRF is handled with real defense in depth, ownership is
installation-scoped with conservative purge semantics, and the performance work
is layered and bounded. The findings above are refinements, not blockers. The
highest-value follow-ups are the `ProxyInsecure` redirect-behavior documentation
or fix, and direct tests for `InstallationAllowsInsecure`/`ProxyInsecure`.
