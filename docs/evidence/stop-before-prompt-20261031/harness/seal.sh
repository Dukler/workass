#!/bin/zsh
# seal.sh OUT.app SRC.zip [DAEMON_BIN] [HOSTS_DIR]: rig copy of a published bundle, rig profile sealed in, re-signed.
set -eu
M=/Users/dev/Workspace/workass; R=$M/.dev/stop-race/rig; OUT=$1; SRC=$2; BIN=${3:-}; H=${4:-}
. $M/scripts/lib/workass-codesign.sh
export WORKASS_DATA_ROOT="$HOME/Library/Application Support/Workass" WORKASS_BUNDLE_ID=com.workass.app
workass_codesign_prepare
T=$(mktemp -d $R/tmp.XXXX); ditto -x -k "$SRC" $T; rm -rf "$OUT"; mv $T/Workass.app "$OUT"; rmdir $T
cp $R/rig-profile.env "$OUT/Contents/Resources/workass-profile.env"
if [ -n "$BIN" ]; then cp "$BIN" "$OUT/Contents/Resources/runtime/workass"; fi
if [ -n "$H" ]; then for f in claude pi omp; do cp "$H/$f-native-host.mjs" "$OUT/Contents/Resources/runtime/frontier-hosts/darwin-arm64/$f-native-host.mjs"; done; fi
workass_codesign_sign_app "$OUT" >/dev/null; codesign -v "$OUT"; echo "sealed $OUT $(plutil -extract CFBundleShortVersionString raw "$OUT/Contents/Info.plist")"
