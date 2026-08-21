#!/bin/sh

# Canonical profile loader shared by macOS launch, package, and
# rebuild scripts. Call workass_load_profile <prod|dev|test> after defining
# WORKASS_REPO_ROOT. Profile files are trusted repository inputs, but still use
# a strict assignment-only grammar so a local override cannot execute commands.

workass_profile_die() {
  echo "workass-profile: $*" >&2
  return 1
}

workass_profile_validate_file() {
  profile_file="$1"
  [ -f "$profile_file" ] || workass_profile_die "missing profile file: $profile_file" || return 1
  awk '
    /^[[:space:]]*($|#)/ { next }
    {
      line=$0
      key=line
      sub(/=.*/, "", key)
      if (key !~ /^WORKASS_[A-Z0-9_]+$/) exit 10
      if (line ~ /`|\$\(|;|&&|\|\|/) exit 11
      allowed[key]=1
      if (key != "WORKASS_PROFILE" && key != "WORKASS_APP_NAME" &&
          key != "WORKASS_BUNDLE_ID" && key != "WORKASS_DAEMON_PORT" &&
          key != "WORKASS_DAEMON_BIND" && key != "WORKASS_VIEW_PORT" &&
          key != "WORKASS_LAUNCHD_LABEL" && key != "WORKASS_DATA_ROOT" &&
          key != "WORKASS_LOG_ROOT") exit 12
    }
  ' "$profile_file" || workass_profile_die "unsafe or unknown assignment in $profile_file" || return 1
}

workass_profile_require_absolute() {
  profile_name="$1"
  profile_value="$2"
  case "$profile_value" in
    /*) ;;
    *) workass_profile_die "$profile_name must be absolute: $profile_value" || return 1 ;;
  esac
}

workass_load_profile() {
  requested_profile="${1:-}"
  case "$requested_profile" in
    prod|dev|test) ;;
    *) workass_profile_die "profile must be prod, dev, or test" || return 1 ;;
  esac
  [ -n "${WORKASS_REPO_ROOT:-}" ] || workass_profile_die "WORKASS_REPO_ROOT is required" || return 1

  canonical_file="$WORKASS_REPO_ROOT/config/environments/$requested_profile.env"
  local_file="$WORKASS_REPO_ROOT/config/environments/local/$requested_profile.local.env"
  workass_profile_validate_file "$canonical_file" || return 1
  # shellcheck disable=SC1090
  . "$canonical_file"
  if [ -f "$local_file" ]; then
    workass_profile_validate_file "$local_file" || return 1
    # shellcheck disable=SC1090
    . "$local_file"
  fi

  [ "$WORKASS_PROFILE" = "$requested_profile" ] || workass_profile_die "profile identity mismatch in $canonical_file" || return 1
  case "$WORKASS_DAEMON_BIND" in localhost|lan) ;; *) workass_profile_die "invalid daemon bind: $WORKASS_DAEMON_BIND" || return 1 ;; esac
  case "$WORKASS_DAEMON_PORT:$WORKASS_VIEW_PORT" in *[!0-9:]*|:*) workass_profile_die "profile ports must be numeric" || return 1 ;; esac
  if [ "$requested_profile" != test ] && { [ "$WORKASS_DAEMON_PORT" -eq 0 ] || [ "$WORKASS_VIEW_PORT" -eq 0 ]; }; then
    workass_profile_die "$requested_profile ports must be non-zero" || return 1
  fi
  if [ "$requested_profile" = test ] && [ -z "${WORKASS_TEST_ROOT:-}" ]; then
    workass_profile_die "WORKASS_TEST_ROOT is required for the test profile" || return 1
  fi
  workass_profile_require_absolute WORKASS_DATA_ROOT "$WORKASS_DATA_ROOT" || return 1
  workass_profile_require_absolute WORKASS_LOG_ROOT "$WORKASS_LOG_ROOT" || return 1

  # macOS privacy grants belong to a code-signing identity, so every profile on
  # this machine signs with the single identity bootstrapped under the
  # production data root. A profile that minted its own signer would make macOS
  # see a different application and ask for authorization again.
  WORKASS_SIGNING_ROOT="$WORKASS_DATA_ROOT/signing"
  if [ "$requested_profile" = prod ]; then
    WORKASS_SHARED_SIGNING_ROOT="$WORKASS_SIGNING_ROOT"
  else
    shared_profile_file="$WORKASS_REPO_ROOT/config/environments/prod.env"
    shared_profile_local="$WORKASS_REPO_ROOT/config/environments/local/prod.local.env"
    workass_profile_validate_file "$shared_profile_file" || return 1
    if [ -f "$shared_profile_local" ]; then
      workass_profile_validate_file "$shared_profile_local" || return 1
    fi
    WORKASS_SHARED_SIGNING_ROOT=$(
      # shellcheck disable=SC1090
      . "$shared_profile_file"
      if [ -f "$shared_profile_local" ]; then
        # shellcheck disable=SC1090
        . "$shared_profile_local"
      fi
      printf '%s/signing' "$WORKASS_DATA_ROOT"
    )
  fi

  WORKASS_STATE_DIR="$WORKASS_DATA_ROOT/state"
  WORKASS_USER_DATA_DIR="$WORKASS_DATA_ROOT/electron"
  WORKASS_RUN_DIR="$WORKASS_DATA_ROOT/run"
  WORKASS_BROWSER_CONTROL_FILE="$WORKASS_RUN_DIR/browser-control.json"
  WORKASS_DAEMON_URL="https://127.0.0.1:$WORKASS_DAEMON_PORT"
  WORKASS_PROFILE_FILE="$canonical_file"

  export WORKASS_PROFILE WORKASS_APP_NAME WORKASS_BUNDLE_ID
  export WORKASS_DAEMON_PORT WORKASS_DAEMON_BIND WORKASS_VIEW_PORT WORKASS_LAUNCHD_LABEL
  export WORKASS_DATA_ROOT WORKASS_LOG_ROOT WORKASS_STATE_DIR WORKASS_USER_DATA_DIR
  export WORKASS_RUN_DIR WORKASS_BROWSER_CONTROL_FILE
  export WORKASS_DAEMON_URL WORKASS_PROFILE_FILE
  export WORKASS_SIGNING_ROOT WORKASS_SHARED_SIGNING_ROOT
}
