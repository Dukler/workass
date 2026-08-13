#!/bin/sh

# Shared macOS release-signing helpers. Production app and daemon updates must
# keep one cryptographic identity so macOS privacy grants survive new bytes.
# This file is sourced after workass_load_profile has set WORKASS_DATA_ROOT and
# WORKASS_BUNDLE_ID.

workass_codesign_die() {
  echo "workass-codesign: $*" >&2
  return 1
}

workass_codesign_run() {
  if [ -n "${WORKASS_RESOLVED_CODESIGN_KEYCHAIN:-}" ]; then
    codesign --keychain "$WORKASS_RESOLVED_CODESIGN_KEYCHAIN" "$@"
  else
    codesign "$@"
  fi
}

workass_codesign_prepare() {
  signing_mode="${1:-stable}"
  case "$signing_mode" in stable|distribution) ;; *) workass_codesign_die "unknown signing mode: $signing_mode" || return 1 ;; esac
  [ "$(uname -s)" = Darwin ] || workass_codesign_die "stable release signing requires macOS" || return 1
  [ -n "${WORKASS_DATA_ROOT:-}" ] || workass_codesign_die "WORKASS_DATA_ROOT is required" || return 1

  signing_root="${WORKASS_CODESIGN_ROOT:-${WORKASS_SIGNING_ROOT:-$WORKASS_DATA_ROOT/signing}}"
  identity="${WORKASS_CODESIGN_IDENTITY:-}"
  keychain="${WORKASS_CODESIGN_KEYCHAIN:-}"
  password_file=''

  # One identity serves every profile. When the active profile has no signer of
  # its own, fall back to the shared one instead of building unsigned code that
  # macOS would treat as a new application after every rebuild.
  if [ -z "$identity" ] && [ ! -f "$signing_root/identity.sha1" ] && \
     [ -n "${WORKASS_SHARED_SIGNING_ROOT:-}" ] && \
     [ -f "$WORKASS_SHARED_SIGNING_ROOT/identity.sha1" ]; then
    signing_root="$WORKASS_SHARED_SIGNING_ROOT"
  fi

  if [ -z "$identity" ]; then
    identity_file="$signing_root/identity.sha1"
    [ -f "$identity_file" ] || {
      if [ "$signing_mode" = distribution ]; then
        workass_codesign_die "public release identity is missing; install Developer ID Application and set WORKASS_CODESIGN_IDENTITY"
      else
        workass_codesign_die "no stable identity; install Developer ID and set WORKASS_CODESIGN_IDENTITY, or run scripts/macos/bootstrap-workass-local-signing.sh"
      fi
      return 1
    }
    identity=$(sed -n '1p' "$identity_file")
    keychain="${keychain:-$signing_root/Workass.keychain-db}"
    password_file="$signing_root/keychain.password"
  fi

  case "$identity" in
    *[!0-9A-Fa-f]*|'')
      # Explicit certificate names are supported for Developer ID identities.
      ;;
    *)
      [ "${#identity}" -eq 40 ] || workass_codesign_die "identity fingerprint must be 40 hexadecimal characters" || return 1
      ;;
  esac

  if [ -n "$keychain" ]; then
    [ -f "$keychain" ] || workass_codesign_die "signing keychain not found: $keychain" || return 1
    if [ -n "$password_file" ]; then
      [ -f "$password_file" ] || workass_codesign_die "signing keychain password file is missing" || return 1
      password_mode=$(stat -f '%Lp' "$password_file")
      case "$password_mode" in 400|600) ;; *)
        workass_codesign_die "signing keychain password file must have mode 400 or 600"
        return 1
      esac
      keychain_password=$(sed -n '1p' "$password_file")
      [ -n "$keychain_password" ] || workass_codesign_die "signing keychain password is empty" || return 1
      security unlock-keychain -p "$keychain_password" "$keychain" >/dev/null || {
        workass_codesign_die "could not unlock the Workass signing keychain"
        return 1
      }
      keychain_password=''
      unset keychain_password
    fi
  fi

  WORKASS_RESOLVED_CODESIGN_IDENTITY="$identity"
  WORKASS_RESOLVED_CODESIGN_KEYCHAIN="$keychain"
  export WORKASS_RESOLVED_CODESIGN_IDENTITY WORKASS_RESOLVED_CODESIGN_KEYCHAIN

  if [ -n "$keychain" ]; then
    identity_listing=$(security find-identity -v -p codesigning "$keychain")
  else
    identity_listing=$(security find-identity -v -p codesigning)
  fi
  identity_line=$(printf '%s\n' "$identity_listing" | awk -v needle="$identity" 'index($0, needle) { print; exit }')
  [ -n "$identity_line" ] || {
    workass_codesign_die "configured signing identity is not valid for code signing"
    return 1
  }
  if [ "$signing_mode" = distribution ]; then
    case "$identity_line" in
      *'Developer ID Application:'*) ;;
      *) workass_codesign_die "public releases require a Developer ID Application identity"; return 1 ;;
    esac
  fi
  WORKASS_RESOLVED_CODESIGN_MODE="$signing_mode"
  export WORKASS_RESOLVED_CODESIGN_MODE
}

workass_codesign_requirement() {
  workass_codesign_requirement_target="$1"
  codesign -d -r- "$workass_codesign_requirement_target" 2>&1 |
    sed -nE 's/^[[:space:]#]*designated =>[[:space:]]*//p' |
    head -n 1
}

workass_codesign_cdhash() {
  # The code directory hash covers the code pages, identifier and entitlements
  # but not the CMS blob, so two signings of identical code under the same
  # identifier compare equal. This is how an update decides it is a no-op.
  workass_codesign_cdhash_target="$1"
  codesign -d --verbose=4 "$workass_codesign_cdhash_target" 2>&1 |
    sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p' |
    head -n 1
}

workass_codesign_verify_stable() {
  workass_codesign_verify_target="$1"
  workass_codesign_verify_deep="${2:-false}"
  if [ "$workass_codesign_verify_deep" = true ]; then
    codesign --verify --deep --strict "$workass_codesign_verify_target" >/dev/null 2>&1 || {
      workass_codesign_die "signature verification failed: $workass_codesign_verify_target"
      return 1
    }
  else
    codesign --verify --strict "$workass_codesign_verify_target" >/dev/null 2>&1 || {
      workass_codesign_die "signature verification failed: $workass_codesign_verify_target"
      return 1
    }
  fi

  workass_codesign_verify_signature_details=$(codesign -d --verbose=4 "$workass_codesign_verify_target" 2>&1)
  if printf '%s\n' "$workass_codesign_verify_signature_details" | grep -Eq 'Signature=adhoc|flags=.*adhoc'; then
    workass_codesign_die "ad-hoc signatures are forbidden for production: $workass_codesign_verify_target"
    return 1
  fi
  workass_codesign_verify_requirement=$(workass_codesign_requirement "$workass_codesign_verify_target")
  [ -n "$workass_codesign_verify_requirement" ] || workass_codesign_die "missing designated requirement: $workass_codesign_verify_target" || return 1
  case "$workass_codesign_verify_requirement" in
    *cdhash*)
      workass_codesign_die "CDHash-only identity is version-specific: $workass_codesign_verify_target"
      return 1
      ;;
  esac
}

workass_codesign_is_adhoc_cdhash() {
  workass_codesign_adhoc_target="$1"
  workass_codesign_adhoc_signature_details=$(codesign -d --verbose=4 "$workass_codesign_adhoc_target" 2>&1) || return 1
  workass_codesign_adhoc_requirement=$(workass_codesign_requirement "$workass_codesign_adhoc_target")
  printf '%s\n' "$workass_codesign_adhoc_signature_details" | grep -Eq 'Signature=adhoc|flags=.*adhoc' || return 1
  case "$workass_codesign_adhoc_requirement" in *cdhash*) return 0 ;; *) return 1 ;; esac
}

workass_codesign_sign_app() {
  app="$1"
  [ -d "$app" ] || workass_codesign_die "app bundle not found: $app" || return 1
  [ -n "${WORKASS_RESOLVED_CODESIGN_IDENTITY:-}" ] || workass_codesign_die "call workass_codesign_prepare first" || return 1
  workass_codesign_run --force --deep --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$app"
  workass_codesign_verify_stable "$app" true
  find "$app/Contents/Frameworks" -type d -name '*.app' -print 2>/dev/null |
    while IFS= read -r helper_app; do
      workass_codesign_verify_stable "$helper_app" false || exit 1
    done
}

workass_codesign_sign_binary() {
  binary="$1"
  identifier="$2"
  [ -f "$binary" ] || workass_codesign_die "binary not found: $binary" || return 1
  [ -n "$identifier" ] || workass_codesign_die "binary signing identifier is required" || return 1
  [ -n "${WORKASS_RESOLVED_CODESIGN_IDENTITY:-}" ] || workass_codesign_die "call workass_codesign_prepare first" || return 1
  workass_codesign_run --force --identifier "$identifier" --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$binary"
  workass_codesign_verify_stable "$binary" false
}

workass_codesign_verify_distribution() {
  workass_codesign_distribution_target="$1"
  workass_codesign_distribution_deep="${2:-false}"
  workass_codesign_verify_stable "$workass_codesign_distribution_target" "$workass_codesign_distribution_deep" || return 1
  workass_codesign_distribution_signature_details=$(codesign -d --verbose=4 "$workass_codesign_distribution_target" 2>&1)
  printf '%s\n' "$workass_codesign_distribution_signature_details" | grep -q '^Authority=Developer ID Application:' || {
    workass_codesign_die "not signed with Developer ID Application: $workass_codesign_distribution_target"
    return 1
  }
  printf '%s\n' "$workass_codesign_distribution_signature_details" | grep -Eq '^TeamIdentifier=[A-Z0-9]+' || {
    workass_codesign_die "Developer ID signature has no team identifier: $workass_codesign_distribution_target"
    return 1
  }
  printf '%s\n' "$workass_codesign_distribution_signature_details" | grep -Eq 'flags=.*runtime' || {
    workass_codesign_die "hardened runtime is not enabled: $workass_codesign_distribution_target"
    return 1
  }
}

workass_codesign_sign_app_distribution() {
  app="$1"
  entitlements="$2"
  [ -d "$app" ] || workass_codesign_die "app bundle not found: $app" || return 1
  [ -f "$entitlements" ] || workass_codesign_die "entitlements not found: $entitlements" || return 1
  plutil -lint "$entitlements" >/dev/null || workass_codesign_die "invalid entitlements: $entitlements" || return 1
  [ "${WORKASS_RESOLVED_CODESIGN_MODE:-}" = distribution ] || workass_codesign_die "call workass_codesign_prepare distribution first" || return 1

  # Apple requires nested code to be signed from the inside out. Do not use
  # --deep as a signing shortcut: it can apply the wrong entitlements to
  # helpers and can hide an incomplete release layout.
  find "$app/Contents" -type f \( -perm -111 -o -name '*.dylib' -o -name '*.node' \) -print |
    while IFS= read -r code_item; do
      if file -b "$code_item" | grep -q 'Mach-O'; then
        if [ "$code_item" = "$app/Contents/Resources/runtime/workass" ]; then
          workass_codesign_run --force --timestamp --options runtime \
            --identifier "$WORKASS_BUNDLE_ID.daemon" \
            --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$code_item" || exit 1
        else
          workass_codesign_run --force --timestamp --options runtime \
            --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$code_item" || exit 1
        fi
      fi
    done || return 1

  find "$app/Contents" -depth -type d \( -name '*.framework' -o -name '*.xpc' \) -print |
    while IFS= read -r code_bundle; do
      workass_codesign_run --force --timestamp --options runtime \
        --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$code_bundle" || exit 1
    done || return 1

  find "$app/Contents" -depth -type d -name '*.app' -print |
    while IFS= read -r helper_app; do
      workass_codesign_run --force --timestamp --options runtime --entitlements "$entitlements" \
        --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$helper_app" || exit 1
    done || return 1

  workass_codesign_run --force --timestamp --options runtime --entitlements "$entitlements" \
    --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$app"
  workass_codesign_verify_distribution "$app" true
}

workass_codesign_sign_container_distribution() {
  container="$1"
  [ -f "$container" ] || workass_codesign_die "release container not found: $container" || return 1
  [ "${WORKASS_RESOLVED_CODESIGN_MODE:-}" = distribution ] || workass_codesign_die "call workass_codesign_prepare distribution first" || return 1
  workass_codesign_run --force --timestamp --sign "$WORKASS_RESOLVED_CODESIGN_IDENTITY" "$container"
  workass_codesign_verify_stable "$container" false
  signature_details=$(codesign -d --verbose=4 "$container" 2>&1)
  printf '%s\n' "$signature_details" | grep -q '^Authority=Developer ID Application:' || {
    workass_codesign_die "container is not signed with Developer ID Application: $container"
    return 1
  }
}

workass_codesign_mutually_compatible() {
  old_target="$1"
  new_target="$2"
  old_requirement=$(workass_codesign_requirement "$old_target")
  new_requirement=$(workass_codesign_requirement "$new_target")
  [ -n "$old_requirement" ] && [ -n "$new_requirement" ] || return 1
  codesign --verify --strict -R="$old_requirement" "$new_target" >/dev/null 2>&1 || return 1
  codesign --verify --strict -R="$new_requirement" "$old_target" >/dev/null 2>&1 || return 1
}
