#!/bin/sh
set -eu

# Reports the code identity of every Workass binary on this Mac. macOS privacy
# grants belong to a designated requirement, so a binary whose requirement is
# only a CDHash loses every grant on its next rebuild. Run this whenever macOS
# asks for an authorization it already had.
#
# A stable requirement is necessary but NOT sufficient: copying a freshly built
# renderer straight into Contents/Resources of the installed app leaves the
# requirement untouched while breaking the bundle's seal, and macOS distrusts a
# bundle whose seal does not verify. This reported "stable" against exactly that
# state on 2026-07-25 while seven added asset files and a rewritten index.html
# had invalidated /Applications/Workass.app, so every target is now sealed-
# verified as well as identity-checked. The only supported way to update the
# installed app is scripts/package-workass-macos.sh, never a copy into it.

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

[ "$(uname -s)" = Darwin ] || { echo "workass-signing-status: macOS only" >&2; exit 1; }

workass_load_profile prod
prod_root="$WORKASS_DATA_ROOT"
installed_app="${WORKASS_INSTALLED_APP:-/Applications/Workass.app}"

unstable=0

report() {
  label="$1"
  path="$2"
  if [ ! -e "$path" ]; then
    printf '%-22s absent    %s\n' "$label" "$path"
    return 0
  fi
  requirement=$(workass_codesign_requirement "$path")
  case "$requirement" in
    '')
      printf '%-22s UNSIGNED  %s\n' "$label" "$path"
      unstable=$((unstable + 1))
      ;;
    *cdhash*)
      printf '%-22s AD-HOC    %s\n' "$label" "$path"
      printf '%-22s           grants reset on every rebuild: %s\n' '' "$requirement"
      unstable=$((unstable + 1))
      ;;
    *)
      if codesign --verify --strict "$path" >/dev/null 2>&1; then
        printf '%-22s stable    %s\n' "$label" "$path"
        printf '%-22s           %s\n' '' "$requirement"
      else
        # The identity is right but the seal is not: something wrote into the
        # bundle after it was signed. Name the offending files, because the
        # remedy depends on what was touched.
        printf '%-22s TAMPERED  %s\n' "$label" "$path"
        printf '%-22s           identity is stable but the seal does not verify\n' ''
        codesign --verify --strict --verbose=4 "$path" 2>&1 \
          | grep -E '^file (added|modified|missing):' \
          | sed 's|^|                                  |' \
          | head -12
        printf '%-22s           repair: scripts/package-workass-macos.sh\n' ''
        unstable=$((unstable + 1))
      fi
      ;;
  esac
}

echo "Workass code identities (stable = macOS privacy grants survive rebuilds)"
echo
report "installed app" "$installed_app"
report "prod daemon" "$prod_root/runtime/workass"
report "dev daemon" "$repo_root/dist-bin/workass-darwin-arm64"
report "dev agent host" "$repo_root/dist-bin/workass-agent-darwin-arm64"
echo

if [ -f "$prod_root/signing/identity.sha1" ]; then
  echo "signing identity: $(sed -n '1p' "$prod_root/signing/identity.sha1") ($prod_root/signing)"
else
  echo "signing identity: NONE — run scripts/macos/bootstrap-workass-local-signing.sh"
  unstable=$((unstable + 1))
fi

if [ "$unstable" -ne 0 ]; then
  echo
  echo "WORKASS_SIGNING_STATUS_UNSTABLE count=$unstable"
  exit 1
fi
echo
echo "WORKASS_SIGNING_STATUS_STABLE"
