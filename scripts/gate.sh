#!/bin/sh
# One-command gate: lanes run this themselves; Fable reads the last line only.
set -e -o pipefail
export PATH="/opt/homebrew/bin:$PATH"
cd "$(dirname "$0")/.."

# Renderer failures are cheap compared with the full Go matrix. Release input
# preparation additionally requires the freshly built renderer to match the
# committed go:embed snapshot, so a stale snapshot fails before any slow test.
renderer_built=0
if [ -d desktop/renderer2/node_modules ]; then
  (cd desktop/renderer2 && npm test --silent && npx tsc --noEmit && npm run build --silent >/dev/null)
  renderer_built=1
fi
if [ "${WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER:-0}" = 1 ]; then
  [ "$renderer_built" -eq 1 ] || {
    echo "release gate requires desktop/renderer2/node_modules" >&2
    exit 1
  }
  if ! diff -qr desktop/renderer2/dist cmd/workass/embedded/dist >/dev/null; then
    echo "renderer build differs from committed embedded output; run scripts/sync-renderer2.sh and commit it before release" >&2
    exit 1
  fi
  echo "WORKASS_RENDERER_SNAPSHOT_VERIFIED"
fi

node --test desktop/shell/*.test.js
go build ./... && go vet ./...
# Brevity is right on the happy path and exactly backwards on the failing one:
# `| tail -12` shows the alphabetical tail, so a failing package early in the
# list loses both its `--- FAIL:` detail and its `FAIL <package>` line, and the
# truncation happens here — before any tee — so no caller can recover them.
# Capture, then keep the tail when it passes and the failures when it does not.
test_log="$(mktemp)"
trap 'rm -f "$test_log"' EXIT INT TERM
if go test ./... -count=1 >"$test_log" 2>&1; then
  tail -12 "$test_log"
else
  # Dropping the packages that passed is what leaves room for the ones that did
  # not, with the assertion text still attached to the name that produced it.
  #
  # sed, not `grep | head`: under `set -e -o pipefail` head exits at its limit,
  # grep takes SIGPIPE, and the non-zero pipeline status aborts this script
  # mid-branch — swallowing the roll-up below and the explicit exit. That is the
  # same swallow this whole branch exists to fix, one layer down.
  grep -vE '^(ok|\?)[[:space:]]' "$test_log" | sed -n '1,60p'
  # Then the names alone, unconditionally: a passing package that logs freely
  # can still push the roll-up past that bound, and the name of what failed is
  # the one line the next command cannot proceed without.
  echo "--- failing packages ---"
  grep -E '^(FAIL|panic)' "$test_log" | head -20
  exit 1
fi
echo "GATE_PASS"
