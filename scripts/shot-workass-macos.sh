#!/bin/sh
# Capture a PNG of the running Workass Electron shell's REAL main window (exactly
# what the user sees) via the shell's /__workass-shell/screenshot dev endpoint.
# Use this instead of guessing at the UI. Requires the shell to be running
# (scripts/rebuild-workass-macos.sh electron) — the rebuild also drops a PNG in
# .dev/rebuild automatically.
#
# usage: scripts/shot-workass-macos.sh [OUT_PNG] [VIEW_PORT]
set -eu

out="${1:-/tmp/workass-shot.png}"
port="${2:-8799}"
url="http://127.0.0.1:$port/__workass-shell/screenshot"

if curl -fsS --max-time 12 "$url" -o "$out" && [ -s "$out" ]; then
  echo "captured: $out"
else
  echo "capture failed (is the Electron shell running on view port $port?): $url" >&2
  rm -f "$out"
  exit 1
fi
