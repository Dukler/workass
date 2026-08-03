#!/bin/sh
# Stage the portable Windows Workass bundle (endpoint-security-friendly) on the Mac
# dev machine. The Windows target never runs npm, downloads nothing, and needs
# no admin: the zip carries the Go daemon, the checksum-pinned portable Node
# runtime, and the vendored ACP native hosts, plus a one-shot user-level
# installer. No registry hives, no services, no machine-wide state.
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
      workass.exe                    Go daemon (binds :80 on Windows per spec)
      node/windows-amd64/node.exe    pinned portable Node (SHA-256 verified)
      frontier-hosts/windows-amd64/  Claude/Codex native hosts + Agent SDK
      Install-Workass.ps1            one-shot user-level installer
      manifest.json
    Workass-X.Y.Z-windows-amd64.zip
    SHA256SUMS

No Electron, no installer framework, no registry writes. Windows needs only
PowerShell 5 (inbox) to expand and optionally register a user Scheduled Task.
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

for tool in go curl shasum ditto; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done

offline_flags=''
[ "$offline" -eq 1 ] && offline_flags='--offline'

# 1. Vendored runtimes, checksum-pinned at build time (never on Windows).
"$repo_root/scripts/vendor-node-runtime.sh" --target "$target" $offline_flags
"$repo_root/scripts/vendor-frontier-hosts.sh" --target "$target" $offline_flags

# 2. Windows daemon. Cross-compile is CGO-free and stdlib-only per spec; the
#    existing build-daemon.sh also signs darwin artifacts, which is irrelevant
#    to the windows-amd64 output, so we build just the windows binary here.
if [ "$skip_build" -eq 0 ]; then
  echo "building dist-bin/workass-windows-amd64.exe"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
    -o "$repo_root/dist-bin/workass-windows-amd64.exe" ./cmd/workass
fi
[ -f "$repo_root/dist-bin/workass-windows-amd64.exe" ] || {
  echo "windows daemon is missing: dist-bin/workass-windows-amd64.exe" >&2
  exit 1
}

# 3. Stage the portable tree the daemon auto-discovers beside its executable.
rm -rf "$stage"
mkdir -p "$stage/node" "$stage/frontier-hosts"
cp "$repo_root/dist-bin/workass-windows-amd64.exe" "$stage/workass.exe"
ditto "$repo_root/dist-bin/node/$target" "$stage/node/$target"
ditto "$repo_root/dist-bin/frontier-hosts/$target" "$stage/frontier-hosts/$target"
cp "$repo_root/scripts/windows/Install-Workass.ps1" "$stage/Install-Workass.ps1"

# Sanity: the exact files the daemon's native lookup requires.
[ -f "$stage/node/$target/node.exe" ] || { echo "staged node.exe missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/claude-native-host.mjs" ] || { echo "staged claude host missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/codex-native-host.mjs" ] || { echo "staged codex host missing" >&2; exit 1; }
[ -f "$stage/frontier-hosts/$target/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs" ] || {
  echo "staged Claude Agent SDK missing" >&2; exit 1;
}

git_rev=$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo unknown)
printf '{"schemaVersion":1,"platform":"windows","arch":"amd64","version":"%s","revision":"%s","portable":true}\n' \
  "$version" "$git_rev" > "$stage/manifest.json"

# 4. Zip + checksums. Use zip (not ditto) so no __MACOSX resource-fork entries
#    leak into the archive a Windows user extracts. -X strips extra file attrs.
command -v zip >/dev/null 2>&1 || { echo "zip is required" >&2; exit 1; }
rm -f "$release_dir/$bundle.zip" "$release_dir/SHA256SUMS"
(
  cd "$release_dir"
  zip -q -r -X "$bundle.zip" "$bundle"
  shasum -a 256 "$bundle.zip" > "SHA256SUMS"
)

echo "WORKASS_WINDOWS_PORTABLE_READY"
echo "version=$version"
echo "bundle=$stage"
echo "zip=$release_dir/$bundle.zip"
echo "sha256sums=$release_dir/SHA256SUMS"
