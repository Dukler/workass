source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh; source $P/status.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL status and hosted burst $(now) tree=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD)+fix"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
for r in 1 2; do burst_host_par fp$r- 12; compare fp$r-; release; wait_idle 200; hold_on; done
status_case fs
echo "== does the status tell the truth: restart the scratch daemon, stock unit, as an update would"
ssh -o BatchMode=yes duklerver 'systemctl --user restart workass-scratch.service; sleep 3; systemctl --user is-active workass-scratch.service'
echo "host sleeps left: $(hs | tr '\n' ';')"; echo "job's scope: $(ssh -o BatchMode=yes duklerver "systemctl --user is-active workass-job-$JOB.scope")"
sleep 25; $P/rows | grep -E "'fs-(hosted|job)" | cut -c1-200
echo "== same on the owner"; owner_bounce 3; renderer_key; sleep 3; hold_on
$P/rows | grep -E "'fs-local" | cut -c1-200
read_chat $CT $CC > $P/.read.json; python3 -I -c "
import json;d=json.load(open('$P/.read.json'));print('claude chat keys',list(d)[:12])"
python3 -I - <<PY
import glob,json
for f in glob.glob('$P/mac-state/spawned-work-receipts/*.jsonl'):
    for l in open(f):
        r=json.loads(l)
        if str(r.get('label','')).find('fs-native')>=0 or r.get('label')=='fs-local': print('receipt',r.get('label'),r.get('kind'),r.get('status'),str(r.get('summary'))[:110])
PY
$P/tool workass_get_update_status "{\"machine_id\":\"$SELF\"}" | sj
sleep 90; echo "== once the job ended"; $P/rows | grep "'fs-job" | cut -c1-160; $P/tool workass_get_update_status "{\"machine_id\":\"$M\"}" | sj
release; echo "### end $(now)"
