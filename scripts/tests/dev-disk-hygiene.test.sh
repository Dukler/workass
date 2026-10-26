#!/bin/sh
# dev-disk-hygiene.sh against a fake repo, scratch root and Brave clone folder.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
hygiene="$script_dir/../dev-disk-hygiene.sh"
work=$(mktemp -d /private/tmp/hygiene-test.XXXXXX)
busy_pid=''
cleanup() { [ -n "$busy_pid" ] && kill "$busy_pid" 2>/dev/null; rm -rf "$work"; }
trap cleanup EXIT

repo="$work/repo"; tmp="$work/tmp"; clones="$work/clones"
mkdir -p "$repo" "$tmp" "$clones"
git -C "$repo" init -q && git -C "$repo" commit -q --allow-empty -m init
old() { touch -t 202001010000 "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
run() {
  WORKASS_HYGIENE_REPO_ROOT="$repo" WORKASS_HYGIENE_TMP_ROOT="$tmp" WORKASS_BRAVE_CLONE_DIR="$clones" \
    WORKASS_INSTALLED_VERSION=0.1.228 sh "$hygiene" "$@"
}

for v in 0.1.226-a 0.1.227-b 0.1.228-c 0.1.229-d 0.1.230-e; do
  mkdir -p "$repo/.dev/release-candidates/$v" "$repo/.dev/release-inputs/$v"
done
mkdir -p "$repo/.dev/release-worktrees" "$repo/.dev/rebuild" "$repo/.dev/package/Workass.app"
git -C "$repo" worktree add -q --detach "$repo/.dev/release-worktrees/macos-0.1.20-abc" 2>/dev/null
echo log > "$repo/.dev/rebuild/old.log"; old "$repo/.dev/rebuild/old.log"
echo log > "$repo/.dev/rebuild/new.log"
old "$repo/.dev/package/Workass.app"

mkdir -p "$tmp/wk-done/sub" "$tmp/wk-busy" "$tmp/wk-keep" "$tmp/wk-recent"
echo x > "$tmp/wk-done/sub/f"; old "$tmp/wk-done/sub/f" "$tmp/wk-done/sub" "$tmp/wk-done"
touch "$tmp/wk-keep/.keep-scratch"; old "$tmp/wk-keep/.keep-scratch" "$tmp/wk-keep"
echo x > "$tmp/wk-recent/f"
old "$tmp/wk-busy"
(cd "$tmp/wk-busy" && exec sleep 60) &
busy_pid=$!

mkdir -p "$clones/code_sign_clone.old" "$clones/code_sign_clone.new"; old "$clones/code_sign_clone.old"

# Dry runs change nothing.
run prune-releases >/dev/null
run sweep-scratch >/dev/null
run brave-clones >/dev/null
[ -d "$repo/.dev/release-candidates/0.1.226-a" ] && [ -d "$tmp/wk-done" ] && [ -d "$clones/code_sign_clone.old" ] ||
  fail "a dry run deleted something"

run auto >/dev/null
# Releases: the installed 0.1.228, the one before it and the newer ones stay.
[ ! -e "$repo/.dev/release-candidates/0.1.226-a" ] || fail "0.1.226 candidate kept"
[ ! -e "$repo/.dev/release-inputs/0.1.226-a" ] || fail "0.1.226 input kept"
for v in 0.1.227-b 0.1.228-c 0.1.229-d 0.1.230-e; do
  [ -d "$repo/.dev/release-candidates/$v" ] || fail "$v candidate deleted"
done
[ ! -e "$repo/.dev/release-worktrees/macos-0.1.20-abc" ] || fail "old release worktree kept"
git -C "$repo" worktree list | grep -q 'macos-0.1.20' && fail "old release worktree still registered"
[ ! -e "$repo/.dev/rebuild/old.log" ] || fail "stale rebuild log kept"
[ -e "$repo/.dev/rebuild/new.log" ] || fail "fresh rebuild log deleted"
[ -d "$repo/.dev/package/Workass.app" ] || fail "staged Workass.app deleted"
# Scratch: finished goes; in use, protected and recent stay.
[ ! -e "$tmp/wk-done" ] || fail "finished scratch kept"
[ -d "$tmp/wk-busy" ] || fail "scratch in use deleted"
[ -d "$tmp/wk-keep" ] || fail ".keep-scratch ignored"
[ -d "$tmp/wk-recent" ] || fail "recently written scratch deleted"
# Brave: the leftover goes, the one as new as a running Brave stays.
[ ! -e "$clones/code_sign_clone.old" ] || fail "leftover Brave clone kept"
[ -d "$clones/code_sign_clone.new" ] || fail "fresh Brave clone deleted"

# Scratch helpers and the budget.
dir=$(run scratch-begin probe)
[ "$dir" = "$tmp/wk-probe" ] && [ -d "$dir" ] || fail "scratch-begin did not create $tmp/wk-probe"
run scratch-end "$dir"
[ ! -e "$dir" ] || fail "scratch-end left $dir"
if run scratch-end "$repo/.dev/release-candidates" 2>/dev/null; then fail "scratch-end deleted outside scratch"; fi
[ -d "$repo/.dev/release-candidates" ] || fail "scratch-end deleted outside scratch"
set +e
WORKASS_DEV_DISK_BUDGET_GB=0 run budget-check >/dev/null 2>&1; budget_code=$?
WORKASS_DEV_DISK_BUDGET_GB=0 run scratch-begin refused >/dev/null 2>&1; begin_code=$?
set -e
[ "$budget_code" = 3 ] || fail "budget-check did not refuse over budget ($budget_code)"
[ "$begin_code" = 3 ] && [ ! -e "$tmp/wk-refused" ] || fail "scratch-begin did not refuse over budget"

echo "dev-disk-hygiene: ok"
