#!/bin/sh
cd /Users/dev/Workspace/workass
export WORKASS_PROFILE=dev WORKASS_DATA_ROOT=/Users/dev/Workspace/workass/.dev/rail-proof/mac-data WORKASS_LOG_ROOT=/Users/dev/Workspace/workass/.dev/rail-proof/mac-logs WORKASS_BROWSER_CONTROL_FILE=/Users/dev/Workspace/workass/.dev/rail-proof/browser-control.json
exec /Users/dev/Workspace/workass/.dev/rail-proof/workass-mac --headless --state-dir /Users/dev/Workspace/workass/.dev/rail-proof/mac-state --port 18990 --bind localhost --renderer-dir /Users/dev/Workspace/workass/desktop/renderer2/dist >>/Users/dev/Workspace/workass/.dev/rail-proof/mac-logs/daemon.log 2>&1
