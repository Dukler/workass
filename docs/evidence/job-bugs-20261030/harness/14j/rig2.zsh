# Shared by the bursts and the status cases. Machine id is the scratch one.
M=$(ssh -o BatchMode=yes duklerver "python3 -I -c \"import json;print(json.load(open('/data/workass-scratch/state/machine-id.json'))['machineId'])\"")
SELF=$(python3 -I -c "import json;print(json.load(open('$P/mac-state/machine-id.json'))['machineId'])")
# Every row of the owner chat against the receipt the daemon wrote for it. The rule under test: a row ends in the outcome its receipt states.
compare() { $P/rows > $P/.rows; python3 -I - "$1" <<PY
import ast,glob,json,sys,collections
pre=sys.argv[1]; rows={}
for l in open('$P/.rows'):
    try: r=ast.literal_eval(l)
    except Exception: continue
    if r.get('label','').startswith(pre): rows[r['label']]=r
rec={}
for f in glob.glob('$P/mac-state/spawned-work-receipts/*.jsonl'):
    for l in open(f):
        try: r=json.loads(l)
        except Exception: continue
        if r.get('label','').startswith(pre): rec[r['label']]=r
bad=[]; c=collections.Counter()
for k,r in sorted(rows.items()):
    c[r['status']]+=1
    x=rec.get(k)
    if not x or x.get('status')!=r['status']: bad.append((k,'row',r['status'],'receipt',x and x.get('status'),x and x.get('outcome'),'rowsummary',r.get('summary'),'receiptsummary',x and x.get('summary')))
print('COMPARE|%s|rows=%d|receipts=%d|statuses=%s|mismatch=%d'%(pre,len(rows),len(rec),dict(c),len(bad)))
for b in bad: print('  MISMATCH',b)
PY
}
burst_local() { # TAG N: N real workass run jobs on the Mac, all ending together
  for n in $(seq 1 $2); do WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state --label $1-$n -- sleep 2 >/dev/null 2>>$P/burst.err & done; sleep 7; }
burst_host() { # TAG N: N real workass run jobs on the host, all ending together
  for n in $(seq 1 $2); do host_job $1-$n 3 >/dev/null; done; sleep 12; }
# Started together so they end together. Each gets its own op file: host_job's scratch names would collide.
burst_host_par() { for n in $(seq 1 $2); do ( python3 -I -c "
import json,sys,random
print(json.dumps(dict(operation_id=sys.argv[1]+'-'+str(random.getrandbits(32)),label=sys.argv[1],cwd='/tmp',command=['sleep','6'],machine_id=sys.argv[2])))" "$1-$n" "$M" | { read J; $P/tool workass_start_process_child "$J" >/dev/null; } ) & done; sleep 16; }
