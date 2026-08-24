#!/bin/sh
# Build the immutable, content-addressed input shared by both private update
# lanes. A new commit/toolchain runs the repository gate once; an exact local
# pass receipt makes interrupted retries verify and reuse that result.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/lib/timing.sh"
# shellcheck disable=SC1091
. "$script_dir/lib/source-state.sh"

version=''
output=''
offline=0

usage() {
  echo "usage: scripts/release/prepare-input.sh --version X.Y.Z [--output DIR] [--offline]"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version=$2; shift 2 ;;
    --output) [ "$#" -ge 2 ] || { echo "--output needs a value" >&2; exit 2; }; output=$2; shift 2 ;;
    --offline) offline=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "release inputs must be built on the Mac build host" >&2; exit 1; }
printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
  echo "--version must be strict X.Y.Z" >&2
  exit 2
}

workass_release_require_source "$repo_root"
commit=$WORKASS_RELEASE_COMMIT

if [ -z "$output" ]; then
  output="$repo_root/.dev/release-inputs/$version-$commit"
fi
case "$output" in /*) ;; *) echo "--output must be absolute" >&2; exit 2 ;; esac
case "$output" in /|"$repo_root"|"$repo_root"/) echo "refusing unsafe release input path: $output" >&2; exit 2 ;; esac

input_tool="$script_dir/lib/release-input.mjs"
gate_tool="$script_dir/lib/repository-gate.mjs"
gate_receipt="$repo_root/.dev/release-gates/$commit.json"
if [ -e "$output" ]; then
  node "$input_tool" verify --root "$output" --version "$version" --commit "$commit" || {
    echo "existing release input is immutable and does not verify: $output" >&2
    exit 1
  }
  echo "WORKASS_RELEASE_INPUT_REUSED"
  echo "input=$output"
  exit 0
fi

for tool in go node npm ditto shasum diff; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing release-input tool: $tool" >&2; exit 1; }
done

mkdir -p "$(dirname -- "$output")"
incoming="$output.incoming.$$"
[ ! -e "$incoming" ] || { echo "release input staging path already exists: $incoming" >&2; exit 1; }
mkdir -p "$incoming/macos/runtime" "$incoming/windows/runtime"
cleanup() { rm -rf "$incoming"; }
trap cleanup EXIT HUP INT TERM

repository_gate() {
  (cd "$repo_root" && \
    GOCACHE="${GOCACHE:-/private/tmp/workass-gocache}" \
    WORKASS_GATE_FRESH=1 \
    WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER=1 \
    scripts/gate.sh)
}

release_contracts() {
  (cd "$repo_root" && node --test scripts/tests/release-pipeline.test.mjs)
}

verify_gate_receipt() {
  node "$gate_tool" verify --repo "$repo_root" --receipt "$gate_receipt" --commit "$commit"
}

record_gate_receipt() {
  node "$gate_tool" record --repo "$repo_root" --receipt "$gate_receipt" --commit "$commit"
}

stage_renderer() {
  workass_release_require_source "$repo_root"
  [ "$WORKASS_RELEASE_COMMIT" = "$commit" ] || {
    echo "release source changed after its repository gate" >&2
    return 1
  }
  ditto "$repo_root/cmd/workass/embedded/dist/." "$incoming/renderer"
}

build_macos_daemon() {
  (cd "$repo_root" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
    -ldflags "-X main.daemonVersion=$version" -o "$incoming/macos/runtime/workass" ./cmd/workass)
}

build_windows_daemon() {
  (cd "$repo_root" && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
    -ldflags "-X main.daemonVersion=$version" -o "$incoming/windows/runtime/workass-daemon.exe" ./cmd/workass)
}

vendor_runtime_core() {
  platform=$1
  electron_target=$2
  node_target=$3
  platform_root="$incoming/$platform"
  offline_arg=''
  [ "$offline" -eq 0 ] || offline_arg=--offline
  "$repo_root/scripts/vendor-electron-runtime.sh" --target "$electron_target" --output-root "$platform_root/electron" $offline_arg
  "$repo_root/scripts/vendor-node-runtime.sh" --target "$node_target" --output-root "$platform_root/runtime/node" $offline_arg
}

vendor_frontier_target() {
  platform=$1
  node_target=$2
  platform_root="$incoming/$platform"
  offline_arg=''
  [ "$offline" -eq 0 ] || offline_arg=--offline
  "$repo_root/scripts/vendor-frontier-hosts.sh" --target "$node_target" --output-root "$platform_root/runtime/frontier-hosts" $offline_arg
}

vendor_macos_runtime() { vendor_runtime_core macos darwin-arm64 darwin-arm64; }
vendor_windows_runtime() { vendor_runtime_core windows win32-x64 windows-amd64; }
vendor_macos_frontier() { vendor_frontier_target macos darwin-arm64; }
vendor_windows_frontier() { vendor_frontier_target windows windows-amd64; }

write_manifest() {
  node "$input_tool" create --root "$incoming" --version "$version" --commit "$commit"
  node "$input_tool" verify --root "$incoming" --version "$version" --commit "$commit"
}

workass_release_run_phase release_contracts release_contracts
if [ -f "$gate_receipt" ] && verify_gate_receipt >/dev/null 2>&1; then
  workass_release_run_phase repository_gate_cached verify_gate_receipt
else
  workass_release_run_phase repository_gate repository_gate
  workass_release_run_phase repository_gate_receipt record_gate_receipt
fi
workass_release_run_phase renderer_snapshot stage_renderer
workass_release_run_parallel_pair macos_daemon build_macos_daemon windows_daemon build_windows_daemon
workass_release_run_parallel_pair macos_runtimes vendor_macos_runtime windows_runtimes vendor_windows_runtime
# Both frontier targets read one shared pinned SDK cache. Stage one first so a
# cold cache cannot race two downloads; the copies themselves are tiny.
workass_release_run_phase macos_frontier vendor_macos_frontier
workass_release_run_phase windows_frontier vendor_windows_frontier
workass_release_run_phase input_manifest write_manifest

mv "$incoming" "$output"
trap - EXIT HUP INT TERM

echo "WORKASS_RELEASE_INPUT_READY"
echo "version=$version"
echo "commit=$commit"
echo "input=$output"
