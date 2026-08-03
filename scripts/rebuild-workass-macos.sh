#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
log_root="$repo_root/.dev/rebuild"
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-electron.sh"

usage() {
  cat <<'EOF'
usage:
  scripts/rebuild-workass-macos.sh electron [--profile dev|prod] [--daemon-url URL] [--view-port PORT]
  scripts/rebuild-workass-macos.sh daemon [--profile prod|dev|test] [--port PORT] [--bind localhost|lan]
                                           [--state-dir ABSOLUTE_PATH]
                                           [--label LAUNCHD_LABEL]
                                           [--view-port PORT|0]
                                           [--target ABSOLUTE_PATH]
                                           [--working-dir ABSOLUTE_PATH]
                                           [--migrate-from ABSOLUTE_STATE_PATH]
                                           [--migrate-signing-identity]

Electron rebuilds and relaunches only the client, then proves the daemon PID
did not change. Daemon builds and preflights a candidate, then dispatches a
detached launchd handoff that relaunches or rolls back without agent supervision.
EOF
}

die() {
  echo "rebuild-workass: $*" >&2
  exit 1
}

require_macos() {
  [ "$(uname -s)" = "Darwin" ] || die "this lifecycle script currently supports macOS only"
}

listener_pid() {
  lsof -nP -tiTCP:"$1" -sTCP:LISTEN 2>/dev/null | head -n 1
}

url_port() {
  printf '%s\n' "$1" | sed -nE 's#^https?://[^:/]+:([0-9]+).*$#\1#p'
}

wait_http() {
  url="$1"
  attempts="$2"
  while [ "$attempts" -gt 0 ]; do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

rebuild_electron() {
  profile=dev
  daemon_url=''
  view_port=''
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --profile) [ "$#" -ge 2 ] || die "--profile needs a value"; profile="$2"; shift 2 ;;
      --daemon-url) [ "$#" -ge 2 ] || die "--daemon-url needs a value"; daemon_url="$2"; shift 2 ;;
      --view-port) [ "$#" -ge 2 ] || die "--view-port needs a value"; view_port="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown electron argument: $1" ;;
    esac
  done
  workass_load_profile "$profile" || exit 1
  daemon_url="${daemon_url:-$WORKASS_DAEMON_URL}"
  view_port="${view_port:-$WORKASS_VIEW_PORT}"

  daemon_port=$(url_port "$daemon_url")
  [ -n "$daemon_port" ] || die "--daemon-url must include an explicit port"
  daemon_pid_before=$(listener_pid "$daemon_port")
  [ -n "$daemon_pid_before" ] || die "no daemon listener found on port $daemon_port"
  curl -fsS --max-time 2 "$daemon_url/workass/health" >/dev/null || die "daemon health check failed before Electron rebuild"

  mkdir -p "$log_root"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  log_file="$log_root/electron-$stamp.log"
  : > "$log_file"

  echo "[electron] building renderer" | tee -a "$log_file"
  step_output="$log_root/electron-build-$stamp.step"
  if (cd "$repo_root" && npm run build --prefix desktop/renderer2) >"$step_output" 2>&1; then
    tee -a "$log_file" < "$step_output"
    rm -f "$step_output"
  else
    tee -a "$log_file" < "$step_output" >&2
    rm -f "$step_output"
    die "renderer build failed; existing Electron shell was not stopped"
  fi
  echo "[electron] testing shell view server" | tee -a "$log_file"
  step_output="$log_root/electron-test-$stamp.step"
  if (cd "$repo_root" && node --test desktop/shell/*.test.js && \
      node --experimental-strip-types --test desktop/renderer2/tests/model-selection.test.ts desktop/renderer2/tests/timeline-layout.test.ts desktop/renderer2/tests/queue-persistence.test.ts desktop/renderer2/tests/image-drafts.test.ts desktop/renderer2/tests/workspaces.test.ts desktop/renderer2/tests/workspace-picker.test.ts desktop/renderer2/tests/notifications.test.ts desktop/renderer2/tests/subagent-layout.test.ts) >"$step_output" 2>&1; then
    tee -a "$log_file" < "$step_output"
    rm -f "$step_output"
  else
    tee -a "$log_file" < "$step_output" >&2
    rm -f "$step_output"
    die "shell view-server tests failed; existing Electron shell was not stopped"
  fi

  # The selected profile's view port is the shell identity. Process-name/path
  # matching confused the packaged production binary with the source dev
  # Electron shell and could stop the wrong profile.
  old_pids=$(listener_pid "$view_port" || true)
  if [ -n "$old_pids" ]; then
    echo "[electron] stopping shell pid(s): $(printf '%s' "$old_pids" | tr '\n' ' ')" | tee -a "$log_file"
    for pid in $old_pids; do kill -TERM "$pid" 2>/dev/null || true; done
    attempts=40
    while [ "$attempts" -gt 0 ] && [ -n "$(listener_pid "$view_port")" ]; do
      attempts=$((attempts - 1))
      sleep 0.25
    done
    remaining=$(listener_pid "$view_port")
    if [ -n "$remaining" ]; then
      for pid in $remaining; do kill -KILL "$pid" 2>/dev/null || true; done
    fi
    # Releasing the port is not enough: a shell can throw during BrowserWindow
    # teardown, lose its listener, and remain alive behind a native error dialog.
    # Wait for the exact profile listener PID to exit before launching its
    # replacement, then force only that already-targeted shell if necessary.
    for pid in $old_pids; do
      attempts=40
      while [ "$attempts" -gt 0 ] && kill -0 "$pid" 2>/dev/null; do
        attempts=$((attempts - 1))
        sleep 0.25
      done
      if kill -0 "$pid" 2>/dev/null; then
        kill -KILL "$pid" 2>/dev/null || true
      fi
      attempts=40
      while [ "$attempts" -gt 0 ] && kill -0 "$pid" 2>/dev/null; do
        attempts=$((attempts - 1))
        sleep 0.25
      done
      kill -0 "$pid" 2>/dev/null && die "old Electron shell pid $pid did not exit" || true
    done
  fi

  attempts=40
  while [ "$attempts" -gt 0 ] && [ -n "$(listener_pid "$view_port")" ]; do
    attempts=$((attempts - 1))
    sleep 0.25
  done
  [ -z "$(listener_pid "$view_port")" ] || die "view port $view_port did not become free"

  shell_log="$log_root/electron-shell-$stamp.log"
  echo "[electron] relaunching shell" | tee -a "$log_file"
  if [ "$WORKASS_PROFILE" = "prod" ]; then
    installed_app="${WORKASS_INSTALLED_APP:-/Applications/Workass.app}"
    [ -d "$installed_app" ] || die "installed Workass app not found: $installed_app"
    open -na "$installed_app" --stdout "$shell_log" --stderr "$shell_log" \
      --env "WORKASS_CONTROLLER_RECOVERY=1"
  else
    electron_app=$(workass_electron_resolve) || die "stage the pinned runtime with scripts/vendor-electron-runtime.sh"
    open -n "$electron_app" --stdout "$shell_log" --stderr "$shell_log" \
      --env "WORKASS_PROFILE=$WORKASS_PROFILE" --env "WORKASS_PROFILE_FILE=$WORKASS_PROFILE_FILE" \
      --env "WORKASS_URL=$daemon_url" --env "WORKASS_VIEW_PORT=$view_port" \
      --env "WORKASS_DATA_ROOT=$WORKASS_DATA_ROOT" \
      --env "WORKASS_BROWSER_CONTROL_FILE=$WORKASS_BROWSER_CONTROL_FILE" \
      --env "WORKASS_CONTROLLER_RECOVERY=1" \
      --args "$repo_root/desktop/shell/main.js"
  fi

  status_url="http://127.0.0.1:$view_port/__workass-shell/status"
  wait_http "$status_url" 80 || die "Electron shell did not expose status; see $shell_log"
  attempts=80
  controller=false
  status_body=''
  while [ "$attempts" -gt 0 ]; do
    status_body=$(curl -fsS --max-time 2 "$status_url" 2>/dev/null || true)
    if printf '%s' "$status_body" | grep -q '"controller":true' && \
       printf '%s' "$status_body" | grep -Eq '"readyModelCount":[1-9][0-9]*' && \
       printf '%s' "$status_body" | grep -Eq '"browser":\{[^}]*"persistent":true[^}]*"cdpAttached":true[^}]*"agentControl":true'; then
      controller=true
      break
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  [ "$controller" = true ] || die "Electron relaunched but did not regain controller authority with a populated provider catalog; status=$status_body"

  shell_pid=$(listener_pid "$view_port")
  [ -n "$shell_pid" ] || die "Electron status passed but the shell process is missing"
  sleep 3
  stable_status=$(curl -fsS --max-time 2 "$status_url" 2>/dev/null || true)
  printf '%s' "$stable_status" | grep -q '"controller":true' || die "Electron shell did not remain controller after the stability window"
  printf '%s' "$stable_status" | grep -Eq '"readyModelCount":[1-9][0-9]*' || die "Electron provider catalog emptied during the stability window"
  printf '%s' "$stable_status" | grep -Eq '"browser":\{[^}]*"persistent":true[^}]*"cdpAttached":true[^}]*"agentControl":true' || die "Electron browser did not remain persistent, CDP-attached, and agent-controllable"
  if [ "$WORKASS_PROFILE" != "prod" ]; then
    electron_version=$(workass_electron_pinned_version)
    printf '%s' "$stable_status" | grep -Fq "\"electronVersion\":\"$electron_version\"" || die "Electron runtime does not match the pinned version $electron_version"
  fi
  kill -0 "$shell_pid" 2>/dev/null || die "Electron shell exited during the stability window; see $shell_log"
  daemon_pid_after=$(listener_pid "$daemon_port")
  [ "$daemon_pid_after" = "$daemon_pid_before" ] || die "daemon PID changed during Electron rebuild ($daemon_pid_before -> ${daemon_pid_after:-none})"

  # Capture a PNG of the freshly-relaunched view so the UI can be reviewed with
  # eyes, not guessed at. Non-fatal: a missing screenshot never fails a build.
  shot_file="$log_root/electron-$stamp.png"
  if curl -fsS --max-time 12 "http://127.0.0.1:$view_port/__workass-shell/screenshot" -o "$shot_file" 2>/dev/null && [ -s "$shot_file" ]; then
    screenshot_line="screenshot=$shot_file"
  else
    rm -f "$shot_file"
    screenshot_line="screenshot=unavailable"
  fi

  echo "ELECTRON_REBUILD_HEALTHY"
  echo "daemon_pid_before=$daemon_pid_before"
  echo "daemon_pid_after=$daemon_pid_after"
  echo "electron_pid=$shell_pid"
  echo "status=$stable_status"
  echo "$screenshot_line"
  echo "log=$log_file"
  echo "shell_log=$shell_log"
}

pick_preflight_port() {
  candidate=$((18000 + ($$ % 20000)))
  while [ "$candidate" -lt 65000 ]; do
    if [ -z "$(listener_pid "$candidate")" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
    candidate=$((candidate + 1))
  done
  return 1
}

rebuild_daemon() {
  profile=prod
  port=''
  bind=''
  state_dir=''
  label=''
  view_port=''
  target=''
  working_dir=''
  migrate_from=''
  signing_migration=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --profile) [ "$#" -ge 2 ] || die "--profile needs a value"; profile="$2"; shift 2 ;;
      --port) [ "$#" -ge 2 ] || die "--port needs a value"; port="$2"; shift 2 ;;
      --bind) [ "$#" -ge 2 ] || die "--bind needs a value"; bind="$2"; shift 2 ;;
      --state-dir) [ "$#" -ge 2 ] || die "--state-dir needs a value"; state_dir="$2"; shift 2 ;;
      --label) [ "$#" -ge 2 ] || die "--label needs a value"; label="$2"; shift 2 ;;
      --view-port) [ "$#" -ge 2 ] || die "--view-port needs a value"; view_port="$2"; shift 2 ;;
      --target) [ "$#" -ge 2 ] || die "--target needs a value"; target="$2"; shift 2 ;;
      --working-dir) [ "$#" -ge 2 ] || die "--working-dir needs a value"; working_dir="$2"; shift 2 ;;
      --migrate-from) [ "$#" -ge 2 ] || die "--migrate-from needs a value"; migrate_from="$2"; shift 2 ;;
      --migrate-signing-identity) signing_migration=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown daemon argument: $1" ;;
    esac
  done
  workass_load_profile "$profile" || exit 1
  port="${port:-$WORKASS_DAEMON_PORT}"
  bind="${bind:-$WORKASS_DAEMON_BIND}"
  state_dir="${state_dir:-$WORKASS_STATE_DIR}"
  label="${label:-$WORKASS_LAUNCHD_LABEL}"
  view_port="${view_port:-$WORKASS_VIEW_PORT}"
  if [ "$profile" = prod ]; then
    target="${target:-$WORKASS_DATA_ROOT/runtime/workass}"
  else
    target="${target:-$repo_root/dist-bin/workass-darwin-arm64}"
  fi
  working_dir="${working_dir:-$repo_root}"

  case "$state_dir" in /*) ;; *) die "--state-dir must be absolute" ;; esac
  case "$target" in /*) ;; *) die "--target must be absolute" ;; esac
  if [ "$profile" = prod ]; then
    case "$target" in
      "$log_root"/*|*.candidate|*.candidate.previous)
        die "production daemon target must not use a temporary candidate path: $target"
        ;;
    esac
  fi
  case "$working_dir" in /*) ;; *) die "--working-dir must be absolute" ;; esac
  if [ -n "$migrate_from" ]; then case "$migrate_from" in /*) ;; *) die "--migrate-from must be absolute" ;; esac; fi
  case "$bind" in localhost|lan) ;; *) die "--bind must be localhost or lan" ;; esac
  case "$label" in *[!A-Za-z0-9._-]*|'') die "invalid launchd label" ;; esac
  case "$port" in *[!0-9]*|'') die "invalid port" ;; esac
  case "$view_port" in *[!0-9]*|'') die "invalid view port" ;; esac
  # Every profile signs with the one persistent identity so a rebuild is never a
  # new application in macOS's eyes. Production requires it; development warns
  # and continues rather than blocking a build on signing setup.
  sign_candidate=1
  if [ "$profile" = prod ]; then
    workass_codesign_prepare
  elif ! workass_codesign_prepare 2>/dev/null; then
    sign_candidate=0
    echo "[daemon] warning: no persistent signing identity for profile $profile" >&2
    echo "[daemon] warning: macOS privacy grants will reset on this rebuild" >&2
  fi

  old_pid=$(listener_pid "$port")
  [ -n "$old_pid" ] || die "no daemon listener found on port $port"
  old_command=$(ps -p "$old_pid" -o command= 2>/dev/null || true)
  printf '%s' "$old_command" | grep -q 'workass' || die "port $port belongs to a non-Workass process: $old_command"
  curl -fsS --max-time 2 "http://127.0.0.1:$port/workass/health" >/dev/null || die "current daemon health check failed"
  if [ "$view_port" -gt 0 ]; then
    client_status=$(curl -fsS --max-time 2 "http://127.0.0.1:$view_port/__workass-shell/status" 2>/dev/null || true)
    printf '%s' "$client_status" | grep -q '"controller":true' || die "Electron is not the active controller before daemon handoff; status=$client_status"
    printf '%s' "$client_status" | grep -Eq '"readyModelCount":[1-9][0-9]*' || die "Electron has no populated provider catalog before daemon handoff; status=$client_status"
  fi

  mkdir -p "$log_root"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  build_log="$log_root/daemon-build-$stamp.log"
  candidate="$log_root/workass-darwin-arm64.$stamp.candidate"
  preflight_log="$log_root/daemon-preflight-$stamp.log"

  echo "[daemon] staging native provider hosts" | tee "$build_log"
  step_output="$log_root/daemon-frontier-hosts-$stamp.step"
  if (cd "$repo_root" && "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64 --offline) >"$step_output" 2>&1; then
    tee -a "$build_log" < "$step_output"
    rm -f "$step_output"
  else
    tee -a "$build_log" < "$step_output" >&2
    rm -f "$step_output"
    die "native provider host staging failed; current daemon was not stopped"
  fi
  if [ "$profile" = prod ]; then
    staged_frontier_hosts="$repo_root/dist-bin/frontier-hosts/darwin-arm64"
    installed_frontier_hosts="$(dirname -- "$target")/frontier-hosts"
    if [ ! -d "$installed_frontier_hosts" ] || \
       ! diff -qr "$staged_frontier_hosts" "$installed_frontier_hosts" >>"$build_log" 2>&1; then
      die "production native provider hosts do not match the gated bundle; install the current Workass app package before the daemon handoff"
    fi
  fi

  echo "[daemon] repository gate" | tee -a "$build_log"
  step_output="$log_root/daemon-gate-$stamp.step"
  if (cd "$repo_root" && GOCACHE="${GOCACHE:-/private/tmp/workass-gocache}" scripts/gate.sh) >"$step_output" 2>&1; then
    tee -a "$build_log" < "$step_output"
    rm -f "$step_output"
  else
    tee -a "$build_log" < "$step_output" >&2
    rm -f "$step_output"
    die "repository gate failed; current daemon was not stopped"
  fi
  echo "[daemon] syncing renderer and building candidate" | tee -a "$build_log"
  step_output="$log_root/daemon-build-$stamp.step"
  if (cd "$repo_root" && scripts/sync-renderer2.sh && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$candidate" ./cmd/workass) >"$step_output" 2>&1; then
    tee -a "$build_log" < "$step_output"
    rm -f "$step_output"
  else
    tee -a "$build_log" < "$step_output" >&2
    rm -f "$step_output"
    die "daemon candidate build failed; current daemon was not stopped"
  fi
  chmod 755 "$candidate"
  if [ "$sign_candidate" -eq 1 ]; then
    echo "[daemon] applying stable $profile signature" | tee -a "$build_log"
    workass_codesign_sign_binary "$candidate" "$WORKASS_BUNDLE_ID.daemon" >>"$build_log" 2>&1
    if [ -f "$target" ] && ! workass_codesign_mutually_compatible "$target" "$candidate"; then
      if [ "$profile" != prod ] && workass_codesign_is_legacy_adhoc "$target"; then
        # A development target that only ever carried the linker's ad-hoc
        # signature already lost its grants on every previous build. Adopting
        # the persistent identity costs one last authorization.
        echo "[daemon] adopting the persistent identity for the $profile daemon" | tee -a "$build_log"
      elif [ "$signing_migration" -ne 1 ] || ! workass_codesign_is_legacy_adhoc "$target"; then
        die "current and candidate daemon identities are incompatible; refusing a rebuild that would reset macOS privacy grants (use --migrate-signing-identity only once)"
      else
        echo "[daemon] one-time signing identity migration authorized" | tee -a "$build_log"
      fi
    fi

    # An update that installs identical code is not an update. Skipping it keeps
    # live ACP engines alive instead of restarting the daemon for nothing.
    candidate_cdhash=$(workass_codesign_cdhash "$candidate")
    target_cdhash=''
    if [ -f "$target" ]; then target_cdhash=$(workass_codesign_cdhash "$target"); fi
    if [ -z "$migrate_from" ] && [ -n "$candidate_cdhash" ] && [ "$candidate_cdhash" = "$target_cdhash" ]; then
      rm -f "$candidate"
      echo "[daemon] installed daemon already matches this build; no handoff dispatched" | tee -a "$build_log"
      echo "DAEMON_ALREADY_CURRENT target=$target cdhash=$candidate_cdhash"
      return 0
    fi
  fi

  preflight_port=$(pick_preflight_port) || die "could not reserve a preflight port"
  preflight_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-rebuild-preflight.XXXXXX")
  preflight_pid=''
  cleanup_preflight() {
    if [ -n "$preflight_pid" ]; then
      kill -TERM "$preflight_pid" 2>/dev/null || true
      wait "$preflight_pid" 2>/dev/null || true
    fi
    # The candidate daemon can outlive its wrapper PID; reap by listener too.
    stray=$(listener_pid "$preflight_port")
    if [ -n "$stray" ]; then
      kill -TERM $stray 2>/dev/null || true
      sleep 1
      stray=$(listener_pid "$preflight_port")
      [ -n "$stray" ] && kill -KILL $stray 2>/dev/null || true
    fi
    rm -rf "$preflight_root"
  }
  trap cleanup_preflight EXIT HUP INT TERM
  # exec so preflight_pid is the daemon itself, not a subshell wrapper whose
  # death would orphan the candidate (observed: orphans surviving for hours).
  (cd "$repo_root" && exec "$candidate" --port "$preflight_port" --bind localhost --state-dir "$preflight_root/state") >"$preflight_log" 2>&1 &
  preflight_pid=$!
  wait_http "http://127.0.0.1:$preflight_port/workass/health" 80 || die "candidate preflight failed; see $preflight_log"
  kill -TERM "$preflight_pid" 2>/dev/null || true
  wait "$preflight_pid" 2>/dev/null || true
  preflight_pid=''
  stray=$(listener_pid "$preflight_port")
  if [ -n "$stray" ]; then
    kill -TERM $stray 2>/dev/null || true
    sleep 1
    stray=$(listener_pid "$preflight_port")
    [ -n "$stray" ] && kill -KILL $stray 2>/dev/null || true
  fi
  rm -rf "$preflight_root"
  trap - EXIT HUP INT TERM

  handoff_id="$stamp-$$"
  handoff_label="com.workass.rebuild.$handoff_id"
  handoff_log="$log_root/daemon-handoff-$handoff_id.log"
  status_file="$log_root/daemon-handoff-$handoff_id.status"
  handoff_plist="$log_root/daemon-handoff-$handoff_id.plist"
  printf 'phase=prepared\nold_pid=%s\nport=%s\nlabel=%s\n' "$old_pid" "$port" "$label" > "$status_file"

  worker="$repo_root/scripts/macos/rebuild-workass-daemon-worker.sh"
  [ -x "$worker" ] || die "handoff worker is not executable: $worker"
  echo "[daemon] candidate healthy; dispatching detached handoff"
  echo "log=$handoff_log"
  echo "status=$status_file"
  echo "handoff_plist=$handoff_plist"
  echo "The current ACP turn may disconnect after dispatch; inspect the status file after reconnect."
  plutil -create xml1 "$handoff_plist"
  plutil -insert Label -string "$handoff_label" "$handoff_plist"
  plutil -insert ProgramArguments -array "$handoff_plist"
  argument_index=0
  runtime_path="${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}"
  runtime_home="$HOME"
  for argument in "$worker" "$repo_root" "$candidate" "$old_pid" "$port" "$bind" "$state_dir" "$label" "$handoff_log" "$status_file" "$view_port" "$runtime_path" "$runtime_home" "$target" "$working_dir" "$migrate_from"; do
    plutil -insert "ProgramArguments.$argument_index" -string "$argument" "$handoff_plist"
    argument_index=$((argument_index + 1))
  done
  plutil -insert RunAtLoad -bool true "$handoff_plist"
  plutil -insert KeepAlive -bool false "$handoff_plist"
  plutil -insert ProcessType -string Background "$handoff_plist"
  plutil -insert EnvironmentVariables -xml '<dict/>' "$handoff_plist"
  plutil -insert EnvironmentVariables.PATH -string "$runtime_path" "$handoff_plist"
  plutil -insert EnvironmentVariables.HOME -string "$runtime_home" "$handoff_plist"
  plutil -insert EnvironmentVariables.WORKASS_PROFILE -string "$WORKASS_PROFILE" "$handoff_plist"
  plutil -insert EnvironmentVariables.WORKASS_DATA_ROOT -string "$WORKASS_DATA_ROOT" "$handoff_plist"
  plutil -insert EnvironmentVariables.WORKASS_BROWSER_CONTROL_FILE -string "$WORKASS_BROWSER_CONTROL_FILE" "$handoff_plist"
  plutil -insert EnvironmentVariables.WORKASS_LOG_ROOT -string "$WORKASS_LOG_ROOT" "$handoff_plist"
  # Each handoff registers its own launchd label and launchd keeps finished job
  # records forever, which turns every rebuild into one more background item.
  # Sweep the dead ones before adding another.
  launchctl list |
    awk '$1 == "-" && $3 ~ /^com\.workass\.rebuild\./ { print $3 }' |
    while IFS= read -r stale_label; do
      [ "$stale_label" = "$handoff_label" ] && continue
      launchctl bootout "gui/$(id -u)/$stale_label" >/dev/null 2>&1 || true
    done
  launchctl bootstrap "gui/$(id -u)" "$handoff_plist"
  echo "DAEMON_HANDOFF_DISPATCHED label=$handoff_label"
}

require_macos
[ "$#" -ge 1 ] || { usage >&2; exit 2; }
mode="$1"
shift
case "$mode" in
  electron) rebuild_electron "$@" ;;
  daemon) rebuild_daemon "$@" ;;
  -h|--help) usage ;;
  *) usage >&2; die "unknown mode: $mode" ;;
esac
