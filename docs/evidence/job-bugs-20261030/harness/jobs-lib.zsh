# Real chats and counting, on top of the notice rig. Every job names --state-dir.
pyj() { python3 -I -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
mk_chat() { # mk_chat PROVIDER MODEL EFFORT TITLE -> "TAB CHAT"
  $P/tool workass_create_chat "{\"operation_id\":\"mk-$4-$RANDOM\",\"title\":\"$4\",\"provider_id\":\"$1\",\"model_id\":\"$2\",\"effort\":\"$3\",\"permission_intent\":\"full\",\"cwd\":\"/tmp\"}" > $P/.mk.json
  python3 -I -c "import json;d=json.load(open('$P/.mk.json'));print(d.get('tabId') or d.get('tab_id') or d.get('chat',{}).get('tabId'), d.get('chatId') or d.get('chat_id') or d.get('chat',{}).get('chatId'))" ; }
send_chat() { # send_chat TAB CHAT TEXT
  python3 -I -c "import json,sys,random;print(json.dumps(dict(tab_id=sys.argv[1],chat_id=sys.argv[2],message=sys.argv[3],operation_id='s-'+str(random.getrandbits(40)))))" "$1" "$2" "$3" > $P/.send.json
  $P/tool workass_send_chat_message "$(cat $P/.send.json)" | cut -c1-160; echo; }
read_chat() { $P/tool workass_read_chat "{\"tab_id\":\"$1\",\"chat_id\":\"$2\",\"limit\":200}"; }
# count_notices TAB CHAT LABEL: role=user task-notifications naming LABEL, with the text a person reads.
count_notices() { read_chat $1 $2 > $P/.read.json; python3 -I - "$3" <<PY
import json,re,sys
d=json.load(open('$P/.read.json')); n=0
for m in d.get('messages',[]):
    c=str(m.get('content') or m.get('text') or '')
    if m.get('role')=='user' and '<task-notification>' in c and sys.argv[1] in c:
        n+=1; s=re.search('<summary>(.*?)</summary>',c); st=re.search('<status>(.*?)</status>',c); print('  notice',st and st.group(1),'|',s and s.group(1))
print('NOTICES|%s|%d'%(sys.argv[1],n))
PY
}
monitors_all() { python3 -I - <<PY
import json,os
for name in ('held','watched'):
    f='$P/mac-state/fleet-monitors/%s.json'%name
    if not os.path.exists(f): print(name,'absent'); continue
    d=json.load(open(f)); 
    for m in d['monitors']: print(name,m.get('label'),m['targetId'][:60],'applied',m.get('appliedSeq'),'acked',m.get('ackedSeq'),'consumed',m.get('consumed'))
PY
}
turn_idle() { # wait until a chat has no foreground turn
  for i in $(seq 1 ${3:-240}); do read_chat $1 $2 > $P/.read.json; python3 -I -c "
import json,sys
d=json.load(open('$P/.read.json')); s=str(d.get('status') or d.get('state') or (d.get('chat') or {}).get('status') or ''); sys.exit(0 if s not in ('running','working','busy') else 1)" && return 0; sleep 1; done; echo "still busy"; }
hstatus() { $P/tool workass_get_update_status "{\"machine_id\":\"$1\"}" | python3 -I -c "import sys,json;d=json.load(sys.stdin);print(json.dumps(d.get('runningJobs',d),ensure_ascii=False)[:900])"; }
pick_model() { python3 -I - "$1" "$2" <<PY
import json,sys
d=json.load(open('$P/.cat.json'))
for g in d['groups']:
    if g['providerId']==sys.argv[1]:
        ms=[m['modelId'] for m in g['models']]
        print(next((m for m in ms if sys.argv[2] in m),ms[0] if ms else '')); break
PY
}
wait_reply() { # wait_reply TAB CHAT WORD SECONDS: the agent's own reply has arrived
  for i in $(seq 1 ${4:-150}); do read_chat $1 $2 > $P/.read.json; python3 -I -c "
import json,sys
d=json.load(open('$P/.read.json'))
sys.exit(0 if any(m.get('role')=='assistant' and sys.argv[1] in str(m.get('content') or m.get('text') or '') for m in d.get('messages',[])) else 1)" "$3" && { echo "reply $3 after ${i}s"; return 0; }; sleep 1; done; echo "NO REPLY $3"; python3 -I -c "
import json;d=json.load(open('$P/.read.json'));print(json.dumps(d)[:600])"; }
agent_job() { # agent_job TAB CHAT LABEL WORD: a real agent runs a job in the foreground, in its own shell
  send_chat $1 $2 "Run this exact shell command with your shell tool, wait for it to finish, and then reply with only the word $4: printf '%s %s\n' \"\$WORKASS_TOOLS_COMMAND\" \"\$WORKASS_TOOL_CONTEXT\" > $P/.shell-$3; \"\$WORKASS_TOOLS_COMMAND\" run --state-dir $P/mac-state --label $3 -- sleep 3"; }
agent_bg_job() { # the turn ends long before the job does
  send_chat $1 $2 "Run this exact shell command with your shell tool. It returns immediately. Then reply with only the word $4: printf '%s %s\n' \"\$WORKASS_TOOLS_COMMAND\" \"\$WORKASS_TOOL_CONTEXT\" > $P/.shell-$3; nohup \"\$WORKASS_TOOLS_COMMAND\" run --state-dir $P/mac-state --label $3 -- sleep $5 >/dev/null 2>&1 &"; }
host_job() { # host_job LABEL SECONDS [ENVJSON]: printed work id
  python3 -I -c "
import json,sys,random
d=dict(operation_id=sys.argv[1]+'-'+str(random.getrandbits(32)),label=sys.argv[1],cwd='/tmp',command=['sleep',sys.argv[2]],machine_id=sys.argv[3])
if len(sys.argv)>4 and sys.argv[4]: d['env']=json.loads(sys.argv[4])
print(json.dumps(d))" "$1" "$2" "$M" "$3" > $P/.job.json; $P/tool workass_start_process_child "$(cat $P/.job.json)" > $P/.jobr.json; python3 -I -c "
import json;d=json.load(open('$P/.jobr.json'));print(d.get('workId') or d.get('work_id') or d)"; }
lane_gone() { # lane_gone TARGET_SUBSTR: with the owner stopped, point that watched monitor at a lane the chat does not have
  python3 -I - "$1" <<PY
import json,sys
f='$P/mac-state/fleet-monitors/watched.json'; d=json.load(open(f)); n=0
for m in d['monitors']:
    if sys.argv[1] in m['targetId']: m['originLaneId']=sys.argv[2] if len(sys.argv)>2 else 'lane-00000000000000000000000000000000'; n+=1
json.dump(d,open(f,'w')); print('monitors pointed at a missing lane:',n)
PY
}
# The renderer enrols with its own stored key, not the daemon's. Rig key only.
renderer_key() { K=$(cat $P/.secret); $P/rail js "(()=>{const had=localStorage.getItem('workass.fleetKey')==='$K';localStorage.setItem('workass.fleetKey','$K');Object.keys(localStorage).filter(k=>/auto-connect-disabled/.test(k)).forEach(k=>localStorage.removeItem(k));return had})()" | grep -q true || { curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; }; $P/rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null; }
owner_down() { pkill -f "workass-mac --headless --state-dir"; for i in $(seq 1 40); do pgrep -f "workass-mac --headless --state-dir" >/dev/null || break; sleep 0.25; done; echo "owner down $(now)"; }
owner_up() { daemon; curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; $P/rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null; echo "owner up $(now)"; ready; }
monitor_of() { monitors_all | grep -- "$1" ; }
# The lane this chat would have on Codex, which it does not have yet. Same formula as provider.DeriveLaneID; checked against the chat's real lane first.
future_lane() { python3 -I - "$1" <<PY
import json,sys,hashlib
d=json.load(open('$P/mac-state/provider-lanes.json'))['bindings']
me=[b for b in d if b['chatId']==sys.argv[1]][0]; cx=[b for b in d if b['providerId']=='codex'][0]
def lane(chat,prov,mach,acct,inst,ep):
    h=hashlib.sha256()
    for p in (chat,prov,mach,acct,inst,ep):
        h.update(('%d:'%len(p)).encode()); h.update(p.encode())
    return 'lane-'+h.hexdigest()[:32]
assert lane(me['chatId'],me['providerId'],me['machineId'],me['accountScope'],me['installScope'],me['workspaceEpoch'])==me['laneId'],'formula'
print(lane(me['chatId'],'codex',me['machineId'],cx['accountScope'],cx['installScope'],me['workspaceEpoch']))
PY
}
set_lane() { python3 -I - "$1" "$2" <<PY
import json,sys
f='$P/mac-state/fleet-monitors/watched.json'; d=json.load(open(f)); n=0
for m in d['monitors']:
    if sys.argv[1] in m['targetId']: m['originLaneId']=sys.argv[2]; n+=1
json.dump(d,open(f,'w')); print('monitor lane set',n,sys.argv[2][:20])
PY
}
ctx_of() { for d in $(ls -td $P/mac-state/tool-bootstrap/*/); do [ "$(python3 -I -c "import json;print(json.load(open('${d}context.json')).get('chat_id',''))" 2>/dev/null)" = "$1" ] && { cp ${d}context.json $2; chmod 600 $2; return 0; }; done; echo "no ctx for $1"; return 1; }
job_id_of_label() { $P/tool workass_list_process_children '{}' | python3 -I -c "
import sys,json
for c in json.load(sys.stdin).get('children',[]):
    if c.get('label')=='$1': print(c.get('work_id'))"; }
newest_local() { ls -t $P/mac-state/fleet-logs 2>/dev/null | head -1; }
