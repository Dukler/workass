#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-codesign.sh"

usage() {
  cat <<'EOF'
usage: scripts/macos/bootstrap-workass-local-signing.sh [--profile prod]

Creates one persistent, locally trusted Workass code-signing identity. This is
for private development installs on this Mac. Public releases should use an
Apple Developer ID Application identity and notarization instead.
EOF
}

profile=prod
while [ "$#" -gt 0 ]; do
  case "$1" in
    --profile) [ "$#" -ge 2 ] || { echo "--profile needs a value" >&2; exit 2; }; profile="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$profile" = prod ] || { echo "local release identity is supported only for the prod profile" >&2; exit 2; }
[ "$(uname -s)" = Darwin ] || { echo "local signing bootstrap requires macOS" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }
workass_load_profile "$profile"

signing_root="$WORKASS_DATA_ROOT/signing"
keychain="$signing_root/Workass.keychain-db"
password_file="$signing_root/keychain.password"
identity_file="$signing_root/identity.sha1"

ensure_keychain_searchable() {
  search_keychains=$(security list-keychains -d user |
    sed -E 's/^[[:space:]]*"(.*)"[[:space:]]*$/\1/')
  if printf '%s\n' "$search_keychains" | grep -Fxq "$keychain"; then
    return 0
  fi
  set --
  while IFS= read -r search_keychain; do
    [ -n "$search_keychain" ] && set -- "$@" "$search_keychain"
  done <<EOF
$search_keychains
EOF
  security list-keychains -d user -s "$@" "$keychain"
}

verify_local_signer() {
  command -v xcrun >/dev/null 2>&1 || {
    echo "xcrun and the macOS Command Line Tools are required" >&2
    return 1
  }
  verify_root=$(mktemp -d "$signing_root/.verify.XXXXXX")
  if ! (
    set -eu
    printf 'int main(void){return 0;}\n' | xcrun clang -x c - -o "$verify_root/canary"
    workass_codesign_sign_binary "$verify_root/canary" "$WORKASS_BUNDLE_ID.signing-canary"
    codesign --verify --strict "$verify_root/canary"
    "$verify_root/canary"
  ); then
    rm -rf "$verify_root"
    echo "the local Workass identity could not sign and execute a disposable canary" >&2
    return 1
  fi
  rm -rf "$verify_root"
  echo "WORKASS_LOCAL_SIGNING_CANARY_PASS"
}

if [ -f "$identity_file" ]; then
  ensure_keychain_searchable
  workass_codesign_prepare
  verify_local_signer
  echo "WORKASS_LOCAL_SIGNING_READY"
  echo "identity=$WORKASS_RESOLVED_CODESIGN_IDENTITY"
  echo "keychain=$WORKASS_RESOLVED_CODESIGN_KEYCHAIN"
  exit 0
fi

[ ! -e "$keychain" ] || {
  echo "partial signing setup exists without identity metadata: $keychain" >&2
  echo "inspect or remove the partial setup before retrying" >&2
  exit 1
}

umask 077
mkdir -p "$signing_root"
bootstrap_root=$(mktemp -d "$signing_root/.bootstrap.XXXXXX")
cleanup() {
  rm -rf "$bootstrap_root"
}
trap cleanup EXIT HUP INT TERM

keychain_password=$(openssl rand -hex 32)
import_password=$(openssl rand -hex 32)
printf '%s\n' "$keychain_password" > "$password_file"
chmod 600 "$password_file"

openssl req -new -newkey rsa:3072 -nodes -x509 -sha256 -days 3650 \
  -subj '/CN=Workass Local Release/O=Workass Local Development' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,digitalSignature,keyCertSign' \
  -addext 'extendedKeyUsage=codeSigning' \
  -keyout "$bootstrap_root/identity.key" \
  -out "$bootstrap_root/identity.crt" >/dev/null 2>&1
openssl pkcs12 -export -legacy \
  -inkey "$bootstrap_root/identity.key" \
  -in "$bootstrap_root/identity.crt" \
  -name 'Workass Local Release' \
  -passout "pass:$import_password" \
  -out "$bootstrap_root/identity.p12"

security create-keychain -p "$keychain_password" "$keychain"
security unlock-keychain -p "$keychain_password" "$keychain"
security import "$bootstrap_root/identity.p12" \
  -k "$keychain" \
  -P "$import_password" \
  -T /usr/bin/codesign >/dev/null
security set-key-partition-list \
  -S apple-tool:,apple: \
  -s \
  -k "$keychain_password" \
  "$keychain" >/dev/null

echo "macOS will request one authorization to trust the private Workass development signer."
echo "This is the one-time local-development substitute for Apple Developer ID signing."
security add-trusted-cert \
  -r trustRoot \
  -p codeSign \
  -k "$keychain" \
  "$bootstrap_root/identity.crt"
ensure_keychain_searchable

identity=$(security find-identity -v -p codesigning "$keychain" |
  awk '/"Workass Local Release"/ { print $2; exit }')
[ -n "$identity" ] || {
  echo "the Workass certificate was imported but is not a valid code-signing identity" >&2
  exit 1
}
printf '%s\n' "$identity" > "$identity_file"
chmod 600 "$identity_file"

keychain_password=''
import_password=''
unset keychain_password import_password
WORKASS_CODESIGN_ROOT="$signing_root"
export WORKASS_CODESIGN_ROOT
workass_codesign_prepare
verify_local_signer

echo "WORKASS_LOCAL_SIGNING_READY"
echo "identity=$WORKASS_RESOLVED_CODESIGN_IDENTITY"
echo "keychain=$WORKASS_RESOLVED_CODESIGN_KEYCHAIN"
echo "Public distribution still requires Developer ID signing and notarization."
