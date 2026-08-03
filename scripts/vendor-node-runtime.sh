#!/bin/sh
# Build-time-only fetch of the pinned portable Node runtime used by the
# official Claude SDK and Workass native hosts. Release machines never download
# npm or Node.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
version=24.17.0
target=''
output_root="$repo_root/dist-bin/node"
offline=0

usage() {
  cat <<'EOF'
usage: scripts/vendor-node-runtime.sh --target <darwin-arm64|darwin-x64|windows-amd64|windows-arm64|linux-amd64|linux-arm64> [--output-root DIR] [--offline]

Downloads the pinned official Node.js LTS archive at build time, verifies its
published SHA-256, and stages only the portable runtime under dist-bin/node/.
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

case "$target" in
  darwin-arm64)
    archive="node-v$version-darwin-arm64.tar.xz"
    expected=cf7e9152d7bd86c140f6eccf3577abfbaf8960be1ca49d9d900e8484984dcb9a
    extracted="node-v$version-darwin-arm64"
    ;;
  darwin-x64)
    archive="node-v$version-darwin-x64.tar.xz"
    expected=fe50e386f6a5e0b29ce44b989e543da9fb9a80aed0b91a2f0cb19c55106921fc
    extracted="node-v$version-darwin-x64"
    ;;
  windows-amd64)
    archive="node-v$version-win-x64.zip"
    expected=f2aa33b35b75aca5f3f7b85675a6f6423201053e9381911e64961f3bda2528ab
    extracted="node-v$version-win-x64"
    ;;
  windows-arm64)
    archive="node-v$version-win-arm64.zip"
    expected=4957712f67fce55779cc794d9b4df9e0e802a18c841ad5a4e42f17be490e634d
    extracted="node-v$version-win-arm64"
    ;;
  linux-amd64)
    archive="node-v$version-linux-x64.tar.xz"
    expected=ab343a1b747c7cbf3630dfd7dbf818c5423fab2eb4f5ad1afc896f6bd121a917
    extracted="node-v$version-linux-x64"
    ;;
  linux-arm64)
    archive="node-v$version-linux-arm64.tar.xz"
    expected=67324b9e515e7d13da72571a5dd522bb23145a820f7dde15497897e466759ab3
    extracted="node-v$version-linux-arm64"
    ;;
  '') usage >&2; exit 2 ;;
  *) echo "unsupported Node runtime target: $target" >&2; exit 2 ;;
esac

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v shasum >/dev/null 2>&1 || { echo "shasum is required" >&2; exit 1; }

cache="$repo_root/.dev/downloads/node/$version"
download="$cache/$archive"
url="https://nodejs.org/download/release/v$version/$archive"
mkdir -p "$cache"
if [ ! -f "$download" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned Node runtime is not present in the local cache and --offline was set" >&2; exit 1; }
  incoming="$download.incoming.$$"
  trap 'rm -f "$incoming"' EXIT HUP INT TERM
  curl -fL --retry 3 --connect-timeout 20 -o "$incoming" "$url"
  mv "$incoming" "$download"
  trap - EXIT HUP INT TERM
fi

actual=$(shasum -a 256 "$download" | awk '{print $1}')
[ "$actual" = "$expected" ] || {
  echo "Node runtime checksum mismatch for $archive" >&2
  echo "expected=$expected" >&2
  echo "actual=$actual" >&2
  exit 1
}

stage_root=$(mktemp -d "$repo_root/.dev/node-runtime.XXXXXX")
cleanup() { rm -rf "$stage_root"; }
trap cleanup EXIT HUP INT TERM
case "$archive" in
  *.zip) ditto -x -k "$download" "$stage_root" ;;
  *.tar.xz) tar -xJf "$download" -C "$stage_root" ;;
  *) echo "unsupported Node archive: $archive" >&2; exit 1 ;;
esac

source_root="$stage_root/$extracted"
[ -d "$source_root" ] || { echo "Node archive has unexpected layout" >&2; exit 1; }
destination="$output_root/$target"
incoming="$destination.incoming.$$"
rm -rf "$incoming"
mkdir -p "$(dirname -- "$destination")"
ditto "$source_root" "$incoming"
rm -rf "$incoming/include" "$incoming/share" "$incoming/lib/node_modules"
if [ -f "$incoming/bin/node" ]; then chmod 755 "$incoming/bin/node"; fi
if [ -f "$incoming/node.exe" ]; then chmod 755 "$incoming/node.exe"; fi
rm -rf "$destination"
mv "$incoming" "$destination"

echo "WORKASS_NODE_RUNTIME_READY"
echo "target=$target"
echo "version=$version"
echo "sha256=$expected"
echo "path=$destination"
