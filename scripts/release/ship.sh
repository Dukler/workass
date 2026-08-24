#!/bin/sh
# One owned production publication action. All gates, platform packaging,
# remote readback, and receipts stay inside the canonical lane; this command
# never activates an installed app.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/lib/source-state.sh"

version=''
macos_output="$HOME/Library/Application Support/Workass/update-feed"
offline=0

usage() {
  cat <<'EOF'
usage: scripts/release/ship.sh [--version X.Y.Z] [--offline] [--macos-output DIR]

Publishes the next paired Mac/Windows update from clean, pushed main and emits
one final receipt. It does not install or activate the release.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version=$2; shift 2 ;;
    --macos-output) [ "$#" -ge 2 ] || { echo "--macos-output needs a value" >&2; exit 2; }; macos_output=$2; shift 2 ;;
    --offline) offline=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "paired publication must run on the Mac build host" >&2; exit 1; }
case "$macos_output" in /*) ;; *) echo "--macos-output must be absolute" >&2; exit 2 ;; esac
workass_release_require_source "$repo_root"
commit=$WORKASS_RELEASE_COMMIT
mkdir -p "$repo_root/.dev"

if [ -z "$version" ]; then
  for tool in gh node plutil; do
    command -v "$tool" >/dev/null 2>&1 || { echo "missing release discovery tool: $tool" >&2; exit 1; }
  done
  discovered_versions=''
  installed_plist=/Applications/Workass.app/Contents/Info.plist
  if [ -f "$installed_plist" ]; then
    installed_version=$(plutil -extract CFBundleShortVersionString raw -o - "$installed_plist")
    discovered_versions="$discovered_versions $installed_version"
  fi
  macos_feed="$macos_output/workass-darwin-arm64-release.json"
  if [ -f "$macos_feed" ]; then
    feed_version=$(node -e 'const fs=require("node:fs"); process.stdout.write(String(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).version||""))' "$macos_feed")
    discovered_versions="$discovered_versions $feed_version"
  fi
  latest_file=$(mktemp "$repo_root/.dev/release-latest.XXXXXX")
  cleanup_latest() { rm -f "$latest_file"; }
  trap cleanup_latest EXIT HUP INT TERM
  gh auth status >/dev/null
  gh release view --repo Dukler/workass --json tagName,isDraft,isPrerelease > "$latest_file"
  latest_version=$(node -e '
    const fs=require("node:fs"); const r=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));
    if (r.isDraft || r.isPrerelease || !/^v\d+\.\d+\.\d+$/.test(r.tagName||"")) throw new Error("GitHub latest is not one stable Workass release");
    process.stdout.write(r.tagName.slice(1));' "$latest_file")
  discovered_versions="$discovered_versions $latest_version"
  # Strict version strings contain no shell metacharacters; intentional word
  # splitting passes each discovered version as one argument to the resolver.
  # shellcheck disable=SC2086
  version=$(node "$script_dir/lib/next-version.mjs" $discovered_versions)
  cleanup_latest
  trap - EXIT HUP INT TERM
fi
printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || { echo "invalid version" >&2; exit 2; }

run_root="$repo_root/.dev/release-runs/$version-$commit"
mkdir -p "$run_root"
run_log="$run_root/ship-$(date -u +%Y%m%dT%H%M%SZ)-$$.log"
started=$(date +%s)
echo "WORKASS_RELEASE_START version=$version commit=$commit"

set -- --version "$version" --macos-output "$macos_output" --publish
[ "$offline" -eq 0 ] || set -- "$@" --offline
if "$script_dir/stage-updates.sh" "$@" >"$run_log" 2>&1; then
  publication=$(sed -n 's/^publication=//p' "$run_log" | tail -n 1)
  [ -n "$publication" ] && [ -f "$publication" ] || {
    echo "publisher completed without its final receipt (full output: $run_log)" >&2
    exit 1
  }
  release=$(node - "$publication" "$version" "$commit" <<'NODE'
const fs = require('node:fs');
const [file, version, commit] = process.argv.slice(2);
const receipt = JSON.parse(fs.readFileSync(file, 'utf8'));
if (receipt.schemaVersion !== 1 || receipt.product !== 'Workass' || receipt.kind !== 'paired-publication' ||
    receipt.status !== 'verified' || receipt.version !== version || receipt.commit !== commit ||
    typeof receipt.windows?.releaseUrl !== 'string' || !receipt.windows.releaseUrl.startsWith('https://github.com/')) {
  throw new Error('final publication receipt identity is invalid');
}
process.stdout.write(receipt.windows.releaseUrl);
NODE
  )
else
  code=$?
  echo "WORKASS_RELEASE_FAILED version=$version log=$run_log" >&2
  tail -24 "$run_log" >&2
  exit "$code"
fi

finished=$(date +%s)
echo "WORKASS_RELEASE_PUBLISHED"
echo "version=$version"
echo "commit=$commit"
echo "publication=$publication"
echo "release=$release"
echo "log=$run_log"
echo "seconds=$((finished - started))"
echo "activation=not-requested"
