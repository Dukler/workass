#!/bin/zsh
# feed.sh APP VERSION BUILD : publish a sealed app into the rig's local feed
set -eu
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; APP=$1; V=$2; BUILD=$3; D=$U/a/update-feed; mkdir -p $D; rm -f $D/Workass-*.zip(N)
T=$(mktemp -d $U/tmp.XXXX); ditto "$APP" $T/Workass.app; Z=$D/Workass-$V-darwin-arm64.zip; ditto -c -k --sequesterRsrc --keepParent $T/Workass.app $Z; rm -rf $T
python3 -I - "$HOME/Library/Application Support/Workass/update-feed/workass-darwin-arm64-release.json" "$D/workass-darwin-arm64-release.json" "$Z" "$V" "$BUILD" <<'P'
import json,sys,hashlib,os
s,d,z,v,b=sys.argv[1:]; r=json.load(open(s)); r['version']=v; r['build']=int(b)
r['artifacts']['update']={'name':os.path.basename(z),'url':os.path.basename(z),'sha256':hashlib.sha256(open(z,'rb').read()).hexdigest(),'size':os.path.getsize(z)}
json.dump(r,open(d,'w'),indent=2); os.chmod(d,0o600); print('feed',r['version'],r['build'],r['artifacts']['update']['sha256'][:12])
P
