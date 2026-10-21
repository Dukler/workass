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
#
# `go mod vendor` copies every platform's files. The SQLite driver
# (modernc.org/sqlite + modernc.org/libc) is machine-generated Go per
# GOOS/GOARCH, so the copy is ~140 MB, almost all of it for platforms Workass
# never builds. After vendoring, every vendored source file that no target
# below compiles is removed. `go build -mod=vendor` checks vendor/modules.txt,
# not file presence, so the pruned tree builds offline for these targets; a
# new target must be added here (and this script re-run) before it can build.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

# Every GOOS/GOARCH a Workass binary is built for: scripts/build-daemon.sh,
# the macOS packaging scripts, stage-windows-portable.sh and
# deploy-remote-daemon.sh (--arch amd64|arm64).
targets="darwin/arm64 windows/amd64 linux/amd64 linux/arm64"

echo "[vendor-go] tidying"
go mod tidy

echo "[vendor-go] vendoring"
go mod vendor

echo "[vendor-go] pruning sources no target compiles ($targets)"
keep=$(mktemp)
all=$(mktemp)
trap 'rm -f "$keep" "$all"' EXIT
vendor_dir="$repo_root/vendor"
# Each target is listed in every build configuration a Workass build or test
# uses there, not only the release one: cgo off (releases, cross-compiles)
# and on (a plain `go build` or `go test` on a Mac with Xcode), each with and
# without the race detector's `race` tag (`go build -race`, `go test -race`).
# A file only one of them compiles, such as golang.org/x/sys/unix/race.go,
# would otherwise be pruned and break that configuration.
for target in $targets; do
  goos=${target%/*}
  goarch=${target#*/}
  for cgo in 0 1; do
    for tags in "" race; do
      CGO_ENABLED=$cgo GOOS="$goos" GOARCH="$goarch" go list -mod=vendor -deps -test -tags "$tags" \
        -f '{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}}
{{end}}{{range .CFiles}}{{$d}}/{{.}}
{{end}}{{range .SFiles}}{{$d}}/{{.}}
{{end}}{{range .HFiles}}{{$d}}/{{.}}
{{end}}{{range .SysoFiles}}{{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}
{{end}}' ./... | grep "^$vendor_dir/" >>"$keep" || true
    done
  done
done
sort -u -o "$keep" "$keep"
find "$vendor_dir" -type f \( -name '*.go' -o -name '*.s' -o -name '*.h' -o -name '*.c' -o -name '*.syso' \) | sort >"$all"
pruned=$(comm -23 "$all" "$keep" | wc -l | tr -d ' ')
comm -23 "$all" "$keep" | while IFS= read -r file; do rm -f "$file"; done
# Upstream agent-instruction files would be loaded by coding agents working
# in this repo as if they were Workass instructions. They are not ours.
find "$vendor_dir" -type f \( -name 'CLAUDE.md' -o -name 'AGENTS.md' -o -name 'GEMINI.md' -o -name '.cursorrules' \) -exec rm -f {} +
find "$vendor_dir" -type d -empty -delete
echo "[vendor-go] pruned $pruned files"

echo "[vendor-go] verifying the tree builds from vendor alone"
go build -mod=vendor ./... >/dev/null
# The race detector needs this host's cgo toolchain; skip it where there is none.
if CGO_ENABLED=1 go env CC >/dev/null 2>&1 && command -v "$(go env CC)" >/dev/null 2>&1; then
  go build -mod=vendor -race ./... >/dev/null
fi
for target in $targets; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -mod=vendor ./cmd/... >/dev/null
done

echo "[vendor-go] dependencies:"
if [ -f vendor/modules.txt ]; then
  grep '^# ' vendor/modules.txt | sed 's/^# /  /'
else
  echo "  (none)"
fi
echo "WORKASS_GO_VENDOR_READY"
