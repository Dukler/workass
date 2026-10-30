# STATUS TAG: three real subagents and a job running, then the status from each side.
stub_updater() { $P/rail js "(()=>{window.workassUpdater=window.workassUpdater||{getState:async()=>({currentVersion:'0.1.900',phase:'idle'}),diagnostics:async()=>({schemaVersion:1,rigStub:true,state:{phase:'idle'}}),applyAuthorized:async()=>{throw new Error('rig')}};return 1})()" >/dev/null; }
sj() { python3 -I -c "
import sys,json
t=sys.stdin.read()
try: d=json.loads(t)
except Exception: print('  NOT JSON:',t[:300]); sys.exit()
r=d.get('runningJobs')
if r is None: print('  NO runningJobs:',json.dumps(d)[:300]); sys.exit()
print('  summary:',r.get('summary'))
print('  total',r.get('total'),'known',r.get('known'))
for k in ('protected','wouldDie'):
    for j in r.get(k) or []: print('  ',k,'|',j.get('kind'),'|',j.get('label'),'|',j.get('workId'),'| owner',j.get('owner'),'|',str(j.get('note'))[:70])
"; }
status_all() { 
  echo "-- asked from the Mac about duklerver"; $P/tool workass_get_update_status "{\"machine_id\":\"$M\"}" | sj
  echo "-- asked from the Mac about itself"; $P/tool workass_get_update_status "{\"machine_id\":\"$SELF\"}" | sj
  echo "-- asked on duklerver about itself, with the hosted child's credential"
  ssh -o BatchMode=yes duklerver "C=\$(ls -td /data/workass-scratch/state/tool-bootstrap/*/ | head -1)context.json; printf '%s' '{\"machine_id\":\"$M\"}' | /data/workass-scratch/runtime/workass tools --context \$C call workass_get_update_status 2>&1" | sj
}
status_case() { T=$1
  stub_updater; $P/tool workass_agent_catalog '{}' > $P/.cat.json; CX=$(pick_model codex luna); CL=$(pick_model claude haiku); echo "models $CX $CL"
  echo "== nothing running"; JOB=$(host_job $T-idlejob 1); sleep 4; $P/tool workass_get_update_status "{\"machine_id\":\"$M\"}" | sj
  JOB=$(host_job $T-job 120); echo "job $JOB"
  HS=$($P/spawn $T-h-$RANDOM $T-hosted 'Run the shell command `sleep 110` and then reply with exactly: H-DONE' - $M); echo "hosted: $(echo $HS | cut -c1-140)"
  LS=$($P/spawn $T-l-$RANDOM $T-local 'Run the shell command `sleep 110` and then reply with exactly: L-DONE' $CX); echo "local: $(echo $LS | cut -c1-140)"
  read -r CT CC <<< "$(mk_chat claude $CL low $T-native)"; echo "claude chat $CT $CC"
  send_chat $CT $CC "Use your Agent tool once, with run_in_background true and description $T-native-agent, giving it this task: run the Bash command sleep 100 and then finish. As soon as it has started reply with only the word OKAG. Do not wait for it."
  wait_reply $CT $CC OKAG 60
  sleep 15
  echo "host sleeps in the scratch cgroup:"; hs
  echo "local sleeps: $(pgrep -fl '^sleep (100|110)$' | wc -l)"
  $P/rows | grep $T- | cut -c1-110
  status_all
}
