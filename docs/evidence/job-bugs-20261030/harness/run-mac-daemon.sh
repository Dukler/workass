#!/bin/sh
# Owner dev daemon. Starts from a clean environment: a shell inside the real app would
# otherwise hand it the real app's credential, profile and data root.
cd /Users/dev/Workspace/workass
exec env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" TMPDIR="$TMPDIR" LANG=en_US.UTF-8 PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Users/dev/.local/bin \
  WORKASS_PROFILE=dev WORKASS_DATA_ROOT=/Users/dev/Workspace/workass/.dev/rail-proof/mac-data WORKASS_LOG_ROOT=/Users/dev/Workspace/workass/.dev/rail-proof/mac-logs WORKASS_BROWSER_CONTROL_FILE=/Users/dev/Workspace/workass/.dev/rail-proof/browser-control.json \
  WORKASS_CLAUDE_EXECUTABLE=/Users/dev/.local/bin/claude WORKASS_CLAUDE_SDK_MODULE="/Applications/Workass.app/Contents/Resources/runtime/frontier-hosts/darwin-arm64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs" \
  /Users/dev/Workspace/workass/.dev/rail-proof/workass-mac --headless --state-dir /Users/dev/Workspace/workass/.dev/rail-proof/mac-state --port 18990 --bind localhost --renderer-dir /Users/dev/Workspace/workass/desktop/renderer2/dist >>/Users/dev/Workspace/workass/.dev/rail-proof/mac-logs/daemon.log 2>&1
