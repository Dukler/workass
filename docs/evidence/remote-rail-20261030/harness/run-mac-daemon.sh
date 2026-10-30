#!/bin/sh
cd /Users/dukler/Workspace/workass
export WORKASS_PROFILE=dev WORKASS_DATA_ROOT=/Users/dukler/Workspace/workass/.dev/rail-proof/mac-data WORKASS_LOG_ROOT=/Users/dukler/Workspace/workass/.dev/rail-proof/mac-logs WORKASS_BROWSER_CONTROL_FILE=/Users/dukler/Workspace/workass/.dev/rail-proof/browser-control.json
exec /Users/dukler/Workspace/workass/.dev/rail-proof/workass-mac --headless --state-dir /Users/dukler/Workspace/workass/.dev/rail-proof/mac-state --port 18990 --bind localhost --renderer-dir /Users/dukler/Workspace/workass/desktop/renderer2/dist >>/Users/dukler/Workspace/workass/.dev/rail-proof/mac-logs/daemon.log 2>&1
