source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL refused delivery, recovering live with no restart and no fault injected $(now)"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on; cp $P/ctx.json $P/ctx-main.json
Y=($(mk_chat mock mock-deterministic low rd-chat)); echo "chat $Y"; rm -f $HOLD; send_chat $Y[1] $Y[2] '[mock:hold-tools]'; sleep 4; ctx_of $Y[2] $P/ctx-y.json
cp $P/ctx-y.json $P/ctx.json
WORKASS_TOOL_CONTEXT=$P/ctx-y.json $P/workass-mac run --state-dir $P/mac-state --label rd-local -- sleep 30 >/dev/null 2>>$P/f3-jobs.err &
JR=$(host_job rd-remote 30); sleep 2; JL=$(job_id_of_label rd-local); echo "local $JL remote $JR"
L2=$(future_lane $Y[2]); echo "lane the chat does not have yet: $L2"
owner_down; set_lane $JL $L2; set_lane $JR $L2; owner_up; renderer_key; sleep 2
A=$(grep -ac "could not deliver" $P/mac-logs/daemon.log)
for i in $(seq 1 60); do n=$(grep -ac "could not deliver" $P/mac-logs/daemon.log); [ $((n-A)) -ge 4 ] && break; sleep 1; done
echo "-- refused, with the chat lacking that lane $(now)"; grep -a "could not deliver\|stays owed" $P/mac-logs/daemon.log | tail -8 | cut -c1-150
monitors_all | grep -E "watched .*($JL|$JR)"; monitors_all | grep -E "held .*$JL"
hold_on; cp $P/ctx.json $P/ctx-main.json; echo "notices so far: local $(read_chat $Y[1] $Y[2] | grep -c 'rd-local.*finished') "
count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote
echo "-- the chat gets the lane $(now)"; $P/tool workass_configure_chat "{\"tab_id\":\"$Y[1]\",\"chat_id\":\"$Y[2]\",\"operation_id\":\"cfg-$RANDOM\",\"provider_id\":\"codex\",\"model_id\":\"gpt-6-luna\",\"effort\":\"low\",\"permission_intent\":\"full\"}" | cut -c1-200; echo
for i in $(seq 1 100); do count_notices $Y[1] $Y[2] rd-local | tail -1 | grep -q '|1$' && count_notices $Y[1] $Y[2] rd-remote | tail -1 | grep -q '|1$' && { echo "both arrived after ${i}s, no restart"; break; }; sleep 1; done
sleep 8; count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote
monitors_all | grep -E "watched .*($JL|$JR)"; monitors_all | grep -E "held .*$JL"
echo "-- owner restart: offered again?"; release; owner_down; owner_up; renderer_key; sleep 8; hold_on
count_notices $Y[1] $Y[2] rd-local; count_notices $Y[1] $Y[2] rd-remote; release
