#!/bin/zsh
set -eu
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; F="$HOME/Library/Application Support/Workass/update-feed"
. $RR/scripts/lib/workass-codesign.sh
export WORKASS_DATA_ROOT="$HOME/Library/Application Support/Workass" WORKASS_BUNDLE_ID=com.workass.app
workass_codesign_prepare
rm -rf $U/in255; mkdir -p $U/in255; ditto -x -k "$F/Workass-0.1.255-darwin-arm64.zip" $U/in255
cp $U/rig-profile.env $U/in255/Workass.app/Contents/Resources/workass-profile.env
workass_codesign_sign_app $U/in255/Workass.app >/dev/null
Z=$U/a/update-feed/Workass-0.1.255-darwin-arm64.zip; ditto -c -k --sequesterRsrc --keepParent $U/in255/Workass.app $Z; rm -f $U/a/update-feed/Workass-0.1.254-darwin-arm64.zip
python3 -I - "$F/workass-darwin-arm64-release.json" "$U/a/update-feed/workass-darwin-arm64-release.json" "$Z" <<'P'
import json,sys,hashlib,os
s,d,z=sys.argv[1:]; r=json.load(open(s))
r['artifacts']['update']['sha256']=hashlib.sha256(open(z,'rb').read()).hexdigest(); r['artifacts']['update']['size']=os.path.getsize(z)
json.dump(r,open(d,'w'),indent=2); print(r['version'],r['build'])
P
