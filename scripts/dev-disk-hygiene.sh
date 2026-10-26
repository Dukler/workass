#!/bin/sh
# Keeps Workass dev work from filling the Mac's disk (operator rule, 2026-10-26).
# Builds, publishes and bench runs call `auto`; harnesses wrap their scratch in
# scratch-begin / scratch-end. Every deleting command is a dry run without
# --apply, except `auto` and `scratch-end`, which are the enforced paths.
#
#   dev-disk-hygiene.sh report                 sizes against the budget
#   dev-disk-hygiene.sh auto                   apply every rule below, then report
#   dev-disk-hygiene.sh prune-releases [--apply]
#       .dev/release-candidates, release-inputs, release-worktrees, package/candidates:
#       keep the installed version, the one before it, and anything newer (built,
#       not yet installed); delete older ones. .dev/rebuild and .dev/package loose
#       files: delete after 14 days.
#   dev-disk-hygiene.sh sweep-scratch [--apply]
#       /private/tmp/wk-*, /private/tmp/t3-nightly and .dev/bench/run-*: delete what
#       no running process uses (args or cwd) and nothing wrote to for 2 hours.
#       A `.keep-scratch` file inside protects a directory.
#   dev-disk-hygiene.sh brave-clones [--apply]
#       Brave leaves a code_sign_clone copy each time it is killed instead of quit;
#       delete the ones no Brave process has open (and none from the last 10 minutes).
#   dev-disk-hygiene.sh budget-check           exit 3 when over budget
#   dev-disk-hygiene.sh scratch-begin NAME     budget-check, then create and print /private/tmp/wk-NAME
#   dev-disk-hygiene.sh scratch-end DIR        delete DIR (only /private/tmp/wk-* or .dev/bench/run-*)
#
# Budget: WORKASS_DEV_DISK_BUDGET_GB (default 20) over .dev plus scratch, as du
# lists it. APFS clones make du overstate, so the golden-state fixtures
# (.dev/bench/golden-state-*, clones of a live state) are reported apart and not
# counted, and every apply step reports the space it really freed (df before/after).
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=${WORKASS_HYGIENE_REPO_ROOT:-$(CDPATH= cd -- "$script_dir/.." && pwd)}
dev="$repo_root/.dev"
tmp_root=${WORKASS_HYGIENE_TMP_ROOT:-/private/tmp}
budget_gb=${WORKASS_DEV_DISK_BUDGET_GB:-20}
quiet_minutes=${WORKASS_HYGIENE_QUIET_MINUTES:-120}
loose_days=${WORKASS_HYGIENE_LOOSE_DAYS:-14}

say() { printf '%s\n' "$*"; }
avail_kb() { df -k "${1:-$dev}" 2>/dev/null | awk 'NR==2 {print $4}'; }
human_kb() { awk -v k="$1" 'BEGIN { if (k >= 1048576) printf "%.1f GB", k/1048576; else printf "%.0f MB", k/1024 }'; }
size_kb() { du -sk "$1" 2>/dev/null | awk '{print $1+0}'; }

installed_version() {
  if [ -n "${WORKASS_INSTALLED_VERSION:-}" ]; then printf '%s' "$WORKASS_INSTALLED_VERSION"; return; fi
  defaults read /Applications/Workass.app/Contents/Info CFBundleShortVersionString 2>/dev/null || true
}

# X.Y.Z found in a directory name, or nothing.
version_of() { printf '%s' "$1" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' | head -n 1; }
# Exit 0 when $1 < $2 (dotted numeric versions).
version_lt() {
  [ "$1" = "$2" ] && return 1
  lowest=$(printf '%s\n%s\n' "$1" "$2" | sort -t. -k1,1n -k2,2n -k3,3n | head -n 1)
  [ "$lowest" = "$1" ]
}

# The lowest version to keep: the release just before the installed one, taken
# across every release folder (or the second highest when nothing is installed).
release_dirs="release-candidates release-inputs release-worktrees package/candidates"
keep_floor() {
  installed=$1
  versions=$(for name in $release_dirs; do
      for entry in "$dev/$name"/*; do [ -e "$entry" ] || continue; version_of "$(basename "$entry")"; echo; done
    done | grep -E . | sort -u -t. -k1,1n -k2,2n -k3,3n)
  [ -n "$versions" ] || return 0
  if [ -n "$installed" ]; then
    below=$(printf '%s\n' "$versions" | while read -r v; do if version_lt "$v" "$installed"; then echo "$v"; fi; done | tail -n 1)
    printf '%s' "${below:-$installed}"
  else
    printf '%s\n' "$versions" | tail -n 2 | head -n 1
  fi
}

remove_path() {
  target=$1
  case "$target" in
    "$dev"/release-worktrees/*)
      if git -C "$repo_root" worktree list --porcelain 2>/dev/null | grep -qxF "worktree $target"; then
        git -C "$repo_root" worktree remove --force "$target" 2>/dev/null || rm -rf "$target"
      else
        rm -rf "$target"
      fi ;;
    *) rm -rf "$target" ;;
  esac
}

prune_releases() {
  apply=$1 installed=$(installed_version)
  floor=$(keep_floor "$installed")
  say "installed version: ${installed:-unknown}; keeping ${floor:-everything} and newer"
  for name in $release_dirs; do
    dir="$dev/$name"
    [ -d "$dir" ] && [ -n "$floor" ] || continue
    for entry in "$dir"/*; do
      [ -e "$entry" ] || continue
      v=$(version_of "$(basename "$entry")")
      [ -n "$v" ] || continue
      version_lt "$v" "$floor" || continue
      say "  old $name/$(basename "$entry") ($v < $floor)"
      if [ "$apply" = 1 ]; then remove_path "$entry"; fi
    done
  done
  if [ "$apply" = 1 ]; then git -C "$repo_root" worktree prune 2>/dev/null || true; fi
  for name in rebuild package; do
    dir="$dev/$name"
    [ -d "$dir" ] || continue
    find "$dir" -mindepth 1 -maxdepth 1 -mtime +"$loose_days" \
      ! -name candidates ! -name Workass.app 2>/dev/null | while read -r entry; do
      say "  stale $name/$(basename "$entry") (older than $loose_days days)"
      if [ "$apply" = 1 ]; then rm -rf "$entry"; fi
    done
  done
}

scratch_dirs() {
  for entry in "$tmp_root"/wk-* "$tmp_root"/t3-nightly "$dev"/bench/run-*; do
    [ -e "$entry" ] && printf '%s\n' "$entry"
  done
}

sweep_scratch() {
  apply=$1
  procs=$(ps -axww -o command= 2>/dev/null || true)
  cwds=$(lsof -nP -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' || true)
  scratch_dirs | while read -r entry; do
    resolved=$(cd "$entry" 2>/dev/null && pwd -P || printf '%s' "$entry")
    if [ -e "$entry/.keep-scratch" ]; then say "  keep $entry (.keep-scratch)"; continue; fi
    if printf '%s\n%s\n' "$procs" "$cwds" | grep -qF -e "$entry" -e "$resolved" -e "/tmp/$(basename "$entry")"; then say "  in use $entry"; continue; fi
    if [ -n "$(find "$entry" -maxdepth 3 -mmin -"$quiet_minutes" 2>/dev/null | head -n 1)" ]; then
      say "  recent $entry (written in the last $quiet_minutes minutes)"; continue
    fi
    say "  finished $entry"
    if [ "$apply" = 1 ]; then rm -rf "$entry"; fi
  done
}

brave_clone_root() {
  if [ -n "${WORKASS_BRAVE_CLONE_DIR:-}" ]; then printf '%s' "$WORKASS_BRAVE_CLONE_DIR"; return; fi
  temp=$(getconf DARWIN_USER_TEMP_DIR 2>/dev/null || true)
  [ -n "$temp" ] && printf '%s/X/com.brave.Browser.code_sign_clone' "$(dirname "$temp")"
}

brave_clones() {
  apply=$1 root=$(brave_clone_root)
  [ -n "$root" ] && [ -d "$root" ] || { say "  no Brave clone folder"; return 0; }
  # A clone is in use while any Brave process has a file in it open or mapped;
  # skip those, and anything from the last 10 minutes (a Brave still starting).
  inuse=$(lsof -nP -c Brave -Fn 2>/dev/null | sed -n 's/^n//p' | grep -F "$root/" | sed "s|^$root/||; s|/.*||" | sort -u || true)
  recent=$(( $(date +%s) - 600 ))
  removed=0 kept=0
  for clone in "$root"/code_sign_clone.*; do
    [ -e "$clone" ] || continue
    if printf '%s\n' "$inuse" | grep -qxF "$(basename "$clone")" || [ "$(stat -f %m "$clone")" -ge "$recent" ]; then
      kept=$((kept + 1))
    else
      removed=$((removed + 1))
      if [ "$apply" = 1 ]; then rm -rf "$clone"; fi
    fi
  done
  say "  Brave clones: $removed leftover, $kept in use or new"
}

report() {
  total=0
  say "Workass dev disk (du sizes; APFS clones make them overstate):"
  for entry in "$dev"/* $(scratch_dirs | grep -v "^$dev/"); do
    [ -e "$entry" ] || continue
    kb=$(size_kb "$entry")
    case "$entry" in
      "$dev"/bench) fixture=0
        for g in "$dev"/bench/golden-state-*; do [ -e "$g" ] && fixture=$((fixture + $(size_kb "$g"))); done
        [ "$fixture" -gt 0 ] && say "  $(human_kb "$fixture")  .dev/bench/golden-state-* (fixture of live-state clones, not counted)"
        kb=$((kb - fixture)) ;;
    esac
    total=$((total + kb))
    [ "$kb" -ge 512000 ] && say "  $(human_kb "$kb")  ${entry#"$repo_root/"}"
  done
  say "total counted: $(human_kb "$total") of a ${budget_gb} GB budget; disk free: $(human_kb "$(avail_kb)")"
  counted_kb=$total
}

over_budget() {
  report >/dev/null
  [ "$counted_kb" -gt $((budget_gb * 1048576)) ]
}

with_freed() {
  label=$1; shift
  before=$(avail_kb)
  "$@"
  after=$(avail_kb)
  say "$label: really freed $(human_kb $(( after > before ? after - before : 0 )))"
}

apply_flag() { [ "${1:-}" = "--apply" ] && echo 1 || echo 0; }

cmd=${1:-report}
[ "$#" -gt 0 ] && shift
case "$cmd" in
  report) report ;;
  prune-releases) a=$(apply_flag "${1:-}"); if [ "$a" = 1 ]; then with_freed "releases" prune_releases 1; else prune_releases 0; fi ;;
  sweep-scratch) a=$(apply_flag "${1:-}"); if [ "$a" = 1 ]; then with_freed "scratch" sweep_scratch 1; else sweep_scratch 0; fi ;;
  brave-clones) a=$(apply_flag "${1:-}"); if [ "$a" = 1 ]; then with_freed "brave clones" brave_clones 1; else brave_clones 0; fi ;;
  auto)
    with_freed "releases" prune_releases 1
    with_freed "scratch" sweep_scratch 1
    with_freed "brave clones" brave_clones 1
    report
    if [ "$counted_kb" -gt $((budget_gb * 1048576)) ]; then say "WARNING: Workass dev disk is over its ${budget_gb} GB budget"; fi ;;
  budget-check)
    if over_budget; then report >&2; say "Workass dev disk is over its ${budget_gb} GB budget; run: scripts/dev-disk-hygiene.sh auto" >&2; exit 3; fi ;;
  scratch-begin)
    name=${1:?usage: scratch-begin NAME}
    case "$name" in */*|.*|'') say "scratch name must be a plain word" >&2; exit 2 ;; esac
    if over_budget; then say "refusing new scratch: Workass dev disk is over its ${budget_gb} GB budget; run: scripts/dev-disk-hygiene.sh auto" >&2; exit 3; fi
    dir="$tmp_root/wk-$name"; mkdir -p "$dir"; printf '%s\n' "$dir" ;;
  scratch-end)
    target=${1:?usage: scratch-end DIR}
    case "$target" in
      "$tmp_root"/wk-?*|"$dev"/bench/run-?*) rm -rf "$target" ;;
      *) say "scratch-end only deletes $tmp_root/wk-* or .dev/bench/run-*: $target" >&2; exit 2 ;;
    esac ;;
  -h|--help) sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//' ;;
  *) say "unknown command: $cmd" >&2; exit 2 ;;
esac
