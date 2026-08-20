#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
guard="$repo_root/scripts/safe-profile-file.sh"
fixture_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-safe-profile.XXXXXX")
trap 'rm -rf -- "$fixture_root"' EXIT HUP INT TERM

expect_rejected() {
  if "$guard" check "$1" >/dev/null 2>&1; then
    printf 'expected rejection: %s\n' "$1" >&2
    exit 1
  fi
}

printf 'alpha\nbeta alpha\n' > "$fixture_root/safe.log"
check_output=$("$guard" check "$fixture_root/safe.log")
case "$check_output" in safe_file=*) ;; *) exit 1 ;; esac

search_output=$("$guard" search-count alpha "$fixture_root/safe.log")
case "$search_output" in *'match_count=2') ;; *) exit 1 ;; esac

mkdir -p "$fixture_root/apple-container" "$fixture_root/directory"
printf 'text\n' > "$fixture_root/apple-container/state.json"
expect_rejected "$fixture_root/apple-container/state.json"
expect_rejected "$fixture_root/directory"

ln -s "$fixture_root/safe.log" "$fixture_root/link.log"
expect_rejected "$fixture_root/link.log"

dd if=/dev/zero of="$fixture_root/sparse.img" bs=1 count=0 seek=1073741824 2>/dev/null
expect_rejected "$fixture_root/sparse.img"

printf '\000\001\002' > "$fixture_root/binary.bin"
expect_rejected "$fixture_root/binary.bin"

printf 'safe profile guard: pass\n'
