#!/bin/zsh
# rig.zsh BIN TAG [RENDERER_DIR]: one daemon build, on its own state dir and its own
# HOME, a stub claude in the native layout, a real renderer in a hidden window.
# The npm registry and the native release pointer are the real public ones.
BIN=${1:a}; TAG=$2; RDIR=${3:-}
R=/Users/dukler/Workspace/workass; D=$R/docs/evidence/claude-update-20261030/harness; P=$R/.dev/claude-update-rig/$TAG
H=$R/docs/evidence/remote-rail-20261030/harness; DP=18994; EP=18995
DPID=; EPID=
rail() { curl -s -X POST --data-binary "$1" http://127.0.0.1:$EP/eval | python3 -I -c "import sys,json;print(json.loads(sys.stdin.read()).get('value'))"; }
down() { [[ -n $EPID ]] && { kill $EPID 2>/dev/null; wait $EPID 2>/dev/null; }; [[ -n $DPID ]] && { kill $DPID 2>/dev/null; wait $DPID 2>/dev/null; }; DPID=; EPID=; }
trap down EXIT INT TERM
up() { # up VERSION MODE
  down; rm -rf $P/home $P/state $P/data; mkdir -p $P/home/.local/bin $P/home/.local/share/claude/versions $P/state $P/data $P/logs $P/shots $P/bin
  sed "s/__VERSION__/$1/" $D/stub-claude.sh > $P/home/.local/share/claude/versions/$1; chmod +x $P/home/.local/share/claude/versions/$1
  ln -s $P/home/.local/share/claude/versions/$1 $P/home/.local/bin/claude; print -r -- "$2" > $P/home/stub-mode
  ln -sf $(/usr/bin/which -a node | head -1) $P/bin/node
  local h=$P/home pth=$P/home/.local/bin:$P/bin:/usr/bin:/bin:/usr/sbin:/sbin
  [[ -n ${REALHOME:-} ]] && { h=$HOME; pth=$HOME/.local/bin:$P/bin:/usr/bin:/bin:/usr/sbin:/sbin; }
  local rd=(); [[ -n $RDIR ]] && rd=(--renderer-dir $RDIR)
  (cd $R; exec env -i HOME=$h USER=$USER LOGNAME=$USER TMPDIR=$TMPDIR LANG=en_US.UTF-8 PATH=$pth \
    WORKASS_PROFILE=dev WORKASS_DATA_ROOT=$P/data WORKASS_LOG_ROOT=$P/logs WORKASS_BROWSER_CONTROL_FILE=$P/bc.json WORKASS_MOCK_ACP_HOLD_FILE=$P/release-hold \
    $BIN --headless --state-dir $P/state --port $DP --bind localhost $rd >>$P/logs/daemon.log 2>&1) &
  DPID=$!
  for i in $(seq 1 80); do curl -ks --max-time 1 https://127.0.0.1:$DP/workass/health >/dev/null && break; sleep 0.5; done
  env -u WORKASS_TOOL_CONTEXT RAIL_URL=https://127.0.0.1:$DP/ RAIL_SHOTS=$P/shots RAIL_PORT=$EP electron --user-data-dir=$P/e $H/rail-harness.cjs >>$P/logs/electron.log 2>&1 &
  EPID=$!
  for i in $(seq 1 60); do curl -s --max-time 1 -X POST --data-binary "1" http://127.0.0.1:$EP/eval >/dev/null && break; sleep 0.5; done
  sleep 3; rail "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null
}
wire() { NODE_NO_WARNINGS=1 node $D/replay.mjs $DP 2>/dev/null; }
offer() { wire | python3 -I -c "
import sys,json
for l in sys.stdin:
    m=json.loads(l)
    if m['channel']=='providers:updates':
        for u in m['payload']['updates']:
            if u['providerId']=='claude': print('installed',u['installed'],'latest',u['latest'],'updateAvailable',u['updateAvailable'],'lastError',u.get('lastError','-'),'exit',u.get('exitCode','-'))
"; }
wait_claude() { for i in $(seq 1 90); do o=$(offer); [[ -n $o ]] && { print -r -- "$o"; return 0; }; sleep 1; done; echo "NO CLAUDE ROW"; sed -n '1,3p' $P/logs/daemon.log | cut -c1-200; return 1; }
card() { rail "(()=>{const c=[...document.querySelectorAll('.updslot .updcard')].pop();return c?c.innerText.replace(/\\n+/g,' | '):'(no card)'})()"; }
click_update() { rail "(()=>{const c=[...document.querySelectorAll('.updslot .updcard')].pop();if(!c)return 'no card';c.click();return 'clicked'})()"; }
wait_terminal() { for i in $(seq 1 ${1:-80}); do t=$(card); case "$t" in *error*|'(no card)') print -r -- "$t"; return 0;; esac; sleep 0.5; done; echo "still: $t"; }
shot() { curl -s "http://127.0.0.1:$EP/shot?name=$1" >/dev/null; }
tool() { printf '%s' "$2" | $BIN tools --context $P/ctx.json call "$1"; }
mockchat() {
  rail "document.querySelector('button.sv2-sq[title^=\"Nueva conversación\"]')?.click()" >/dev/null; sleep 2
  for i in $(seq 1 20); do
    case "$(rail "(document.querySelector('.modelsel')?.textContent||'')")" in *Mock*) break;; esac
    rail "document.querySelector('.mprow')?1:document.querySelector('.modelsel')?.click()" >/dev/null; sleep 1
    rail "[...document.querySelectorAll('.mprow')].find(e=>/Mock/.test(e.textContent))?.querySelector('.mpchoose')?.click()" >/dev/null; sleep 1
  done
  rm -f $P/release-hold
  rail "(()=>{const a=document.querySelector('.comp textarea');const s=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;s.call(a,'[mock:hold-tools]');a.dispatchEvent(new Event('input',{bubbles:true}));return 1})()" >/dev/null; sleep 1
  rail "(()=>{const b=[...document.querySelectorAll('.comp button')].pop();b&&b.click();return 1})()" >/dev/null
  for i in $(seq 1 30); do for d in $(ls -td $P/state/tool-bootstrap/*/ 2>/dev/null); do case "$(python3 -I -c "import json;print(json.load(open('${d}context.json')).get('chat_id',''))" 2>/dev/null)" in subagent:*|'') ;; *) cp ${d}context.json $P/ctx.json; chmod 600 $P/ctx.json; return 0;; esac; done; sleep 1; done
  echo "NO CONTEXT"; return 1
}
spawn() { tool workass_spawn_subagent "{\"operation_id\":\"sp-$1\",\"label\":\"$1\",\"task\":\"[mock:hold-tools]\",\"provider_id\":\"mock\",\"effort\":\"low\",\"permission_intent\":\"full\",\"cwd\":\"/tmp\"}" | python3 -I -c "import sys,json;print(json.load(sys.stdin).get('id',''))"; }
own() { TAB=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['tab_id'])"); CHAT=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['chat_id'])"); }
states() { own; tool workass_list_subagents "{\"tab_id\":\"$TAB\",\"chat_id\":\"$CHAT\"}" | python3 -I -c "
import sys,json
d=json.load(sys.stdin)
for s in (d.get('subagents') or d.get('runs') or d.get('items') or []): print(' ',s.get('label'),'|',s.get('status'),'|',(s.get('result') or s.get('error') or '')[:60],'|',s.get('latestActivity'))"; }

