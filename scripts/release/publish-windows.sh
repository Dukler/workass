#!/bin/sh
# Publish and read back the immutable update assets of one GitHub release: the
# three Windows ones, and — when --linux-release-dir is given — the three
# headless Linux ones beside them. One tag carries every non-Mac target, so a
# Windows client and a Linux daemon read their own manifest from the same
# immutable release and never see each other's.
#
# This script never activates an installed app or daemon; on Windows
# installation remains a user click, and on Linux it remains an explicit
# authorized workass_apply_update for that exact machine.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
version=''
commit=''
release_dir=''
repository=Dukler/workass
verify_only=0
receipt=''
linux_release_dir=''

usage() {
  echo "usage: scripts/release/publish-windows.sh --version X.Y.Z --commit FULL_SHA --release-dir DIR [--linux-release-dir DIR] [--verify-only] [--receipt FILE]"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version=$2; shift 2 ;;
    --commit) [ "$#" -ge 2 ] || { echo "--commit needs a value" >&2; exit 2; }; commit=$2; shift 2 ;;
    --release-dir) [ "$#" -ge 2 ] || { echo "--release-dir needs a value" >&2; exit 2; }; release_dir=$2; shift 2 ;;
    --linux-release-dir) [ "$#" -ge 2 ] || { echo "--linux-release-dir needs a value" >&2; exit 2; }; linux_release_dir=$2; shift 2 ;;
    --verify-only) verify_only=1; shift ;;
    --receipt) [ "$#" -ge 2 ] || { echo "--receipt needs a value" >&2; exit 2; }; receipt=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || { echo "invalid version" >&2; exit 2; }
printf '%s' "$commit" | grep -Eq '^[0-9a-f]{40}$' || { echo "invalid commit" >&2; exit 2; }
case "$release_dir" in /*) ;; *) echo "--release-dir must be absolute" >&2; exit 2 ;; esac
case "$linux_release_dir" in ''|/*) ;; *) echo "--linux-release-dir must be absolute" >&2; exit 2 ;; esac
case "$receipt" in ''|/*) ;; *) echo "--receipt must be absolute" >&2; exit 2 ;; esac
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
linux_archive="Workass-$version-linux-amd64.tar.gz"
linux_manifest=workass-linux-amd64-release.json
linux_checksums=SHA256SUMS-linux-amd64
if [ -n "$linux_release_dir" ]; then
  for name in "$linux_archive" "$linux_manifest" "$linux_checksums"; do
    [ -f "$linux_release_dir/$name" ] || { echo "Linux release asset is missing: $linux_release_dir/$name" >&2; exit 1; }
  done
  (cd "$linux_release_dir" && shasum -a 256 -c "$linux_checksums") >/dev/null
fi
release_title="Workass $version — Windows portable"
[ -z "$linux_release_dir" ] || release_title="Workass $version — Windows portable + Linux headless"
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

# Every asset is printed as "<root>/<name>" so a missing one is re-uploaded
# from the platform directory it actually lives in.
validate_release() {
  mode=$1
  node - "$view" "$version" "$commit" "$release_dir" "$mode" "$release_title" "$linux_release_dir" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [viewFile, version, commit, root, mode, expectedTitle, linuxRoot] = process.argv.slice(2);
const release = JSON.parse(fs.readFileSync(viewFile, 'utf8'));
if (release.tagName !== `v${version}` || release.targetCommitish !== commit || release.name !== expectedTitle ||
    release.isDraft !== false || release.isPrerelease !== false) {
  throw new Error('GitHub release identity, target, title, or stable state does not match');
}
const expected = [
  [root, `Workass-${version}-windows-amd64.zip`],
  [root, 'workass-windows-amd64-release.json'],
  [root, 'SHA256SUMS'],
];
if (linuxRoot) {
  expected.push(
    [linuxRoot, `Workass-${version}-linux-amd64.tar.gz`],
    [linuxRoot, 'workass-linux-amd64-release.json'],
    [linuxRoot, 'SHA256SUMS-linux-amd64'],
  );
}
const assets = new Map((release.assets || []).map((asset) => [asset.name, asset]));
const missing = [];
for (const [assetRoot, name] of expected) {
  const file = path.join(assetRoot, name);
  const size = fs.statSync(file).size;
  const digest = `sha256:${crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')}`;
  const remote = assets.get(name);
  if (!remote) {
    missing.push(file);
    continue;
  }
  if (remote.size !== size || remote.digest !== digest) throw new Error(`immutable GitHub asset mismatch: ${name}`);
}
if (mode === 'complete' && missing.length) throw new Error(`GitHub assets are missing: ${missing.join(', ')}`);
process.stdout.write(missing.join('\n'));
NODE
}

if [ "$verify_only" -eq 1 ]; then
  gh release view "$tag" --repo "$repository" \
    --json tagName,targetCommitish,name,isDraft,isPrerelease,assets,url > "$view" || {
      echo "GitHub release does not exist for verification: $tag" >&2
      exit 1
    }
  validate_release complete >/dev/null
elif gh release view "$tag" --repo "$repository" \
    --json tagName,targetCommitish,name,isDraft,isPrerelease,assets,url > "$view" 2>/dev/null; then
  missing_assets=$(validate_release allow-missing)
  if [ -n "$missing_assets" ]; then
    old_ifs=$IFS
    IFS='
'
    for asset in $missing_assets; do
      gh release upload "$tag" "$asset" --repo "$repository"
    done
    IFS=$old_ifs
  fi
else
  if tag_commit=$(gh api "repos/$repository/commits/$tag" --jq .sha 2>/dev/null); then
    [ "$tag_commit" = "$commit" ] || { echo "existing tag $tag targets a different commit" >&2; exit 1; }
  fi
  set -- "$release_dir/$archive" "$release_dir/$manifest" "$release_dir/$checksums"
  notes="Portable Windows update. Installation remains an explicit in-app click."
  if [ -n "$linux_release_dir" ]; then
    set -- "$@" "$linux_release_dir/$linux_archive" "$linux_release_dir/$linux_manifest" "$linux_release_dir/$linux_checksums"
    notes="Portable Windows update plus the headless Linux daemon tree. Installation remains an explicit in-app click on Windows, and an explicitly authorized per-machine activation on a Linux service host."
  fi
  gh release create "$tag" "$@" \
    --repo "$repository" \
    --target "$commit" \
    --title "$release_title" \
    --notes "$notes"
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

# Both clients discover through the same stable /releases/latest/download/
# redirect, so both manifests have to resolve there before the release is
# called published. A Linux daemon that cannot read its own manifest is a
# machine that silently never updates.
verify_latest_manifest() {
  manifest_name=$1
  local_copy=$2
  latest="$tmp_root/latest-$manifest_name"
  latest_url="https://github.com/$repository/releases/latest/download/$manifest_name"
  attempt=1
  while [ "$attempt" -le 6 ]; do
    if curl -fLsS --max-time 30 "$latest_url?workass_release=$commit" -o "$latest" && cmp -s "$local_copy" "$latest"; then
      echo "$latest_url"
      return 0
    fi
    sleep 2
    attempt=$((attempt + 1))
  done
  echo "GitHub latest $manifest_name does not resolve to the published immutable bytes" >&2
  return 1
}
latest_url=$(verify_latest_manifest "$manifest" "$release_dir/$manifest")
linux_latest_url=''
if [ -n "$linux_release_dir" ]; then
  linux_latest_url=$(verify_latest_manifest "$linux_manifest" "$linux_release_dir/$linux_manifest")
fi

release_url=$(node -e 'const fs=require("node:fs"); process.stdout.write(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).url)' "$view")
if [ -n "$receipt" ]; then
  node - "$receipt" "$view" "$release_dir" "$version" "$commit" "$repository" "$latest_url" "$linux_release_dir" "$linux_latest_url" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [receiptFile, viewFile, root, version, commit, repository, latestManifestUrl, linuxRoot, linuxLatestManifestUrl] = process.argv.slice(2);
const release = JSON.parse(fs.readFileSync(viewFile, 'utf8'));
const names = {
  archive: `Workass-${version}-windows-amd64.zip`,
  manifest: 'workass-windows-amd64-release.json',
  checksums: 'SHA256SUMS',
};
const linuxNames = {
  archive: `Workass-${version}-linux-amd64.tar.gz`,
  manifest: 'workass-linux-amd64-release.json',
  checksums: 'SHA256SUMS-linux-amd64',
};
const artifactIn = (assetRoot) => (name) => {
  const file = path.join(assetRoot, name);
  const bytes = fs.readFileSync(file);
  return { name, sha256: crypto.createHash('sha256').update(bytes).digest('hex'), size: bytes.length };
};
const artifact = artifactIn(root);
const expected = {
  schemaVersion: 1,
  product: 'Workass',
  // The receipt kind is unchanged on purpose: it is referenced by name in the
  // paired-publication verifier and by the ship receipt. Linux rides along as
  // additive fields rather than a renamed document.
  kind: 'windows-publication',
  status: 'verified',
  repository,
  version,
  commit,
  tag: `v${version}`,
  releaseUrl: release.url,
  latestManifestUrl,
  assets: Object.fromEntries(Object.entries(names).map(([key, name]) => [key, artifact(name)])),
  ...(linuxRoot ? {
    linuxLatestManifestUrl,
    linux: Object.fromEntries(Object.entries(linuxNames).map(([key, name]) => [key, artifactIn(linuxRoot)(name)])),
  } : {}),
};
let recorded = null;
try { recorded = JSON.parse(fs.readFileSync(receiptFile, 'utf8')); } catch {}
if (recorded && JSON.stringify(recorded) !== JSON.stringify(expected)) {
  throw new Error('Windows publication differs from its immutable receipt');
}
if (!recorded) {
  fs.mkdirSync(path.dirname(receiptFile), { recursive: true });
  const incoming = `${receiptFile}.incoming.${process.pid}`;
  try {
    fs.writeFileSync(incoming, `${JSON.stringify(expected, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
    fs.renameSync(incoming, receiptFile);
  } finally {
    fs.rmSync(incoming, { force: true });
  }
}
NODE
fi
echo "WORKASS_WINDOWS_GITHUB_RELEASE_VERIFIED"
echo "version=$version"
echo "commit=$commit"
echo "release=$release_url"
[ -z "$linux_release_dir" ] || echo "WORKASS_LINUX_GITHUB_RELEASE_VERIFIED"
[ -z "$linux_latest_url" ] || echo "linux_latest_manifest=$linux_latest_url"
[ -z "$receipt" ] || echo "windows_publication=$receipt"
