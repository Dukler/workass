#!/bin/sh
# Build-time-only fetch of the exact Electron runtimes used by Workass.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-electron.sh"

target=darwin-arm64
output_root="$repo_root/.dev/runtime/electron"
offline=0

usage() {
  cat <<'EOF'
usage: scripts/vendor-electron-runtime.sh [--target darwin-arm64|win32-x64] [--output-root DIR] [--offline]

Downloads the pinned official Electron archive at build time, verifies its
published SHA-256, and stages the unpacked runtime outside the source
dependency tree. Windows is deliberately staged without npm or electron-builder.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) [ "$#" -ge 2 ] || { echo "--target needs a value" >&2; exit 2; }; target="$2"; shift 2 ;;
    --output-root) [ "$#" -ge 2 ] || { echo "--output-root needs a value" >&2; exit 2; }; output_root="$2"; shift 2 ;;
    --offline) offline=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$output_root" in /*) ;; *) echo "--output-root must be absolute" >&2; exit 2 ;; esac
for tool in curl ditto shasum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done

version=$(workass_electron_pinned_version)
case "$version:$target" in
  43.1.1:darwin-arm64)
    expected=d6d0598d042ef4d146278d08d84deac9dde145eae31eb4f32ef46206d6bd6169
    archive="electron-v$version-darwin-arm64.zip"
    source_kind=darwin
    ;;
  43.1.1:win32-x64)
    expected=b4e9995cd3f65785eb8818276aa9020f3165ab11da41b3c762616d4a0ad8c7ad
    archive="electron-v$version-win32-x64.zip"
    source_kind=windows
    ;;
  *)
    echo "no audited Electron checksum for $version ($target)" >&2
    exit 1
    ;;
esac

cache="$repo_root/.dev/downloads/electron/$version"
download="$cache/$archive"
url="https://github.com/electron/electron/releases/download/v$version/$archive"
mkdir -p "$cache"
if [ ! -f "$download" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned Electron runtime is not cached and --offline was set" >&2; exit 1; }
  incoming="$download.incoming.$$"
  trap 'rm -f "$incoming"' EXIT HUP INT TERM
  curl -fL --retry 3 --connect-timeout 20 -o "$incoming" "$url"
  mv "$incoming" "$download"
  trap - EXIT HUP INT TERM
fi

actual=$(shasum -a 256 "$download" | awk '{print $1}')
[ "$actual" = "$expected" ] || {
  echo "Electron runtime checksum mismatch for $archive" >&2
  echo "expected=$expected" >&2
  echo "actual=$actual" >&2
  exit 1
}

stage_root=$(mktemp -d "$repo_root/.dev/electron-runtime.XXXXXX")
cleanup() { rm -rf "$stage_root"; }
trap cleanup EXIT HUP INT TERM
ditto -x -k "$download" "$stage_root"
source_app="$stage_root/Electron.app"
if [ "$source_kind" = darwin ]; then
  [ -d "$source_app" ] || { echo "Electron archive has unexpected layout" >&2; exit 1; }
  destination="$output_root/$target/Electron.app"
  incoming="$destination.incoming.$$"
  rm -rf "$incoming"
  mkdir -p "$(dirname -- "$destination")"
  ditto "$source_app" "$incoming"
  staged_version=$(workass_electron_app_version "$incoming")
  [ "$staged_version" = "$version" ] || {
    echo "Electron archive version mismatch: expected $version, found $staged_version" >&2
    exit 1
  }
else
  [ -f "$stage_root/electron.exe" ] || { echo "Electron archive has unexpected layout" >&2; exit 1; }
  destination="$output_root/$target"
  incoming="$destination.incoming.$$"
  rm -rf "$incoming"
  mkdir -p "$(dirname -- "$destination")" "$incoming"
  ditto "$stage_root/." "$incoming"
  staged_version="$version"
fi
rm -rf "$destination"
mv "$incoming" "$destination"

echo "WORKASS_ELECTRON_RUNTIME_READY"
echo "target=$target"
echo "version=$version"
echo "sha256=$expected"
echo "path=$destination"
