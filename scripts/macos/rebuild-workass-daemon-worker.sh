#!/bin/sh
set -eu

[ "$#" -eq 14 ] || { echo "internal handoff worker: invalid arguments" >&2; exit 2; }
repo_root="$1"
candidate="$2"
expected_pid="$3"
port="$4"
bind="$5"
state_dir="$6"
label="$7"
log_file="$8"
status_file="$9"
view_port="${10}"
runtime_path="${11}"
runtime_home="${12}"
target="${13}"
working_dir="${14}"
backup="$target.previous"
lock_dir="$repo_root/.dev/rebuild/daemon-$label.lock"
plist="$HOME/Library/LaunchAgents/$label.plist"
old_plist_backup="$status_file.previous.plist"
state_parent=$(dirname -- "$state_dir")
status_name=$(basename -- "$status_file")
case "$status_name" in
  *[!A-Za-z0-9._-]*|'') echo "internal handoff worker: unsafe status filename" >&2; exit 2 ;;
esac
state_snapshot="$state_parent/.$status_name.state.previous"
state_snapshot_staging="$state_snapshot.incomplete"
state_failed="$state_parent/.$status_name.state.failed"
state_snapshot_ready=0

mkdir -p "$(dirname -- "$log_file")" "$(dirname -- "$target")"
exec >>"$log_file" 2>&1

status() {
  phase="$1"
  detail="${2:-}"
  tmp="$status_file.tmp.$$"
  {
    printf 'phase=%s\n' "$phase"
    printf 'detail=%s\n' "$detail"
    printf 'expected_pid=%s\n' "$expected_pid"
    printf 'port=%s\n' "$port"
    printf 'label=%s\n' "$label"
    printf 'updated_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "$tmp"
  mv "$tmp" "$status_file"
}

listener_pid() {
  lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n 1
}

wait_health() {
  attempts=120
  while [ "$attempts" -gt 0 ]; do
    if curl -kfsS --max-time 2 "https://127.0.0.1:$port/workass/health" >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

catalog_stamp() {
  printf '%s' "$1" | sed -nE 's/.*"catalog":\{"reportedAt":"([^"]+)".*/\1/p'
}

client_status() {
  curl -fsS --max-time 2 "http://127.0.0.1:$view_port/__workass-shell/status" 2>/dev/null || true
}

wait_client_ready() {
  previous_catalog_stamp="$1"
  [ "$view_port" -gt 0 ] || return 0
  attempts=160
  while [ "$attempts" -gt 0 ]; do
    body=$(client_status)
    stamp=$(catalog_stamp "$body")
    if [ -n "$stamp" ] && [ "$stamp" != "$previous_catalog_stamp" ] && \
       printf '%s' "$body" | grep -q '"controller":true' && \
       printf '%s' "$body" | grep -Eq '"readyModelCount":[1-9][0-9]*'; then
      sleep 5
      stable=$(client_status)
      printf '%s' "$stable" | grep -q '"controller":true' || return 1
      printf '%s' "$stable" | grep -Eq '"readyModelCount":[1-9][0-9]*' || return 1
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

snapshot_state() {
  [ -d "$state_dir" ] || return 1
  [ ! -e "$state_snapshot" ] || return 1
  [ ! -e "$state_snapshot_staging" ] || return 1
  [ ! -e "$state_failed" ] || return 1
  if ! cp -pR "$state_dir" "$state_snapshot_staging"; then
    rm -rf "$state_snapshot_staging"
    return 1
  fi
  if ! mv "$state_snapshot_staging" "$state_snapshot"; then
    rm -rf "$state_snapshot_staging"
    return 1
  fi
  state_snapshot_ready=1
}

restore_state_snapshot() {
  [ "$state_snapshot_ready" -eq 1 ] || return 0
  [ -d "$state_snapshot" ] || return 1
  [ ! -e "$state_failed" ] || return 1
  state_was_present=0
  if [ -e "$state_dir" ]; then
    mv "$state_dir" "$state_failed" || return 1
    state_was_present=1
  fi
  if mv "$state_snapshot" "$state_dir"; then
    state_snapshot_ready=0
    return 0
  fi
  if [ "$state_was_present" -eq 1 ] && [ ! -e "$state_dir" ]; then
    mv "$state_failed" "$state_dir" || true
  fi
  return 1
}

install_launchd() {
  "$repo_root/scripts/macos/install-workass-launchd.sh" "$target" "$state_dir" "$port" "$bind" "$label" "$working_dir" "$runtime_path" "$runtime_home"
}

rollback() {
  reason="$1"
  echo "[handoff] activation failed: $reason"
  status rollback "$reason"
  launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
  if [ -f "$backup" ]; then
    cp "$backup" "$target"
    chmod 755 "$target"
  else
    rm -f "$target"
  fi
  if ! restore_state_snapshot; then
    status failed "new daemon failed and state rollback failed: $reason"
    echo "[handoff] state rollback failed"
    exit 1
  fi
  rollback_catalog_stamp=''
  if [ "$view_port" -gt 0 ]; then rollback_catalog_stamp=$(catalog_stamp "$(client_status)"); fi
  restored=0
  if [ -f "$old_plist_backup" ]; then
    cp "$old_plist_backup" "$plist"
    if launchctl bootstrap "gui/$(id -u)" "$plist" && \
       launchctl enable "gui/$(id -u)/$label" && \
       launchctl kickstart -k "gui/$(id -u)/$label"; then
      restored=1
    fi
  elif install_launchd; then
    restored=1
  fi
  if [ "$restored" -eq 1 ] && wait_health && wait_client_ready "$rollback_catalog_stamp"; then
    restored_pid=$(listener_pid)
    if [ -e "$state_failed" ]; then rm -rf "$state_failed"; fi
    status rollback_healthy "new daemon failed; previous daemon restored as pid ${restored_pid:-unknown}"
    echo "[handoff] rollback healthy pid=${restored_pid:-unknown}"
    exit 1
  fi
  status failed "new daemon failed and rollback did not become healthy: $reason"
  echo "[handoff] rollback failed"
  exit 1
}

if ! mkdir "$lock_dir" 2>/dev/null; then
  status failed "another daemon rebuild holds $lock_dir"
  exit 1
fi
trap 'rm -rf "$lock_dir"' EXIT HUP INT TERM

sleep 1
status activating "detached worker owns replacement"
echo "[handoff] start expected_pid=$expected_pid port=$port label=$label"
[ -x "$candidate" ] || { status failed "candidate missing"; exit 1; }
[ -d "$working_dir" ] || { status failed "target working directory missing: $working_dir"; exit 1; }
if [ "${WORKASS_PROFILE:-}" = prod ]; then
  codesign --verify --strict "$candidate" >/dev/null 2>&1 || {
    status failed "production candidate signature is invalid"
    exit 1
  }
  candidate_requirement=$(codesign -d -r- "$candidate" 2>&1 |
    sed -nE 's/^[[:space:]#]*designated =>[[:space:]]*//p' |
    head -n 1)
  case "$candidate_requirement" in
    ''|*cdhash*)
      status failed "production candidate has no stable designated requirement"
      exit 1
      ;;
  esac
fi
if [ -f "$plist" ]; then cp "$plist" "$old_plist_backup"; fi

baseline_catalog_stamp=''
if [ "$view_port" -gt 0 ]; then
  baseline_client=$(client_status)
  printf '%s' "$baseline_client" | grep -q '"controller":true' || {
    status failed "Electron client was not controller before activation"
    exit 1
  }
  baseline_catalog_stamp=$(catalog_stamp "$baseline_client")
  [ -n "$baseline_catalog_stamp" ] || {
    status failed "Electron client had no catalog receipt before activation"
    exit 1
  }
fi

current_pid=$(listener_pid)
[ "$current_pid" = "$expected_pid" ] || {
  status failed "daemon pid changed before activation: expected $expected_pid, found ${current_pid:-none}"
  exit 1
}

if [ -f "$target" ]; then
  cp "$target" "$backup"
  chmod 755 "$backup"
fi
mv "$candidate" "$target"
chmod 755 "$target"

if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  echo "[handoff] replacing existing launchd daemon"
  launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || rollback "could not stop existing launchd daemon"
else
  echo "[handoff] stopping directly launched daemon pid=$expected_pid"
  kill -TERM "$expected_pid" 2>/dev/null || true
  attempts=60
  while [ "$attempts" -gt 0 ] && kill -0 "$expected_pid" 2>/dev/null; do
    attempts=$((attempts - 1))
    sleep 0.25
  done
  if kill -0 "$expected_pid" 2>/dev/null; then
    kill -KILL "$expected_pid" 2>/dev/null || true
  fi
fi

attempts=80
while [ "$attempts" -gt 0 ] && { [ -n "$(listener_pid)" ] || kill -0 "$expected_pid" 2>/dev/null; }; do
  attempts=$((attempts - 1))
  sleep 0.25
done
[ -z "$(listener_pid)" ] || rollback "old daemon listener did not stop"

# A candidate may evolve durable storage before exposing health. The previous
# binary and the previous state therefore form one rollback unit: restoring
# only the executable can strand a valid profile on a schema it cannot read.
snapshot_state || rollback "could not snapshot state before activation"

install_launchd || rollback "launchd install failed"
wait_health || rollback "health endpoint did not recover"
new_pid=$(listener_pid)
[ -n "$new_pid" ] || rollback "health passed without a listener pid"
[ "$new_pid" != "$expected_pid" ] || rollback "daemon pid did not change"
wait_client_ready "$baseline_catalog_stamp" || rollback "Electron did not reconnect as controller with a populated provider catalog"

if [ "$state_snapshot_ready" -eq 1 ]; then
  if rm -rf "$state_snapshot"; then
    state_snapshot_ready=0
  else
    echo "[handoff] warning: could not remove successful state snapshot $state_snapshot"
  fi
fi

status healthy "daemon relaunched as pid $new_pid; Electron controller and provider catalog recovered"
echo "[handoff] healthy new_pid=$new_pid"
exit 0
