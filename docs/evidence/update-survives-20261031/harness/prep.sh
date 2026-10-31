#!/bin/zsh
# Prepare installed 0.1.253 and feed 0.1.254 with a rig-only profile sealed into each bundle.
set -eu
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; OLD=$RR/.dev/update-rig-20261031
APP=$HOME/Applications/WorkassUpdateRig.app
rm -rf "$HOME/Applications/.Workass.app.incoming-upd-mvww0j1d-af2c0071469a2a5a"
mkdir -p $U/in $U/a/update-feed $U/a-logs $U/shots
cat > $U/rig-profile.env <<P
WORKASS_PROFILE=prod
WORKASS_APP_NAME=Workass
WORKASS_BUNDLE_ID=com.workass.app
WORKASS_DAEMON_PORT=28990
WORKASS_DAEMON_BIND=lan
WORKASS_VIEW_PORT=28991
WORKASS_LAUNCHD_LABEL=com.workass.daemon.urig
WORKASS_DATA_ROOT="$U/a"
WORKASS_LOG_ROOT="$U/a-logs"
WORKASS_UPDATE_CHANNEL=local
P
. $RR/scripts/lib/workass-codesign.sh
export WORKASS_DATA_ROOT="$HOME/Library/Application Support/Workass" WORKASS_BUNDLE_ID=com.workass.app
workass_codesign_prepare
[ -d $U/in/Workass.app ] || ditto -x -k $OLD/mac-data/update-feed/Workass-0.1.254-darwin-arm64.zip $U/in
for app in "$APP" $U/in/Workass.app; do
  cp $U/rig-profile.env "$app/Contents/Resources/workass-profile.env"
  workass_codesign_sign_app "$app" >/dev/null
  codesign -v "$app"; plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist"
done
Z=$U/a/update-feed/Workass-0.1.254-darwin-arm64.zip
ditto -c -k --sequesterRsrc --keepParent $U/in/Workass.app $Z
python3 -I - "$OLD/mac-data/update-feed/workass-darwin-arm64-release.json" "$U/a/update-feed/workass-darwin-arm64-release.json" "$Z" <<'P'
import json,sys,hashlib,os
s,d,z=sys.argv[1:]; r=json.load(open(s))
r['artifacts']['update']['sha256']=hashlib.sha256(open(z,'rb').read()).hexdigest(); r['artifacts']['update']['size']=os.path.getsize(z)
json.dump(r,open(d,'w'),indent=2); os.chmod(d,0o600); print(r['version'],r['build'],r['artifacts']['update']['sha256'][:12])
P
codesign -d -r- "$APP" 2>&1 | grep designated
