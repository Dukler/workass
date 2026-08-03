#!/bin/sh
# Launch the workass Electron shell on macOS against a running daemon.
# Starts the daemon from dist-bin if nothing answers on the port.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
port="${WORKASS_PORT:-8788}"
url="http://127.0.0.1:$port"

if ! curl -fsS --max-time 2 "$url/workass/health" >/dev/null 2>&1; then
  echo "daemon not answering on :$port — starting dist-bin/workass-darwin-arm64"
  "$repo_root/dist-bin/workass-darwin-arm64" --port "$port" &
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    sleep 0.3
    if curl -fsS --max-time 2 "$url/workass/health" >/dev/null 2>&1; then break; fi
  done
fi

WORKASS_URL="$url" exec electron "$repo_root/desktop/shell/main.js"
