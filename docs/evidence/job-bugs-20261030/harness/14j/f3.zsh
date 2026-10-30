source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh; source $P/status.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL native Claude agent $(now) tree=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD)+fix"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on; stub_updater
$P/tool workass_agent_catalog '{}' > $P/.cat.json; CL=$(pick_model claude haiku)
read -r CT CC <<< "$(mk_chat claude $CL low fn-native)"
send_chat $CT $CC "Use your Agent tool once, with run_in_background true and description fn-native-agent, giving it this task: run this exact Bash command in the foreground, without run_in_background, and wait for it, then finish: python3 -c 'import time;time.sleep(75)'. As soon as it has started reply with only the word OKAG. Do not wait for it."
wait_reply $CT $CC OKAG 60; sleep 12
echo "agent's process: $(pgrep -f 'time.sleep\(75\)' | wc -l)"
echo "-- asked from the Mac about itself"; $P/tool workass_get_update_status "{\"machine_id\":\"$SELF\"}" | sj
echo "-- asked from the Mac about duklerver"; $P/tool workass_get_update_status "{\"machine_id\":\"$M\"}" | sj
owner_bounce 3; sleep 3
python3 -I - <<PY
import glob,json
for f in glob.glob('$P/mac-state/spawned-work-receipts/*.jsonl'):
    for l in open(f):
        r=json.loads(l)
        if r.get('label')=='fn-native-agent': print('receipt',r.get('label'),r.get('kind'),r.get('status'),str(r.get('summary'))[:120],'elapsedMs',r.get('elapsedMs'))
PY
release; echo "### end $(now)"
