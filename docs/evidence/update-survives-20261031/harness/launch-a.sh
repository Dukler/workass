#!/bin/sh
cd /tmp
exec env -i HOME="/Users/dev" USER="dev" LOGNAME="dev" TMPDIR="${TMPDIR:-/tmp}" LANG=en_US.UTF-8 PATH=/usr/bin:/bin:/usr/sbin:/sbin   "/Users/dev/Applications/WorkassUpdateRig.app/Contents/MacOS/Workass" --remote-debugging-port=28992
