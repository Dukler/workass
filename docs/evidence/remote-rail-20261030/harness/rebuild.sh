#!/bin/sh
# Rebuild both dev daemons from the working tree, restart both, reset chat state only when asked.
set -e
cd /Users/dukler/Workspace/workass
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.daemonVersion=0.1.900" -o /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-900 ./cmd/workass
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.daemonVersion=0.1.901" -o /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-901 ./cmd/workass
go build -ldflags "-X main.daemonVersion=0.1.900" -o /Users/dukler/Workspace/workass/.dev/rail-proof/workass-mac ./cmd/workass
scp -q -o BatchMode=yes /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-900 /Users/dukler/Workspace/workass/.dev/rail-proof/workass-linux-901 duklerver:/data/workass-scratch/
ssh -o BatchMode=yes duklerver 'systemctl --user stop workass-scratch.service; chmod +x /data/workass-scratch/workass-linux-90*; cp /data/workass-scratch/workass-linux-900 /data/workass-scratch/runtime/workass; systemctl --user start workass-scratch.service; sleep 1; systemctl --user is-active workass-scratch.service'
