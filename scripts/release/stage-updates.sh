#!/bin/sh
# Canonical private release lane: prepare one immutable source input, stage and
# verify both platform updates, then optionally publish. It never clicks either
# updater and therefore never replaces the user's manual activation decision.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/lib/timing.sh"
# shellcheck disable=SC1091
. "$script_dir/lib/source-state.sh"

version=''
build_number=''
input=''
candidate=''
macos_output="$HOME/Library/Application Support/Workass/update-feed"
offline=0
publish=0

usage() {
  cat <<'EOF'
usage: scripts/release/stage-updates.sh --version X.Y.Z [options]

Options:
  --build-number N       macOS bundle build (default: exact commit UTC timestamp)
  --input DIR            reuse an exact verified release input
  --candidate DIR        isolated candidate output
  --macos-output DIR     local Mac update feed used only with --publish
  --offline              require all pinned runtimes to be cached
  --publish              publish GitHub Windows assets and Mac local feed

Without --publish, production feeds and GitHub are read-only.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version=$2; shift 2 ;;
    --build-number) [ "$#" -ge 2 ] || { echo "--build-number needs a value" >&2; exit 2; }; build_number=$2; shift 2 ;;
    --input) [ "$#" -ge 2 ] || { echo "--input needs a value" >&2; exit 2; }; input=$2; shift 2 ;;
    --candidate) [ "$#" -ge 2 ] || { echo "--candidate needs a value" >&2; exit 2; }; candidate=$2; shift 2 ;;
    --macos-output) [ "$#" -ge 2 ] || { echo "--macos-output needs a value" >&2; exit 2; }; macos_output=$2; shift 2 ;;
    --offline) offline=1; shift ;;
    --publish) publish=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Darwin ] || { echo "private releases must be staged on the Mac build host" >&2; exit 1; }
printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || { echo "invalid version" >&2; exit 2; }
case "$input" in ''|/*) ;; *) echo "--input must be absolute" >&2; exit 2 ;; esac
case "$candidate" in ''|/*) ;; *) echo "--candidate must be absolute" >&2; exit 2 ;; esac
case "$macos_output" in /*) ;; *) echo "--macos-output must be absolute" >&2; exit 2 ;; esac
case "$candidate" in /|"$repo_root"|"$repo_root"/) echo "refusing unsafe candidate root: $candidate" >&2; exit 2 ;; esac

workass_release_require_source "$repo_root"
commit=$WORKASS_RELEASE_COMMIT
if [ -z "$build_number" ]; then build_number=$(workass_release_build_number "$repo_root" "$commit"); fi
printf '%s' "$build_number" | grep -Eq '^[1-9][0-9]*$' || { echo "invalid build number" >&2; exit 2; }

if [ -z "$input" ]; then input="$repo_root/.dev/release-inputs/$version-$commit"; fi
if [ -z "$candidate" ]; then candidate="$repo_root/.dev/release-candidates/$version-$commit"; fi
case "$input" in /|"$repo_root"|"$repo_root"/) echo "refusing unsafe release input: $input" >&2; exit 2 ;; esac

mkdir -p "$candidate"
WORKASS_RELEASE_TIMING_FILE="$candidate/timings.log"
WORKASS_RELEASE_PHASE_LOG_DIR="$candidate/phase-logs/$build_number-$$"
export WORKASS_RELEASE_TIMING_FILE WORKASS_RELEASE_PHASE_LOG_DIR
: > "$WORKASS_RELEASE_TIMING_FILE"
mkdir -p "$WORKASS_RELEASE_PHASE_LOG_DIR"
release_started=$(workass_release_now)

for tool in node ditto unzip codesign plutil file shasum cmp awk diff; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing release staging tool: $tool" >&2; exit 1; }
done

prepare_input() {
  set -- --version "$version" --output "$input"
  [ "$offline" -eq 0 ] || set -- "$@" --offline
  "$script_dir/prepare-input.sh" "$@"
}

stage_macos() {
  "$repo_root/scripts/stage-macos-local-update.sh" \
    --version "$version" \
    --build-number "$build_number" \
    --release-input "$input" \
    --candidate-root "$candidate/macos-app" \
    --output "$candidate/macos-feed"
}

stage_windows() {
  "$repo_root/scripts/stage-windows-portable.sh" \
    --version "$version" \
    --release-input "$input" \
    --output-root "$candidate/windows"
}

safe_zip_entries() {
  zip_file=$1
  unzip -Z1 "$zip_file" | LC_ALL=C awk '
    /^\// || /\\/ { bad=1 }
    { count=split($0, parts, "/"); for (i=1; i<=count; i++) if (parts[i] == "..") bad=1 }
    END { exit bad ? 1 : 0 }
  '
}

verify_candidate() {
  verify_tool="$script_dir/lib/verify-candidate.mjs"
  node "$verify_tool" verify --root "$candidate" --input "$input" \
    --version "$version" --build "$build_number" --commit "$commit"

  mac_zip="$candidate/macos-feed/Workass-$version-darwin-arm64.zip"
  windows_root="$candidate/windows/$version"
  windows_bundle="$windows_root/Workass-$version-windows-amd64"
  windows_zip="$windows_root/Workass-$version-windows-amd64.zip"

  verify_root=$(mktemp -d "$candidate/.verify.XXXXXX")
  trap 'rm -rf "$verify_root"' EXIT HUP INT TERM
  mkdir -p "$verify_root/macos" "$verify_root/windows"
  ditto -x -k "$mac_zip" "$verify_root/macos"
  unzip -q "$windows_zip" -d "$verify_root/windows"
  mac_app="$verify_root/macos/Workass.app"
  extracted_windows_bundle="$verify_root/windows/Workass-$version-windows-amd64"

  diff -qr "$windows_bundle" "$extracted_windows_bundle" >/dev/null
  codesign --verify --deep --strict "$mac_app"
  [ "$(plutil -extract CFBundleShortVersionString raw -o - "$mac_app/Contents/Info.plist")" = "$version" ]
  [ "$(plutil -extract CFBundleVersion raw -o - "$mac_app/Contents/Info.plist")" = "$build_number" ]
  node - "$mac_app/Contents/Resources/app/package.json" \
    "$mac_app/Contents/Resources/runtime/manifest.json" "$version" "$build_number" <<'NODE'
const fs = require('node:fs');
const [shellFile, runtimeFile, version, build] = process.argv.slice(2);
const shell = JSON.parse(fs.readFileSync(shellFile, 'utf8'));
const runtime = JSON.parse(fs.readFileSync(runtimeFile, 'utf8'));
if (shell.version !== version || runtime.version !== version || runtime.build !== build ||
    runtime.platform !== 'darwin' || runtime.arch !== 'arm64') {
  throw new Error('archived macOS shell and runtime versions do not match the candidate');
}
NODE
  requirement=$(codesign -d -r- "$mac_app" 2>&1 | sed -nE 's/^[[:space:]#]*designated =>[[:space:]]*//p' | head -n 1)
  feed_requirement=$(node -e 'const fs=require("node:fs"); process.stdout.write(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).designatedRequirement)' \
    "$candidate/macos-feed/workass-darwin-arm64-release.json")
  [ "$requirement" = "$feed_requirement" ] || { echo "macOS designated requirement differs from its feed" >&2; return 1; }
  unzip -tq "$mac_zip" >/dev/null
  safe_zip_entries "$mac_zip"

  unzip -tq "$windows_zip" >/dev/null
  safe_zip_entries "$windows_zip"
  file "$extracted_windows_bundle/Workass.exe" | grep -Eq 'PE32\+ executable.*x86-64'
  file "$extracted_windows_bundle/workass-daemon.exe" | grep -Eq 'PE32\+ executable.*x86-64'
  node "$repo_root/desktop/scripts/stamp-windows-icon.mjs" --verify \
    --exe "$extracted_windows_bundle/Workass.exe" --icon "$repo_root/desktop/assets/icon.ico"

  node "$verify_tool" record --root "$candidate" --input "$input" \
    --version "$version" --build "$build_number" --commit "$commit"
  rm -rf "$verify_root"
  trap - EXIT HUP INT TERM
}

preflight_macos_publish() {
  candidate_feed="$candidate/macos-feed/workass-darwin-arm64-release.json"
  installed_feed="$macos_output/workass-darwin-arm64-release.json"
  node - "$candidate_feed" "$installed_feed" "$macos_output" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const [candidateFile, installedFile, outputRoot] = process.argv.slice(2);
const candidate = JSON.parse(fs.readFileSync(candidateFile, 'utf8'));
const parse = (value) => {
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(value)) throw new Error(`invalid feed version: ${value}`);
  return value.split('.').map(Number);
};
const compare = (left, right) => {
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return left[index] - right[index];
  }
  return 0;
};
if (!fs.existsSync(installedFile)) process.exit(0);
const installed = JSON.parse(fs.readFileSync(installedFile, 'utf8'));
const ordering = compare(parse(candidate.version), parse(installed.version));
if (ordering < 0) throw new Error(`candidate ${candidate.version} is older than published Mac feed ${installed.version}`);
if (ordering === 0) {
  const candidateArchive = path.join(path.dirname(candidateFile), candidate.artifacts.update.name);
  const installedArchive = path.join(outputRoot, installed.artifacts.update.name);
  if (!fs.existsSync(installedArchive) || !fs.readFileSync(candidateFile).equals(fs.readFileSync(installedFile)) ||
      !fs.readFileSync(candidateArchive).equals(fs.readFileSync(installedArchive))) {
    throw new Error(`published Mac version ${candidate.version} is immutable and has different bytes`);
  }
  process.stdout.write('already-current\n');
}
NODE
}

publish_macos() {
  candidate_feed_root="$candidate/macos-feed"
  archive_name="Workass-$version-darwin-arm64.zip"
  feed_name=workass-darwin-arm64-release.json
  mkdir -p "$macos_output"
  if [ -f "$macos_output/$feed_name" ] && cmp -s "$candidate_feed_root/$feed_name" "$macos_output/$feed_name"; then
    [ -f "$macos_output/$archive_name" ] && cmp -s "$candidate_feed_root/$archive_name" "$macos_output/$archive_name" || {
      echo "published Mac manifest points at different archive bytes" >&2
      return 1
    }
    echo "WORKASS_MACOS_LOCAL_UPDATE_ALREADY_CURRENT"
    return 0
  fi

  archive_incoming="$macos_output/.$archive_name.incoming.$$"
  sums_incoming="$macos_output/.SHA256SUMS.incoming.$$"
  feed_incoming="$macos_output/.$feed_name.incoming.$$"
  cleanup_publish_incoming() {
    rm -f "$archive_incoming" "$sums_incoming" "$feed_incoming"
  }
  trap cleanup_publish_incoming EXIT HUP INT TERM
  cp "$candidate_feed_root/$archive_name" "$archive_incoming"
  cp "$candidate_feed_root/SHA256SUMS" "$sums_incoming"
  cp "$candidate_feed_root/$feed_name" "$feed_incoming"
  expected=$(shasum -a 256 "$candidate_feed_root/$archive_name" | awk '{print $1}')
  actual=$(shasum -a 256 "$archive_incoming" | awk '{print $1}')
  [ "$actual" = "$expected" ] || { echo "Mac feed copy changed archive bytes" >&2; return 1; }
  mv -f "$archive_incoming" "$macos_output/$archive_name"
  mv -f "$sums_incoming" "$macos_output/SHA256SUMS"
  mv -f "$feed_incoming" "$macos_output/$feed_name"
  trap - EXIT HUP INT TERM
  echo "WORKASS_MACOS_LOCAL_UPDATE_READY"
  echo "feed=$macos_output/$feed_name"
}

publish_windows() {
  "$script_dir/publish-windows.sh" \
    --version "$version" \
    --commit "$commit" \
    --release-dir "$candidate/windows/$version" \
    --receipt "$candidate/windows-publication.json"
}

verify_published_release() {
  "$script_dir/publish-windows.sh" \
    --version "$version" \
    --commit "$commit" \
    --release-dir "$candidate/windows/$version" \
    --verify-only \
    --receipt "$candidate/windows-publication.json"
  node "$script_dir/lib/verify-publication.mjs" record \
    --root "$candidate" \
    --macos-output "$macos_output" \
    --version "$version" \
    --build "$build_number" \
    --commit "$commit"
}

workass_release_run_phase prepare_release_input prepare_input
if [ -f "$candidate/receipt.json" ]; then
  workass_release_run_phase verify_cached_candidate verify_candidate
  echo "WORKASS_RELEASE_CANDIDATE_REUSED"
else
  workass_release_run_parallel_pair stage_macos stage_macos stage_windows stage_windows
  workass_release_run_phase verify_candidates verify_candidate
fi

if [ "$publish" -eq 1 ]; then
  workass_release_run_phase preflight_macos_publish preflight_macos_publish
  workass_release_run_phase publish_windows publish_windows
  workass_release_run_phase publish_macos publish_macos
  workass_release_run_phase verify_published_release verify_published_release
fi

release_finished=$(workass_release_now)
release_seconds=$((release_finished - release_started))
workass_release_timing_emit "WORKASS_RELEASE_TOTAL status=passed seconds=$release_seconds"
echo "WORKASS_RELEASE_CANDIDATE_READY"
echo "version=$version"
echo "commit=$commit"
echo "candidate=$candidate"
echo "published=$publish"
echo "timings=$WORKASS_RELEASE_TIMING_FILE"
echo "phase_logs=$WORKASS_RELEASE_PHASE_LOG_DIR"
if [ "$publish" -eq 1 ]; then
  echo "publication=$candidate/publication.json"
  echo "release=https://github.com/Dukler/workass/releases/tag/v$version"
fi
