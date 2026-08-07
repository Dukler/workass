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
# Every installed Workass is one self-contained release. The old shell-only
# default made it possible to update Electron while leaving an unrelated daemon
# behind in Application Support; the transactional in-app updater cannot permit
# that split-brain layout.
portable_runtime=1
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
for build_tool in node npm; do
  command -v "$build_tool" >/dev/null 2>&1 || {
    echo "missing build tool before package gate: $build_tool" >&2
    exit 1
  }
done
if [ "$runtime_input_root" = "$repo_root/dist-bin" ]; then
  command -v go >/dev/null 2>&1 || {
    echo "missing build tool before package gate: go" >&2
    exit 1
  }
fi
if [ -z "$artifact_output" ]; then
  if ! curl -kfsS --max-time 2 "$WORKASS_DAEMON_URL/workass/health" >/dev/null 2>&1 && \
     ! curl -fsS --max-time 2 "http://127.0.0.1:$WORKASS_DAEMON_PORT/workass/health" >/dev/null 2>&1; then
    echo "production daemon is not healthy at $WORKASS_DAEMON_URL" >&2
    exit 1
  fi
fi
if [ "$release_signing" -eq 1 ]; then workass_codesign_prepare distribution; else workass_codesign_prepare; fi

package_root="$repo_root/.dev/package"
stage="$package_root/Workass.app"
iconset="$package_root/Workass.iconset"
log_file="$package_root/package-$(date -u +%Y%m%dT%H%M%SZ).log"
mkdir -p "$package_root" "$WORKASS_LOG_ROOT"
: > "$log_file"
packaged_daemon=''

# Packaging/lifecycle contracts are fast and must fail before the expensive Go
# gate. A stale installer assertion must never waste a complete ACP run and
# force the candidate build to start over minutes later.
echo "[package] testing shell, installer, signing, and profile isolation" | tee -a "$log_file"
(cd "$repo_root" && node --test \
  desktop/shell/runtime-profile.test.js \
  desktop/shell/runtime-bootstrap.test.js \
  desktop/shell/view-server.test.js \
  desktop/shell/browser-manager.test.js \
  desktop/shell/browser-control-server.test.js \
  desktop/shell/app-icon.test.js \
  desktop/shell/image-copy.test.js \
  desktop/shell/profile-singleton.test.js \
  desktop/shell/update-manager.test.js \
  desktop/shell/update-worker.test.js \
  scripts/tests/package-workass-macos.test.mjs \
  scripts/tests/migrate-workass-chats.test.mjs) >>"$log_file" 2>&1
(cd "$repo_root" && scripts/tests/workass-codesign.test.sh) >>"$log_file" 2>&1
(cd "$repo_root" && scripts/tests/workass-signing-persistence.test.sh) >>"$log_file" 2>&1

if [ "$runtime_input_root" = "$repo_root/dist-bin" ]; then
  echo "[package] repository gate" | tee -a "$log_file"
  (cd "$repo_root" && GOCACHE="${GOCACHE:-/private/tmp/workass-gocache}" scripts/gate.sh) >>"$log_file" 2>&1
  packaged_daemon="$package_root/workass-$bundle_version-$bundle_build"
  echo "[package] building bundled daemon $bundle_version" | tee -a "$log_file"
  (cd "$repo_root" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
    -ldflags "-X main.daemonVersion=$bundle_version" -o "$packaged_daemon" ./cmd/workass) >>"$log_file" 2>&1
  echo "[package] staging native provider hosts" | tee -a "$log_file"
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64 --offline >>"$log_file" 2>&1
  echo "[package] staging portable Node runtime" | tee -a "$log_file"
  "$repo_root/scripts/vendor-node-runtime.sh" --target darwin-arm64 --offline >>"$log_file" 2>&1
fi

echo "[package] building renderer" | tee -a "$log_file"
(cd "$repo_root" && npm run build --prefix desktop/renderer2) >>"$log_file" 2>&1

rm -rf "$stage" "$iconset"
ditto "$electron_app" "$stage"
mkdir -p "$stage/Contents/Resources/app" "$stage/Contents/Resources/renderer" "$iconset"

mv "$stage/Contents/MacOS/Electron" "$stage/Contents/MacOS/Workass"
rm -f "$stage/Contents/Resources/default_app.asar"
for shell_file in main.js preload.js view-server.js browser-manager.js browser-control-server.js runtime-profile.js runtime-bootstrap.js app-icon.js image-copy.js profile-singleton.js update-manager.js update-worker.js; do
  cp "$repo_root/desktop/shell/$shell_file" "$stage/Contents/Resources/app/$shell_file"
done
cp "$repo_root/desktop/shell/package.production.json" "$stage/Contents/Resources/app/package.json"
plutil -replace version -string "$bundle_version" "$stage/Contents/Resources/app/package.json"
ditto "$repo_root/desktop/renderer2/dist" "$stage/Contents/Resources/renderer"
cp "$repo_root/config/environments/prod.env" "$stage/Contents/Resources/workass-profile.env"
if [ "$release_signing" -eq 1 ]; then
  printf '%s\n' 'WORKASS_UPDATE_CHANNEL=github' >> "$stage/Contents/Resources/workass-profile.env"
else
  printf '%s\n' 'WORKASS_UPDATE_CHANNEL=local' >> "$stage/Contents/Resources/workass-profile.env"
fi
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
  if [ "$runtime_input_root" = "$repo_root/dist-bin" ]; then
    daemon_source="$packaged_daemon"
  fi
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

[ "$launch" -eq 1 ] || {
  echo "--no-launch is not valid for a live install; use --artifact-only to build without activation" >&2
  exit 2
}
set -- --candidate "$stage" --install-root "$install_root"
if [ "$signing_migration" -eq 1 ]; then set -- "$@" --migrate-signing-identity; fi
"$repo_root/scripts/install-workass-macos.sh" "$@"
echo "WORKASS_MACOS_PACKAGE_HEALTHY"
echo "profile=$WORKASS_PROFILE"
echo "data_root=$WORKASS_DATA_ROOT"
echo "package_log=$log_file"
