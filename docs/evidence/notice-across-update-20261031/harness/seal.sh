#!/bin/zsh
# seal.sh OUT.app SRC.zip VERSION [DAEMON_BIN] : rig copy of a published bundle, rig profile sealed in, re-signed.
set -eu
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; OUT=$1; SRC=$2; V=$3; BIN=${4:-}
. $RR/scripts/lib/workass-codesign.sh
export WORKASS_DATA_ROOT="$HOME/Library/Application Support/Workass" WORKASS_BUNDLE_ID=com.workass.app
workass_codesign_prepare
T=$(mktemp -d $U/tmp.XXXX); ditto -x -k "$SRC" $T; rm -rf "$OUT"; mkdir -p "$(dirname $OUT)"; mv $T/Workass.app "$OUT"; rmdir $T
cp $U/rig-profile.env "$OUT/Contents/Resources/workass-profile.env"
if [ -n "$BIN" ]; then cp "$BIN" "$OUT/Contents/Resources/runtime/workass"; fi
plutil -replace CFBundleShortVersionString -string "$V" "$OUT/Contents/Info.plist"
B=$(plutil -extract CFBundleVersion raw "$OUT/Contents/Info.plist")
python3 -I - "$OUT/Contents/Resources/runtime/manifest.json" "$V" <<'P'
import json,sys
m=json.load(open(sys.argv[1])); m['version']=sys.argv[2]; json.dump(m,open(sys.argv[1],'w'),separators=(',',':'))
P
python3 -I - "$OUT/Contents/Resources/app/package.json" "$V" <<'P'
import json,sys
m=json.load(open(sys.argv[1])); m['version']=sys.argv[2]; json.dump(m,open(sys.argv[1],'w'),indent=2)
P
workass_codesign_sign_app "$OUT" >/dev/null; codesign -v "$OUT"; echo "sealed $OUT $V build $B"
