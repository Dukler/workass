#!/bin/sh
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
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-electron.sh"

icon_source="$repo_root/desktop/assets/workass-macos.png"
bundle_build=$(date -u +%Y%m%d%H%M%S)
bundle_version=0.1.0
install_root=/Applications
launch=1
signing_migration=0
artifact_output=''
release_signing=0
portable_runtime=0
release_arch=arm64
runtime_input_root="$repo_root/dist-bin"
electron_app_input=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --icon) [ "$#" -ge 2 ] || { echo "--icon needs a value" >&2; exit 2; }; icon_source="$2"; shift 2 ;;
    --install-root) [ "$#" -ge 2 ] || { echo "--install-root needs a value" >&2; exit 2; }; install_root="$2"; shift 2 ;;
    --no-launch) launch=0; shift ;;
    --migrate-signing-identity) signing_migration=1; shift ;;
    --artifact-only) [ "$#" -ge 2 ] || { echo "--artifact-only needs a value" >&2; exit 2; }; artifact_output="$2"; shift 2 ;;
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; bundle_version="$2"; shift 2 ;;
    --build-number) [ "$#" -ge 2 ] || { echo "--build-number needs a value" >&2; exit 2; }; bundle_build="$2"; shift 2 ;;
    --release-signing) release_signing=1; shift ;;
    --portable-runtime) portable_runtime=1; shift ;;
    --arch) [ "$#" -ge 2 ] || { echo "--arch needs a value" >&2; exit 2; }; release_arch="$2"; shift 2 ;;
    --runtime-root) [ "$#" -ge 2 ] || { echo "--runtime-root needs a value" >&2; exit 2; }; runtime_input_root="$2"; shift 2 ;;
    --electron-app) [ "$#" -ge 2 ] || { echo "--electron-app needs a value" >&2; exit 2; }; electron_app_input="$2"; shift 2 ;;
    -h|--help)
      echo "usage: scripts/package-workass-macos.sh [--icon PNG] [--install-root DIR] [--no-launch] [--migrate-signing-identity] [--artifact-only APP] [--version X.Y.Z] [--build-number N] [--release-signing] [--portable-runtime] [--arch arm64] [--runtime-root DIR] [--electron-app APP]"
      exit 0
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "macOS packaging requires Darwin" >&2; exit 1; }
case "$install_root" in /*) ;; *) echo "--install-root must be absolute" >&2; exit 2 ;; esac
case "$artifact_output" in ''|/*.app) ;; *) echo "--artifact-only must be an absolute .app path" >&2; exit 2 ;; esac
case "$runtime_input_root" in /*) ;; *) echo "--runtime-root must be absolute" >&2; exit 2 ;; esac
case "$electron_app_input" in ''|/*.app) ;; *) echo "--electron-app must be an absolute .app path" >&2; exit 2 ;; esac
printf '%s' "$bundle_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "--version must be X.Y.Z" >&2; exit 2; }
printf '%s' "$bundle_build" | grep -Eq '^[1-9][0-9]*$' || { echo "--build-number must be a positive integer" >&2; exit 2; }
[ "$release_arch" = arm64 ] || { echo "this release lane currently supports --arch arm64 only" >&2; exit 2; }
[ "$release_signing" -eq 0 ] || [ "$portable_runtime" -eq 1 ] || { echo "--release-signing requires --portable-runtime" >&2; exit 2; }
[ -f "$icon_source" ] || { echo "icon not found: $icon_source" >&2; exit 1; }
electron_app=$(workass_electron_resolve "$electron_app_input") || {
  echo "stage the pinned runtime with scripts/vendor-electron-runtime.sh" >&2
  exit 1
}
if [ -z "$artifact_output" ]; then
  curl -fsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" >/dev/null || {
    echo "production daemon is not healthy at $WORKASS_DAEMON_URL" >&2
    exit 1
  }
fi
if [ "$release_signing" -eq 1 ]; then workass_codesign_prepare distribution; else workass_codesign_prepare; fi

package_root="$repo_root/.dev/package"
stage="$package_root/Workass.app"
iconset="$package_root/Workass.iconset"
log_file="$package_root/package-$(date -u +%Y%m%dT%H%M%SZ).log"
mkdir -p "$package_root" "$WORKASS_LOG_ROOT" "$WORKASS_DATA_ROOT/runtime"
: > "$log_file"

if [ -z "$artifact_output" ] && [ "$runtime_input_root" = "$repo_root/dist-bin" ]; then
  echo "[package] staging native provider hosts" | tee -a "$log_file"
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64 --offline >>"$log_file" 2>&1
fi

echo "[package] building renderer" | tee -a "$log_file"
(cd "$repo_root" && npm run build --prefix desktop/renderer2) >>"$log_file" 2>&1
echo "[package] testing shell and profile isolation" | tee -a "$log_file"
(cd "$repo_root" && node --test \
  desktop/shell/runtime-profile.test.js \
  desktop/shell/runtime-bootstrap.test.js \
  desktop/shell/view-server.test.js \
  desktop/shell/browser-manager.test.js \
  desktop/shell/browser-control-server.test.js \
  desktop/shell/app-icon.test.js \
  desktop/shell/image-copy.test.js \
  desktop/shell/profile-singleton.test.js \
  scripts/tests/package-workass-macos.test.mjs \
  scripts/tests/migrate-workass-chats.test.mjs) >>"$log_file" 2>&1
(cd "$repo_root" && scripts/tests/workass-codesign.test.sh) >>"$log_file" 2>&1
(cd "$repo_root" && scripts/tests/workass-signing-persistence.test.sh) >>"$log_file" 2>&1

rm -rf "$stage" "$iconset"
ditto "$electron_app" "$stage"
mkdir -p "$stage/Contents/Resources/app" "$stage/Contents/Resources/renderer" "$iconset"

mv "$stage/Contents/MacOS/Electron" "$stage/Contents/MacOS/Workass"
rm -f "$stage/Contents/Resources/default_app.asar"
for shell_file in main.js preload.js view-server.js browser-manager.js browser-control-server.js runtime-profile.js runtime-bootstrap.js app-icon.js image-copy.js profile-singleton.js; do
  cp "$repo_root/desktop/shell/$shell_file" "$stage/Contents/Resources/app/$shell_file"
done
cp "$repo_root/desktop/shell/package.production.json" "$stage/Contents/Resources/app/package.json"
ditto "$repo_root/desktop/renderer2/dist" "$stage/Contents/Resources/renderer"
cp "$repo_root/config/environments/prod.env" "$stage/Contents/Resources/workass-profile.env"
cp "$icon_source" "$stage/Contents/Resources/Workass.png"

for spec in '16 icon_16x16.png' '32 icon_16x16@2x.png' '32 icon_32x32.png' \
            '64 icon_32x32@2x.png' '128 icon_128x128.png' '256 icon_128x128@2x.png' \
            '256 icon_256x256.png' '512 icon_256x256@2x.png' '512 icon_512x512.png' \
            '1024 icon_512x512@2x.png'; do
  size=${spec%% *}
  name=${spec#* }
  sips -s format png -z "$size" "$size" "$icon_source" --out "$iconset/$name" >/dev/null
done
iconutil -c icns "$iconset" -o "$stage/Contents/Resources/Workass.icns"
rm -rf "$iconset"

plist="$stage/Contents/Info.plist"
plutil -replace CFBundleDisplayName -string Workass "$plist"
plutil -replace CFBundleExecutable -string Workass "$plist"
plutil -replace CFBundleIconFile -string Workass.icns "$plist"
plutil -replace CFBundleIdentifier -string "$WORKASS_BUNDLE_ID" "$plist"
plutil -replace CFBundleName -string Workass "$plist"
plutil -replace CFBundleShortVersionString -string "$bundle_version" "$plist"
plutil -replace CFBundleVersion -string "$bundle_build" "$plist"
plutil -remove ElectronAsarIntegrity "$plist" >/dev/null 2>&1 || true

if [ "$portable_runtime" -eq 1 ]; then
  daemon_source="$runtime_input_root/workass"
  frontier_hosts_source="$runtime_input_root/frontier-hosts/darwin-arm64"
  node_source="$runtime_input_root/node/darwin-arm64"
  [ -x "$daemon_source" ] || { echo "portable daemon is missing from runtime root: $daemon_source" >&2; exit 1; }
  [ -x "$frontier_hosts_source/claude-native-host.mjs" ] && \
    [ -x "$frontier_hosts_source/codex-native-host.mjs" ] && \
    [ -f "$frontier_hosts_source/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs" ] || {
    echo "portable native provider hosts are missing: run scripts/vendor-frontier-hosts.sh" >&2
    exit 1
  }
  [ -x "$node_source/bin/node" ] || {
    echo "portable Node runtime is missing: run scripts/vendor-node-runtime.sh --target darwin-arm64" >&2
    exit 1
  }
  runtime_stage="$stage/Contents/Resources/runtime"
  mkdir -p "$runtime_stage/frontier-hosts" "$runtime_stage/node"
  cp "$daemon_source" "$runtime_stage/workass"
  chmod 755 "$runtime_stage/workass"
  ditto "$frontier_hosts_source" "$runtime_stage/frontier-hosts/darwin-arm64"
  ditto "$node_source" "$runtime_stage/node/darwin-arm64"
  printf '{"schemaVersion":1,"platform":"darwin","arch":"%s","version":"%s","build":"%s"}\n' \
    "$release_arch" "$bundle_version" "$bundle_build" > "$runtime_stage/manifest.json"
fi

xattr -cr "$stage"
if [ "$release_signing" -eq 1 ]; then
  workass_codesign_sign_app_distribution "$stage" "$repo_root/config/macos/entitlements.plist" >>"$log_file" 2>&1
else
  workass_codesign_sign_app "$stage" >>"$log_file" 2>&1
fi

if [ -n "$artifact_output" ]; then
  rm -rf "$artifact_output"
  mkdir -p "$(dirname -- "$artifact_output")"
  ditto "$stage" "$artifact_output"
  if [ "$release_signing" -eq 1 ]; then
    workass_codesign_verify_distribution "$artifact_output" true >>"$log_file" 2>&1
  else
    workass_codesign_verify_stable "$artifact_output" true >>"$log_file" 2>&1
  fi
  echo "WORKASS_MACOS_APP_ARTIFACT_READY"
  echo "app=$artifact_output"
  echo "version=$bundle_version"
  echo "build=$bundle_build"
  echo "portable_runtime=$portable_runtime"
  echo "log=$log_file"
  exit 0
fi

installed="$install_root/Workass.app"
if [ -d "$installed" ] && ! workass_codesign_mutually_compatible "$installed" "$stage"; then
  if [ "$signing_migration" -ne 1 ] || ! workass_codesign_is_legacy_adhoc "$installed"; then
    # The compatibility test is mutual --verify --strict, so it also fails when
    # the identities match and one bundle's SEAL is broken. Say which it is: the
    # generic "identities are incompatible" wording sent a real investigation
    # down the wrong path on 2026-07-25, when the true cause was a renderer
    # copied into the installed bundle after signing.
    if ! codesign --verify --strict "$installed" >/dev/null 2>&1; then
      echo "the INSTALLED Workass bundle's seal is broken, so this update cannot be verified" >&2
      echo "its identity is unchanged; files were written into it after it was signed:" >&2
      codesign --verify --strict --verbose=4 "$installed" 2>&1 \
        | grep -E '^file (added|modified|missing):' | head -12 >&2
      echo "never copy a build into $installed; re-run this packager to reseal it" >&2
    else
      echo "installed and staged Workass identities are incompatible" >&2
      echo "use --migrate-signing-identity only for the one-time move away from the old ad-hoc build" >&2
    fi
    echo "refusing an update that would reset macOS privacy grants" >&2
    exit 1
  fi
  echo "[package] one-time signing identity migration authorized" | tee -a "$log_file"
fi

# Stage the production ACP runtime separately from the repository. The daemon
# handoff replaces this bootstrap binary with the gate-tested candidate.
if [ -x "$repo_root/dist-bin/workass-darwin-arm64" ]; then
  # Stage and sign beside the target, then swap. Copying onto the live binary
  # mutates the running daemon's mapped image, which the kernel kills for an
  # invalid signature.
  runtime_incoming="$WORKASS_DATA_ROOT/runtime/.workass.incoming.$$"
  cp "$repo_root/dist-bin/workass-darwin-arm64" "$runtime_incoming"
  chmod 755 "$runtime_incoming"
  workass_codesign_sign_binary \
    "$runtime_incoming" \
    "$WORKASS_BUNDLE_ID.daemon" >>"$log_file" 2>&1
  if [ -f "$WORKASS_DATA_ROOT/runtime/workass" ] && \
     [ "$(workass_codesign_cdhash "$runtime_incoming")" = "$(workass_codesign_cdhash "$WORKASS_DATA_ROOT/runtime/workass")" ]; then
    rm -f "$runtime_incoming"
    echo "[package] staged runtime already matches the installed daemon" | tee -a "$log_file"
  else
    mv "$runtime_incoming" "$WORKASS_DATA_ROOT/runtime/workass"
  fi
fi
rm -rf "$WORKASS_DATA_ROOT/runtime/adapters"
if [ -d "$repo_root/dist-bin/frontier-hosts/darwin-arm64" ]; then
  rm -rf "$WORKASS_DATA_ROOT/runtime/frontier-hosts"
  ditto "$repo_root/dist-bin/frontier-hosts/darwin-arm64" "$WORKASS_DATA_ROOT/runtime/frontier-hosts"
fi

mkdir -p "$install_root"
incoming="$install_root/.Workass.app.incoming.$$"
backup="$install_root/.Workass.app.previous.$$"

installed_shell_pids() {
  {
    lsof -nP -tiTCP:"$WORKASS_VIEW_PORT" -sTCP:LISTEN 2>/dev/null || true
    ps -axo pid=,command= | awk -v exe="$installed/Contents/MacOS/Workass" '$2 == exe { print $1 }'
  } | awk 'NF && !seen[$1]++ { print $1 }'
}

stop_installed_shell() {
  shell_pids=$(installed_shell_pids)
  for pid in $shell_pids; do kill -TERM "$pid" 2>/dev/null || true; done
  attempts=80
  while [ "$attempts" -gt 0 ]; do
    remaining=''
    for pid in $shell_pids; do
      if kill -0 "$pid" 2>/dev/null; then remaining="$remaining $pid"; fi
    done
    [ -z "$remaining" ] && return 0
    attempts=$((attempts - 1))
    sleep 0.25
  done
  for pid in $shell_pids; do
    if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
  done
  attempts=40
  while [ "$attempts" -gt 0 ]; do
    remaining=''
    for pid in $shell_pids; do
      if kill -0 "$pid" 2>/dev/null; then remaining="$remaining $pid"; fi
    done
    [ -z "$remaining" ] && return 0
    attempts=$((attempts - 1))
    sleep 0.25
  done
  return 1
}

rollback_install() {
  reason="$1"
  stop_installed_shell || true
  rm -rf "$installed"
  if [ -d "$backup" ]; then
    mv "$backup" "$installed"
    /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$installed"
    if [ "$launch" -eq 1 ]; then
      open -na "$installed" --stdout "$WORKASS_LOG_ROOT/shell.out.log" --stderr "$WORKASS_LOG_ROOT/shell.err.log" \
        --env "WORKASS_CONTROLLER_RECOVERY=1" || true
    fi
  fi
  echo "Workass update rolled back: $reason" >&2
  exit 1
}

rm -rf "$incoming"
ditto "$stage" "$incoming"
workass_codesign_verify_stable "$incoming" true >>"$log_file" 2>&1

# Never replace on-disk code underneath a running process. Besides being a
# broken update transaction, that makes TCC inspect old memory against new
# bytes and invalidates identity attribution.
if ! stop_installed_shell; then
  echo "old Workass process did not stop; installed bundle was not changed" >&2
  exit 1
fi

rm -rf "$backup"
if [ -d "$installed" ]; then mv "$installed" "$backup"; fi
if ! mv "$incoming" "$installed"; then
  rollback_install "could not move the staged app into place"
fi
touch "$installed" || rollback_install "could not finalize the installed app"
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$installed" ||
  rollback_install "LaunchServices registration failed"
workass_codesign_verify_stable "$installed" true >>"$log_file" 2>&1 ||
  rollback_install "installed signature verification failed"

if [ "$launch" -eq 1 ]; then
  if ! open -na "$installed" --stdout "$WORKASS_LOG_ROOT/shell.out.log" --stderr "$WORKASS_LOG_ROOT/shell.err.log" \
    --env "WORKASS_CONTROLLER_RECOVERY=1"; then
    rollback_install "new app could not be launched"
  fi
  status_url="http://127.0.0.1:$WORKASS_VIEW_PORT/__workass-shell/status"
  attempts=160
  status_body=''
  while [ "$attempts" -gt 0 ]; do
    status_body=$(curl -fsS --max-time 2 "$status_url" 2>/dev/null || true)
    if printf '%s' "$status_body" | grep -q '"controller":true' && \
       printf '%s' "$status_body" | grep -Eq '"readyModelCount":[1-9][0-9]*'; then
      break
    fi
    attempts=$((attempts - 1))
    sleep 0.25
  done
  printf '%s' "$status_body" | grep -q '"controller":true' || rollback_install "new app did not become controller"
  printf '%s' "$status_body" | grep -Eq '"readyModelCount":[1-9][0-9]*' || rollback_install "new app has no provider catalog"
fi
rm -rf "$backup"

echo "WORKASS_MACOS_PACKAGE_HEALTHY"
echo "app=$installed"
echo "profile=$WORKASS_PROFILE"
echo "daemon_url=$WORKASS_DAEMON_URL"
echo "view_port=$WORKASS_VIEW_PORT"
echo "data_root=$WORKASS_DATA_ROOT"
echo "dev_view_port=8799"
echo "log=$log_file"
