#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
release="$repo_root/scripts/release-workass-macos.sh"
vendor_node="$repo_root/scripts/vendor-node-runtime.sh"
vendor_electron="$repo_root/scripts/vendor-electron-runtime.sh"
vendor_frontier_hosts="$repo_root/scripts/vendor-frontier-hosts.sh"
codesign_helper="$repo_root/scripts/lib/workass-codesign.sh"

sh -n "$release"
sh -n "$vendor_node"
sh -n "$vendor_electron"
sh -n "$vendor_frontier_hosts"
sh -n "$codesign_helper"
"$release" --help >/dev/null
"$vendor_node" --help >/dev/null
"$vendor_electron" --help >/dev/null

if "$release" --version 1.2.3 --build-number 1 --notary-profile test --base-url http://insecure.invalid >/dev/null 2>&1; then
  echo "release accepted an insecure update base URL" >&2
  exit 1
fi

grep -Fq 'workass_codesign_prepare distribution' "$release"
grep -Fq 'workass_codesign_sign_container_distribution "$dmg"' "$release"
grep -Fq 'xcrun notarytool submit' "$release"
grep -Fq 'xcrun stapler staple "$app"' "$release"
grep -Fq 'xcrun stapler staple "$dmg"' "$release"
grep -Fq 'spctl -a -vv --type execute "$app"' "$release"
grep -Fq 'WORKASS_MACOS_RELEASE_HEALTHY' "$release"
grep -Fq 'RELEASES.json' "$release"
grep -Fq -- '--output-root "$runtime_input/frontier-hosts"' "$release"
grep -Fq -- '--output-root "$runtime_input/node"' "$release"
grep -Fq -- '--output-root "$runtime_input/electron"' "$release"
grep -Fq -- '--electron-app "$runtime_input/electron/darwin-arm64/Electron.app"' "$release"
grep -Fq 'go test ./...' "$release"
grep -Fq '"$WORKASS_BUNDLE_ID.daemon"' "$codesign_helper"
if grep -Eq -- '--skip-(sign|notari)' "$release"; then
  echo "public release must not expose a skip-signing or skip-notarization escape hatch" >&2
  exit 1
fi

grep -Fq 'claude_sdk_version=0.3.217' "$vendor_frontier_hosts"
grep -Fq '20363761b29724950b749ecbc5186c46e29f2a0330554ca309a9e7ff8d6e5799' "$vendor_frontier_hosts"
grep -Fq 'claude-native-host.mjs' "$vendor_frontier_hosts"
grep -Fq 'codex-native-host.mjs' "$vendor_frontier_hosts"
if grep -Eq 'claude-agent-acp|codex-acp|vendor-adapters' "$release" "$vendor_frontier_hosts"; then
  echo "native frontier release still references a Zed ACP adapter" >&2
  exit 1
fi
grep -Fq 'version=24.17.0' "$vendor_node"
grep -Fq 'archive="node-v$version-darwin-arm64.tar.xz"' "$vendor_node"
grep -Fq 'cf7e9152d7bd86c140f6eccf3577abfbaf8960be1ca49d9d900e8484984dcb9a' "$vendor_node"
grep -Fq 'config/macos/electron.version' "$repo_root/scripts/lib/workass-electron.sh"
grep -Fq 'd6d0598d042ef4d146278d08d84deac9dde145eae31eb4f32ef46206d6bd6169' "$vendor_electron"
[ "$(sed -n '1p' "$repo_root/config/macos/electron.version")" = 43.1.1 ]

echo "WORKASS_MACOS_RELEASE_CONTRACT_PASS"
