#!/bin/zsh
RR=/Users/dev/Workspace/workass; U=$RR/.dev/urig; A=$HOME/Applications/WorkassUpdateRig.app/Contents/Resources/runtime/workass; M=m-e007ead07e494386c22f66134b279ea9
TAB=tab-1793480841833-1; CHAT=conv-mvww6q95-fmr6ao; L=$U/t2.log; : > $L
c(){ node $U/scripts/cdp.mjs 28992 "$1"; }
atool(){ printf '%s' "$2" | $A tools --context $U/a-ctx.json call "$1"; }
btool(){ printf '%s' "$2" | $U/workass-dev tools --context $U/b-ctx.json call "$1"; }
log(){ echo "$(date -u +%T) $*" | tee -a $L; }
rows(){ node $U/scripts/wire.mjs 28990 spawned-work:list "[{\"tabId\":\"$TAB\",\"chatId\":\"$CHAT\"}]" 2>/dev/null | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('rows-unreadable'); sys.exit()
it=d.get('items') if isinstance(d,dict) else d
print(' ; '.join('%s=%s'%(i.get('label'),i.get('status')) for i in (it or []) if i.get('label','').startswith(('job3','sub3','sub-cancel'))) or str(d)[:100])"; }
log "subagent: $(atool workass_spawn_subagent '{"operation_id":"t3-sub","label":"sub3-orphan","task":"Run this shell command, timeout 300000: python3 -c \"import time;time.sleep(291)\" then reply DONE.","permission_intent":"full"}' | cut -c1-90)"
WORKASS_TOOL_CONTEXT=$U/a-ctx.json nohup $A run --state-dir $U/a/state --label job3-cli --operation-id t3-cli -- sleep 90 >$U/job3-cli.out 2>$U/job3-cli.err &
log "job3-remote: $(btool workass_start_process_child "{\"operation_id\":\"t3-remote\",\"label\":\"job3-remote\",\"cwd\":\"/tmp\",\"command\":[\"sleep\",\"90\"],\"machine_id\":\"$M\"}" | cut -c1-120)"
sleep 10
log "rows: $(rows)"; curl -s -o $U/keep/t2-before-update.png http://127.0.0.1:28991/__workass-shell/screenshot
log "click: $(c "(async()=>{const w=ms=>new Promise(r=>setTimeout(r,ms));const f=()=>document.querySelector('.updcard');let b=f();const t0=b.textContent;b.click();await w(1500);b=f();const t1=b?b.textContent:'gone';if(b&&/igual/.test(t1)){b.click();return 'armed, second click: '+t1}return 'one click: '+t0+' -> '+t1})()")"
shot=0
for i in {1..60}; do
  h=$(curl -ks --max-time 1 https://127.0.0.1:28990/workass/health | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["version"])' 2>/dev/null || echo down)
  s=$(curl -s --max-time 1 http://127.0.0.1:28991/__workass-shell/status | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("appVersion"),d.get("controller"))' 2>/dev/null || echo noshell)
  n=$(pgrep -f 'runtime/workass run' | wc -l | tr -d ' ')
  log "t=$i health=[$h] shell=[$s] supervisors=$n rows=$(rows)"
  if [ $shot = 0 ] && [ "$h" = 0.1.255 ] && [ "${s%% *}" = 0.1.255 ]; then shot=1; sleep 5; curl -s -o $U/keep/t2-after-update-running.png http://127.0.0.1:28991/__workass-shell/screenshot; fi
  sleep 2
done
python3 -c "import json;d=json.load(open('$U/a/updates/receipt.json'));print({k:d.get(k) for k in ('phase','previousVersion','targetVersion','installedVersion','activated','rolledBack','error','healthGates')})" | tee -a $L
printf '{"tab_id":"%s","chat_id":"%s","limit":300}' $TAB $CHAT | $A tools --context $U/a-ctx.json call workass_read_chat | python3 $U/scripts/msgs.py | grep -E 'job3|sub3|sub-cancel' | cut -c1-140 | tee -a $L
printf '{"tab_id":"%s","chat_id":"%s","limit":300}' $(python3 -c "import json;d=json.load(open('$U/b-ctx.json'));print(d['tab_id'],d['chat_id'])") | $U/workass-dev tools --context $U/b-ctx.json call workass_read_chat | python3 $U/scripts/msgs.py | grep -E 'job3' | cut -c1-140 | tee -a $L
curl -s -X POST --data-binary "window.api.spawnedWorkList('$(python3 -c "import json;print(json.load(open('$U/b-ctx.json'))['tab_id'])")','$(python3 -c "import json;print(json.load(open('$U/b-ctx.json'))['chat_id'])")').then(r=>JSON.stringify((r.items||r).filter(i=>/job3/.test(i.label)).map(i=>[i.label,i.status,i.summary])))" http://127.0.0.1:28892/eval | tee -a $L; echo
curl -s -o $U/keep/t2-after-all.png http://127.0.0.1:28991/__workass-shell/screenshot
