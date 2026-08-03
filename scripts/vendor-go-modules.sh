#!/bin/sh
# Refresh the vendored Go dependencies, matching the pattern the Electron, Node
# and frontier-host vendor scripts already follow: third-party code lives in the
# tree, so a build never needs a registry.
#
# That is not a style preference here. The Windows production laptop has package
# fetching blocked, and Go builds for it are cross-compiled on the Mac — a build
# that reached out to proxy.golang.org would be one more thing that works in one
# place and not the other. `vendor/` present means `go build` uses it
# automatically and offline.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

echo "[vendor-go] tidying"
go mod tidy

echo "[vendor-go] vendoring"
go mod vendor

echo "[vendor-go] verifying the tree builds from vendor alone"
go build -mod=vendor ./... >/dev/null

echo "[vendor-go] dependencies:"
if [ -f vendor/modules.txt ]; then
  grep '^# ' vendor/modules.txt | sed 's/^# /  /'
else
  echo "  (none)"
fi
echo "WORKASS_GO_VENDOR_READY"
