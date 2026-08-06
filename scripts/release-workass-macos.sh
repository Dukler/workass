#!/bin/sh
# Public macOS release: self-contained app, Developer ID, hardened runtime,
# Apple notarization, portable DMG, and updater-ready ZIP/metadata.
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

version=''
build=''
arch=arm64
notary_profile="${WORKASS_NOTARY_PROFILE:-}"
output_root=''
base_url=''
offline=0
overwrite=0

usage() {
  cat <<'EOF'
usage: scripts/release-workass-macos.sh --version X.Y.Z --build-number N --notary-profile PROFILE [options]

Options:
  --arch arm64          Current supported public Mac architecture.
  --output DIR          Final artifact directory (default: dist-release/macos/X.Y.Z).
  --base-url URL        Base URL written into update metadata.
  --offline             Do not fetch the pinned Node runtime if it is absent.
  --overwrite           Replace an existing version artifact directory.

Required environment:
  WORKASS_CODESIGN_IDENTITY   Developer ID Application fingerprint or name.
  WORKASS_CODESIGN_KEYCHAIN   Optional absolute keychain path.

The notary profile must already exist in Keychain (created with
`xcrun notarytool store-credentials`). Secrets are never passed on argv.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --build-number) [ "$#" -ge 2 ] || { echo "--build-number needs a value" >&2; exit 2; }; build="$2"; shift 2 ;;
    --notary-profile) [ "$#" -ge 2 ] || { echo "--notary-profile needs a value" >&2; exit 2; }; notary_profile="$2"; shift 2 ;;
    --arch) [ "$#" -ge 2 ] || { echo "--arch needs a value" >&2; exit 2; }; arch="$2"; shift 2 ;;
    --output) [ "$#" -ge 2 ] || { echo "--output needs a value" >&2; exit 2; }; output_root="$2"; shift 2 ;;
    --base-url) [ "$#" -ge 2 ] || { echo "--base-url needs a value" >&2; exit 2; }; base_url="$2"; shift 2 ;;
    --offline) offline=1; shift ;;
    --overwrite) overwrite=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "macOS releases must be built on macOS" >&2; exit 1; }
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "--version must be X.Y.Z" >&2; exit 2; }
printf '%s' "$build" | grep -Eq '^[1-9][0-9]*$' || { echo "--build-number must be a positive integer" >&2; exit 2; }
[ "$arch" = arm64 ] || { echo "public macOS releases currently support arm64 only" >&2; exit 2; }
[ "$(uname -m)" = arm64 ] || { echo "the arm64 release must be built on an arm64 Mac or with an explicit arm64 Electron runtime" >&2; exit 1; }
[ -n "$notary_profile" ] || { echo "--notary-profile or WORKASS_NOTARY_PROFILE is required" >&2; exit 2; }
case "$notary_profile" in -*|*[!A-Za-z0-9._-]*) echo "notary profile must contain only letters, numbers, dot, underscore, or hyphen" >&2; exit 2 ;; esac
case "$base_url" in ''|https://*) ;; *) echo "--base-url must use https://" >&2; exit 2 ;; esac
case "$output_root" in
  '') output_root="$repo_root/dist-release/macos/$version" ;;
  /*) ;;
  *) output_root="$repo_root/$output_root" ;;
esac
case "$output_root" in /|"$repo_root"|"$repo_root"/) echo "refusing unsafe release output: $output_root" >&2; exit 2 ;; esac
if [ -e "$output_root" ] && [ "$overwrite" -ne 1 ]; then
  echo "release output already exists: $output_root (use --overwrite)" >&2
  exit 1
fi

for tool in xcrun hdiutil ditto shasum plutil spctl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing release tool: $tool" >&2; exit 1; }
done
workass_codesign_prepare distribution

stage_root=$(mktemp -d "$repo_root/.dev/macos-release.$version.XXXXXX")
cleanup() { rm -rf "$stage_root"; }
trap cleanup EXIT HUP INT TERM
runtime_input="$stage_root/runtime-input"
mkdir -p "$runtime_input"

echo "[release] renderer and repository gates"
(cd "$repo_root" && npm run build --prefix desktop/renderer2)
(cd "$repo_root" && scripts/sync-renderer2.sh)
(cd "$repo_root" && go vet ./...)
(cd "$repo_root" && go test ./...)

echo "[release] isolated daemon and runtime staging"
(cd "$repo_root" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.daemonVersion=$version" -o "$runtime_input/workass" ./cmd/workass)
if [ "$offline" -eq 1 ]; then
  "$repo_root/scripts/vendor-electron-runtime.sh" --target darwin-arm64 --output-root "$runtime_input/electron" --offline
  "$repo_root/scripts/vendor-node-runtime.sh" --target darwin-arm64 --output-root "$runtime_input/node" --offline
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64 --output-root "$runtime_input/frontier-hosts" --offline
else
  "$repo_root/scripts/vendor-electron-runtime.sh" --target darwin-arm64 --output-root "$runtime_input/electron"
  "$repo_root/scripts/vendor-node-runtime.sh" --target darwin-arm64 --output-root "$runtime_input/node"
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64 --output-root "$runtime_input/frontier-hosts"
fi

app="$stage_root/Workass.app"
artifact_name="Workass-$version-darwin-$arch"
zip="$stage_root/$artifact_name.zip"
dmg="$stage_root/$artifact_name.dmg"
notary_app="$stage_root/notary-app.json"
notary_dmg="$stage_root/notary-dmg.json"

"$repo_root/scripts/package-workass-macos.sh" \
  --artifact-only "$app" \
  --version "$version" \
  --build-number "$build" \
  --arch "$arch" \
  --electron-app "$runtime_input/electron/darwin-arm64/Electron.app" \
  --runtime-root "$runtime_input" \
  --portable-runtime \
  --release-signing

notarize() {
  artifact="$1"
  receipt="$2"
  xcrun notarytool submit "$artifact" \
    --keychain-profile "$notary_profile" \
    --wait \
    --output-format json > "$receipt"
  notary_status=$(plutil -extract status raw -o - "$receipt" 2>/dev/null || true)
  [ "$notary_status" = Accepted ] || {
    echo "Apple notarization did not accept $(basename -- "$artifact"); receipt=$receipt" >&2
    exit 1
  }
}

# Submit the app in Apple's supported ZIP transport, staple the accepted
# ticket to the app, then rebuild the public ZIP so offline machines receive
# the stapled ticket too.
ditto -c -k --sequesterRsrc --keepParent "$app" "$zip"
notarize "$zip" "$notary_app"
xcrun stapler staple "$app"
xcrun stapler validate "$app"
rm -f "$zip"
ditto -c -k --sequesterRsrc --keepParent "$app" "$zip"

dmg_root="$stage_root/dmg-root"
mkdir -p "$dmg_root"
ditto "$app" "$dmg_root/Workass.app"
ln -s /Applications "$dmg_root/Applications"
hdiutil create -quiet -volname "Workass $version" -srcfolder "$dmg_root" -ov -format UDZO "$dmg"
workass_codesign_sign_container_distribution "$dmg"
notarize "$dmg" "$notary_dmg"
xcrun stapler staple "$dmg"
xcrun stapler validate "$dmg"

codesign --verify --deep --strict "$app"
spctl -a -vv --type execute "$app"
spctl -a -vv --type open --context context:primary-signature "$dmg"

zip_sha=$(shasum -a 256 "$zip" | awk '{print $1}')
dmg_sha=$(shasum -a 256 "$dmg" | awk '{print $1}')
zip_size=$(stat -f '%z' "$zip")
dmg_size=$(stat -f '%z' "$dmg")
published_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
zip_name=$(basename -- "$zip")
dmg_name=$(basename -- "$dmg")
if [ -n "$base_url" ]; then
  base_url=${base_url%/}
  zip_url="$base_url/$zip_name"
  dmg_url="$base_url/$dmg_name"
else
  zip_url="$zip_name"
  dmg_url="$dmg_name"
fi
requirement=$(workass_codesign_requirement "$app")

node - "$stage_root/release.json" "$stage_root/RELEASES.json" \
  "$version" "$build" "$arch" "$published_at" "$zip_name" "$zip_url" "$zip_sha" "$zip_size" \
  "$dmg_name" "$dmg_url" "$dmg_sha" "$dmg_size" "$requirement" <<'NODE'
const fs = require('node:fs');
const [releasePath, updatesPath, version, build, arch, publishedAt, zipName, zipURL, zipSHA, zipSize, dmgName, dmgURL, dmgSHA, dmgSize, requirement] = process.argv.slice(2);
const release = {
  schemaVersion: 1,
  product: 'Workass',
  bundleId: 'com.workass.app',
  version,
  build: Number(build),
  platform: 'darwin',
  arch,
  publishedAt,
  designatedRequirement: requirement,
  artifacts: {
    update: { name: zipName, url: zipURL, sha256: zipSHA, size: Number(zipSize) },
    installer: { name: dmgName, url: dmgURL, sha256: dmgSHA, size: Number(dmgSize) },
  },
};
const updates = {
  currentRelease: version,
  releases: [{
    version,
    updateTo: { version, pub_date: publishedAt, notes: '', name: `Workass ${version}`, url: zipURL },
  }],
};
fs.writeFileSync(releasePath, `${JSON.stringify(release, null, 2)}\n`);
fs.writeFileSync(updatesPath, `${JSON.stringify(updates, null, 2)}\n`);
NODE

rm -rf "$output_root"
mkdir -p "$output_root"
mv "$zip" "$dmg" "$notary_app" "$notary_dmg" "$stage_root/release.json" "$stage_root/RELEASES.json" "$output_root/"
(cd "$output_root" && shasum -a 256 "$zip_name" "$dmg_name" > SHA256SUMS)

echo "WORKASS_MACOS_RELEASE_HEALTHY"
echo "version=$version"
echo "build=$build"
echo "arch=$arch"
echo "output=$output_root"
echo "dmg=$output_root/$dmg_name"
echo "update_zip=$output_root/$zip_name"
