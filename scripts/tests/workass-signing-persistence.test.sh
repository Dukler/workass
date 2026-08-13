#!/bin/sh
set -eu

# macOS binds every privacy grant to a code-signing designated requirement. A
# locally built binary that carries only the linker's ad-hoc signature is a new
# application after every rebuild, so macOS asks for authorization again. These
# assertions keep every build path on the one persistent identity.

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
builder="$repo_root/scripts/build-daemon.sh"
runner="$repo_root/scripts/rebuild-workass-macos.sh"
packager="$repo_root/scripts/package-workass-macos.sh"
installer="$repo_root/scripts/install-workass-macos.sh"
profile_lib="$repo_root/scripts/lib/workass-profile.sh"

sh -n "$builder"
sh -n "$profile_lib"

# Development artifacts are signed, not left ad-hoc.
grep -Fq 'workass_codesign_sign_binary "$out" "$identifier"' "$builder"
grep -Fq 'workass_codesign_prepare' "$builder"
grep -Fq 'darwin artifacts stay ad-hoc' "$builder"

# Signing is not restricted to the production profile any more.
grep -Fq 'if [ "$sign_candidate" -eq 1 ]; then' "$runner"
if grep -Fq 'if [ "$profile" = prod ]; then workass_codesign_prepare; fi' "$runner"; then
  echo "daemon rebuild still signs only the production profile" >&2
  exit 1
fi

# An update that installs identical code must not restart the daemon.
grep -Fq 'DAEMON_ALREADY_CURRENT' "$runner"
grep -Fq 'workass_codesign_cdhash' "$runner"

# Replaying an already-installed complete app release must not restart either
# the shell or daemon, even when an external launch wrapper retries it.
grep -Fq 'WORKASS_MACOS_INSTALL_ALREADY_CURRENT' "$installer"
grep -Fq 'candidate_cdhash=$(workass_codesign_cdhash "$candidate")' "$installer"
grep -Fq '[ "$candidate_cdhash" = "$installed_cdhash" ] && new_release_healthy' "$installer"

# Finished handoff jobs are swept so each rebuild is not one more launchd item.
grep -Fq 'com\.workass\.rebuild\.' "$runner"

# The complete app/runtime release is staged and swapped, never overwritten in
# place: the kernel kills a running process whose mapped image stops matching
# its signature.
grep -Fq 'incoming="$install_root/.Workass.app.incoming-' "$installer"
grep -Fq 'backup="$install_root/.Workass.app.previous-' "$installer"
grep -Fq 'mv "$incoming" "$installed"' "$installer"
if grep -Fq -- '--repair-broken-installed-seal' "$installer"; then
  echo "installer still exposes broken-seal mutation" >&2
  exit 1
fi
grep -Fq 'workass_codesign_mutually_compatible "$installed" "$candidate"' "$installer"
if grep -Fq 'cp "$repo_root/dist-bin/workass-darwin-arm64" "$WORKASS_DATA_ROOT/runtime/workass"' "$packager"; then
  echo "packager still copies onto the live daemon binary" >&2
  exit 1
fi

[ "$(uname -s)" = Darwin ] || {
  echo "WORKASS_SIGNING_PERSISTENCE_TEST_SKIP non-macOS"
  exit 0
}

WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$profile_lib"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

# Every profile resolves to one shared signer, so a development rebuild cannot
# mint a second identity and reset the grants of the first.
workass_load_profile prod
prod_signing_root="$WORKASS_SIGNING_ROOT"
workass_load_profile dev
[ "$WORKASS_SHARED_SIGNING_ROOT" = "$prod_signing_root" ] || {
  echo "dev profile does not share the production signing identity" >&2
  exit 1
}
[ "$WORKASS_SIGNING_ROOT" != "$prod_signing_root" ] || {
  echo "dev profile signing root collided with production" >&2
  exit 1
}

test_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-signing-test.XXXXXX")
cleanup() {
  rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

# The code directory hash is what an update compares to decide it is a no-op.
cp /usr/bin/true "$test_root/same"
cp /usr/bin/true "$test_root/same-again"
cp /usr/bin/false "$test_root/different"
for binary in same same-again different; do
  codesign --force --sign - --identifier com.workass.cdhash-test "$test_root/$binary" 2>/dev/null
done
same_hash=$(workass_codesign_cdhash "$test_root/same")
same_again_hash=$(workass_codesign_cdhash "$test_root/same-again")
different_hash=$(workass_codesign_cdhash "$test_root/different")
[ -n "$same_hash" ] || { echo "cdhash helper returned nothing" >&2; exit 1; }
[ "$same_hash" = "$same_again_hash" ] || {
  echo "identical code under one identifier produced different cdhashes" >&2
  exit 1
}
[ "$same_hash" != "$different_hash" ] || {
  echo "different code produced the same cdhash" >&2
  exit 1
}

# The shared-identity fallback is exercised without touching the real Keychain.
WORKASS_DATA_ROOT="$test_root/profile"
WORKASS_SIGNING_ROOT="$WORKASS_DATA_ROOT/signing"
WORKASS_SHARED_SIGNING_ROOT="$test_root/shared/signing"
WORKASS_CODESIGN_IDENTITY=''
WORKASS_CODESIGN_KEYCHAIN=''
WORKASS_CODESIGN_ROOT=''
unset WORKASS_CODESIGN_ROOT
mkdir -p "$WORKASS_SHARED_SIGNING_ROOT"
fake_identity=0123456789ABCDEF0123456789ABCDEF01234567
printf '%s\n' "$fake_identity" > "$WORKASS_SHARED_SIGNING_ROOT/identity.sha1"
: > "$WORKASS_SHARED_SIGNING_ROOT/Workass.keychain-db"
printf 'test-password\n' > "$WORKASS_SHARED_SIGNING_ROOT/keychain.password"
chmod 600 "$WORKASS_SHARED_SIGNING_ROOT/keychain.password"
security() {
  case "$1" in
    find-identity)
      printf '  1) %s "Workass Local Release"\n     1 valid identities found\n' "$fake_identity"
      return 0
      ;;
    unlock-keychain) return 0 ;;
    *) return 1 ;;
  esac
}
workass_codesign_prepare
[ "$WORKASS_RESOLVED_CODESIGN_IDENTITY" = "$fake_identity" ] || {
  echo "prepare did not resolve the shared identity" >&2
  exit 1
}
[ "$WORKASS_RESOLVED_CODESIGN_KEYCHAIN" = "$WORKASS_SHARED_SIGNING_ROOT/Workass.keychain-db" ] || {
  echo "prepare did not fall back to the shared signing keychain" >&2
  exit 1
}

echo "WORKASS_SIGNING_PERSISTENCE_TEST_PASS shared_identity=true dev_signed=true noop_update=true atomic_swap=true"
