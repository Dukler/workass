#!/bin/sh
# Build one complete, machine-locally-signed Workass.app and publish it to the
# stable filesystem feed consumed by macOS dogfood installs. This lane requires
# neither Apple Developer ID nor notarization; public releases remain separate.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
version=''
build_number=$(date -u +%Y%m%d%H%M%S)
output_root="$HOME/Library/Application Support/Workass/update-feed"
candidate_root=''
release_input=''

usage() {
  echo "usage: scripts/stage-macos-local-update.sh --version X.Y.Z [--build-number N] [--output DIR] [--candidate-root DIR] [--release-input DIR]"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --build-number) [ "$#" -ge 2 ] || { echo "--build-number needs a value" >&2; exit 2; }; build_number="$2"; shift 2 ;;
    --output) [ "$#" -ge 2 ] || { echo "--output needs a value" >&2; exit 2; }; output_root="$2"; shift 2 ;;
    --candidate-root) [ "$#" -ge 2 ] || { echo "--candidate-root needs a value" >&2; exit 2; }; candidate_root="$2"; shift 2 ;;
    --release-input) [ "$#" -ge 2 ] || { echo "--release-input needs a value" >&2; exit 2; }; release_input="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "local Mac updates require macOS" >&2; exit 1; }
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "--version must be X.Y.Z" >&2; exit 2; }
printf '%s' "$build_number" | grep -Eq '^[1-9][0-9]*$' || { echo "--build-number must be a positive integer" >&2; exit 2; }
case "$output_root" in /*) ;; *) echo "--output must be absolute" >&2; exit 2 ;; esac
case "$output_root" in /|"$repo_root"|"$repo_root"/) echo "refusing unsafe local feed root: $output_root" >&2; exit 2 ;; esac
case "$candidate_root" in ''|/*) ;; *) echo "--candidate-root must be absolute" >&2; exit 2 ;; esac
case "$release_input" in ''|/*) ;; *) echo "--release-input must be absolute" >&2; exit 2 ;; esac

commit=$(git -C "$repo_root" rev-parse HEAD)
if [ -n "$release_input" ]; then
  # shellcheck disable=SC1091
  . "$repo_root/scripts/release/lib/source-state.sh"
  workass_release_require_source "$repo_root"
  commit=$WORKASS_RELEASE_COMMIT
  node "$repo_root/scripts/release/lib/release-input.mjs" verify \
    --root "$release_input" --version "$version" --commit "$commit"
fi

if [ -z "$candidate_root" ]; then
  candidate_root="$repo_root/.dev/local-updates/$version-$build_number"
fi
candidate="$candidate_root/Workass.app"
archive_name="Workass-$version-darwin-arm64.zip"
archive_incoming="$output_root/.$archive_name.incoming-$$"
archive="$output_root/$archive_name"
feed_name=workass-darwin-arm64-release.json
feed_incoming="$output_root/.$feed_name.incoming-$$"
feed="$output_root/$feed_name"

cleanup_incoming() {
  rm -f "$archive_incoming" "$feed_incoming"
}
trap cleanup_incoming EXIT HUP INT TERM

mkdir -p "$candidate_root" "$output_root"
set -- \
  --artifact-only "$candidate" \
  --version "$version" \
  --build-number "$build_number" \
  --portable-runtime
if [ -n "$release_input" ]; then
  set -- "$@" \
    --runtime-root "$release_input/macos/runtime" \
    --electron-app "$release_input/macos/electron/darwin-arm64/Electron.app" \
    --renderer-root "$release_input/renderer"
fi
"$repo_root/scripts/package-workass-macos.sh" "$@"

ditto -c -k --sequesterRsrc --keepParent "$candidate" "$archive_incoming"
archive_sha=$(shasum -a 256 "$archive_incoming" | awk '{print $1}')
archive_size=$(stat -f '%z' "$archive_incoming")
requirement=$(codesign -d -r- "$candidate" 2>&1 | sed -nE 's/^[[:space:]#]*designated =>[[:space:]]*//p' | head -n 1)
[ -n "$requirement" ] || { echo "local candidate has no stable signing requirement" >&2; exit 1; }
published_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

node - "$feed_incoming" "$version" "$build_number" "$published_at" "$archive_name" "$archive_sha" "$archive_size" "$requirement" <<'NODE'
const fs = require('node:fs');
const [file, version, build, publishedAt, archiveName, sha256, size, requirement] = process.argv.slice(2);
const release = {
  schemaVersion: 1,
  product: 'Workass',
  bundleId: 'com.workass.app',
  version,
  build: Number(build),
  platform: 'darwin',
  arch: 'arm64',
  publishedAt,
  designatedRequirement: requirement,
  notes: 'Local Workass update',
  artifacts: {
    update: { name: archiveName, url: archiveName, sha256, size: Number(size) },
  },
};
fs.writeFileSync(file, `${JSON.stringify(release, null, 2)}\n`, { mode: 0o600 });
NODE

# Publish the archive first and the manifest last. A concurrent app check sees
# either the previous complete release or this complete release, never a feed
# pointing at partial bytes.
mv -f "$archive_incoming" "$archive"
mv -f "$feed_incoming" "$feed"
(cd "$output_root" && shasum -a 256 "$archive_name" > SHA256SUMS)
trap - EXIT HUP INT TERM

echo "WORKASS_MACOS_LOCAL_UPDATE_READY"
echo "version=$version"
echo "app=$candidate"
echo "archive=$archive"
echo "feed=$feed"
