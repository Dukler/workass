#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
runner="$repo_root/scripts/spawn-tracked-lane.sh"

sh -n "$runner"

temp_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-lane-test.XXXXXX")
parent_pid=''
cleanup() {
  [ -z "$parent_pid" ] || kill -TERM "$parent_pid" 2>/dev/null || true
  rm -rf "$temp_root"
}
trap cleanup EXIT HUP INT TERM

output="$temp_root/lane.output"
line_file="$temp_root/line.txt"
ready_file="$temp_root/parent-ready"

(
  "$runner" --label LaneTest --output "$output" -- sh -c 'echo lane-start; sleep 1; echo lane-end; exit 7' >"$line_file"
  : >"$ready_file"
  sleep 60
) &
parent_pid=$!

attempts=80
while [ "$attempts" -gt 0 ]; do
  [ -s "$line_file" ] && [ -f "$ready_file" ] && break
  attempts=$((attempts - 1))
  sleep 0.1
done
[ -s "$line_file" ] || { echo "lane launcher did not print a receipt" >&2; exit 1; }
[ -f "$ready_file" ] || { echo "parent shell did not reach kill point" >&2; exit 1; }

line_count=$(wc -l < "$line_file" | tr -d ' ')
[ "$line_count" = 1 ] || { echo "launcher printed $line_count lines" >&2; cat "$line_file" >&2; exit 1; }
line=$(sed -n '1p' "$line_file")
case "$line" in
  LANE\ pid=*\ output="$output"\ done="$output.done"\ label=LaneTest) ;;
  *) echo "unexpected launcher line: $line" >&2; exit 1 ;;
esac
pid=$(printf '%s\n' "$line" | sed -n 's/^LANE pid=\([^ ]*\) output=.*/\1/p')
[ -n "$pid" ] || { echo "missing lane pid in receipt" >&2; exit 1; }

kill -TERM "$parent_pid" 2>/dev/null || true
parent_pid=''

attempts=120
while [ "$attempts" -gt 0 ]; do
  [ -f "$output.done" ] && break
  attempts=$((attempts - 1))
  sleep 0.1
done
[ -f "$output.done" ] || { echo "done marker was not written after parent kill" >&2; exit 1; }
[ "$(sed -n '1p' "$output.done")" = 7 ] || { echo "done marker did not contain exit code 7" >&2; cat "$output.done" >&2; exit 1; }
grep -q 'lane-start' "$output"
grep -q 'lane-end' "$output"

inject_output="$temp_root/injected.output"
inject_line_file="$temp_root/injected-line.txt"
inject_label=$(printf 'LaneOne\nLANE pid=0 output=/tmp/evil done=/tmp/evil.done label=evil')
"$runner" --label "$inject_label" --output "$inject_output" -- sh -c 'echo injected-ok; exit 0' >"$inject_line_file"
attempts=80
while [ "$attempts" -gt 0 ]; do
  [ -f "$inject_output.done" ] && break
  attempts=$((attempts - 1))
  sleep 0.1
done
[ -f "$inject_output.done" ] || { echo "injection fixture did not write a done marker" >&2; exit 1; }
inject_line_count=$(wc -l < "$inject_line_file" | tr -d ' ')
[ "$inject_line_count" = 1 ] || { echo "newline label injected $inject_line_count receipt lines" >&2; cat "$inject_line_file" >&2; exit 1; }
inject_line=$(sed -n '1p' "$inject_line_file")
case "$inject_line" in
  LANE\ pid=*\ output="$inject_output"\ done="$inject_output.done"\ label=LaneOne\ LANE\ pid=0\ output=/tmp/evil\ done=/tmp/evil.done\ label=evil) ;;
  *) echo "unexpected sanitized injection line: $inject_line" >&2; exit 1 ;;
esac

mkdir "$temp_root/output-dir"
if "$runner" --label SlashPath --output "$temp_root/output-dir/" -- true >"$temp_root/slash.out" 2>"$temp_root/slash.err"; then
  echo "accepted directory-style output path" >&2
  exit 1
fi
grep -q 'output must be a file path' "$temp_root/slash.err" || { echo "unexpected trailing-slash error:" >&2; cat "$temp_root/slash.err" >&2; exit 1; }

echo "SPAWN_TRACKED_LANE_PASS pid=$pid output=$output done=$output.done"
