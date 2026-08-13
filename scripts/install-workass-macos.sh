#!/bin/sh
# Install one already-built, self-contained Workass.app candidate.
#
# This script intentionally has no compiler, npm, source-build, or repository
# gate dependency. Building and proving a candidate is one transaction;
# activating those immutable bytes is a separate, short transaction that can
# be retried without rebuilding anything.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"
workass_load_profile prod
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

candidate=''
install_root=/Applications
signing_migration=0

usage() {
  echo "usage: scripts/install-workass-macos.sh --candidate ABSOLUTE.app [--install-root DIR] [--migrate-signing-identity]"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --candidate) [ "$#" -ge 2 ] || { echo "--candidate needs a value" >&2; exit 2; }; candidate="$2"; shift 2 ;;
    --install-root) [ "$#" -ge 2 ] || { echo "--install-root needs a value" >&2; exit 2; }; install_root="$2"; shift 2 ;;
    --migrate-signing-identity) signing_migration=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "macOS installation requires Darwin" >&2; exit 1; }
case "$candidate" in /*.app) ;; *) echo "--candidate must be an absolute .app path" >&2; exit 2 ;; esac
case "$install_root" in /*) ;; *) echo "--install-root must be absolute" >&2; exit 2 ;; esac
[ "$install_root" != / ] || { echo "refusing unsafe install root: /" >&2; exit 2; }
[ -d "$candidate" ] || { echo "candidate app not found: $candidate" >&2; exit 1; }

installed="$install_root/Workass.app"
[ "$candidate" != "$installed" ] || { echo "candidate must not be the installed app" >&2; exit 2; }
candidate_plist="$candidate/Contents/Info.plist"
shell_package="$candidate/Contents/Resources/app/package.json"
runtime_manifest="$candidate/Contents/Resources/runtime/manifest.json"
[ -f "$candidate_plist" ] || { echo "candidate Info.plist is missing" >&2; exit 1; }
[ -f "$shell_package" ] || { echo "candidate Electron package manifest is missing" >&2; exit 1; }
[ -f "$runtime_manifest" ] || { echo "candidate runtime manifest is missing" >&2; exit 1; }
[ -x "$candidate/Contents/Resources/runtime/workass" ] || { echo "candidate daemon is missing" >&2; exit 1; }

target_version=$(plutil -extract CFBundleShortVersionString raw -o - "$candidate_plist")
target_build=$(plutil -extract CFBundleVersion raw -o - "$candidate_plist")
target_bundle_id=$(plutil -extract CFBundleIdentifier raw -o - "$candidate_plist")
target_executable=$(plutil -extract CFBundleExecutable raw -o - "$candidate_plist")
shell_version=$(plutil -extract version raw -o - "$shell_package")
runtime_version=$(plutil -extract version raw -o - "$runtime_manifest")
runtime_platform=$(plutil -extract platform raw -o - "$runtime_manifest")
[ "$target_bundle_id" = "$WORKASS_BUNDLE_ID" ] || { echo "candidate bundle identifier is not Workass" >&2; exit 1; }
[ "$target_executable" = Workass ] || { echo "candidate executable is not Workass" >&2; exit 1; }
[ "$runtime_platform" = darwin ] || { echo "candidate runtime targets another platform" >&2; exit 1; }
[ "$shell_version" = "$target_version" ] || { echo "candidate Info.plist and Electron package versions differ" >&2; exit 1; }
[ "$runtime_version" = "$target_version" ] || { echo "candidate shell and daemon versions differ" >&2; exit 1; }

workass_codesign_prepare
workass_codesign_verify_stable "$candidate" true
if [ -d "$installed" ] && ! workass_codesign_mutually_compatible "$installed" "$candidate"; then
  if ! codesign --verify --strict "$installed" >/dev/null 2>&1; then
    echo "the installed Workass signature seal is broken; refusing to replace it automatically" >&2
    exit 1
  elif [ "$signing_migration" -eq 1 ] && workass_codesign_is_adhoc_cdhash "$installed"; then
    echo "[install] one-time signing identity migration authorized"
  else
    echo "installed and candidate Workass identities are incompatible" >&2
    echo "refusing an install that would reset macOS privacy grants" >&2
    exit 1
  fi
fi

mkdir -p "$install_root" "$WORKASS_LOG_ROOT" "$repo_root/.dev/package"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
log_file="$repo_root/.dev/package/install-$stamp.log"
incoming="$install_root/.Workass.app.incoming-$stamp-$$"
backup="$install_root/.Workass.app.previous-$stamp-$$"
launch_agent_path="$HOME/Library/LaunchAgents/$WORKASS_LAUNCHD_LABEL.plist"
launch_agent_backup="$repo_root/.dev/package/launch-agent-$stamp.previous.plist"
: > "$log_file"

installed_shell_pids() {
  {
    lsof -nP -tiTCP:"$WORKASS_VIEW_PORT" -sTCP:LISTEN 2>/dev/null || true
    ps -axo pid=,command= | awk -v exe="$installed/Contents/MacOS/Workass" '$2 == exe { print $1 }'
  } | awk 'NF && !seen[$1]++ { print $1 }'
}

stop_installed_shell() {
  shell_pids=$(installed_shell_pids)
  for shell_pid in $shell_pids; do kill -TERM "$shell_pid" 2>/dev/null || true; done
  attempts=80
  while [ "$attempts" -gt 0 ]; do
    remaining=''
    for shell_pid in $shell_pids; do
      if kill -0 "$shell_pid" 2>/dev/null; then remaining="$remaining $shell_pid"; fi
    done
    [ -z "$remaining" ] && return 0
    attempts=$((attempts - 1))
    sleep 0.25
  done
  for shell_pid in $shell_pids; do
    if kill -0 "$shell_pid" 2>/dev/null; then kill -KILL "$shell_pid" 2>/dev/null || true; fi
  done
  attempts=40
  while [ "$attempts" -gt 0 ]; do
    remaining=''
    for shell_pid in $shell_pids; do
      if kill -0 "$shell_pid" 2>/dev/null; then remaining="$remaining $shell_pid"; fi
    done
    [ -z "$remaining" ] && return 0
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

launch_installed() {
  open -na "$installed" --stdout "$WORKASS_LOG_ROOT/shell.out.log" --stderr "$WORKASS_LOG_ROOT/shell.err.log" \
    --env "WORKASS_CONTROLLER_RECOVERY=1"
}

shell_recovered() {
  shell_status=$(curl -fsS --max-time 2 "http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/status" 2>/dev/null || true)
  printf '%s' "$shell_status" | grep -q '"controller":true' &&
    printf '%s' "$shell_status" | grep -Eq '"readyModelCount":[1-9][0-9]*'
}

recover_shell_in_place() {
  curl -fsS --max-time 2 -X POST \
    "http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/reload?recoverController=1" \
    >/dev/null 2>&1
}

new_release_healthy() {
  shell_status=$(curl -fsS --max-time 2 "http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/status" 2>/dev/null || true)
  daemon_status=$(curl -kfsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" 2>/dev/null || true)
  printf '%s' "$shell_status" | grep -q '"controller":true' &&
    printf '%s' "$shell_status" | grep -Eq '"readyModelCount":[1-9][0-9]*' &&
    printf '%s' "$shell_status" | grep -Fq "\"appVersion\":\"$target_version\"" &&
    printf '%s' "$shell_status" | grep -Eq '"browser":\{[^}]*"persistent":true[^}]*"cdpAttached":true[^}]*"agentControl":true' &&
    printf '%s' "$daemon_status" | grep -Fq "\"version\":\"$target_version\"" &&
    printf '%s' "$daemon_status" | grep -q '"secure":true'
}

wait_for_new_release() {
  attempts=240
  recovery_attempted=0
  while [ "$attempts" -gt 0 ]; do
    new_release_healthy && return 0
    # A promoted shell can win its listener race before the renderer's first
    # bridge request reaches the new daemon. The shell owns a bounded in-place
    # recovery path for exactly that lifecycle gap; use it once after five
    # seconds instead of relaunching Electron or waiting blindly for rollback.
    if [ "$attempts" -le 220 ] && [ "$recovery_attempted" -eq 0 ] && recover_shell_in_place; then
      recovery_attempted=1
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

wait_for_previous_release() {
  attempts=240
  recovery_attempted=0
  while [ "$attempts" -gt 0 ]; do
    shell_recovered && return 0
    if [ "$attempts" -le 220 ] && [ "$recovery_attempted" -eq 0 ] && recover_shell_in_place; then
      recovery_attempted=1
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

rollback_install() {
  reason="$1"
  stop_installed_shell || true
  launchd_domain="gui/$(id -u)"
  launchctl bootout "$launchd_domain" "$launch_agent_path" >/dev/null 2>&1 || true
  if [ -d "$backup" ]; then
    rm -rf "$installed"
    mv "$backup" "$installed"
    /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$installed" >/dev/null 2>&1 || true
    if [ -f "$launch_agent_backup" ]; then
      cp "$launch_agent_backup" "$launch_agent_path"
      launchctl bootstrap "$launchd_domain" "$launch_agent_path" >/dev/null 2>&1 || true
      launchctl enable "$launchd_domain/$WORKASS_LAUNCHD_LABEL" >/dev/null 2>&1 || true
      launchctl kickstart -k "$launchd_domain/$WORKASS_LAUNCHD_LABEL" >/dev/null 2>&1 || true
    else
      rm -f "$launch_agent_path"
    fi
    launch_installed >/dev/null 2>&1 || true
    if ! wait_for_previous_release; then
      echo "rollback could not recover the previous Workass runtime" >> "$log_file"
    fi
  fi
  rm -rf "$incoming"
  rm -f "$launch_agent_backup"
  echo "Workass install rolled back: $reason" | tee -a "$log_file" >&2
  exit 1
}

# A launch wrapper can be retried by launchd after a successful install. Never
# turn that wrapper mistake into a shell restart loop: once the exact signed
# bundle is already installed and the joint runtime health gate passes, the
# installer is complete and must not stop either process again.
if [ -d "$installed" ] && workass_codesign_verify_stable "$installed" true >/dev/null 2>&1; then
  candidate_cdhash=$(workass_codesign_cdhash "$candidate")
  installed_cdhash=$(workass_codesign_cdhash "$installed")
  if [ -n "$candidate_cdhash" ] && [ "$candidate_cdhash" = "$installed_cdhash" ] && new_release_healthy; then
    echo "WORKASS_MACOS_INSTALL_ALREADY_CURRENT"
    echo "app=$installed"
    echo "version=$target_version"
    echo "build=$target_build"
    echo "daemon_url=$WORKASS_DAEMON_URL"
    echo "view_port=$WORKASS_VIEW_PORT"
    echo "log=$log_file"
    exit 0
  fi
fi

rm -rf "$incoming" "$backup"
ditto "$candidate" "$incoming"
workass_codesign_verify_stable "$incoming" true >> "$log_file" 2>&1
if [ -f "$launch_agent_path" ]; then cp "$launch_agent_path" "$launch_agent_backup"; fi

# The slow build has already finished. From here to the terminal health receipt
# every operation is a bounded same-volume install or rollback step.
if ! stop_installed_shell; then
  rm -rf "$incoming"
  echo "old Workass shell did not stop; installed bytes were not changed" >&2
  exit 1
fi

if [ -d "$installed" ]; then mv "$installed" "$backup"; fi
if ! mv "$incoming" "$installed"; then rollback_install "could not activate the candidate app"; fi
touch "$installed" || rollback_install "could not finalize the installed app"
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$installed" ||
  rollback_install "LaunchServices registration failed"
workass_codesign_verify_stable "$installed" true >> "$log_file" 2>&1 ||
  rollback_install "installed signature verification failed"

launch_installed || rollback_install "new Workass app could not be launched"
wait_for_new_release || rollback_install "new Electron and daemon release did not pass the joint health gate"

rm -rf "$backup"
rm -f "$launch_agent_backup"
shot_file="$repo_root/.dev/package/install-$stamp.png"
if ! curl -fsS --max-time 12 "http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/screenshot" -o "$shot_file" 2>/dev/null || [ ! -s "$shot_file" ]; then
  rm -f "$shot_file"
  shot_file=unavailable
fi

echo "WORKASS_MACOS_INSTALL_HEALTHY"
echo "app=$installed"
echo "version=$target_version"
echo "build=$target_build"
echo "daemon_url=$WORKASS_DAEMON_URL"
echo "view_port=$WORKASS_VIEW_PORT"
echo "screenshot=$shot_file"
echo "log=$log_file"
