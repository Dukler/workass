source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE $(now) head=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD) dirty=$(git -C /Users/dukler/Workspace/workass status --short | wc -l)"
mesh_up; mockmodel; hold_on; $P/tool workass_agent_catalog '{}' > $P/.cat.json
SELF=$(python3 -I -c "import json;print(json.load(open('$P/mac-state/machine-id.json'))['machineId'])")
echo "== 3: status from the Mac with jobs on the host"
W1=$(host_job rb-protected 70); W2=$(host_job rb-exposed 70 '{"WORKASS_RUN_SCOPED":"1"}'); echo "work $W1 $W2"; sleep 5
ssh -o BatchMode=yes duklerver "systemctl --user is-active workass-job-$W1.scope workass-job-$W2.scope; true"
$P/tool workass_list_process_children '{}' | python3 -I -c "
import sys,json
d=json.load(sys.stdin)
for c in d.get('children',[]): print('LISTED',c.get('work_id'),c.get('label'),c.get('status'),'host',c.get('host_machine'),'survives',c.get('survives_restart'),str(c.get('survival_note'))[:70])"
echo -n "STATUS for host, asked from the Mac: "; hstatus $M
echo "== 1: real agents, real shells"
CL=$(pick_model claude haiku); CX=$(pick_model codex luna); echo models $CL $CX
read -r -A ALL < $P/.realchats; C=($ALL[1] $ALL[2]); X=($ALL[3] $ALL[4]); echo "chats ${C[*]} ${X[*]}"
agent_job $C[1] $C[2] rb-claude-fg OKFG; agent_job $X[1] $X[2] rb-codex-fg OKFG
wait_reply $C[1] $C[2] OKFG 150; wait_reply $X[1] $X[2] OKFG 150
agent_bg_job $C[1] $C[2] rb-claude-bg OKBG 25; agent_bg_job $X[1] $X[2] rb-codex-bg OKBG 25
wait_reply $C[1] $C[2] OKBG 120; wait_reply $X[1] $X[2] OKBG 120
sleep 50
echo "shell credential seen by the agents:"; for f in $P/.shell-*; do echo "$f: $(cat $f)"; done
for spec in "$C[1] $C[2] rb-claude-fg" "$C[1] $C[2] rb-claude-bg" "$X[1] $X[2] rb-codex-fg" "$X[1] $X[2] rb-codex-bg"; do count_notices ${=spec}; done
grep -a "\[fleet\]" $P/mac-logs/daemon.log | tail -8 | cut -c1-200
monitors_all | grep -E "rb-|^(held|watched) absent"
release; wait_idle
