#!/bin/zsh
# daemon-rig.zsh BIN TAG ROUNDS N: a dev daemon on its own state, a real renderer in a hidden window,
# a held mock turn, and N reported `workass run` jobs at once on a fresh outbox, ROUNDS times.
BIN=${1:a}; TAG=$2; ROUNDS=$3; N=$4
R=/Users/dukler/Workspace/workass; P=$R/.dev/outbox-race/rig-$TAG; H=$R/docs/evidence/remote-rail-20261030/harness
rm -rf $P; mkdir -p $P/mac-data $P/mac-logs $P/mac-state $P/shots $P/out
rail() { case $1 in js) curl -s -X POST --data-binary "$2" http://127.0.0.1:18992/eval;; esac; }
cd $R
env -i HOME="$HOME" USER="$USER" LOGNAME="$USER" TMPDIR="$TMPDIR" LANG=en_US.UTF-8 PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin \
  WORKASS_PROFILE=dev WORKASS_DATA_ROOT=$P/mac-data WORKASS_LOG_ROOT=$P/mac-logs WORKASS_BROWSER_CONTROL_FILE=$P/browser-control.json WORKASS_MOCK_HOLD_RELEASE_FILE=$P/release-hold \
  $BIN --headless --state-dir $P/mac-state --port 18990 --bind localhost --renderer-dir $R/desktop/renderer2/dist >>$P/mac-logs/daemon.log 2>&1 &
DPID=$!
env -u WORKASS_TOOL_CONTEXT RAIL_URL=https://127.0.0.1:18990/ RAIL_SHOTS=$P/shots RAIL_PORT=18992 electron $H/rail-harness.cjs >>$P/mac-logs/electron.log 2>&1 &
EPID=$!
cleanup() { pkill -f '[r]ail-harness.cjs'; kill $DPID 2>/dev/null; wait $EPID $DPID 2>/dev/null; }
trap cleanup EXIT INT TERM
for i in $(seq 1 60); do curl -ks --max-time 1 https://127.0.0.1:18990/workass/health >/dev/null && break; sleep 0.5; done
for i in $(seq 1 60); do curl -s --max-time 1 -X POST --data-binary "1" http://127.0.0.1:18992/eval >/dev/null && break; sleep 0.5; done
sleep 5
rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null
rail js "document.querySelector('button.sv2-sq[title^=\"Nueva conversación\"]')?.click()" >/dev/null; sleep 2
for i in $(seq 1 20); do
  case "$(rail js "(document.querySelector('.modelsel')?.textContent||'')")" in *Mock*) break;; esac
  rail js "document.querySelector('.mprow')?1:document.querySelector('.modelsel')?.click()" >/dev/null; sleep 1
  rail js "[...document.querySelectorAll('.mprow')].find(e=>/Mock/.test(e.textContent))?.querySelector('.mpchoose')?.click()" >/dev/null; sleep 1
done
echo "model: $(rail js "(document.querySelector('.modelsel')?.textContent||'')")"
[[ "${DEBUG:-}" = 1 ]] && { rail js "window.api.lanTakeControl().then(x=>JSON.stringify(x).slice(0,300))"; echo; rail js "JSON.stringify(Object.keys(window.api).filter(k=>/chat|session|new|control|tab/i.test(k)))"; echo; rail js "JSON.stringify([...document.querySelectorAll('button')].map(b=>(b.className||'')+'|'+(b.title||'')+'|'+b.textContent.slice(0,20)+'|'+b.disabled).slice(0,40))"; echo; rail js "JSON.stringify({rows:[...document.querySelectorAll('.mprow')].map(e=>e.textContent.slice(0,40)),sel:document.querySelector('.modelsel')?.outerHTML?.slice(0,200),body:document.body.innerText.slice(0,400)})"; echo; }
rail js "(()=>{const a=document.querySelector('.comp textarea');const s=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;s.call(a,'[mock:hold-tools]');a.dispatchEvent(new Event('input',{bubbles:true}));return a.value.length})()" >/dev/null; sleep 0.5
rail js "(()=>{const b=[...document.querySelectorAll('.comp button')].pop();b&&b.click();return b&&b.className})()" >/dev/null; sleep 5
for i in $(seq 1 30); do setopt nullglob; d=($P/mac-state/tool-bootstrap/*/context.json); unsetopt nullglob; (( $#d )) && break; sleep 1; done
[[ "${DEBUG:-}" = 1 ]] && { rail js "document.body.innerText.slice(0,600)"; echo; ls $P/mac-state | head -40; grep -ai 'mock\|error' $P/mac-logs/daemon.log | tail -8 | cut -c1-200; }
(( $#d )) || { echo "NO CREDENTIAL: the held turn did not start"; exit 3; }
cp $(ls -td $P/mac-state/tool-bootstrap/*/ | head -1)context.json $P/ctx.json; chmod 600 $P/ctx.json
TAB=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['tab_id'])"); CHAT=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['chat_id'])")
echo "chat $CHAT"
failed=0 total=0 unreported=0 kept=0
for r in $(seq 1 $ROUNDS); do
  [[ -e $P/mac-state/process-outbox.db ]] && { lsof -t $P/mac-state/process-outbox.db 2>/dev/null | grep -qx $DPID && { kept=$((kept+1)); }; rm -f $P/mac-state/process-outbox.db*; }
  pids=()
  for i in $(seq 1 $N); do
    env -i PATH=/usr/bin:/bin HOME=$HOME WORKASS_TOOL_CONTEXT=$P/ctx.json $BIN run --quiet --state-dir $P/mac-state --label r$r-$i -- /usr/bin/true >$P/out/o$r-$i 2>$P/out/e$r-$i &
    pids+=($!)
  done
  i=0
  for p in $pids; do i=$((i+1)); wait $p; c=$?; total=$((total+1))
    grep -q '"exitCode":0' $P/out/o$r-$i && [[ $c -eq 0 ]] || { failed=$((failed+1)); cut -c1-220 $P/out/e$r-$i | sed -E 's#/[^ ]*process-outbox.db#OUTBOX#' >> $P/refused.txt; }
    grep -q '"reported":true' $P/out/o$r-$i || unreported=$((unreported+1))
  done
done
sleep 4
rail js "window.api.spawnedWorkList('$TAB','$CHAT').then(r=>JSON.stringify((r.items||r).map(i=>[i.label,i.status])))" > $P/rows.json
python3 -I - "$P" "$ROUNDS" "$N" <<'PY'
import glob,json,sys,collections
P,R,N=sys.argv[1],int(sys.argv[2]),int(sys.argv[3])
want={'r%d-%d'%(r,i) for r in range(1,R+1) for i in range(1,N+1)}
rec=collections.Counter()
for f in glob.glob(P+'/mac-state/spawned-work-receipts/*.jsonl'):
    for l in open(f):
        try: r=json.loads(l)
        except Exception: continue
        if r.get('label') in want: rec[r['label']]=r.get('status')
rows=dict(json.loads(json.load(open(P+'/rows.json')).get('value') or '[]'))
seen=set(rec)|(set(rows)&want)
print('DAEMON wanted=%d with_row_or_receipt=%d lost=%d receipt_status=%s row_status=%s'%(len(want),len(seen),len(want-seen),dict(collections.Counter(rec.values())),dict(collections.Counter(v for k,v in rows.items() if k in want))))
print('LOST',sorted(want-seen)[:30])
PY
echo "RESULT tag=$TAG rounds=$ROUNDS n=$N jobs=$total refused=$failed notreported=$unreported daemonHeldOutbox=$kept"
[[ -f $P/refused.txt ]] && sort $P/refused.txt | uniq -c | sort -rn | head
touch $P/release-hold
