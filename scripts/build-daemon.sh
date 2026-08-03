#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
cd "$repo_root"
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

with_frontier_hosts=0
for arg in "$@"; do
  case "$arg" in
    --with-frontier-hosts)
      with_frontier_hosts=1
      ;;
    -h|--help)
      echo "usage: scripts/build-daemon.sh [--with-frontier-hosts]"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      echo "usage: scripts/build-daemon.sh [--with-frontier-hosts]" >&2
      exit 2
      ;;
  esac
done

"$repo_root/scripts/sync-renderer2.sh"
mkdir -p "$repo_root/dist-bin"

# macOS binds every privacy grant to a code-signing designated requirement. A
# Go binary carries only the linker's ad-hoc signature, whose identity is its
# CDHash, so an unsigned rebuild is a brand-new application that must be
# authorized again. These development artifacts therefore carry the same
# persistent identity as the production install. dist-bin holds the development
# daemon, so it uses the development profile's identifiers; the production
# package re-signs its own copy with the production identifier.
sign_darwin=0
if [ "$(uname -s)" = Darwin ]; then
  workass_load_profile dev
  if workass_codesign_prepare 2>/dev/null; then
    sign_darwin=1
    echo "signing darwin artifacts with the persistent Workass identity"
  else
    echo "warning: no persistent signing identity; darwin artifacts stay ad-hoc" >&2
    echo "warning: macOS will ask for permissions again after every rebuild" >&2
    echo "warning: run scripts/macos/bootstrap-workass-local-signing.sh once to fix this" >&2
  fi
fi
if [ "$with_frontier_hosts" -eq 1 ]; then
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target darwin-arm64
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target windows-amd64
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target linux-amd64
fi

build_one() {
  goos="$1"
  goarch="$2"
  suffix="$3"
  build_pkg "$goos" "$goarch" "$suffix" workass ./cmd/workass
  build_pkg "$goos" "$goarch" "$suffix" workass-agent ./cmd/workass-agent
}

build_pkg() {
  goos="$1"
  goarch="$2"
  suffix="$3"
  name="$4"
  pkg="$5"
  out="$repo_root/dist-bin/$name-$goos-$goarch$suffix"
  echo "building $out"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -o "$out" "$pkg"
  if [ "$sign_darwin" -eq 1 ] && [ "$goos" = darwin ]; then
    case "$name" in
      workass) identifier="$WORKASS_BUNDLE_ID.daemon" ;;
      workass-agent) identifier="$WORKASS_BUNDLE_ID.agent" ;;
      *) identifier="$WORKASS_BUNDLE_ID.$name" ;;
    esac
    workass_codesign_sign_binary "$out" "$identifier"
    echo "signed $out as $identifier"
  fi
  bytes=$(wc -c < "$out" | tr -d ' ')
  echo "$out $bytes bytes"
}

build_one darwin arm64 ""
build_one windows amd64 ".exe"
build_one linux amd64 ""
