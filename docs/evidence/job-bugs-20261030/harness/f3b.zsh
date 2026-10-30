source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL the owed notices land, no fault injected $(now)"
Y=(tab-2dc8eaebff07525c50c72d5e chat-9b95221c28f11fa69f012132); JL=xwDWcpOIF0xB1Z; JR=xwg2IkrO27m-F9
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
monitors_all | grep -E "watched .*($JL|$JR)"; count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote
echo "-- a message gives the chat its Codex lane $(now)"; send_chat $Y[1] $Y[2] "Reply with only the word LANEUP"
for i in $(seq 1 150); do a=$(count_notices $Y[1] $Y[2] rd-local | tail -1); b=$(count_notices $Y[1] $Y[2] rd-remote | tail -1); case "$a$b" in *\|1*\|1) echo "both in the chat after ${i}s $(now)"; break;; esac; sleep 1; done
sleep 10; count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote
monitors_all | grep -E "(watched|held) .*($JL|$JR)"; grep -a "could not deliver\|stays owed" $P/mac-logs/daemon.log | tail -2 | cut -c1-110
echo "-- owner restart: offered again?"; release; owner_down; owner_up; renderer_key; sleep 8; hold_on
count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote
echo "== other endings"
R() { WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state "$@" >/dev/null 2>>$P/f3-jobs.err; }
R --label rh-cancel -- sleep 60 & sleep 2; W=$(job_id_of_label rh-cancel); $P/tool workass_cancel_process_child "{\"work_id\":\"$W\",\"operation_id\":\"c-$RANDOM\"}" | cut -c1-120; echo
R --label rh-down -- sleep 8 & sleep 2; echo "-- owner down while rh-down runs and ends"; owner_down; sleep 10; owner_up; renderer_key; sleep 6; hold_on; cp $P/ctx.json $P/ctx-main.json
R --label rh-burst1 -- sleep 1 & R --label rh-burst2 -- sleep 1 & R --label rh-burst3 -- sleep 1 & R --label rh-burst4 -- sleep 1 &
sleep 6; $P/rows | grep -E "rh-burst" | cut -c1-110
echo "-- parent Stop with a job running"; R --label rh-stopped -- sleep 12 & sleep 2; stopbtn; sleep 14
hold_on; $P/notices > $P/.n; for l in rh-cancel rh-down rh-burst1 rh-burst2 rh-burst3 rh-burst4 rh-stopped; do echo "NOTICES|$l|$(grep -c "\"$l\"" $P/.n)|$(grep "\"$l\"" $P/.n | cut -d' ' -f2 | tr '\n' ' ')"; done
monitors_all | grep -E "watched rh-" | cut -c1-120; grep -a "settled without" $P/mac-logs/daemon.log | tail -2 | cut -c1-150
release
