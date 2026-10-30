M=m-99e14fb28a97b49ceb7116675e8fab7f
id_of() { python3 -I -c "import sys,json;print(json.loads(sys.stdin.read())['id'])"; }
cards() { $P/rail js "JSON.stringify([...document.querySelectorAll('.evt-sub')].map(e=>(e.querySelector('.evt-title')?.textContent||'')+' => '+(e.querySelector('.evt-st')?.textContent||'')+' ['+e.dataset.status+']'))"; echo; }
hs() { ssh -o BatchMode=yes duklerver 'for p in $(pgrep -x sleep); do grep -q "app.slice/workass-scratch.service" /proc/$p/cgroup && echo "scratch sleep $p $(tr "\0" " " </proc/$p/cmdline)"; done; true'; }
ls_local_sleep() { pgrep -fl "^sleep (4|5|6|7)[0-9]$" || true; }
link() {
  $P/rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null
  $P/rail js "window.api.machinesAdd('192.168.0.134:18788').then(x=>JSON.stringify({ok:x.ok,e:x.error,m:(x.machines||[]).map(m=>[m.machineId,m.state||m.status])}))"; echo
  for i in $(seq 1 30); do sleep 1; [ $i -ge 5 ] && break; done
}
stopbtn() { $P/rail js "(()=>{const b=[...document.querySelectorAll('.comp button')].pop();const c=b&&b.className;if(c&&/stop/.test(c)){b.click();return 'clicked '+c}return 'not stop: '+c})()"; echo; }
watch_rows() { for n in $(seq 1 ${1:-5}); do sleep ${2:-5}; echo "-- t+$((n*${2:-5})) $(date -u +%T)"; $P/rows | cut -c1-170; cards; done; }
ready() { for i in $(seq 1 60); do case "$($P/rail js "(document.querySelector('.modelsel')?.textContent||'')")" in *Mock*) return 0;; esac; sleep 1; done; echo "no mock model"; }
# Open every folded subagent group, then read each transcript card: the same words a person reads.
cards() { $P/rail js "(()=>{document.querySelectorAll('.tg-head,.tgroup-head,[class*=toolgroup] > button,.egrp > summary').forEach(()=>{});[...document.querySelectorAll('button,summary,[role=button]')].filter(e=>/\d+ subagentes?|Subagente ·/.test(e.textContent)&&!e.closest('.evt-sub')&&!e.dataset.opened&&!e.closest('.rail')).forEach(e=>{e.dataset.opened=1;e.click()});return 1})()" >/dev/null; sleep 0.4; $P/rail js "JSON.stringify([...document.querySelectorAll('.evt-sub')].map(e=>(e.querySelector('.evt-title')?.textContent||'')+' => '+(e.querySelector('.evt-st')?.textContent||'')+' ['+e.dataset.status+']'))"; echo; }
mine() { sed 's/Subagente · //g' | python3 -I -c "
import sys,json,os
d=json.loads(sys.stdin.read() or '{}'); v=json.loads(d.get('value') or '[]'); print('CARDS',[c for c in v if c.startswith(os.environ['LBL']+' ')])"; }
owner_bounce() { pkill -f "workass-mac --headless --state-dir"; echo "owner down $(date -u +%T)"; sleep ${1:-3}; daemon; curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; $P/rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null; echo "owner up $(date -u +%T)"; ready; }
wait_end() { for n in $(seq 1 ${1:-14}); do sleep 6; S=$($P/rows | grep "$LBL " | cut -c1-175); echo "$(date -u +%T) $S" | cut -c1-175; cards | mine; echo "$S" | grep -q "'running'" || break; done; }
# The stored transcript, not the screen: what session:get returns for the card, with no row laid over it.
rawcard() { TAB=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['tab_id'])"); $P/rail js "window.api.getSession({tabId:'$TAB'}).then(s=>{const out=[];const walk=(o,d)=>{if(!o||typeof o!=='object'||d>9)return;if(Array.isArray(o)){o.forEach(x=>walk(x,d+1));return}if(o.subagentHeader===true&&String(o.title||'').startsWith('$LBL '))out.push(o.title+' = '+o.status);for(const k in o)walk(o[k],d+1)};walk(s,0);return JSON.stringify(out)}).catch(e=>'ERR '+e)"; echo; }
