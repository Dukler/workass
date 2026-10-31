#!/bin/zsh
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; A=$HOME/Applications/WorkassUpdateRig.app/Contents/Resources/runtime/workass; M=m-e007ead07e494386c22f66134b279ea9
TAB=tab-1793480841833-1; CHAT=conv-mvww6q95-fmr6ao; L=$U/t1.log; : > $L
c(){ node $U/scripts/cdp.mjs 28992 "$1"; }
send(){ node $U/scripts/cdp.mjs 28992 "$(cat $U/scripts/send.js)($(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1"))"; }
atool(){ printf '%s' "$2" | $A tools --context $U/a-ctx.json call "$1"; }
btool(){ printf '%s' "$2" | $U/workass-dev tools --context $U/b-ctx.json call "$1"; }
log(){ echo "$(date -u +%T.%3N 2>/dev/null || date -u +%T) $*" | tee -a $L; }
rows(){ node $U/scripts/wire.mjs 28990 spawned-work:list "[{\"tabId\":\"$TAB\",\"chatId\":\"$CHAT\"}]" 2>/dev/null | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('rows-unreadable'); sys.exit()
it=d.get('items') if isinstance(d,dict) else d
print(' ; '.join('%s=%s/%s'%(i.get('label'),i.get('status'),i.get('lastToolName')) for i in (it or [])) or str(d)[:120])"; }
curl -s -o /dev/null "http://127.0.0.1:28991/__workass-shell/screenshot?click=$(python3 -c 'import urllib.parse;print(urllib.parse.quote(".tico[title=\"Mostrar/ocultar panel\"]"))')" ; 
send "Do these two steps with your shell tool, in order. Step 1, run: nohup \"\$WORKASS_TOOLS_COMMAND\" run --state-dir $U/a/state --label job-agent2 -- sleep 170 >/dev/null 2>&1 &   Step 2, run in the foreground with a 600000 ms timeout: python3 -c 'import time;time.sleep(900)'. When step 2 finishes reply only with DONE-TURN." >>$L
for i in {1..50}; do ps -axo command | grep -v -E 'zsh|grep' | grep -q 'time.sleep(900)' && break; sleep 1; done; log "turn tool running: $(ps -axo pid,command | grep -v -E 'zsh|grep' | grep 'time.sleep(900)' | cut -c1-80)"
cp $(ls -td $U/a/state/tool-bootstrap/*/ | head -1)context.json $U/a-ctx.json
log "subagent: $(atool workass_spawn_subagent '{"operation_id":"t1b-sub","label":"sub-orphan","task":"Run this shell command with your shell tool, timeout 600000: python3 -c \"import time;time.sleep(421)\" then reply DONE-SUB.","permission_intent":"full"}' | cut -c1-200)"
WORKASS_TOOL_CONTEXT=$U/a-ctx.json nohup $A run --state-dir $U/a/state --label job-cli2 --operation-id t1b-cli -- sleep 150 >$U/job-cli.out 2>$U/job-cli.err &
log "job-remote: $(btool workass_start_process_child "{\"operation_id\":\"t1b-remote\",\"label\":\"job-remote2\",\"cwd\":\"/tmp\",\"command\":[\"sleep\",\"150\"],\"machine_id\":\"$M\"}" | cut -c1-240)"
sleep 12
log "rows: $(rows)"; log "supervisors: $(pgrep -fl 'workass run' | grep urig | cut -c1-20 | tr '\n' ' ')"; pgrep -fl 'run .*--label' | grep -c WorkassUpdateRig
ps -axo pid,command | grep -E 'WorkassUpdateRig.*runtime/workass run|[s]leep 1[57]0|[s]leep 12' | grep -v grep | cut -c1-170 | tee -a $L
curl -s -o $U/shots/t1-before.png http://127.0.0.1:28991/__workass-shell/screenshot
log "status before: $(atool workass_get_update_status "{\"machine_id\":\"$M\"}" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(json.dumps(d.get("runningJobs"))[:900])')"
WORKASS_TOOL_CONTEXT=$U/a-ctx.json nohup $A run --state-dir $U/a/state --label job-short2 --operation-id t1b-short -- sleep 7 >$U/job-short.out 2>$U/job-short.err &
log "click: $(c "(async()=>{const w=ms=>new Promise(r=>setTimeout(r,ms));const f=()=>document.querySelector('.updcard');let b=f();const t0=b.textContent;b.click();await w(1800);b=f();const t1=b?b.textContent:'gone';if(b&&/igual/.test(t1)){b.click();return 'armed then second click: '+t1}return 'one click: '+t0+' -> '+t1})()")"
for i in {1..110}; do
  h=$(curl -ks --max-time 1 https://127.0.0.1:28990/workass/health | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["version"],d.get("instanceId"))' 2>/dev/null || echo down)
  s=$(curl -s --max-time 1 http://127.0.0.1:28991/__workass-shell/status | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("appVersion"),d.get("controller"),(d.get("catalog")or{}).get("readyModelCount"))' 2>/dev/null || echo noshell)
  j=$(python3 -c "
import json,glob
f=sorted(glob.glob('$U/a/updates/transactions/*/journal.json'))
print(json.load(open(f[-1])).get('phase') if f else '-')" 2>/dev/null)
  n=$(pgrep -f 'runtime/workass run' | wc -l | tr -d ' ')
  sl=$(pgrep -x sleep | xargs -I{} ps -o command= -p {} 2>/dev/null | grep -cE '^sleep (150|170)$')
  log "t=$i health=[$h] shell=[$s] journal=$j supervisors=$n sleeps=$sl rows=$(rows | cut -c1-260)"
  sleep 2
done
curl -s -o $U/shots/t1-after.png http://127.0.0.1:28991/__workass-shell/screenshot
python3 -c "import json;d=json.load(open('$U/a/updates/receipt.json'));print({k:d.get(k) for k in ('phase','previousVersion','targetVersion','installedVersion','activated','rolledBack','error','healthGates','interruptedWork')})" | tee -a $L
