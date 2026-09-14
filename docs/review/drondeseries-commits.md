# drondeseries Commit Inventory (139 commits, grouped by feature)

Read-only grouping of all commits authored by `drondeseries` on this branch, organized by feature area. No history was rewritten.

## Virtual Playback Engine
- `cc369991` feat(playback): add host-managed virtual media playback and plugin support
- `a85ff860` feat(playback): plan virtual sources like local media
- `220d7030` fix(playback): use virtual scheme for playback
- `cf4a9d8f` fix(playback): route virtual sources through capability planner
- `e2bb7f2a` fix(playback): expand virtual results for mobile clients
- `68b1313a` fix(playback): plan legacy virtual streams for browser audio
- `82cc53fc` fix(playback): rank virtual versions by quality
- `ddfe3296` fix(playback): preserve profile stream selection mode
- `8047358e` fix(virtual): harden playback and provider fallback
- `96d3c272` fix(catalog,playback): virtual media ID collision, fallback guard, transcode deadline, admin context

## Virtual Stream Resolution & Probing
- `155bba20` feat(playback): load virtual stream alternatives on play
- `ec9dd303` feat(playback): load virtual versions before play response
- `24ae2024` feat(playback): populate virtual stream picker after play
- `a1501016` fix(playback): derive virtual ffprobe path safely
- `29afd661` fix(playback): use candidate metadata when ffprobe fails
- `a3c6bbe1` fix(playback): widen the virtual source probe budget for slow remote streams
- `3368abc2` perf(playback): skip ffprobe for HLS and tighter probe budget for known codecs
- `334db94e` perf(playback): skip ffprobe for mp4/mkv/webm/ts when codecs known
- `8bc87e30` fix(playback): never skip ffprobe for Dolby Vision virtual streams
- `d935d118` perf(playback): tighten probe budgets and expand skip-probe criteria for faster virtual playback
- `010ba446` feat(playback): virtual→local parity — fill codec, resolution, HDR, channels, video tracks

## Virtual Playback Performance
- `bf023f91` perf(playback): resolve all virtual candidates in parallel before probing
- `91dadf4c` perf(playback): lazy background resolve — one provider hit in common case
- `101472a9` perf(playback): winning-candidate cache, fallback persistence, HTTP/2 pooling, range detection
- `23c89bb0` perf(playback): resolve-once memo, owner-scoped cache key, lifecycle cache clear, subtitle dedup
- `6df2f503` fix(playback): reduce virtual stream cache TTL to 1 minute
- `86f33214` perf(virtual): coalesce and cache stream listings
- `390363e9` fix(virtual): cap stream listings at fifty
- `f7b9fee5` fix(virtual): bound proxy resolver responses
- `e882852a` fix(virtual): bound profile response payloads

## Pre-Warm & Detail-to-Playback
- `f06a6504` feat(playback): add virtual playback pre-warm for faster detail-to-playback transitions
- `9fa834b4` feat(admin): expose virtual playback pre-warm toggle in Playback settings

## Loopback Relay Removal
- `0f1c5630` fix(playback): feed virtual-provider URL directly to ffmpeg instead of the loopback relay
- `18bf52f3` fix(playback): remove loopback relay from all server-side ffmpeg input paths
- `ae9a254e` fix(playback): fall back to a provider-neutral candidate when a persisted result= is stale
- `9f840434` debug: log virtual stale fallback resolution path
- `52be0603` chore(playback): remove dead TrySoftwareFallback scaffolding
- `47dba209` Merge pull request #25 from drondeseries/fix/relay-removal-complete

## Subtitles & Audio Tracks
- `45e6c744` feat(playback): auto-subtitle-search trigger for virtual streams
- `4971161d` feat(playback): wire VirtualSubtitleSearcher to subtitle Manager in router.go
- `08311a4c` feat(playback): merge virtual candidate audio/subtitle languages into probed tracks
- `7c276c20` feat(playback): persist virtual probed audio/subtitle tracks to DB for watch detail
- `f2fbd647` feat(virtual): persist stream bitrate and track languages
- `ca6b5ae9` fix(playback): preserve episode ownership for virtual results

## Catalog: Variants & Reconciliation
- `ac5a9f3e` feat(catalog): add support for multiple virtual media variants per item
- `81850f60` feat(virtual): persist provider variant metadata
- `57d2f469` feat(virtual): add source-scoped reconciliation
- `20159cfe` feat(catalog): reconcile zero-storage virtual media
- `b14e8f3f` fix(catalog): refuse empty-keep virtual reconciliation when claims exist
- `41bd0c1c` fix(catalog): protect collection-linked virtual files from deletion during reconciliation
- `98048bfe` fix(virtual): remove duplicate variant assignments
- `1e1a7b3f` fix(virtual): bind variant metadata parameters safely
- `7c80e8fe` fix(virtual): avoid duplicate media file columns
- `481f6270` refactor(virtual): remove unused v3 playback path

## Ownership Isolation & Purge
- `a4e3ae16` feat(catalog): isolate virtual files by installation
- `22769fee` fix(catalog): preserve local file upserts after virtual ownership
- `109adada` fix(virtual): bind plugin ownership during catalog upsert
- `06ecac26` fix(virtual): preserve installation ownership in host wiring
- `657b11cb` fix(virtual): preserve installation ownership for JIT results
- `e11f97ce` fix(virtual): protect ownership and provider playback
- `408e209e` fix(catalog): scan virtual media ownership
- `b5937776` fix(scanner): preserve legacy virtual owner zero
- `a5595ce0` fix(scanner): retain virtual file ownership on reads
- `cb83303f` fix(catalog): purge orphaned virtual items with no media_files
- `37ab6a6a` fix(catalog): purge orphaned library memberships for items without files
- `5ca53bb9` fix(catalog): invalidate catalog queries after virtual purge and clean orphaned items
- `e5fde6f8` test(catalog): cover installation-scoped virtual purge
- `63f58018` fix(scanner): exclude virtual files from ListIDsOutsideRoots to prevent scan_libraries from deleting virtual URIs
- `8c23b87c` fix(scanner): skip virtual:// roots entirely during scan
- `d7f2176c` fix(rootcheck): treat virtual:// roots as reachable empty dirs

## Collections & Library Placement
- `a86a25ea` fix(collections): skip unreleased movies during collection sync
- `8c010389` fix(collections): also gate unreleased TV series from collection sync
- `a7b74113` fix(collections): add TMDB Franchise Collection support to admin collection edit modal
- `5a148e07` fix(collections): reconcile virtual library placement
- `eda3974b` fix(collections): allow global virtual placement reconciliation
- `62e4fb59` fix(collections): repair orphaned virtual placements
- `f1d30325` fix(collections): route virtual files by media type
- `989a6e3c` fix(collections): refresh virtual metadata immediately
- `0fe61a09` feat(collections): materialize configured virtual profiles
- `4a001397` feat(collections): expose virtual playback for every source

## Plugin Config, Security & SSRF
- `e498faee` feat(plugins): host typed virtual stream providers
- `607930e9` fix(plugins): respect allow_insecure_http config for private IP stream URLs
- `8f65142a` fix(plugins): check multiple value formats in installationAllowsInsecure
- `b7663afb` fix(plugins): bypass SSRF in relay proxy when allow_insecure_http is enabled
- `7b8fbcca` fix(plugins): use correct RuntimeConfig field names (Key, Value)
- `42e8e356` fix(plugins): lazy-init resolved URL memo map
- `6096548e` fix(plugins): use server-computed checksum instead of binary self-reported value
- `27aac549` fix(plugins): parse array config fields from textareas
- `f157e13e` fix(plugins): accept inline regex flags in profile validation
- `57e40e40` fix(plugins): restore request router connection checks
- `ee71a508` fix(plugins): test request router connections
- `12a196a3` fix(plugins): satisfy frontend regex validator type check

## Plugin Profile Preview
- `7430090c` feat(plugins): preview quality profile configuration
- `667abe47` feat(plugins): validate profile regexes in preview

## Admin & Web UI
- `67811aee` feat: implement virtual media core and admin config UI
- `a162f5bd` feat(admin): configure virtual playback safely
- `7fdd301c` feat(admin): expand virtual library purge controls
- `881d8adc` feat(admin): convert virtual purge library and plugin filters to select dropdowns
- `bd752656` fix(admin): restore clean header design and red Purge Virtual Items button in Danger Zone
- `0ef2d07b` fix(admin): show plugin display name in configure header
- `68753a7f` fix(web): allow play action for virtual media without file versions
- `e3ceb6da` fix(web): pass purge mutation options
- `224c5698` fix(web): use non-empty string for select default options in DatabaseSettings
- `0a5d5a12` fix(web): restore missing collection source types and purge dry-run label refs
- `5bed8777` fix(web): address CI failures
- `313055c5` fix(admin): correct renderWithClient call syntax in PluginConfigForm tests
- `328594d3` feat(catalog): add DELETE /api/v1/admin/items/{id} endpoint and red Delete Show/Movie button to 3-dots media menus
- `673e2e29` fix(ui): distinguish virtual results from More results action
- `ab8375d0` feat(ui): label virtual more-results action
- `73d65589` feat(web): include source hint (Remux, WEB-DL) in version quality badges

## Requests & Metadata Fixes
- `cc5c3cfb` fix(requests): handle fulfillment errors gracefully instead of returning 500
- `f0efb8fd` fix(metadata): queue refresh for virtual items

## CI, Build & Infrastructure
- `243c563b` ci: skip Discord webhook on fork repos
- `f7f98970` ci: lock docker.yml to fork version on upstream merges
- `4d6af8a9` ci(docker): restore GHCR_PAT secret for fork registry auth
- `ee842e5c` ci(docker): add workflow-level packages:write permission
- `4b94b5fa` ci(docker): install libvips before backend verification
- `a80e0ac3` ci: build greenfield virtual playback image
- `c7d5ec7d` build(virtual): use published greenfield SDK

## Style / Formatting
- `dad3d543` style: gofmt
- `f0a6e16c` style: gofmt
- `8eb34597` style: gofmt library_collection_service.go
- `972fed48` style: gofmt internal/api/handlers/items.go
- `3ccffcf3` style: gofmt internal/api/router.go
- `77339eb2` style: format admin UI components to pass CI
- `4b6393a5` style(web): format files with prettier
- `86c763bb` style(web): format DatabaseSettings.tsx
- `c36c7b27` style(web): use font-semibold typography for quality summary badge headers

## Docs & Merges
- `016eed2a` docs(playback): define greenfield virtual architecture
- 12 merge commits syncing `upstream/main` into the `feat/virtual-playback-greenfield` branch

## Audit Notes
- The dominant theme is a single coherent feature: **virtual playback for zero-storage media**, spanning backend playback, catalog ownership, plugin SDK wiring, admin UI, and CI.
- Security-sensitive changes (SSRF handling, `allow_insecure_http`, URL validation) are well-scoped but deserve targeted review — flagged areas: `AllowInsecure` bypass paths and the relay-proxy exemption in `internal/plugins/url_security.go`.
- Performance work is layered (memoization, parallel resolve, probe skip, TTL tuning) — good direction, but cache correctness under ownership changes and fallback staleness is the riskiest cluster (`23c89bb0`, `ae9a254e`, `6df2f503`).
