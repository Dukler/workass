#!/bin/sh
cd /Users/dev/Workspace/workass
env -i HOME="/Users/dev" USER="dev" LOGNAME="dev" TMPDIR="${TMPDIR:-/tmp}" LANG=en_US.UTF-8 PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin   WORKASS_PROFILE=dev WORKASS_DATA_ROOT=/Users/dev/Workspace/workass/.dev/urig/b WORKASS_LOG_ROOT=/Users/dev/Workspace/workass/.dev/urig/b-logs WORKASS_BROWSER_CONTROL_FILE=/Users/dev/Workspace/workass/.dev/urig/b/browser-control.json   /Users/dev/Workspace/workass/.dev/urig/workass-dev --headless --state-dir /Users/dev/Workspace/workass/.dev/urig/b/state --port 28890 --bind localhost --renderer-dir /Users/dev/Workspace/workass/desktop/renderer2/dist >>/Users/dev/Workspace/workass/.dev/urig/b-logs/daemon.log 2>&1 &
sleep 3
RAIL_URL=https://127.0.0.1:28890/ RAIL_SHOTS=/Users/dev/Workspace/workass/.dev/urig/shots RAIL_PORT=28892 exec electron /Users/dev/Workspace/workass/docs/evidence/job-bugs-20261030/harness/rail-harness.cjs >>/Users/dev/Workspace/workass/.dev/urig/b-logs/electron.log 2>&1
