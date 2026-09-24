# Catalog API

> **API lifecycle:** this documents the stable `/api/v2` native contract, which locks with Silo
> 1.0. The frozen alpha `/api/v1` surface answers the same features through the pre-1.0 bridge
> window and is then retired. See [the native API contract](architecture/api-contract.md).

## Version liveness

`FileVersion.available` reports the durable per-version health signal on the
item detail and item-versions responses: a virtual candidate is available when
it has not been stamped failed (`failed_at` is NULL), a local file when it is
not marked missing (`missing_since` is NULL). The field is omitted when the
version is available, so an absent `available` means available/unknown; `false`
means the version is currently unavailable. `FileVersion.failed` remains the
virtual-only "produced no bytes at stream-open" flag.

On the playback side, the `/api/v2` playback plan projection carries
`effective_virtual_uri` alongside `virtual_source_revision`, so a client can
detect a substituted virtual candidate and adopt it in its version menu.

`POST /api/v1/catalog/versions/check` batch-tests a set of media file IDs and
stamps the durable signal. The request is:

```json
{
  "file_ids": [123, 456]
}
```

At most 40 IDs are accepted per request (413 `too_large` beyond that; 400
`bad_request` when the body is missing or the list is empty). Each file is
tested cheaply — virtual rows resolve their pinned `?result=` candidate through
the provider (no media transfer), local rows are read from `missing_since` with
no probe — with bounded concurrency and a per-file timeout. A confirmed dead
pin stamps `failed_at`; a successful resolution clears it. Ambiguous provider
errors (provider down, timeout) leave the stamp unchanged and report the row's
current computed availability, so a provider outage cannot mass-tag versions.

**Response** (200 OK):

```json
{
  "results": [
    { "file_id": 123, "available": true },
    { "file_id": 456, "available": false }
  ]
}
```

Unknown or deleted file IDs are reported as `available: false`.

## Multi-audio language support and release metadata

MULTi/DUAL releases — a single audio stream tagged `und`/`mul`/empty whose
languages live in the track title (e.g. "English / French / Spanish") — now
carry real language identity instead of being collapsed to a single code.

- **`FileVersion.audio_tracks[].languages`** — the full advertised language
  list for a MULTi/DUAL track, parsed from the track title at probe time when
  the container language tag is absent, undetermined, or multiple. `language`
  keeps the primary code (the first concrete one when the tag was
  undetermined). Rows probed before this support are backfilled at read time
  from the embedded title until the probe-version re-probe catches up.
- **`FileVersion.audio_tracks[].index`** — the **absolute container stream
  index** as ffprobe reported it (video is typically stream 0, the first
  audio stream 1, subtitles interleaved between them). It is NOT the
  audio-only FFmpeg ordinal used by `0:a:N` map specifiers, and NOT the
  track's position in the `audio_tracks` array. Track list order can differ
  from container order on MULTi releases; clients that need to select a
  track for ffmpeg must convert this index to the audio-only ordinal
  (the rank of the selected track among tracks ordered by absolute index —
  see `playback.AudioStreamOrdinal` server-side) rather than sending this
  value directly.
- **`FileVersion.release_name`** / **`FileVersion.release_group`** — the file
  stem (basename without extension) and the trailing group tag on
  release-style names (`Movie.2023.2160p.AltMount` → group `AltMount`), so
  clients can show which release a version is. `release_group` is empty when no
  group tag is present.

`GET /api/v1/catalog/filters` reports the language facets on
`audio_languages` and `subtitle_languages` (alongside `resolutions`) when
`include_technical` is true (the default). A MULTi track satisfies the
audio-language browse filter for any of its `languages[]` codes, not just its
primary `language`.
## People search

`GET /api/v2/catalog/people` (`listPeople`) accepts a name fragment in `q` and
`limit` from 1 to 100 (default 20). Case-insensitive exact name matches come first;
other matches sort by name, with person ID breaking ties. Ranking happens before
applying the limit.

The optional `media_scope` parameter limits results to people credited on items
in that scope. It accepts `video` (movies and series), `movie`, `series`, `episode`,
`audiobook`, `ebook`, or `manga`. Omit it to search credits across all media scopes.
Results require at least one credit visible to the viewer. Library restrictions, disabled libraries, rating
limits, and excluded media types apply before the limit, including when the media
scope is omitted. Every credit role participates, so directors match video searches
and authors and narrators match audiobook searches. A person with several matching
credits appears once.

Episode credits inherit library visibility and rating limits from their parent
series. Media scope and excluded media types still apply to the credited episode.

`GET /api/v2/catalog/search/capabilities` advertises `people_media_scope: true`
when people search supports media scopes and viewer access filtering. Clients
must check this signal before offering people results, including unscoped
searches. Web omits the People row on older servers that do not advertise support.

Web search applies its selected media scope to both titles and people. People
responses remain an `{items}` collection with string IDs. The v1 bridge retains
its existing alphabetical, unscoped search.

## Person detail

`GET /api/v2/catalog/people/{id}` (`getPerson`) returns one person. A read counts
as a view: when the person's metadata is incomplete or stale and no provider lookup
ran recently, the server queues a background refresh.

Clients that warm a cache speculatively, such as web prefetching the cast of an
open item, pass `prefetch=true`. A prefetch returns the same person but does not
queue a refresh; missing metadata is left to the server's background sweep. Read
the person without `prefetch` when the user actually opens them.

`GET /api/v2/catalog/search/capabilities` advertises `person_prefetch: true` when
the server accepts the parameter. Check it first: `/api/v2` rejects unknown query
parameters, so an older server answers a prefetch read with `422`.

## Saved browse sort

`PUT /api/v2/collections/sort-preference` (`setCollectionSortPreference`) saves the
acting profile's sort for a library collection, user collection, Watchlist, or
Favorites. The profile is identified by the `X-Profile-Id` header. The request body
is a `CollectionSortPreference`:

```json
{
  "collection_kind": "watchlist",
  "field": "added_at",
  "order": "desc"
}
```

A successful save returns `200` with the stored `CollectionSortPreference`.

`collection_kind` accepts `library`, `user`, `watchlist`, or `favorites`.
`collection_id` is a string and is required for collection kinds; it is omitted or
ignored for Watchlist and Favorites. Saved personal-list preferences accept
non-personalized sort fields; `added_at` means the date the item was added to the
list. Personalized sorts (`progress`, `date_viewed`, and `plays`) are rejected for
both saved preferences and Favorites/Watchlist browse. History accepts
`date_viewed` with an active profile, but rejects mutable `progress` and `plays`
sorts. An empty `field` pins the profile to list source order.

`DELETE /api/v2/collections/sort-preference?collection_kind=watchlist`
(`clearCollectionSortPreference`) removes the saved preference and returns `204`.
Collection kinds also require `collection_id` on DELETE.

When a catalog request has no explicit sort, its saved preference is applied
before the source default. `GET /api/v2/catalog` reports an applied saved/default
sort as `effective_sort`; source order omits that field. `effective_sort` is
reported the same way for `group=work` requests, and `sort_metrics` on each item
describes the effective sort rather than the (possibly empty) requested one.

## Feature detection

`GET /api/v2/collections/capabilities` (`getCollectionCapabilities`) returns a
`CollectionCapabilities` document whose `sort_preference_kinds` lists the
`collection_kind` values this server accepts, and `admin_item_materialize`:

```json
{
  "sort_preference_kinds": ["library", "user", "watchlist", "favorites"],
  "admin_item_materialize": true
}
```

Check it before saving a Watchlist or Favorites preference. The
`collection_sort_preferences` boolean reports only that saved preferences exist at
all, so it cannot be used to detect the personal-list kinds. When
`sort_preference_kinds` is absent, assume `library` and `user` only.
`admin_item_materialize` indicates support for
`POST /api/v2/admin/collections/{id}/materialize/{item_id}` to repair or
materialize virtual placeholder files for a collection item. The document supports
`If-None-Match` and returns `304` when the caller's copy is current.

## Library-scoped version lists

`library_id` on `getCatalogItem`, `listCatalogItemVersions`, `listCatalogItemEpisodes`,
`listSeriesSeasons`, `getSeriesSeason`, and `listSeasonEpisodes` names the library the
viewer opened the item from. It always validates that the item is a member of that
library and picks the library's metadata language as the presentation fallback. It
does not, by default, change which files come back: an item stored in several
libraries lists every version the viewer may access, so a "Movies" and "Movies 4K"
split still shows both files from either library.

The administrator setting `catalog.scope_versions_to_library` (default `false`)
makes those reads answer only the versions stored in the named library. A read
without `library_id` is unaffected, and so is `getWatchDetail`, watch-together
selection, and the Jellyfin compatibility surface: an item always plays from its
full accessible version list. No client change is needed: the setting only
changes what an existing `library_id` request returns.

## Section quality badges

Home and library section cards derive `overlay_summary` from the best accessible,
non-missing media file: resolution first, then dynamic range. A series includes
all its eligible episode files even when an episode also appears on the page.
Library restrictions and the playback quality ceiling apply before selecting the
file, so a restricted profile's badge describes a file that profile can access.

Each section response reads current committed file metadata. Badge summaries have
no result cache: a subsequent request sees file updates, removals, and library
moves. Clients must fetch again to update their existing cards.

Recently-added section membership is shared only within the same library and
access scope. Scan-complete events are coalesced into invalidations at most once
per 30 seconds; invalidation requests a refresh on the next read. While
one background rebuild runs, readers may use the previous membership for at most
30 seconds from the first read after invalidation, capped by its original expiry.
An idle scope retains that original expiry until a reader requests the refresh. Repeated scans and failed refreshes
cannot extend that deadline. Cold or expired membership requires a fresh build;
an older in-flight build cannot replace the current generation. Badge summaries
and per-profile playability are recomputed during this grace period.

## Bridge note

The alpha `/api/v1` surface exposes the same saved-sort and capability features at
`/api/v1/collections/sort-preference`, `/api/v1/collections/capabilities`, and
`/api/v1/catalog`, with numeric `collection_id` values instead of strings. Those
paths are frozen: no feature work lands on them, and Silo 1.0 answers the whole
`/api/v1` namespace with `410 Gone` and the `client_upgrade_required` problem code.
Build against `/api/v2`.

## Personal-list pagination

`GET /api/v2/favorites` and `GET /api/v2/watchlist` use opaque cursors over descending
`added_at`, then descending item ID. The cursor retains the database timestamp's full precision;
clients must send it unchanged rather than construct it from visible timestamps. PostgreSQL orders
by the stored timestamp column so the existing profile/time indexes can serve the page. Visible
`added_at` fields remain UTC timestamps with millisecond precision. The frozen v1 list queries and their timestamp
formatting are unchanged.

## Catalog query windows

`POST /api/v2/catalog/query` is the structured-body form of `GET /api/v2/catalog`.
It accepts the browse source identifiers, `q`, `name_prefix`, `type`, rule
`groups` and `match`, `sort`/`order`, a page `limit` up to 100 (GET allows 200), and an optional
`query_limit` for the complete result traversal. GET accepts the same rule groups
as a JSON array in `groups` and expresses descending sort as `sort=-field`.
Unknown rule fields and unsupported operators return `422`.

Both operations return shared catalog cards, `page.next_cursor`, `page.has_more`,
`total`, `total_exact`, and `window_cursor`. Send `next_cursor` unchanged for the
next page. A virtualized client can retain `window_cursor` and send it with
`seek`, a zero-based result position, to request a distant window or return to
position zero. A seek locates one SQL ordering boundary; it can scan the sorted
prefix and does not have constant cost. The complete browse request has a
10-second deadline and honors client cancellation. Keep only visible and
overscan pages active, and cancel requests when the query changes.

Cursors are bound to the operation, viewer/access policy, filters, page size,
query cap, and requested sort. Changing these inputs starts a new traversal.
`skip_total` may change between windows without invalidating the cursor. The
cursor retains a resolved saved sort so later pages do not reread a changed
preference. A nonexact total is an estimate or lower bound, not a verified final result count.

SQL query continuation retains the complete typed ordering tuple, including the
unique item identity and explicit null ordering. Page rows and an optional count
share one PostgreSQL snapshot. Later pages read live data: an insertion cutoff
excludes newer catalog arrivals where supported, but does not freeze titles,
ratings, progress, visibility, or other mutable sort/filter values. Clients must
not treat a cursor as a frozen catalog export.

Collection-source cursors additionally retain the selected collection's durable
revision. Authoritative revision reads bracket parent/access resolution and page
construction; a committed definition, membership, or order change invalidates the
result. A changed collection returns `400` `invalid_cursor`; restart the query.
These checks do not invalidate a collection when unrelated catalog data changes.

PostgreSQL-dependent viewer predicates require the selected user-store provider
to expose its authoritative SQL state. Unsupported SQLite query combinations
return `501` `capability_unsupported`, rather than silently reading unrelated
PostgreSQL viewer rows. SQLite manual collection source-order paging remains
supported; arbitrary manual sorting, nonzero manual seeks, and personalized SQL
filters are unsupported during storage consolidation.

Recent-TV continuation compares the final event timestamp, target type, target
identity, and event identity after event grouping. Recently-added, released, and
random sections retain their source ordering; random sections retain a seed in
the cursor. Audiobook author/narrator/series groups compare their normalized group
identity after any count or duration sort. Work grouping chooses the first
accessible ebook/audiobook edition under the complete source order before applying
the group cursor. A query cap limits source editions before grouping.

### Search continuation

Text searches with a nonempty `q` and the default `query` source accept explicit
`relevance` sorting, including structured requests with rule groups. Other
sources and saved collection definitions reject `relevance`; it describes a
text query's ranking rather than a persistent collection order.

`GET /api/v2/catalog/search/capabilities` reports the selected provider and, for
Meilisearch, `result_window_limit`, `session_ttl_seconds`, and
`max_sessions_per_account`. Catalog query bodies default to 50 results per page. Search
responses also expose the applicable window limit and fixed session expiry in
`search_diagnostics`.

PostgreSQL search retains the complete relevance tuple or requested SQL sort
rather than a numeric page. It selects the FTS or bounded fuzzy retrieval family
on the first page and retains that choice. The existing fuzzy candidate cap and
reranking remain in force. A fuzzy-family query that becomes a richer FTS query
returns `invalid_cursor` so the client can restart. PostgreSQL search retains its
three-second deadline inside the overall browse deadline.

Meilisearch captures the configured ranked result window in one provider response,
then stores its filtered, ordered candidate IDs in shared Redis for 15 minutes.
The configured window must be between 1 and 1,000 candidates; a larger runtime
index setting returns `capability_unsupported` before serving a partial ranking.
This is the provider's reachable window, not an exact global match count.
At most 16 ranking sessions are retained per account; starting another discards
the oldest retained session. Session requests do not extend expiry.

The retained ranking binds the query, provider configuration, account/profile,
and access scope. Pages reauthorize each candidate and advance past deleted or
inaccessible IDs. Explicit window seeks count visible rows within this bounded
ranking. Metadata and access remain live; the retained IDs and their order are
immutable. Fallback may select PostgreSQL before the first page, but a continuation
never switches providers. Expiry or Redis eviction returns `invalid_cursor`;
Redis failure returns `dependency_unavailable`. Restarting performs a new search.

## Admin item materialization

`POST /api/v2/admin/collections/{id}/materialize/{item_id}`

Requires administrator authentication. Idempotently establishes or repairs
virtual playback files, profile variants, and released episodes for a
collection item.

`files_created` and `files_existing` count distinct virtual file identities
across the base item and released episode variants for this operation. A file
identity is the `(owner installation, target library, virtual URI)` tuple.
`episodes_materialized` counts distinct released episodes represented by the
result, not the number of provider/profile files. Repeating the request reports
the same episode count and moves files from `files_created` to
`files_existing`.

**Response** (200 OK):

```json
{
  "success": true,
  "content_id": "movie-tmdb-12345",
  "media_type": "movie",
  "files_created": 1,
  "files_existing": 0,
  "episodes_materialized": 0,
  "message": "Materialized 1 virtual files (0 existing) for movie-tmdb-12345 (movie)"
}
```

**Status Codes**:
- `200 OK`: Item was successfully materialized or verified (idempotent).
- `400 Bad Request`: Ineligible media type, incompatible target library, or virtual playback is disabled on the collection.
- `401 Unauthorized`: Missing or invalid authentication token.
- `403 Forbidden`: Authenticated user is not an administrator.
- `404 Not Found`: Collection or item not found, or item is not a member of the collection.
- `503 Service Unavailable`: Upstream virtual provider plugin is unavailable or misconfigured.

**Sync staging**: during collection sync, newly prepared files stay hidden
until membership acceptance commits. Removing a collection member only deletes
catalog rows proven to be collection-created virtual state; ordinary
metadata-only entries survive as catalog rows.

## History ordering

History defaults to chronological watch-event order. Explicit `date_viewed`
sorting uses each displayed item's latest visible history event, including
episode events collapsed into their parent series; it does not require a
completed watch. `order=asc` puts the oldest latest watch first, and `desc`
puts the newest first. Library/media-scope/search overlays retain this order
before pagination. History does not currently support saved sort preferences.

## Season-list artwork

`GET /api/v2/images/capabilities` advertises
`"season_list_artwork_param": "include_artwork"`. On
`GET /api/v2/catalog/series/{id}/seasons`, this optional boolean defaults to
`true`: omitted and `true` retain the usual artwork. `false` skips poster
preparation and omits `poster_url` and `poster_thumbhash` from each season,
while preserving metadata, viewer rollups, play targets and the `items` envelope.
Invalid booleans return `422 validation_failed`. The parameter does not apply
to single-season or episode operations. Clients can use the capability to
select text-only season lists; callers that omit it keep their existing behavior.

## Collection membership titles

`GET /api/v2/collections/{id}/items` and
`GET /api/v2/admin/collections/{id}/items` include an optional `title` on each
membership row when its catalog title is available. Editors can display that
title while retaining `media_item_id` for mutations and ordering. Clients should
fall back to the ID when the title is absent. Personal membership pages hydrate
titles through the existing viewer access filter; admin pages require acting
administrator access. Membership identity, ordering and cursor revision checks
are unchanged. Frozen v1 membership responses do not expose this field.

## Collection virtual playback default

Collection creation requests accept a `virtual_playback` boolean: the TMDB,
Trakt, and MDBList import endpoints and the template-bundle apply endpoint. It
controls whether items matched outside the selected libraries are kept as
zero-storage virtual entries that Silo Virtual Library resolves at playback.

The `/api/v2` import and template-apply request bodies expose the same optional
`virtual_playback` boolean; omitting it defaults to on, and `false` opts out.

`virtual_playback` defaults to on when the field is omitted, so third-party
clients and creates-from-template get the same behavior as the first-party
admin UI. An explicit `false` disables it and limits the collection to items
present in the selected libraries; an explicit `true` keeps the default
behavior. The default does not change the stored field's semantics: an enabled
collection stores `virtual_playback: true`, and a disabled one omits the key,
which every reader treats as false.
