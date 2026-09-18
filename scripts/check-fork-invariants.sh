#!/usr/bin/env bash
# Fork invariants: fail loudly when an upstream merge (or local edit) breaks a
# deliberate Vio divergence. See docs/architecture/fork-divergence.md.
# Commands assume the repository root is the cwd.
set -uo pipefail

fail=0
pass() { echo "ok   - $1"; }
reject() { echo "FAIL - $1"; fail=1; }

# 1. Web client identifies as Vio Web.
grep -q '"X-Vio-Client": "Vio Web"' web/src/api/v2/request.ts \
  && pass "Vio client headers" \
  || reject "Vio client headers missing from web/src/api/v2/request.ts"
grep -q '"vio web"' internal/apiv2/observe.go \
  && pass "Vio telemetry labels" \
  || reject "Vio telemetry labels missing from internal/apiv2/observe.go"

# 2. No virtual→plugin dispatch outside the plugins package itself (tests
# excluded). Virtual resolution is core-only.
dispatch=$(grep -rn --include='*.go' \
  -e 'ResolveVirtualPlaybackForInstallation' \
  -e 'ListVirtualPlaybackStreamsForInstallation' \
  -e 'RefreshVirtualPlaybackForInstallation' \
  -e 'ResolveVirtualPlaybackDetailedForInstallation' \
  -e 'ConfiguredVirtualVariants' \
  internal/ cmd/ \
  | grep -v '_test.go' \
  | grep -v 'internal/plugins/virtual_playback' \
  | grep -v -E ':[0-9]+:\s*//' \
  || true)
if [ -z "$dispatch" ]; then
  pass "core-only virtual dispatch"
else
  reject "virtual→plugin dispatch callers remain: $dispatch"
fi

# 3. Core virtual router installation (0) validates; dormant saves are 422s.
grep -q 'id < 0' internal/apiv2/admin_requests.go \
  && pass "installation 0 validates" \
  || reject "installation 0 rejected in internal/apiv2/admin_requests.go"
grep -q 'ErrProviderUnavailable' internal/apiv2/admin_collections.go \
  && pass "sync provider-unavailable mapping" \
  || reject "503 mapping missing from internal/apiv2/admin_collections.go"

# 4. Onboarding additions are wired.
grep -q 'RequestsStep' web/src/pages/SetupWizard.tsx \
  && grep -q '"requests"' web/src/pages/setup-wizard/setupStorage.ts \
  && pass "wizard requests step" \
  || reject "wizard requests step unwired"
grep -q 'MetadataProvidersSection' web/src/pages/setup-wizard/steps/LibraryStep.tsx \
  && pass "wizard metadata auto-install" \
  || reject "metadata auto-install missing from LibraryStep"

# 5. Indexer secrets use SecretField with a clear path.
grep -q 'SecretField' web/src/pages/admin-settings/StreamingSettings.tsx \
  && grep -q 'virtual_library.indexer_api_key' web/src/pages/admin-settings/StreamingSettings.tsx \
  && pass "indexer SecretField rows" \
  || reject "indexer SecretField rows missing from StreamingSettings"

# 6. New collections default virtual playback on.
grep -q 'setVirtualPlayback] = useState(true)' web/src/pages/adminCollectionsShared.tsx \
  && pass "virtual_playback add-flow defaults" \
  || reject "virtual_playback add-flow defaults flipped off"

if [ "$fail" -ne 0 ]; then
  echo "fork invariants BROKEN — see docs/architecture/fork-divergence.md" >&2
  exit 1
fi

# Working-tree formatting: CI gates gofmt and prettier --check, and the repo
# pre-commit hook does not. Catch it here before pushing, not after.
if [ -n "$(gofmt -l internal cmd 2>/dev/null)" ]; then
  reject "gofmt clean: $(gofmt -l internal cmd 2>/dev/null | tr '\n' ' ')"
else
  pass "gofmt clean"
fi
if [ -d web/node_modules ]; then
  if pnpm --dir web run format:check >/dev/null 2>&1; then
    pass "prettier clean"
  else
    reject "prettier clean (run: pnpm --dir web exec prettier --write <files>)"
  fi
else
  echo "skip  - prettier clean (web/node_modules absent)"
fi

if [ "$fail" -ne 0 ]; then
  echo "fork invariants BROKEN — see docs/architecture/fork-divergence.md" >&2
  exit 1
fi
echo "fork invariants hold"
