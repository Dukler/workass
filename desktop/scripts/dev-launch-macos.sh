#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DESKTOP_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
ROOT_DIR=$(CDPATH= cd -- "$DESKTOP_DIR/.." && pwd)
WORKASS_REPO_ROOT="$ROOT_DIR"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/lib/workass-profile.sh"
workass_load_profile dev
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/lib/workass-electron.sh"
ELECTRON_APP=$(workass_electron_resolve) || {
  echo "Stage the pinned runtime with: scripts/vendor-electron-runtime.sh" >&2
  exit 1
}

if [ ! -d "$ELECTRON_APP" ]; then
  echo "Electron.app not found at $ELECTRON_APP" >&2
  echo "Stage it with: scripts/vendor-electron-runtime.sh" >&2
  exit 1
fi

mkdir -p "$WORKASS_STATE_DIR" "$WORKASS_USER_DATA_DIR" "$WORKASS_RUN_DIR" "$WORKASS_LOG_ROOT"
if ! curl -fsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" >/dev/null 2>&1; then
  daemon="$ROOT_DIR/dist-bin/workass-darwin-arm64"
  [ -x "$daemon" ] || { echo "development daemon missing: $daemon" >&2; exit 1; }
  "$ROOT_DIR/scripts/macos/install-workass-launchd.sh" \
    "$daemon" "$WORKASS_STATE_DIR" "$WORKASS_DAEMON_PORT" "$WORKASS_DAEMON_BIND" \
    "$WORKASS_LAUNCHD_LABEL" "$ROOT_DIR" "${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}" "$HOME"
  attempts=80
  while [ "$attempts" -gt 0 ]; do
    curl -fsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" >/dev/null 2>&1 && break
    attempts=$((attempts - 1))
    sleep 0.25
  done
  curl -fsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" >/dev/null
fi

status_url="http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/status"
existing_status=$(curl -fsS --max-time 2 "$status_url" 2>/dev/null || true)
if [ -n "$existing_status" ]; then
  if printf '%s' "$existing_status" | grep -Fq "\"daemonOrigin\":\"$WORKASS_DAEMON_URL\""; then
    echo "Workass DEV already running. daemon=$WORKASS_DAEMON_URL renderer=http://localhost:$WORKASS_VIEW_PORT/"
    exit 0
  fi
  echo "development view port $WORKASS_VIEW_PORT is owned by a different runtime" >&2
  exit 1
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
shell_out="$WORKASS_LOG_ROOT/shell-$stamp.out.log"
shell_err="$WORKASS_LOG_ROOT/shell-$stamp.err.log"
open -n "$ELECTRON_APP" --stdout "$shell_out" --stderr "$shell_err" \
  --env "WORKASS_PROFILE=$WORKASS_PROFILE" \
  --env "WORKASS_PROFILE_FILE=$WORKASS_PROFILE_FILE" \
  --env "WORKASS_URL=$WORKASS_DAEMON_URL" \
  --env "WORKASS_VIEW_PORT=$WORKASS_VIEW_PORT" \
  --env "WORKASS_DATA_ROOT=$WORKASS_DATA_ROOT" \
  --env "WORKASS_BROWSER_CONTROL_FILE=$WORKASS_BROWSER_CONTROL_FILE" \
  --env "ELECTRON_ENABLE_LOGGING=1" \
  --env "ELECTRON_ENABLE_STACK_DUMPING=1" \
  --args "$DESKTOP_DIR/shell/main.js" "--user-data-dir=$WORKASS_USER_DATA_DIR" \
  "--enable-logging=stderr" "--v=1"

echo "Workass DEV launched. daemon=$WORKASS_DAEMON_URL renderer=http://localhost:$WORKASS_VIEW_PORT/"
echo "shell_stdout=$shell_out"
echo "shell_stderr=$shell_err"
