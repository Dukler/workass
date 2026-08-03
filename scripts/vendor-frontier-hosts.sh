#!/bin/sh
# Stage Workass's native Codex/Claude hosts plus Anthropic's official Agent
# SDK. No Zed ACP package is downloaded or copied by this build step.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
claude_sdk_version=0.3.217
claude_sdk_sha256=20363761b29724950b749ecbc5186c46e29f2a0330554ca309a9e7ff8d6e5799
target=''
output_root="$repo_root/dist-bin/frontier-hosts"
offline=0

usage() {
  cat <<'EOF'
usage: scripts/vendor-frontier-hosts.sh --target <darwin-arm64|darwin-x64|windows-amd64|windows-arm64|linux-amd64|linux-arm64> [--output-root DIR] [--offline]

Stages the Workass-owned direct provider hosts and the exact official Claude
Agent SDK package. Codex is supplied by the user's official `codex` install;
Claude is supplied by the user's official `claude` install and local login.
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

for tool in curl shasum tar node; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 1; }
done

cache="$repo_root/.dev/downloads/claude-agent-sdk/$claude_sdk_version"
archive="$cache/claude-agent-sdk-$claude_sdk_version.tgz"
url="https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-$claude_sdk_version.tgz"
mkdir -p "$cache"
if [ ! -f "$archive" ]; then
  [ "$offline" -eq 0 ] || { echo "pinned Claude Agent SDK is absent and --offline was set" >&2; exit 1; }
  incoming_archive="$archive.incoming.$$"
  trap 'rm -f "$incoming_archive"' EXIT HUP INT TERM
  curl -fL --retry 3 --connect-timeout 20 -o "$incoming_archive" "$url"
  mv "$incoming_archive" "$archive"
  trap - EXIT HUP INT TERM
fi
actual=$(shasum -a 256 "$archive" | awk '{print $1}')
[ "$actual" = "$claude_sdk_sha256" ] || {
  echo "Claude Agent SDK checksum mismatch" >&2
  echo "expected=$claude_sdk_sha256" >&2
  echo "actual=$actual" >&2
  exit 1
}

stage_root=$(mktemp -d "$repo_root/.dev/frontier-hosts.XXXXXX")
cleanup() { rm -rf "$stage_root"; }
trap cleanup EXIT HUP INT TERM
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

destination="$output_root/$target"
incoming="$destination.incoming.$$"
rm -rf "$incoming"
mkdir -p "$incoming/node_modules/@anthropic-ai"
ditto "$sdk_source" "$incoming/node_modules/@anthropic-ai/claude-agent-sdk"
cp "$repo_root/scripts/claude-native-host.mjs" "$incoming/claude-native-host.mjs"
cp "$repo_root/scripts/codex-native-host.mjs" "$incoming/codex-native-host.mjs"
chmod 755 "$incoming/claude-native-host.mjs" "$incoming/codex-native-host.mjs"
mkdir -p "$(dirname -- "$destination")"
rm -rf "$destination"
mv "$incoming" "$destination"

echo "WORKASS_FRONTIER_HOSTS_READY"
echo "target=$target"
echo "claude_sdk_version=$claude_sdk_version"
echo "claude_sdk_sha256=$claude_sdk_sha256"
echo "path=$destination"
