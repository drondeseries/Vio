# Fork divergence (Vio)

This repo tracks upstream `Silo-Server/silo-server` with deliberate Vio
divergences. Read this before merging upstream — an upstream change that looks
innocent can silently undo one of these. The merge procedure below plus
`scripts/check-fork-invariants.sh` exist so both humans and AI agents catch
that before it ships.

## Owned divergences

| Area | Divergence | Key files | Pinned by |
| --- | --- | --- | --- |
| Client identity | Web client identifies as `Vio Web` (dual `X-Vio-*` / `X-Silo-*` headers); telemetry labels `vio *` alongside `silo *` | `web/src/api/v2/request.ts`, `web/src/api/client.ts`, `web/src/lib/plexAuth.ts`, `internal/apiv2/observe.go` | `request.test.ts`, `observe_test.go`, invariant script |
| Virtual routing | All virtual-library traffic resolves through core (`internal/virtuallibrary`); no virtual→plugin dispatch anywhere | `internal/api/router.go`, `cmd/silo/main.go`, `internal/catalog/item_repo.go`, `internal/catalog/library_collection_repo.go` | catalog integration tests, `router_virtual_library_core_test.go`, invariant script |
| Request integrations | `installation_id` 0 (core virtual router) validates; virtual-dormant saves answer 422, not 500 | `internal/apiv2/admin_requests.go`, `internal/requests/service.go` | `service_test.go` |
| Collection sync | Missing virtual provider answers 503, not 500 | `internal/apiv2/admin_collections.go` | `admin_collections_test.go` |
| Onboarding | Requests step, TMDB/TVDB auto-install section, format scoring card | `web/src/pages/setup-wizard/` | step tests |
| Collections | `virtual_playback` defaults on in add flows; integration form defaults | `web/src/pages/adminCollectionsShared.tsx`, `web/src/components/CollectionTemplateGallery/`, `web/src/pages/AdminRequests.tsx` | step tests (defaults are UI state) |
| Settings secrets | Indexer keys use `SecretField` (configured indicator + clear) | `web/src/pages/admin-settings/StreamingSettings.tsx` | `StreamingSettings.test.tsx` |
| Settings checks | `remuxdb` + `virtual_library` check kinds; safe messages | `internal/api/handlers/admin_settings_checks*.go` | `admin_settings_check_service_test.go` |
| Monitor registrar | Library-scoped registration, episode URIs, plugin-path fallbacks | `internal/virtuallibrary/registrar.go`, `monitor/monitor.go` | virtuallibrary suites |

## Merge procedure

1. `git fetch upstream` and review every commit: subjects plus `git show
   --stat` for each. Pay special attention to dropped or renamed operations,
   parameters, settings keys, and anything touching the files above.
2. Red flags that need a decision, not a blind resolve:
   - A removed API operation: grep `web/src` for its operation ID — if the
     fork's UI calls it, keep calling code working (adapt or vendor a
     replacement) before merging.
   - New hunks in `internal/api/router.go` or `cmd/silo/main.go` near virtual
     dispatch: upstream may reintroduce plugin fallbacks. The invariant script
     catches callers; keep routing core-only.
   - `internal/config/admin_settings.go` changes: check for key renames or
     removed fork-used keys.
   - `contracts/api/v2/*` and `web/src/api/v2/{operations,schema}.ts` churn:
     expected when upstream changes the contract. These are generated —
     resolve by regenerating (`make apiv2-*`), never by hand-editing.
3. Merge with a merge commit so fork history stays readable. Never rebase
   pushed history.
4. Verify before pushing: `scripts/check-fork-invariants.sh`, `go build
   ./...`, the focused suites for touched areas (`internal/apiv2` including
   the fixtures/document tests that pin the contract, `internal/catalog`
   virtual suites against a disposable DB, `internal/api` router tests, web
   typecheck plus touched step tests).
5. Push. The image pipeline builds from the merge.
