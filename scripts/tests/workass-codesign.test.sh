#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

[ "$(uname -s)" = Darwin ] || {
  echo "WORKASS_CODESIGN_TEST_SKIP non-macOS"
  exit 0
}

test_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-codesign-test.XXXXXX")
cleanup() {
  rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

cp /usr/bin/true "$test_root/old"
cp /usr/bin/false "$test_root/new"
cp /usr/bin/false "$test_root/other"
cp /usr/bin/true "$test_root/legacy"
codesign --remove-signature "$test_root/old"
codesign --remove-signature "$test_root/new"
codesign --remove-signature "$test_root/other"
codesign --remove-signature "$test_root/legacy"
codesign --force --sign - --identifier com.workass.test \
  --requirements '=designated => identifier "com.workass.test"' \
  "$test_root/old"
codesign --force --sign - --identifier com.workass.test \
  --requirements '=designated => identifier "com.workass.test"' \
  "$test_root/new"
codesign --force --sign - --identifier com.workass.other \
  --requirements '=designated => identifier "com.workass.other"' \
  "$test_root/other"
codesign --force --sign - --identifier com.workass.legacy "$test_root/legacy"

workass_codesign_mutually_compatible "$test_root/old" "$test_root/new"
if workass_codesign_mutually_compatible "$test_root/old" "$test_root/other"; then
  echo "different designated requirements were accepted" >&2
  exit 1
fi

# The release runner sources this helper and owns a variable named `target`
# for the stable daemon install path. POSIX sh functions share the caller's
# variable scope, so signing/verification helpers must not silently replace it
# with the timestamped candidate path.
target="$test_root/stable-runtime/workass"
expected_target="$target"
workass_codesign_requirement "$test_root/new" >/dev/null
[ "$target" = "$expected_target" ] || {
  echo "codesign helper clobbered the caller's stable daemon target: $target" >&2
  exit 1
}

if workass_codesign_verify_stable "$test_root/old" false >/dev/null 2>&1; then
  echo "ad-hoc identity was accepted as stable" >&2
  exit 1
fi
workass_codesign_is_legacy_adhoc "$test_root/legacy"
if workass_codesign_is_legacy_adhoc "$test_root/old"; then
  echo "an arbitrary incompatible identity was accepted as the legacy migration source" >&2
  exit 1
fi

# Identity-classification is tested without changing the user's Keychain.
WORKASS_DATA_ROOT="$test_root/data"
WORKASS_CODESIGN_IDENTITY=0123456789ABCDEF0123456789ABCDEF01234567
WORKASS_CODESIGN_KEYCHAIN=''
fake_identity_name='Developer ID Application: Workass Test (ABCDEFGHIJ)'
security() {
  if [ "$1" = find-identity ]; then
    printf '  1) %s "%s"\n     1 valid identities found\n' "$WORKASS_CODESIGN_IDENTITY" "$fake_identity_name"
    return 0
  fi
  return 1
}
workass_codesign_prepare distribution
[ "$WORKASS_RESOLVED_CODESIGN_MODE" = distribution ]
fake_identity_name='Workass Local Release'
if workass_codesign_prepare distribution >/dev/null 2>&1; then
  echo "public release accepted a private local identity" >&2
  exit 1
fi

echo "WORKASS_CODESIGN_TEST_PASS compatible_updates=true incompatible_rejected=true caller_target_preserved=true adhoc_rejected=true legacy_migration_scoped=true developer_id_required=true"
