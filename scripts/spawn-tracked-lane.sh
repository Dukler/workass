#!/bin/sh
set -eu

usage() {
  echo "usage: scripts/spawn-tracked-lane.sh --label LABEL [--output FILE] [--service] -- cmd args..." >&2
  exit 2
}

label=''
output=''
# Work unless told otherwise. --service marks a lane that is expected never to
# finish (a dev server, a watcher): it is echoed on the LANE line so the caller
# passes role=service to workass_register_external_work, and the chat is not
# reported as working for as long as the process lives.
role='work'
while [ "$#" -gt 0 ]; do
  case "$1" in
    --label)
      [ "$#" -ge 2 ] || usage
      label=$2
      shift 2
      ;;
    --service)
      role='service'
      shift
      ;;
    --output)
      [ "$#" -ge 2 ] || usage
      output=$2
      shift 2
      ;;
    --)
      shift
      break
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$label" ] || usage
[ "$#" -gt 0 ] || usage

if [ -z "$output" ]; then
  # /tmp deliberately, not $TMPDIR: the daemon validates registered paths
  # against ITS OWN temp roots, and the agent's per-user /var/folders TMPDIR
  # is not among them (learned live 2026-07-18).
  lane_dir="/tmp/workass-lanes"
  mkdir -p "$lane_dir"
  chmod 700 "$lane_dir" 2>/dev/null || true
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  random=$(dd if=/dev/urandom bs=6 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
  output="$lane_dir/lane-$stamp-${random:-$$}.output"
fi

case "$output" in
  /*) ;;
  *) echo "output must be an absolute path" >&2; exit 2 ;;
esac
case "$output" in
  */) echo "output must be a file path, not a directory path" >&2; exit 2 ;;
esac

done_file="$output.done"
receipt_label=$(printf '%s' "$label" | tr '\r\n\t' '   ')
umask 077
worker='output=$1
done_file=$2
shift 2
"$@"
code=$?
tmp_done="$done_file.$$"
if printf "%s\n" "$code" > "$tmp_done"; then
  mv -f "$tmp_done" "$done_file" || rm -f "$tmp_done"
else
  rm -f "$tmp_done"
fi
exit "$code"'

if command -v setsid >/dev/null 2>&1; then
  nohup setsid sh -c "$worker" workass-lane "$output" "$done_file" "$@" >>"$output" 2>&1 &
else
  python3=$(command -v python3 2>/dev/null || true)
  [ -n "$python3" ] || { echo "setsid command is unavailable and python3 fallback is missing" >&2; exit 2; }
  nohup "$python3" -c 'import os, sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' sh -c "$worker" workass-lane "$output" "$done_file" "$@" >>"$output" 2>&1 &
fi
pid=$!

printf 'LANE pid=%s output=%s done=%s role=%s label=%s\n' "$pid" "$output" "$done_file" "$role" "$receipt_label"
