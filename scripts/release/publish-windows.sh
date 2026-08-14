#!/bin/sh
# Publish and read back the three immutable Windows update assets. This script
# never activates an installed app; discovery and installation remain a user
# click in the packaged Windows renderer.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
version=''
commit=''
release_dir=''
repository=Dukler/workass

usage() {
  echo "usage: scripts/release/publish-windows.sh --version X.Y.Z --commit FULL_SHA --release-dir DIR"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version=$2; shift 2 ;;
    --commit) [ "$#" -ge 2 ] || { echo "--commit needs a value" >&2; exit 2; }; commit=$2; shift 2 ;;
    --release-dir) [ "$#" -ge 2 ] || { echo "--release-dir needs a value" >&2; exit 2; }; release_dir=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || { echo "invalid version" >&2; exit 2; }
printf '%s' "$commit" | grep -Eq '^[0-9a-f]{40}$' || { echo "invalid commit" >&2; exit 2; }
case "$release_dir" in /*) ;; *) echo "--release-dir must be absolute" >&2; exit 2 ;; esac
for tool in gh curl node shasum cmp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing Windows publisher tool: $tool" >&2; exit 1; }
done

tag="v$version"
archive="Workass-$version-windows-amd64.zip"
manifest=workass-windows-amd64-release.json
checksums=SHA256SUMS
for name in "$archive" "$manifest" "$checksums"; do
  [ -f "$release_dir/$name" ] || { echo "Windows release asset is missing: $release_dir/$name" >&2; exit 1; }
done
(cd "$release_dir" && shasum -a 256 -c "$checksums") >/dev/null
gh auth status >/dev/null

tmp_root=$(mktemp -d "$repo_root/.dev/github-release.XXXXXX")
cleanup() { rm -rf "$tmp_root"; }
trap cleanup EXIT HUP INT TERM
view="$tmp_root/release.json"

# A newly published older release can become GitHub's `latest` by publication
# time and strand every Windows client on a stale manifest. Reject that state
# before creating a tag or uploading a byte.
if gh release view --repo "$repository" --json tagName,isDraft,isPrerelease > "$tmp_root/latest-release.json" 2>/dev/null; then
  node - "$tmp_root/latest-release.json" "$version" <<'NODE'
const fs = require('node:fs');
const [file, version] = process.argv.slice(2);
const latest = JSON.parse(fs.readFileSync(file, 'utf8'));
if (latest.isDraft || latest.isPrerelease || !/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(latest.tagName)) {
  throw new Error('GitHub latest release is not one stable strict Workass version');
}
const parse = (value) => value.replace(/^v/, '').split('.').map(Number);
const compare = (left, right) => {
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return left[index] - right[index];
  }
  return 0;
};
if (compare(parse(version), parse(latest.tagName)) < 0) {
  throw new Error(`refusing to publish ${version} behind latest ${latest.tagName}`);
}
NODE
fi

validate_release() {
  mode=$1
  node - "$view" "$version" "$commit" "$release_dir" "$mode" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [viewFile, version, commit, root, mode] = process.argv.slice(2);
const release = JSON.parse(fs.readFileSync(viewFile, 'utf8'));
const expectedTitle = `Workass ${version} — Windows portable`;
if (release.tagName !== `v${version}` || release.targetCommitish !== commit || release.name !== expectedTitle ||
    release.isDraft !== false || release.isPrerelease !== false) {
  throw new Error('GitHub release identity, target, title, or stable state does not match');
}
const names = [`Workass-${version}-windows-amd64.zip`, 'workass-windows-amd64-release.json', 'SHA256SUMS'];
const assets = new Map((release.assets || []).map((asset) => [asset.name, asset]));
const missing = [];
for (const name of names) {
  const file = path.join(root, name);
  const size = fs.statSync(file).size;
  const digest = `sha256:${crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')}`;
  const remote = assets.get(name);
  if (!remote) {
    missing.push(name);
    continue;
  }
  if (remote.size !== size || remote.digest !== digest) throw new Error(`immutable GitHub asset mismatch: ${name}`);
}
if (mode === 'complete' && missing.length) throw new Error(`GitHub assets are missing: ${missing.join(', ')}`);
process.stdout.write(missing.join('\n'));
NODE
}

if gh release view "$tag" --repo "$repository" \
    --json tagName,targetCommitish,name,isDraft,isPrerelease,assets,url > "$view" 2>/dev/null; then
  missing_assets=$(validate_release allow-missing)
  if [ -n "$missing_assets" ]; then
    old_ifs=$IFS
    IFS='
'
    for name in $missing_assets; do
      gh release upload "$tag" "$release_dir/$name" --repo "$repository"
    done
    IFS=$old_ifs
  fi
else
  if tag_commit=$(gh api "repos/$repository/commits/$tag" --jq .sha 2>/dev/null); then
    [ "$tag_commit" = "$commit" ] || { echo "existing tag $tag targets a different commit" >&2; exit 1; }
  fi
  gh release create "$tag" \
    "$release_dir/$archive" "$release_dir/$manifest" "$release_dir/$checksums" \
    --repo "$repository" \
    --target "$commit" \
    --title "Workass $version — Windows portable" \
    --notes "Portable Windows update. Installation remains an explicit in-app click."
fi

verified=0
attempt=1
while [ "$attempt" -le 6 ]; do
  gh release view "$tag" --repo "$repository" \
    --json tagName,targetCommitish,name,isDraft,isPrerelease,assets,url > "$view"
  if validate_release complete >/dev/null 2>&1; then
    verified=1
    break
  fi
  sleep 2
  attempt=$((attempt + 1))
done
[ "$verified" -eq 1 ] || { validate_release complete; exit 1; }

latest="$tmp_root/latest.json"
latest_url="https://github.com/$repository/releases/latest/download/$manifest"
latest_verified=0
attempt=1
while [ "$attempt" -le 6 ]; do
  if curl -fLsS --max-time 30 "$latest_url?workass_release=$commit" -o "$latest" && cmp -s "$release_dir/$manifest" "$latest"; then
    latest_verified=1
    break
  fi
  sleep 2
  attempt=$((attempt + 1))
done
[ "$latest_verified" -eq 1 ] || { echo "GitHub latest manifest does not resolve to the published immutable bytes" >&2; exit 1; }

release_url=$(node -e 'const fs=require("node:fs"); process.stdout.write(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).url)' "$view")
echo "WORKASS_WINDOWS_GITHUB_RELEASE_VERIFIED"
echo "version=$version"
echo "commit=$commit"
echo "release=$release_url"
