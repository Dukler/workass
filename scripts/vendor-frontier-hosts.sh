#!/bin/sh
# Stage Workass's native Codex/Claude/OMP hosts plus the official Claude Agent
# SDK and the pinned OMP SDK dependency tree. No Zed ACP package is downloaded
# or copied by this build step.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
claude_sdk_version=0.3.217
claude_sdk_sha256=20363761b29724950b749ecbc5186c46e29f2a0330554ca309a9e7ff8d6e5799
omp_sdk_version=18.1.8
omp_sdk_sha256=020677e1ce37d2df16bca1e1771f5701731310d091219b6a669ec9d5efb3d97d
omp_sdk_integrity='sha512-gI4EB7y6xBEDFpytx+HTZgptcV3phuhix9ZH1Qus9m2vw1EOZV5AJ4ggc+xFDvJOJVTmF08c3wRzp+foEY9/7w=='
bun_version=1.3.14
target=''
output_root="$repo_root/dist-bin/frontier-hosts"
offline=0
stage_root=''
omp_project=''
incoming=''
incoming_archive=''
bun_incoming=''
cleanup() {
  for dir in "$stage_root" "$omp_project" "$incoming"; do
    [ -z "$dir" ] || rm -rf "$dir"
  done
  for file in "$incoming_archive" "$bun_incoming"; do
    [ -z "$file" ] || rm -f "$file"
  done
}
trap cleanup EXIT HUP INT TERM

usage() {
  cat <<'EOF'
usage: scripts/vendor-frontier-hosts.sh --target <darwin-arm64|darwin-x64|windows-amd64|windows-arm64|linux-amd64|linux-arm64> [--output-root DIR] [--offline]

Stages the Workass-owned direct provider hosts, the exact official Claude Agent
SDK package, and the pinned Oh My Pi SDK plus Bun runtime. Codex is supplied by
the user's official `codex` install; Claude is supplied by the user's official
`claude` install and local login.
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
case "$target" in
  darwin-arm64|darwin-x64|windows-amd64|windows-arm64|linux-amd64|linux-arm64) ;;
  '') usage >&2; exit 2 ;;
  *) echo "unsupported frontier host target: $target" >&2; exit 2 ;;
esac
case "$output_root" in /*) ;; *) echo "--output-root must be absolute" >&2; exit 2 ;; esac

for tool in curl shasum tar unzip node npm; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 1; }
done

cache="$repo_root/.dev/downloads/claude-agent-sdk/$claude_sdk_version"
archive="$cache/claude-agent-sdk-$claude_sdk_version.tgz"
url="https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-$claude_sdk_version.tgz"
mkdir -p "$cache"
if [ ! -f "$archive" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned Claude Agent SDK is absent and --offline was set" >&2; exit 1; }
  incoming_archive="$archive.incoming.$$"
  curl -fL --retry 3 --connect-timeout 20 -o "$incoming_archive" "$url"
  mv "$incoming_archive" "$archive"
fi
actual=$(shasum -a 256 "$archive" | awk '{print $1}')
[ "$actual" = "$claude_sdk_sha256" ] || {
  echo "Claude Agent SDK checksum mismatch" >&2
  echo "expected=$claude_sdk_sha256" >&2
  echo "actual=$actual" >&2
  exit 1
}

stage_root=$(mktemp -d "$repo_root/.dev/frontier-hosts.XXXXXX")
tar -xzf "$archive" -C "$stage_root"
sdk_source="$stage_root/package"
[ -f "$sdk_source/sdk.mjs" ] && [ -f "$sdk_source/package.json" ] || {
  echo "Claude Agent SDK archive has an unexpected layout" >&2
  exit 1
}
actual_version=$(node -e 'const fs=require("node:fs"); const p=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(String(p.version||""))' "$sdk_source/package.json")
[ "$actual_version" = "$claude_sdk_version" ] || {
  echo "Claude Agent SDK package version mismatch: $actual_version" >&2
  exit 1
}

# OMP publishes TypeScript source and requires Bun. Resolve its complete,
# integrity-checked npm dependency graph at build time; the target machine
# receives only the staged tree and never runs npm.
omp_cache="$repo_root/.dev/downloads/omp-sdk/$omp_sdk_version"
omp_archive="$omp_cache/pi-coding-agent-$omp_sdk_version.tgz"
omp_url="https://registry.npmjs.org/@oh-my-pi/pi-coding-agent/-/pi-coding-agent-$omp_sdk_version.tgz"
mkdir -p "$omp_cache"
if [ ! -f "$omp_archive" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned OMP SDK is absent and --offline was set" >&2; exit 1; }
  incoming_archive="$omp_archive.incoming.$$"
  curl -fL --retry 3 --connect-timeout 20 -o "$incoming_archive" "$omp_url"
  mv "$incoming_archive" "$omp_archive"
fi
actual=$(shasum -a 256 "$omp_archive" | awk '{print $1}')
[ "$actual" = "$omp_sdk_sha256" ] || {
  echo "OMP SDK checksum mismatch" >&2
  echo "expected=$omp_sdk_sha256" >&2
  echo "actual=$actual" >&2
  exit 1
}
actual_integrity="sha512-$(node -e 'const fs=require("node:fs"), c=require("node:crypto"); process.stdout.write(c.createHash("sha512").update(fs.readFileSync(process.argv[1])).digest("base64"))' "$omp_archive")"
[ "$actual_integrity" = "$omp_sdk_integrity" ] || { echo "OMP SDK integrity mismatch" >&2; exit 1; }

omp_project=$(mktemp -d "$repo_root/.dev/omp-sdk.XXXXXX")
omp_os=darwin
omp_cpu=arm64
if [ "$offline" -eq 1 ]; then npm_offline_arg=--offline; else npm_offline_arg=''; fi
case "$target" in
  windows-amd64) omp_os=win32; omp_cpu=x64 ;;
  windows-arm64) omp_os=win32; omp_cpu=arm64 ;;
  linux-amd64) omp_os=linux; omp_cpu=x64 ;;
  linux-arm64) omp_os=linux; omp_cpu=arm64 ;;
  darwin-x64) omp_os=darwin; omp_cpu=x64 ;;
esac
(
  cd "$omp_project"
  cp "$repo_root/scripts/omp-sdk/package.json" ./package.json
  cp "$repo_root/scripts/omp-sdk/package-lock.json" ./package-lock.json
  # Seed npm with the independently checksum-verified root tarball, then
  # resolve the registry lock entry so every package retains its published
  # URL/integrity rather than a machine-local file reference.
  npm cache add "$omp_archive" >/dev/null
  npm ci --ignore-scripts --no-audit --no-fund $npm_offline_arg \
    --os="$omp_os" --cpu="$omp_cpu" >/dev/null
)
omp_pkg="$omp_project/node_modules/@oh-my-pi/pi-coding-agent"
[ -f "$omp_pkg/src/index.ts" ] && [ -f "$omp_project/package-lock.json" ] || {
  echo "OMP SDK dependency tree has an unexpected layout" >&2
  exit 1
}
omp_actual_version=$(node -e 'const fs=require("node:fs"); const p=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(String(p.version||""))' "$omp_pkg/package.json")
[ "$omp_actual_version" = "$omp_sdk_version" ] || { echo "OMP SDK package version mismatch: $omp_actual_version" >&2; exit 1; }
case "$target" in
  darwin-arm64) omp_native_package=pi-natives-darwin-arm64; omp_native_file=pi_natives.darwin-arm64.node ;;
  darwin-x64) omp_native_package=pi-natives-darwin-x64; omp_native_file=pi_natives.darwin-x64-baseline.node ;;
  windows-amd64) omp_native_package=pi-natives-win32-x64; omp_native_file=pi_natives.win32-x64-baseline.node ;;
  windows-arm64) omp_native_package=pi-natives-win32-arm64; omp_native_file=pi_natives.win32-arm64-baseline.node ;;
  linux-amd64) omp_native_package=pi-natives-linux-x64; omp_native_file=pi_natives.linux-x64-baseline.node ;;
  linux-arm64) omp_native_package=pi-natives-linux-arm64; omp_native_file=pi_natives.linux-arm64-baseline.node ;;
esac
[ -f "$omp_project/node_modules/@oh-my-pi/$omp_native_package/$omp_native_file" ] || {
  echo "OMP native addon missing for $target: $omp_native_package/$omp_native_file" >&2
  exit 1
}

bun_cache="$repo_root/.dev/downloads/bun/$bun_version"
case "$target" in
  darwin-arm64) bun_archive="bun-darwin-aarch64.zip"; bun_dir=bun-darwin-aarch64; bun_sha256=d8b96221828ad6f97ac7ac0ab7e95872341af763001e8803e8267652c2652620; bun_exe=bun ;;
  darwin-x64) bun_archive="bun-darwin-x64.zip"; bun_dir=bun-darwin-x64; bun_sha256=4183df3374623e5bab315c547cfa0974533cd457d86b73b639f7a87974cd6633; bun_exe=bun ;;
  windows-amd64) bun_archive="bun-windows-x64.zip"; bun_dir=bun-windows-x64; bun_sha256=0a0620930b6675d7ba440e81f4e0e00d3cfbe096c4b140d3fff02205e9e18922; bun_exe=bun.exe ;;
  windows-arm64) bun_archive="bun-windows-aarch64.zip"; bun_dir=bun-windows-aarch64; bun_sha256=89841f5a57f2348b67ec0839b718f4bf4ea7d07c371c9ba4b77b6c790f918953; bun_exe=bun.exe ;;
  linux-amd64) bun_archive="bun-linux-x64.zip"; bun_dir=bun-linux-x64; bun_sha256=951ee2aee855f08595aeec6225226a298d3fea83a3dcd6465c09cbccdf7e848f; bun_exe=bun ;;
  linux-arm64) bun_archive="bun-linux-aarch64.zip"; bun_dir=bun-linux-aarch64; bun_sha256=a27ffb63a8310375836e0d6f668ae17fa8d8d18b88c37c821c65331973a19a3b; bun_exe=bun ;;
  *) echo "Bun runtime is not published for target $target" >&2; exit 1 ;;
esac
[ -n "$bun_sha256" ] || { echo "Bun checksum is not audited for target $target" >&2; exit 1; }
bun_download="$bun_cache/$bun_archive"
bun_url="https://github.com/oven-sh/bun/releases/download/bun-v$bun_version/$bun_archive"
mkdir -p "$bun_cache"
if [ ! -f "$bun_download" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned Bun runtime is absent and --offline was set" >&2; exit 1; }
  bun_incoming="$bun_download.incoming.$$"
  curl -fL --retry 3 --connect-timeout 20 -o "$bun_incoming" "$bun_url"
  mv "$bun_incoming" "$bun_download"
fi
[ "$(shasum -a 256 "$bun_download" | awk '{print $1}')" = "$bun_sha256" ] || { echo "Bun checksum mismatch" >&2; exit 1; }
bun_stage="$omp_project/bun-extract"
mkdir -p "$bun_stage"
unzip -q "$bun_download" -d "$bun_stage"
[ -f "$bun_stage/$bun_dir/$bun_exe" ] || { echo "Bun archive has an unexpected layout" >&2; exit 1; }

destination="$output_root/$target"
incoming="$destination.incoming.$$"
rm -rf "$incoming"
mkdir -p "$incoming/node_modules/@anthropic-ai"
cp -R "$sdk_source" "$incoming/node_modules/@anthropic-ai/claude-agent-sdk"
mkdir -p "$incoming/node_modules/@oh-my-pi"
cp -R "$omp_project/node_modules/." "$incoming/node_modules"
cp "$omp_project/package-lock.json" "$incoming/omp-sdk-package-lock.json"
cp "$bun_stage/$bun_dir/$bun_exe" "$incoming/$bun_exe"
chmod 755 "$incoming/$bun_exe"
cp "$repo_root/scripts/claude-native-host.mjs" "$incoming/claude-native-host.mjs"
cp "$repo_root/scripts/codex-native-host.mjs" "$incoming/codex-native-host.mjs"
[ -f "$repo_root/scripts/omp-native-host.mjs" ] || { echo "OMP native host is missing" >&2; exit 1; }
cp "$repo_root/scripts/omp-native-host.mjs" "$incoming/omp-native-host.mjs"
chmod 755 "$incoming/claude-native-host.mjs" "$incoming/codex-native-host.mjs"
mkdir -p "$(dirname -- "$destination")"
rm -rf "$destination"
mv "$incoming" "$destination"

echo "WORKASS_FRONTIER_HOSTS_READY"
echo "target=$target"
echo "claude_sdk_version=$claude_sdk_version"
echo "claude_sdk_sha256=$claude_sdk_sha256"
echo "omp_sdk_version=$omp_sdk_version"
echo "omp_sdk_sha256=$omp_sdk_sha256"
echo "bun_version=$bun_version"
echo "bun_sha256=$bun_sha256"
echo "path=$destination"
