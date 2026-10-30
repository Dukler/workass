#!/bin/sh
# Build the owner and host binaries from the tree and put the host one in place.
set -e
cd /Users/dukler/Workspace/workass
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.daemonVersion=0.1.900" -o /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-900 ./cmd/workass
rm -f /Users/dukler/Workspace/workass/.dev/rail-proof/workass-mac; go build -ldflags "-X main.daemonVersion=0.1.900" -o /Users/dukler/Workspace/workass/.dev/rail-proof/workass-mac ./cmd/workass
scp -q -o BatchMode=yes /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-900 duklerver:/data/workass-scratch/workass-linux-900
ssh -o BatchMode=yes duklerver 'systemctl --user stop workass-scratch.service 2>/dev/null; chmod +x /data/workass-scratch/workass-linux-900; mkdir -p /data/workass-scratch/runtime; cp /data/workass-scratch/workass-linux-900 /data/workass-scratch/runtime/workass; systemctl --user start workass-scratch.service; sleep 1; systemctl --user is-active workass-scratch.service'
