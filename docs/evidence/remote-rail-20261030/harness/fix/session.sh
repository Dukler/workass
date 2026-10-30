#!/bin/zsh
# One foreground rig session: owner daemon + hidden real renderer live only as long
# as this command. session.sh SCRIPT [args]; SCRIPT sees $P.
export P=${0:a:h}
daemon() { "$P/run-mac-daemon.sh" & DPID=$!; for i in $(seq 1 60); do curl -ks --max-time 1 https://127.0.0.1:18990/workass/health >/dev/null && return 0; sleep 0.5; done; echo "owner did not come up"; }
owner_stop() { pkill -f "workass-mac --headless --state-dir" ; for i in $(seq 1 40); do pgrep -f "workass-mac --headless --state-dir" >/dev/null || return 0; sleep 0.25; done; }
cleanup() { pkill -f "rail-harness.cjs" ; owner_stop; }
trap cleanup EXIT INT TERM
daemon
RAIL_URL=https://127.0.0.1:18990/ RAIL_SHOTS=$P/shots RAIL_PORT=18992 electron "$P/rail-harness.cjs" >>"$P/mac-logs/electron.log" 2>&1 &
for i in $(seq 1 60); do curl -s --max-time 1 -X POST --data-binary "1" http://127.0.0.1:18992/eval >/dev/null && break; sleep 0.5; done
sleep 4
source "$1"
