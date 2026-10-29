#!/bin/sh
# Stage the headless Linux Workass daemon on the Mac build host.
#
# A Linux fleet machine runs no window, so this bundle carries no Electron and
# no renderer resources: just the Go daemon, the tools client, the LM Studio
# ACP server, the pinned portable Node, and the vendored native hosts the
# daemon launches (Claude, Codex, Pi, oMLX). That is everything a headless
# install needs, which is what makes "install from the release alone" true
# rather than aspirational — the hand-provisioned record in
# lagspace/tmp/workass-duklerver-20261028/SETUP.md had to assemble these four
# pieces by hand, and this script is that assembly, repeatable.
#
# The daemon's own updater replaces this same tree in place
# (internal/headlessupdate), so the archive layout IS the install layout: one
# top-level directory whose entries sit directly in the install root.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
version=''
output_root="$repo_root/dist-release/linux"
offline=0
release_input=''
arch=amd64

usage() {
  cat <<'EOF'
usage: scripts/stage-linux-headless.sh --version X.Y.Z [--arch amd64|arm64] [--output-root DIR] [--offline] [--release-input DIR]

Builds and stages the headless Linux bundle:
  dist-release/linux/X.Y.Z/
    Workass-X.Y.Z-linux-amd64/
      workass                        Go daemon (run with -headless)
      workass-tools                  tools client
      workass-agent                  LM Studio ACP server
      node/linux-amd64/bin/node      pinned portable Node (SHA-256 verified)
      frontier-hosts/linux-amd64/    Claude/Codex/Pi/oMLX hosts + Agent SDK
      manifest.json
    Workass-X.Y.Z-linux-amd64.tar.gz
    SHA256SUMS-linux-amd64
    workass-linux-amd64-release.json

No installer, no root, no system packages. Extract into the install root and
run it under a `systemd --user` unit; see docs/LINUX-HEADLESS.md.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --arch) [ "$#" -ge 2 ] || { echo "--arch needs a value" >&2; exit 2; }; arch="$2"; shift 2 ;;
    --output-root) [ "$#" -ge 2 ] || { echo "--output-root needs a value" >&2; exit 2; }; output_root="$2"; shift 2 ;;
    --offline) offline=1; shift ;;
    --release-input) [ "$#" -ge 2 ] || { echo "--release-input needs a value" >&2; exit 2; }; release_input="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$version" ] || { echo "--version is required" >&2; usage >&2; exit 2; }
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "invalid version: $version" >&2; exit 2; }
case "$arch" in amd64|arm64) ;; *) echo "unsupported linux arch: $arch" >&2; exit 2 ;; esac
case "$output_root" in /*) ;; *) echo "--output-root must be absolute" >&2; exit 2 ;; esac
case "$release_input" in ''|/*) ;; *) echo "--release-input must be absolute" >&2; exit 2 ;; esac

target="linux-$arch"
bundle="Workass-$version-$target"
release_dir="$output_root/$version"
stage="$release_dir/$bundle"
feed_name="workass-$target-release.json"
checksums="SHA256SUMS-$target"

for tool in shasum ditto node tar file; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
if [ -z "$release_input" ]; then
  for tool in go curl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
  done
fi

offline_flags=''
[ "$offline" -eq 1 ] && offline_flags='--offline'

commit=$(git -C "$repo_root" rev-parse HEAD)
if [ -n "$release_input" ]; then
  # shellcheck disable=SC1091
  . "$repo_root/scripts/release/lib/source-state.sh"
  workass_release_require_source "$repo_root"
  commit=$WORKASS_RELEASE_COMMIT
  node "$repo_root/scripts/release/lib/release-input.mjs" verify \
    --root "$release_input" --version "$version" --commit "$commit"
  daemon_source="$release_input/linux/runtime/workass"
  tools_source="$release_input/linux/runtime/workass-tools"
  agent_source="$release_input/linux/runtime/workass-agent"
  node_source="$release_input/linux/runtime/node/$target"
  frontier_hosts_source="$release_input/linux/runtime/frontier-hosts/$target"
else
  "$repo_root/scripts/vendor-node-runtime.sh" --target "$target" $offline_flags
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target "$target" $offline_flags
  echo "building dist-bin/workass-$target"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.daemonVersion=$version" \
    -o "$repo_root/dist-bin/workass-$target" ./cmd/workass
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -o "$repo_root/dist-bin/workass-tools-$target" ./cmd/workass-tools
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -o "$repo_root/dist-bin/workass-agent-$target" ./cmd/workass-agent
  daemon_source="$repo_root/dist-bin/workass-$target"
  tools_source="$repo_root/dist-bin/workass-tools-$target"
  agent_source="$repo_root/dist-bin/workass-agent-$target"
  node_source="$repo_root/dist-bin/node/$target"
  frontier_hosts_source="$repo_root/dist-bin/frontier-hosts/$target"
fi

for required in "$daemon_source" "$tools_source" "$agent_source"; do
  [ -f "$required" ] || { echo "linux release input is missing: $required" >&2; exit 1; }
done

rm -rf "$stage"
mkdir -p "$stage" "$stage/node" "$stage/frontier-hosts"
cp "$daemon_source" "$stage/workass"
cp "$tools_source" "$stage/workass-tools"
cp "$agent_source" "$stage/workass-agent"
chmod 755 "$stage/workass" "$stage/workass-tools" "$stage/workass-agent"
ditto "$node_source" "$stage/node/$target"
ditto "$frontier_hosts_source" "$stage/frontier-hosts/$target"

# Sanity: the exact files a headless daemon's native lookup requires. A missing
# Pi or Claude host is an install that detects a provider and then cannot launch
# it, which is a far worse failure than a release that refuses to build.
[ -f "$stage/node/$target/bin/node" ] || { echo "staged node missing" >&2; exit 1; }
for host in claude-native-host.mjs codex-native-host.mjs pi-native-host.mjs omp-native-host.mjs; do
  [ -f "$stage/frontier-hosts/$target/$host" ] || { echo "staged $host missing" >&2; exit 1; }
done
[ -f "$stage/frontier-hosts/$target/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs" ] || {
  echo "staged Claude Agent SDK missing" >&2; exit 1;
}
# A linux binary built on the Mac is the one mistake worth checking for twice.
file "$stage/workass" | grep -q 'ELF 64-bit' || { echo "staged daemon is not a Linux ELF binary" >&2; exit 1; }

printf '{"schemaVersion":2,"platform":"linux","arch":"%s","version":"%s","revision":"%s","headless":true,"electron":false}\n' \
  "$arch" "$version" "$commit" > "$stage/manifest.json"

# tar, not zip: a Linux install extracts with tar, and gzip keeps the executable
# bits and directory layout the daemon's own updater renames into place.
rm -f "$release_dir/$bundle.tar.gz" "$release_dir/$checksums"
(
  cd "$release_dir"
  # COPYFILE_DISABLE keeps macOS ._ AppleDouble companions out of an archive a
  # Linux host extracts.
  COPYFILE_DISABLE=1 tar --no-xattrs -czf "$bundle.tar.gz" "$bundle"
  shasum -a 256 "$bundle.tar.gz" > "$checksums"
)
archive_sha=$(shasum -a 256 "$release_dir/$bundle.tar.gz" | awk '{print $1}')
archive_size=$(stat -f '%z' "$release_dir/$bundle.tar.gz")
node - "$release_dir/$feed_name" "$version" "$arch" "$bundle.tar.gz" "$archive_sha" "$archive_size" <<'NODE'
const fs = require('node:fs');
const [file, version, arch, artifactName, sha256, size] = process.argv.slice(2);
const release = {
  schemaVersion: 1,
  product: 'Workass',
  version,
  platform: 'linux',
  arch,
  // `headless` is to this feed what `portable` is to the Windows one: it says
  // what shape the artifact is, so a daemon never accepts an app bundle and an
  // app never accepts a daemon tree.
  headless: true,
  service: 'systemd-user',
  artifacts: {
    update: {
      name: artifactName,
      url: `https://github.com/Dukler/workass/releases/download/v${version}/${artifactName}`,
      sha256,
      size: Number(size),
    },
  },
};
fs.writeFileSync(file, `${JSON.stringify(release, null, 2)}\n`);
NODE

echo "WORKASS_LINUX_HEADLESS_READY"
echo "version=$version"
echo "bundle=$stage"
echo "archive=$release_dir/$bundle.tar.gz"
echo "sha256sums=$release_dir/$checksums"
echo "update_feed=$release_dir/$feed_name"
