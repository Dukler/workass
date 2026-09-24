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
  run_gate_phase renderer_prepare sh -c 'cd desktop/renderer2 && npx tsc --noEmit && npm run build --silent >/dev/null'
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

run_gate_phase go_build go build ./...
run_gate_phase go_vet go vet ./...
run_gate_phase test_suites node scripts/test-suite.mjs
echo "GATE_PASS"
