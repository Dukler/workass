source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE status + refused delivery $(now) head=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD)"
renderer_key; mesh_up; sleep 3; mockmodel; hold_on
echo "== 3: status from the Mac with jobs on the host"
W1=$(host_job rb3-protected 45); W2=$(host_job rb3-exposed 45 '{"WORKASS_RUN_SCOPED":"1"}'); echo "work $W1 $W2"; sleep 5
ssh -o BatchMode=yes duklerver "systemctl --user is-active workass-job-$W1.scope; systemctl --user is-active workass-job-$W2.scope; true"
$P/tool workass_list_process_children '{}' | python3 -I -c "
import sys,json
d=json.load(sys.stdin)
for c in d.get('children',[]): print('LISTED',c.get('label'),c.get('status'),'survives',c.get('survives_restart'),str(c.get('survival_note'))[:70])"
echo -n "STATUS asked from the Mac: "; hstatus $M
echo "== 2: a refused delivery, remote job, no fault injected: the monitor names a lane the chat does not have"
W3=$(host_job rb2-remote 30); echo "work $W3"; sleep 2; monitor_of rb2-remote
release; wait_idle; owner_down; lane_gone $W3; owner_up; renderer_key
for i in $(seq 1 50); do grep -aq "could not deliver" $P/mac-logs/daemon.log && break; sleep 1; done
grep -a "could not deliver\|held a" $P/mac-logs/daemon.log | tail -3 | cut -c1-200
monitor_of rb2-remote
echo "second owner restart: is it offered again?"; owner_down; owner_up; renderer_key; sleep 8
hold_on; $P/notices | grep -c rb2-remote; $P/notices | cut -c1-110; monitor_of rb2-remote; release; wait_idle
