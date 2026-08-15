#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
runner="$repo_root/scripts/rebuild-workass-macos.sh"
worker="$repo_root/scripts/macos/rebuild-workass-daemon-worker.sh"

sh -n "$runner"
sh -n "$worker"
sh -n "$repo_root/scripts/macos/install-workass-launchd.sh"
sh -n "$repo_root/scripts/macos/bootstrap-workass-local-signing.sh"
sh -n "$repo_root/scripts/lib/workass-codesign.sh"
sh -n "$repo_root/scripts/lib/workass-electron.sh"
sh -n "$repo_root/scripts/vendor-electron-runtime.sh"
sh -n "$repo_root/scripts/tests/workass-codesign.test.sh"
node --check "$repo_root/scripts/tests/rebuild-client-status-fixture.js"
"$runner" --help >/dev/null
"$runner" electron --help >/dev/null
"$runner" daemon --help >/dev/null
grep -q '<string>__LABEL__</string>' "$repo_root/scripts/macos/workass.launchd.plist"
if grep -Eq '\) 2>&1 \| tee' "$runner"; then
  echo "build/test pipeline can mask a failing command" >&2
  exit 1
fi
grep -Fq 'old_pids=$(listener_pid "$view_port" || true)' "$runner"
grep -Fq 'old Electron shell pid $pid did not exit' "$runner"
grep -Fq 'kill -0 "$pid"' "$runner"
grep -Fq 'node --test desktop/shell/*.test.js' "$runner"
grep -Fq 'WORKASS_CONTROLLER_RECOVERY=1' "$runner"
grep -Fq 'installed_app="${WORKASS_INSTALLED_APP:-/Applications/Workass.app}"' "$runner"
grep -Fq 'workass_codesign_sign_binary "$candidate" "$WORKASS_BUNDLE_ID.daemon"' "$runner"
prepare_line=$(grep -n '^[[:space:]]*workass_codesign_prepare$' "$runner" | tail -n 1 | cut -d: -f1)
sign_line=$(grep -n 'workass_codesign_sign_binary "$candidate" "$WORKASS_BUNDLE_ID.daemon"' "$runner" | cut -d: -f1)
[ -n "$prepare_line" ] && [ "$prepare_line" -lt "$sign_line" ] || {
  echo "daemon rebuild does not refresh the signing keychain immediately before codesign" >&2
  exit 1
}
grep -Fq 'workass_codesign_is_adhoc_cdhash "$target"' "$runner"
grep -Fq 'target="${target:-$WORKASS_DATA_ROOT/runtime/workass}"' "$runner"
grep -Fq 'production daemon target must not use a temporary candidate path' "$runner"
grep -Fq 'vendor-frontier-hosts.sh" --target darwin-arm64 --offline' "$runner"
grep -Fq 'native provider host staging failed; current daemon was not stopped' "$runner"
grep -Fq 'production native provider hosts do not match the gated bundle' "$runner"
grep -Fq 'workass_electron_resolve' "$runner"
grep -Fq 'security list-keychains -d user -s' "$repo_root/scripts/macos/bootstrap-workass-local-signing.sh"
grep -Fq 'WORKASS_LOCAL_SIGNING_CANARY_PASS' "$repo_root/scripts/macos/bootstrap-workass-local-signing.sh"
if grep -Fq 'npm root -g' "$runner"; then
  echo "rebuild script still depends on an unpinned global Electron" >&2
  exit 1
fi
grep -Fq 'production candidate has no stable designated requirement' "$worker"
grep -Fq 'snapshot_state || rollback "could not snapshot state before activation"' "$worker"
grep -Fq 'if ! restore_state_snapshot; then' "$worker"
grep -Fq 'new daemon failed and state rollback failed' "$worker"

set +e
temporary_target_output=$(
  "$runner" daemon --profile prod --port 1 --view-port 0 \
    --target "$repo_root/.dev/rebuild/workass-contract-test.candidate" 2>&1
)
temporary_target_status=$?
set -e
[ "$temporary_target_status" -ne 0 ] || {
  echo "production rebuild accepted a temporary candidate target" >&2
  exit 1
}
case "$temporary_target_output" in
  *"production daemon target must not use a temporary candidate path"*) ;;
  *) echo "temporary production target failed for the wrong reason" >&2; exit 1 ;;
esac

if [ "${1:-}" != "--isolated-daemon" ]; then
  echo "REBUILD_SCRIPT_STATIC_TEST_PASS"
  exit 0
fi

[ "$(uname -s)" = "Darwin" ] || { echo "isolated handoff test requires macOS" >&2; exit 1; }
port=$((42000 + ($$ % 10000)))
while lsof -nP -tiTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; do port=$((port + 1)); done
label="com.workass.rebuild.test.$$"
temp_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-rebuild-test.XXXXXX")
path_marker="$temp_root/path-marker"
mkdir -p "$path_marker"
plist="$HOME/Library/LaunchAgents/$label.plist"
old_pid=''
old_listener_pid=''
handoff_plist=''
status_fixture_pid=''
view_port=$((port + 1))
while lsof -nP -tiTCP:"$view_port" -sTCP:LISTEN >/dev/null 2>&1; do view_port=$((view_port + 1)); done
real_daemon_before=$(lsof -nP -tiTCP:8788 -sTCP:LISTEN 2>/dev/null | head -n 1 || true)

cleanup() {
  launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
  launchctl disable "gui/$(id -u)/$label" >/dev/null 2>&1 || true
  if [ -n "$handoff_plist" ]; then
    launchctl bootout "gui/$(id -u)" "$handoff_plist" >/dev/null 2>&1 || true
    rm -f "$handoff_plist"
  fi
  rm -f "$plist"
  pid=$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n 1 || true)
  [ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null || true
  [ -z "$old_pid" ] || kill -TERM "$old_pid" 2>/dev/null || true
  [ -z "$status_fixture_pid" ] || kill -TERM "$status_fixture_pid" 2>/dev/null || true
  rm -rf "$temp_root"
}
trap cleanup EXIT HUP INT TERM

node "$repo_root/scripts/tests/rebuild-client-status-fixture.js" "$view_port" >"$temp_root/client-status.log" 2>&1 &
status_fixture_pid=$!
attempts=40
while [ "$attempts" -gt 0 ]; do
  curl -fsS --max-time 2 "http://127.0.0.1:$view_port/__workass-shell/status" >/dev/null 2>&1 && break
  attempts=$((attempts - 1))
  sleep 0.25
done
curl -fsS --max-time 2 "http://127.0.0.1:$view_port/__workass-shell/status" >/dev/null

if [ ! -x "$repo_root/dist-bin/workass-darwin-arm64" ]; then
  (cd "$repo_root" && scripts/sync-renderer2.sh && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist-bin/workass-darwin-arm64 ./cmd/workass)
fi
(cd "$repo_root" && "$repo_root/dist-bin/workass-darwin-arm64" --port "$port" --bind localhost --state-dir "$temp_root/state") >"$temp_root/old.log" 2>&1 &
old_pid=$!
attempts=80
while [ "$attempts" -gt 0 ]; do
  curl -fsS --max-time 2 "http://127.0.0.1:$port/workass/health" >/dev/null 2>&1 && break
  attempts=$((attempts - 1))
  sleep 0.25
done
curl -fsS --max-time 2 "http://127.0.0.1:$port/workass/health" >/dev/null
old_listener_pid=$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n 1)
[ -n "$old_listener_pid" ]

output=$(cd "$repo_root" && \
  WORKASS_TEST_ROOT="$temp_root/profile" \
  PATH="$path_marker:$PATH" \
  "$runner" daemon \
    --profile test \
    --port "$port" \
    --bind localhost \
    --state-dir "$temp_root/state" \
    --label "$label" \
    --view-port "$view_port" \
    --target "$temp_root/workass" \
    --working-dir "$repo_root")
printf '%s\n' "$output"
status_file=$(printf '%s\n' "$output" | sed -n 's/^status=//p' | tail -n 1)
handoff_plist=$(printf '%s\n' "$output" | sed -n 's/^handoff_plist=//p' | tail -n 1)
[ -n "$status_file" ] || { echo "handoff did not report a status file" >&2; exit 1; }
[ -n "$handoff_plist" ] || { echo "handoff did not report its transient plist" >&2; exit 1; }

attempts=240
while [ "$attempts" -gt 0 ]; do
  if grep -q '^phase=healthy$' "$status_file" 2>/dev/null; then break; fi
  if grep -Eq '^phase=(failed|rollback_healthy)$' "$status_file" 2>/dev/null; then cat "$status_file" >&2; exit 1; fi
  attempts=$((attempts - 1))
  sleep 0.25
done
cat "$status_file"
grep -q '^phase=healthy$' "$status_file"
grep -q 'Electron controller and provider catalog recovered' "$status_file"
sleep 3
grep -q '^phase=healthy$' "$status_file"
[ "$(grep -c '^\[handoff\] start ' "${status_file%.status}.log")" -eq 1 ]
new_pid=$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n 1)
[ -n "$new_pid" ] && [ "$new_pid" != "$old_listener_pid" ]
curl -fsS --max-time 2 "http://127.0.0.1:$port/workass/health" >/dev/null
installed_path=$(plutil -extract EnvironmentVariables.PATH raw -o - "$plist")
case "$installed_path" in "$path_marker":*) ;; *) echo "launchd PATH marker missing: $installed_path" >&2; exit 1 ;; esac
installed_home=$(plutil -extract EnvironmentVariables.HOME raw -o - "$plist")
[ "$installed_home" = "$HOME" ]
real_daemon_after=$(lsof -nP -tiTCP:8788 -sTCP:LISTEN 2>/dev/null | head -n 1 || true)
[ "$real_daemon_after" = "$real_daemon_before" ]
echo "ISOLATED_DAEMON_HANDOFF_PASS old_pid=$old_listener_pid new_pid=$new_pid live_daemon_pid=${real_daemon_after:-none} client_reconnected=true path_preserved=true"
