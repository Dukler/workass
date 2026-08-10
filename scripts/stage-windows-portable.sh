#!/bin/sh
# Stage the portable Windows Workass app on the Mac dev machine. The Windows
# target never runs npm or downloads anything: the zip carries a pinned
# Electron executable, the Go daemon beside it, the renderer, portable Node,
# and the vendored ACP native hosts. No installer script is needed to launch it.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
version=''
output_root="$repo_root/dist-release/windows"
offline=0
skip_build=0

usage() {
  cat <<'EOF'
usage: scripts/stage-windows-portable.sh --version X.Y.Z [--output-root DIR] [--offline] [--skip-build]

Builds and stages the portable Windows bundle:
  dist-release/windows/X.Y.Z/
    Workass-X.Y.Z-windows-amd64/
      Workass.exe                    portable Electron executable
      workass-daemon.exe             Go daemon beside the app
      resources/app/                  Electron shell
      resources/renderer/             built renderer
      node/windows-amd64/node.exe    pinned portable Node (SHA-256 verified)
      frontier-hosts/windows-amd64/  Claude/Codex native hosts + Agent SDK
      manifest.json
    Workass-X.Y.Z-windows-amd64.zip
    SHA256SUMS

No installer framework, registry writes, or Windows-side build step. Launch
Workass.exe directly from the extracted folder.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --output-root) [ "$#" -ge 2 ] || { echo "--output-root needs a value" >&2; exit 2; }; output_root="$2"; shift 2 ;;
    --offline) offline=1; shift ;;
    --skip-build) skip_build=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required" >&2; usage >&2; exit 2; }
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "invalid version: $version" >&2; exit 2; }
case "$output_root" in /*) ;; *) echo "--output-root must be absolute" >&2; exit 2 ;; esac

target=windows-amd64
bundle="Workass-$version-$target"
release_dir="$output_root/$version"
stage="$release_dir/$bundle"

for tool in go curl shasum ditto node npm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done

offline_flags=''
[ "$offline" -eq 1 ] && offline_flags='--offline'

# 1. Vendored runtimes, checksum-pinned at build time (never on Windows).
"$repo_root/scripts/vendor-electron-runtime.sh" --target win32-x64 $offline_flags
"$repo_root/scripts/vendor-node-runtime.sh" --target "$target" $offline_flags
"$repo_root/scripts/vendor-frontier-hosts.sh" --target "$target" $offline_flags

# 2. Windows daemon. Cross-compile is CGO-free and stdlib-only per spec; the
#    existing build-daemon.sh also signs darwin artifacts, which is irrelevant
#    to the windows-amd64 output, so we build just the windows binary here.
if [ "$skip_build" -eq 0 ]; then
  echo "building dist-bin/workass-windows-amd64.exe"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X main.daemonVersion=$version" \
    -o "$repo_root/dist-bin/workass-windows-amd64.exe" ./cmd/workass
fi
[ -f "$repo_root/dist-bin/workass-windows-amd64.exe" ] || {
  echo "windows daemon is missing: dist-bin/workass-windows-amd64.exe" >&2
  exit 1
}

# 3. Stage Electron, the shell, renderer, and the portable tree the daemon
#    auto-discovers beside its executable.
rm -rf "$stage"
mkdir -p "$stage" "$stage/resources/app" "$stage/resources/renderer" "$stage/node" "$stage/frontier-hosts"
ditto "$repo_root/.dev/runtime/electron/win32-x64/." "$stage"
mv "$stage/electron.exe" "$stage/Workass.exe"
node "$repo_root/desktop/scripts/stamp-windows-icon.mjs" --exe "$stage/Workass.exe" --icon "$repo_root/desktop/assets/icon.ico"
rm -f "$stage/resources/default_app.asar"
cp "$repo_root/dist-bin/workass-windows-amd64.exe" "$stage/workass-daemon.exe"
ditto "$repo_root/dist-bin/node/$target" "$stage/node/$target"
ditto "$repo_root/dist-bin/frontier-hosts/$target" "$stage/frontier-hosts/$target"

if [ "$skip_build" -eq 0 ]; then
  echo "building renderer"
  (cd "$repo_root" && npm run build --prefix desktop/renderer2)
fi
[ -f "$repo_root/desktop/renderer2/dist/index.html" ] || {
  echo "renderer build is missing: desktop/renderer2/dist/index.html" >&2
  exit 1
}
ditto "$repo_root/desktop/renderer2/dist/." "$stage/resources/renderer"
for shell_file in main.js preload.js view-server.js browser-manager.js browser-control-server.js runtime-profile.js runtime-bootstrap.js certificate-pins.js app-icon.js image-copy.js profile-singleton.js update-manager.js update-worker.js; do
  cp "$repo_root/desktop/shell/$shell_file" "$stage/resources/app/$shell_file"
done
cp "$repo_root/desktop/shell/package.production.json" "$stage/resources/app/package.json"
node - "$stage/resources/app/package.json" "$version" <<'NODE'
const fs = require('node:fs');
const [file, version] = process.argv.slice(2);
const manifest = JSON.parse(fs.readFileSync(file, 'utf8'));
manifest.version = version;
fs.writeFileSync(file, `${JSON.stringify(manifest, null, 2)}\n`);
NODE
cp "$repo_root/config/environments/windows-prod.env" "$stage/resources/workass-profile.env"
cp "$repo_root/desktop/assets/icon.ico" "$stage/resources/Workass.ico"

# Sanity: the exact files the daemon's native lookup requires.
[ -f "$stage/node/$target/node.exe" ] || { echo "staged node.exe missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/claude-native-host.mjs" ] || { echo "staged claude host missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/codex-native-host.mjs" ] || { echo "staged codex host missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs" ] || {
  echo "staged Claude Agent SDK missing" >&2; exit 1;
}

git_rev=$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo unknown)
printf '{"schemaVersion":2,"platform":"windows","arch":"amd64","version":"%s","revision":"%s","portable":true,"electron":true}\n' \
  "$version" "$git_rev" > "$stage/manifest.json"
node "$repo_root/desktop/scripts/stamp-windows-icon.mjs" --verify --exe "$stage/Workass.exe" --icon "$repo_root/desktop/assets/icon.ico"

# 4. Zip + checksums. Use zip (not ditto) so no __MACOSX resource-fork entries
#    leak into the archive a Windows user extracts. -X strips extra file attrs.
command -v zip >/dev/null 2>&1 || { echo "zip is required" >&2; exit 1; }
rm -f "$release_dir/$bundle.zip" "$release_dir/SHA256SUMS"
(
  cd "$release_dir"
  zip -q -r -X "$bundle.zip" "$bundle"
  shasum -a 256 "$bundle.zip" > "SHA256SUMS"
)
zip_sha=$(shasum -a 256 "$release_dir/$bundle.zip" | awk '{print $1}')
zip_size=$(stat -f '%z' "$release_dir/$bundle.zip")
node - "$release_dir/workass-windows-amd64-release.json" "$version" "$bundle.zip" "$zip_sha" "$zip_size" <<'NODE'
const fs = require('node:fs');
const [file, version, artifactName, sha256, size] = process.argv.slice(2);
const release = {
  schemaVersion: 1,
  product: 'Workass',
  version,
  platform: 'windows',
  arch: 'amd64',
  portable: true,
  // Informational only. Portable Windows updates trust the immutable GitHub
  // release manifest over HTTPS plus the archive's exact SHA-256 and size.
  authenticode: false,
  artifacts: { update: { name: artifactName, url: artifactName, sha256, size: Number(size) } },
};
fs.writeFileSync(file, `${JSON.stringify(release, null, 2)}\n`);
NODE

echo "WORKASS_WINDOWS_PORTABLE_READY"
echo "version=$version"
echo "bundle=$stage"
echo "zip=$release_dir/$bundle.zip"
echo "sha256sums=$release_dir/SHA256SUMS"
echo "update_feed=$release_dir/workass-windows-amd64-release.json"
