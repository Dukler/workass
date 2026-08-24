#!/bin/sh
# Materialize generated renderer source before the one reviewed release commit.
# This command intentionally does not commit, push, package, publish, or alter
# either installed Workass profile.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)

[ "$(git -C "$repo_root" branch --show-current)" = main ] || {
  echo "release source preparation requires the primary main worktree" >&2
  exit 1
}
[ -d "$repo_root/desktop/renderer2/node_modules" ] || {
  echo "release source preparation requires desktop/renderer2/node_modules" >&2
  exit 1
}

(cd "$repo_root/desktop/renderer2" && npm run build --silent)
(cd "$repo_root" && scripts/sync-renderer2.sh)
(cd "$repo_root" && diff -qr desktop/renderer2/dist cmd/workass/embedded/dist >/dev/null)

echo "WORKASS_RELEASE_SOURCE_PREPARED"
echo "review_and_commit=cmd/workass/embedded/dist"
