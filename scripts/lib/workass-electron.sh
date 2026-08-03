#!/bin/sh

# Resolve the exact Electron runtime audited for Workass macOS builds. This is
# sourced by launch/package scripts after WORKASS_REPO_ROOT has been set.

workass_electron_die() {
  echo "workass-electron: $*" >&2
  return 1
}

workass_electron_pinned_version() {
  workass_electron_version_file="$WORKASS_REPO_ROOT/config/macos/electron.version"
  [ -f "$workass_electron_version_file" ] || {
    workass_electron_die "version pin is missing: $workass_electron_version_file"
    return 1
  }
  workass_electron_version=$(sed -n '1p' "$workass_electron_version_file")
  printf '%s' "$workass_electron_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
    workass_electron_die "invalid version pin: $workass_electron_version"
    return 1
  }
  printf '%s\n' "$workass_electron_version"
}

workass_electron_app_version() {
  workass_electron_app="$1"
  [ -d "$workass_electron_app" ] || {
    workass_electron_die "Electron.app not found: $workass_electron_app"
    return 1
  }
  workass_electron_plist="$workass_electron_app/Contents/Info.plist"
  [ -f "$workass_electron_plist" ] || {
    workass_electron_die "Electron Info.plist is missing: $workass_electron_plist"
    return 1
  }
  plutil -extract CFBundleShortVersionString raw -o - "$workass_electron_plist" 2>/dev/null || {
    workass_electron_die "Electron version is unreadable: $workass_electron_plist"
    return 1
  }
}

workass_electron_resolve() {
  workass_electron_requested="${1:-${WORKASS_ELECTRON_APP:-}}"
  workass_electron_pin=$(workass_electron_pinned_version) || return 1
  if [ -n "$workass_electron_requested" ]; then
    workass_electron_resolved="$workass_electron_requested"
  else
    workass_electron_resolved="$WORKASS_REPO_ROOT/.dev/runtime/electron/darwin-arm64/Electron.app"
  fi
  workass_electron_actual=$(workass_electron_app_version "$workass_electron_resolved") || return 1
  [ "$workass_electron_actual" = "$workass_electron_pin" ] || {
    workass_electron_die "expected Electron $workass_electron_pin, found $workass_electron_actual at $workass_electron_resolved"
    return 1
  }
  printf '%s\n' "$workass_electron_resolved"
}
