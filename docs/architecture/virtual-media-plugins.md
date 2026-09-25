# Virtual media plugin integration

Silo supports storage-free catalog entries through an additive RuntimeHost
contract. Plugins submit validated metadata and provider-neutral `virtual://`
URIs; the host
owns persistence, indexing, metadata refresh, authorization, and playback.

## Trust boundary

- Plugins never receive PostgreSQL credentials and never depend on Silo table
  layouts.
- `RuntimeHost.UpsertVirtualMedia` derives the caller's installation identity
  from the authenticated plugin process. It does not accept an installation ID
  from plugin input.
- The host validates media type, destination library, external identity, and
  URI scheme before opening a transaction.
- Registration is idempotent. Stable provider IDs select the canonical item,
  while a URI-level advisory lock prevents duplicate virtual files.
- Search-index events commit in the same transaction. Metadata refresh and
  home-section cache invalidation run through normal Silo services afterward.

## Playback lifecycle

1. A request-router plugin registers a movie or already-aired series episodes.
2. Silo stores virtual `media_files` with `container=virtual`; no placeholder
   files are created.
3. Playback planning recognizes a virtual source without filesystem probing.
4. At playback time Silo resolves the virtual URI through the core virtual
   library (`internal/virtuallibrary`) by path — including rows left over from
   plugin ownership. No virtual traffic dispatches to plugin installations.
5. Direct-compatible sources are proxied through Silo with byte-range support.
   Sources requiring conversion continue through Silo's normal capability-aware
   remux/transcode path. Provider credentials never enter client responses,
   durable plans, or transcode recipes.

The resolver runs at playback time so signed upstream URLs are not persisted.

## Indexer-only releases

A title can exist at the indexers before the provider has downloaded it. The
"Refresh List" flow (`POST /api/v2/media/{media_id}/virtual-candidates:refresh`)
lists the provider candidates, searches Prowlarr for matching usenet releases
the provider does not already list, and persists them in
`virtual_indexer_releases` (one scope per content id, episode id, and media
folder). The watch detail exposes them additively as `indexer_releases`, and a
user can request one on the provider
(`POST /api/v2/media/{media_id}/virtual-releases/{release_id}:request`).

The stored `download_url` is server-internal: the request endpoint resolves the
row by its opaque id, uses the stored URL, and never accepts or returns a URL,
so the surface cannot be turned into an SSRF primitive. `MarkIndexerReleaseQueued`
is the domain identity that makes a repeated request idempotent. The refresh is
a durable `adminjob` job owned by the requesting user; the provider re-list is
the only fatal stage, while the indexer search and candidate probing degrade to
warnings. A completed refresh publishes `catalog.item.changed` with
`change: "versions_updated"` so clients invalidate the version list.

## Administrator setup

In Vio, Stremio virtual streaming is built directly into core (`internal/virtuallibrary`),
configured via `virtual_library.*` server settings (Admin › Settings › Streaming).
The standalone virtual-library plugin (`com.drondeseries.vio-virtual-library`) is
retired: collection syncs, repairs, and variant discovery route to core, and new
virtual rows persist under the core owner identity (installation 0). The plugin
configuration reference below remains for the generic config mechanism only.

`virtual_library.indexer_search_timeout_seconds` (default `20`, range `5`–`900`)
bounds one Prowlarr `/api/v1/search` request. Raise it when Prowlarr aggregates
many slow indexers, or a search fails with a timeout cause. A transport failure
is wrapped so its timeout, DNS, or TLS cause stays visible to the monitor log.

### Choosing a library in the plugin config

A plugin can declare a config property that should be chosen rather than typed
by setting a host-known JSON Schema `format` on it:

- `silo-library` — any enabled library.
- `silo-library-movie` — an enabled movie-capable library (`movie`/`movies`,
  plus `mixed`).
- `silo-library-tv` — an enabled TV-capable library (`series`/`tv`/`show`/
  `tvshows`, plus `mixed`).

The admin draws that property as a dropdown of enabled libraries labelled
`Name (ID)`, and the value written to the field is the library's numeric id as a
string. This is how a virtual-library plugin lets an administrator pick its
movie and series libraries by name instead of typing a database id. The marker
applies only to fields the admin infers from `json_schema`; explicit
`admin_form` fields cannot carry it. If a saved id no longer matches an enabled
library, the form keeps it visible as `"<id> (not found)"` rather than dropping
it.

## Updating from upstream

The feature is intentionally isolated to an additive SDK RPC, the catalog
registrar, RuntimeHost wiring, and narrow playback-source handling. To update:

```sh
git fetch upstream
git rebase upstream/main
go test ./internal/catalog ./internal/pluginhost ./internal/playback ./internal/requests ./internal/sections ./internal/plugins ./internal/api/handlers ./cmd/silo
go vet ./internal/catalog ./internal/pluginhost ./internal/playback ./internal/requests ./internal/sections ./internal/plugins ./internal/api/handlers ./cmd/silo
docker build -t silo-server:virtual-playback .
```

Commands assume the repository root is the cwd. If upstream publishes the
RuntimeHost contract in a newer SDK release, update `go.mod`, remove the local
SDK compatibility replacement, regenerate no code locally, and rerun the same
contract and playback tests.
