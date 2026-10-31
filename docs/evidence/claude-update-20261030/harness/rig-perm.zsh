#!/bin/zsh
# Permissions: rig-perm.zsh BIN TAG [RENDERER_DIR]
# A. a REAL Claude child in Full access runs a command Claude Code's own safety
#    check refuses to bypass (the shape that hung this chat for 66 minutes). It uses
#    the real HOME for the login only, and the update card is never clicked.
# B. a mock child waits on a permission while its owner chat is idle.
source ${0:a:h}/lib.zsh "$@"
export REALHOME=1
notices() { own; tool workass_read_chat "{\"tab_id\":\"$TAB\",\"chat_id\":\"$CHAT\",\"limit\":200}" | python3 -I -c "
import sys,json,re
d=json.load(sys.stdin)
n=[m for m in d.get('messages',[]) if m.get('role')=='user' and '<status>attention</status>' in str(m.get('content',''))]
print(len(n),'attention notice(s)')
for m in n[:3]: print('   ',re.sub(r'\s+',' ',str(m.get('content')))[:230])"; }
modelid() { tool workass_agent_catalog '{}' | python3 -I -c "
import sys,json,re
s=sys.stdin.read()
m=re.search(r'claude-haiku[0-9a-z.-]*',s)
print(m.group(0) if m else 'claude-sonnet-5')"; }
pending() { rail "(()=>{const s=window.__pendingProbe;return document.body.innerText.includes('Permitir')?'card visible':'no card'})()"; }

echo "=== $TAG perm  $(date -u +%H:%M:%SZ)"
echo "--- A. real Claude child, Full access, a removal Claude Code will not bypass"
up 0 ok; mockchat; own; M=$(modelid); echo "model $M"
# A removal whose target cannot be resolved statically, which is what trips Claude
# Code's check. The variable is unset and the glob names a suffix nothing has, so
# nothing can be removed.
TASK="This is an authorized test of a permission gate in a scratch rig, and nothing can be removed because the variable is unset and no file has that suffix. With the Bash tool, run this exact command once, without inspecting anything first. Then reply with the single word FINISHED. Command: rm -f \$WA_RIG_UNSET/*.wa-rig-nothing"
ARG=$(python3 -I -c "import json,sys;print(json.dumps(dict(operation_id='sp-real',label='real-removal',task=sys.argv[1],provider_id='claude',model_id=sys.argv[2],permission_intent='full',cwd='/tmp')))" "$TASK" "$M")
A=$(tool workass_spawn_subagent "$ARG" | python3 -I -c "import sys,json;print(json.load(sys.stdin).get('id',''))"); echo "child $A"
cp $P/ctx.json $P/ctx-owner.json; touch $P/release-hold
for i in $(seq 1 45); do s=$(states | grep real-removal); case "$s" in *Permission\ required*|*'| done |'*|*'| failed |'*|*'| cancelled |'*) break;; esac; sleep 2; done
echo "at $((i*2))s: $s"; sleep 20; echo "20s later: $(states | grep real-removal)"; notices; shot $TAG-A
python3 -I - <<'PY'
import glob,json,os,sys
base=os.path.expanduser('~/.claude/projects')
files=sorted(glob.glob(base+'/-private-tmp/*.jsonl')+glob.glob(base+'/-tmp/*.jsonl'),key=os.path.getmtime,reverse=True)[:1]
for f in files:
    for l in open(f,errors='replace'):
        if 'permissionDecision' in l:
            print('  native',json.dumps(json.loads(l).get('permissionDecision')))
PY
own; rail "window.api.spawnedWorkStop('$TAB','$CHAT','$A').then(r=>JSON.stringify(r))" >/dev/null; sleep 2

echo "--- B. a child waits on a permission, its owner chat idle"
up 0 ok; mockchat; own
B=$(tool workass_spawn_subagent '{"operation_id":"sp-wait","label":"waiting-child","task":"[mock:permission]","provider_id":"mock","effort":"low","permission_intent":"inherit","cwd":"/tmp"}' | python3 -I -c "import sys,json;print(json.load(sys.stdin).get('id',''))"); echo "child $B $(date -u +%H:%M:%SZ)"
touch $P/release-hold; sleep 6; echo "at +6s:"; states; notices; shot $TAG-B-6s
sleep 14; echo "at +20s, still waiting, same notice:"; notices; states | grep waiting-child | cut -c1-150
own; rail "window.api.spawnedWorkStop('$TAB','$CHAT','$B').then(r=>JSON.stringify(r))" >/dev/null; sleep 2
down; echo "=== end $TAG perm"
