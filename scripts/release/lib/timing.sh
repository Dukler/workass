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
  if "$@"; then
    phase_status=passed
    phase_code=0
  else
    phase_code=$?
    phase_status=failed
  fi
  phase_finished=$(workass_release_now)
  phase_seconds=$((phase_finished - phase_started))
  workass_release_timing_emit "WORKASS_RELEASE_PHASE_END name=$phase_name status=$phase_status seconds=$phase_seconds"
  return "$phase_code"
}
