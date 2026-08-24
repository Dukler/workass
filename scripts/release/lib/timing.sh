#!/bin/sh

# Small POSIX timing surface shared by the release pipeline. Timing output is
# deliberately machine-readable so a slow release identifies its own phase in
# the profile/build history instead of requiring another forensic pass.

workass_release_now() {
  date +%s
}

workass_release_timing_emit() {
  timing_line=$1
  printf '%s\n' "$timing_line"
  if [ -n "${WORKASS_RELEASE_TIMING_FILE:-}" ]; then
    printf '%s\n' "$timing_line" >> "$WORKASS_RELEASE_TIMING_FILE"
  fi
}

workass_release_run_phase() {
  phase_name=$1
  shift
  case "$phase_name" in
    ''|*[!a-z0-9_-]*)
      echo "invalid release phase name: $phase_name" >&2
      return 2
      ;;
  esac

  phase_started=$(workass_release_now)
  workass_release_timing_emit "WORKASS_RELEASE_PHASE_START name=$phase_name epoch=$phase_started"
  phase_log=''
  if [ -n "${WORKASS_RELEASE_PHASE_LOG_DIR:-}" ]; then
    mkdir -p "$WORKASS_RELEASE_PHASE_LOG_DIR"
    phase_log="$WORKASS_RELEASE_PHASE_LOG_DIR/$phase_name.log"
    : > "$phase_log"
  fi
  if [ -n "$phase_log" ]; then
    if "$@" >"$phase_log" 2>&1; then
      phase_status=passed
      phase_code=0
    else
      phase_code=$?
      phase_status=failed
    fi
  elif "$@"; then
    phase_status=passed
    phase_code=0
  else
    phase_code=$?
    phase_status=failed
  fi
  phase_finished=$(workass_release_now)
  phase_seconds=$((phase_finished - phase_started))
  workass_release_timing_emit "WORKASS_RELEASE_PHASE_END name=$phase_name status=$phase_status seconds=$phase_seconds"
  if [ -n "$phase_log" ]; then
    workass_release_timing_emit "WORKASS_RELEASE_PHASE_LOG name=$phase_name log=$phase_log"
    if [ "$phase_status" = failed ]; then
      echo "release phase failed: $phase_name (full output: $phase_log)" >&2
      tail -24 "$phase_log" >&2
    fi
  fi
  return "$phase_code"
}

# Run two independent, already-named release phases concurrently. Both are
# always reaped before the function returns, so a fast failure cannot leave an
# untracked packager or runtime copy behind.
workass_release_run_parallel_pair() {
  [ "$#" -eq 4 ] || {
    echo "parallel release pair requires: PHASE COMMAND PHASE COMMAND" >&2
    return 2
  }
  parallel_left_phase=$1
  parallel_left_command=$2
  parallel_right_phase=$3
  parallel_right_command=$4

  (workass_release_run_phase "$parallel_left_phase" "$parallel_left_command") &
  parallel_left_pid=$!
  (workass_release_run_phase "$parallel_right_phase" "$parallel_right_command") &
  parallel_right_pid=$!

  parallel_left_status=0
  parallel_right_status=0
  wait "$parallel_left_pid" || parallel_left_status=$?
  wait "$parallel_right_pid" || parallel_right_status=$?
  [ "$parallel_left_status" -eq 0 ] || return "$parallel_left_status"
  return "$parallel_right_status"
}
