source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL local jobs $(now)"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
read -r -A ALL < $P/.realchats; C=($ALL[1] $ALL[2]); X=($ALL[3] $ALL[4])
echo "== real agents"
agent_job $C[1] $C[2] rf-claude-fg OKAA1; agent_job $X[1] $X[2] rf-codex-fg OKAA1
wait_reply $C[1] $C[2] OKAA1 120; wait_reply $X[1] $X[2] OKAA1 120
agent_bg_job $C[1] $C[2] rf-claude-bg OKAA2 20; agent_bg_job $X[1] $X[2] rf-codex-bg OKAA2 20
wait_reply $C[1] $C[2] OKAA2 90; wait_reply $X[1] $X[2] OKAA2 90
sleep 35
for f in $P/.shell-rf-*; do echo "$f: $(cut -d' ' -f2 $f | sed 's#.*/mac-state#mac-state#')"; done
for spec in "$C[1] $C[2] rf-claude-fg" "$C[1] $C[2] rf-claude-bg" "$X[1] $X[2] rf-codex-fg" "$X[1] $X[2] rf-codex-bg"; do count_notices ${=spec}; done
echo "agent turns the notices started: $(read_chat $C[1] $C[2] | python3 -I -c "import sys,json;d=json.load(sys.stdin);print(d.get('running'),d.get('messageCount'))")"
echo "== shapes, in a held mock turn"
R() { WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state "$@" >/dev/null 2>>$P/f2-jobs.err; }
R --label rf-fail -- sh -c 'exit 3' &
R --label rf-timeout --max-runtime 2s -- sleep 30 &
R --label rf-refused --inputs /nonexistent/in -- true &
R --label rf-instant -- true &
for n in 1 2 3 4 5 6 7 8; do R --label rf-burst$n -- sleep 2 & done
R --label rf-cancelled -- sleep 60 &
sleep 7; $P/rows | grep rf- | cut -c1-180
$P/tool workass_list_process_children '{}' | python3 -I -c "
import sys,json
for c in json.load(sys.stdin).get('children',[]):
    if c.get('label')=='rf-cancelled': print(c.get('work_id'))" > $P/.cancel; W=$(cat $P/.cancel); echo cancel $W
$P/tool workass_cancel_process_child "{\"work_id\":\"$W\"}" | cut -c1-140; echo; sleep 5
release; wait_idle 200; hold_on
$P/notices > $P/.n; for l in rf-fail rf-timeout rf-refused rf-instant rf-burst1 rf-burst2 rf-burst3 rf-burst4 rf-burst5 rf-burst6 rf-burst7 rf-burst8 rf-cancelled; do echo "NOTICES|$l|$(grep -c "\"$l\"" $P/.n)|$(grep "\"$l\"" $P/.n | cut -d' ' -f2 | tr '\n' ' ')"; done
grep -a "\[fleet\]" $P/mac-logs/daemon.log | grep -a "could not\|held a\|settled without" | tail -4 | cut -c1-170
monitors_all | grep -E "^watched rf-" | wc -l
release
