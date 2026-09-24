#!/bin/sh
# One-command gate: lanes run this themselves; Fable reads the last line only.
set -e -o pipefail
export PATH="/opt/homebrew/bin:$PATH"
cd "$(dirname "$0")/.."

run_gate_phase() {
  phase_name=$1
  shift
  phase_started=$(date +%s)
  if "$@"; then
    phase_status=passed
  else
    phase_status=failed
  fi
  echo "WORKASS_GATE_PHASE name=$phase_name status=$phase_status seconds=$(($(date +%s) - phase_started))"
  [ "$phase_status" = passed ]
}

# Renderer failures are cheap compared with the full Go matrix. Release input
# preparation additionally requires the freshly built renderer to match the
# committed go:embed snapshot, so a stale snapshot fails before any slow test.
renderer_built=0
if [ -d desktop/renderer2/node_modules ]; then
  run_gate_phase renderer sh -c 'cd desktop/renderer2 && npm test --silent && npx tsc --noEmit && npm run build --silent >/dev/null'
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

run_gate_phase shell_tests node --test desktop/shell/*.test.js
run_gate_phase go_build go build ./...
run_gate_phase go_vet go vet ./...
# Brevity is right on the happy path and exactly backwards on the failing one:
# `| tail -12` shows the alphabetical tail, so a failing package early in the
# list loses both its `--- FAIL:` detail and its `FAIL <package>` line, and the
# truncation happens here — before any tee — so no caller can recover them.
# Capture, then keep the tail when it passes and the failures when it does not.
test_log="$(mktemp)"
trap 'rm -f "$test_log"' EXIT INT TERM
run_go_tests() {
  if [ "${WORKASS_GATE_FRESH:-0}" = 1 ]; then
    go test ./... -count=1 -p=2 -parallel=2
  else
    go test ./... -p=2 -parallel=2
  fi
}
go_tests_started=$(date +%s)
if run_go_tests >"$test_log" 2>&1; then
  tail -12 "$test_log"
  echo "WORKASS_GATE_PHASE name=go_tests status=passed seconds=$(($(date +%s) - go_tests_started))"
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
  echo "WORKASS_GATE_PHASE name=go_tests status=failed seconds=$(($(date +%s) - go_tests_started))"
  exit 1
fi
echo "GATE_PASS"
