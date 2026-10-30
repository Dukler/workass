# Moments for one child's end against its parent turn. The owner turn is the mock
# held open by [mock:hold-tools]; touching $HOLD ends it NATURALLY (Stop would
# suppress the notice by law). Times are UTC with milliseconds.
HOLD=$P/release-hold
M=$(ssh -o BatchMode=yes duklerver "python3 -I -c \"import json;print(json.load(open('/data/workass-scratch/state/machine-id.json'))['machineId'])\"")
now() { python3 -I -c "import datetime;print(datetime.datetime.utcnow().strftime('%H:%M:%S.%f')[:-3])"; }
# turn takes the newest tool context, which can be a local child's: that one has no spawn tool ("unknown Workass tool"). Take the newest that is the chat's.
own_ctx() { for d in $(ls -td $P/mac-state/tool-bootstrap/*/); do case "$(python3 -I -c "import json;print(json.load(open('${d}context.json')).get('chat_id',''))" 2>/dev/null)" in subagent:*|'') ;; *) cp ${d}context.json $P/ctx.json; chmod 600 $P/ctx.json; return 0;; esac; done; }
hold_on() { rm -f $HOLD; $P/turn; own_ctx; }
release() { touch $HOLD; }
busy() { case "$($P/rail js "[...document.querySelectorAll('.comp button')].pop().className")" in *stop*) return 0;; *) return 1;; esac; }
wait_idle() { for i in $(seq 1 ${1:-120}); do busy || return 0; sleep 0.25; done; echo "still busy"; return 1; }
# spawnedWorkList for this chat, one label's status; empty when the row is gone.
row_status() { TAB=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['tab_id'])"); CHAT=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['chat_id'])"); $P/rail js "window.api.spawnedWorkList('$TAB','$CHAT').then(r=>((r.items||r).find(i=>i.label==='$1')||{}).status||'')" | python3 -I -c "import sys,json;print(json.loads(sys.stdin.read()).get('value',''))"; }
wait_settled() { seen=; for i in $(seq 1 ${2:-900}); do s=$(row_status "$1"); [ "$s" = running ] && seen=1; { [ -n "$s" ] && [ "$s" != running ]; } && { SETTLE_STATUS=$s; return 0; }; [ -z "$s" ] && [ -n "$seen" ] && { SETTLE_STATUS=gone; return 0; }; sleep 0.05; done; SETTLE_STATUS=timeout; return 1; }
child() { # child LABEL WHERE SECONDS
  T="Run this shell command: \`sleep $3; date\`. Then reply with exactly: N-DONE"
  if [ "$2" = remote ]; then $P/spawn "$1-$RANDOM$RANDOM" "$1" "$T" gpt-5.6-luna $M; else $P/spawn "$1-$RANDOM$RANDOM" "$1" "$T" gpt-5.6-luna; fi | cut -c1-80; echo
}
host_sleep() { ssh -o BatchMode=yes duklerver 'for p in $(pgrep -x sleep); do grep -q "app.slice/workass-scratch.service" /proc/$p/cgroup && echo $p; done; true'; }
local_sleep() { pgrep -f "^sleep $1\$" || true; }
line() { echo "RESULT|$1|$2|$3|settled=$4|turn_at_settle=$5|released_gap=$6"; }
# The child ends while the turn runs, and the turn runs on for 6 more seconds.
mid() { hold_on; child "$1" "$2" 2; wait_settled "$1"; t=$(now); b=running; busy || b=IDLE; sleep 6; busy || b="$b,ended-early"; release; line "$1" "$2" mid "$t" "$b" 6s; wait_idle; }
# The child ends and the turn is released on the very next poll.
last() { hold_on; child "$1" "$2" 2; wait_settled "$1"; t=$(now); b=running; busy || b=IDLE; release; r=$(now); line "$1" "$2" last "$t" "$b" "$r"; wait_idle; }
# The turn ends long before the child: nothing is running when it ends.
none() { hold_on; child "$1" "$2" 14; release; sleep 1; wait_idle; e=$(now); wait_settled "$1"; t=$(now); b=IDLE; busy && b=running; line "$1" "$2" none "$t" "$b" "turn-ended-$e"; sleep 1; wait_idle; }
# The turn ends on the moment the child's command ends, seconds before its reply.
justafter() { hold_on; child "$1" "$2" 7; for i in $(seq 1 60); do if [ "$2" = remote ]; then [ -n "$(host_sleep)" ] && break; else [ -n "$(local_sleep 7)" ] && break; fi; sleep 0.3; done; for i in $(seq 1 100); do if [ "$2" = remote ]; then [ -z "$(host_sleep)" ] && break; else [ -z "$(local_sleep 7)" ] && break; fi; sleep 0.1; done; release; e=$(now); wait_settled "$1"; t=$(now); b=IDLE; busy && b=running; line "$1" "$2" just-after "$t" "$b" "released-$e"; sleep 1; wait_idle; }
tally() { rm -f $HOLD; $P/turn; own_ctx; echo "### tally $(now)"; $P/notices > $P/.notices; for l in "$@"; do c=$(grep -cE "(Agent|Job) \"$l\" " $P/.notices); echo "NOTICES|$l|$c"; done; cut -c1-110 $P/.notices; echo "### order"; order; echo "### monitors"; monitors; echo "### held lines"; grep -a 'held a\|could not deliver' $P/mac-logs/daemon.log | cut -c1-170; release; wait_idle; }
# A fresh owner state has no model chosen; the same click a person makes.
mockmodel() { case "$($P/rail js "(document.querySelector('.modelsel')?.textContent||'')")" in *Mock*) return 0;; esac; $P/rail js "document.querySelector('.modelsel')?.click()" >/dev/null; sleep 1; $P/rail js "[...document.querySelectorAll('.mprow')].find(e=>/Mock/.test(e.textContent))?.click()" >/dev/null; sleep 1; $P/rail js "(document.querySelector('.modelsel')?.textContent||'')"; echo; }
# The daemon's own link, not the renderer's: re-adding reads the host's fleet ids into the book.
mesh_up() { link; for i in $(seq 1 40); do $P/workass-mac fleet links --state-dir $P/mac-state | grep -q "$M" && { echo "mesh link up after ${i}s"; return 0; }; sleep 1; done; echo "NO MESH LINK"; $P/workass-mac fleet links --state-dir $P/mac-state | cut -c1-200; }
# The owner chat as stored, in order, so a notice's place against its parent turn can be read.
order() { TAB=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['tab_id'])"); CHAT=$(python3 -I -c "import json;print(json.load(open('$P/ctx.json'))['chat_id'])"); $P/tool workass_read_chat "{\"tab_id\":\"$TAB\",\"chat_id\":\"$CHAT\",\"limit\":200}" | python3 -I -c "
import sys,json,re
d=json.loads(sys.stdin.read())
for m in d.get('messages',[]):
    c=str(m.get('content') or m.get('text') or '')
    s=re.search('<summary>(.*?)</summary>',c)
    print(str(m.get('at') or m.get('createdAt') or m.get('timestamp') or '')[11:23],m.get('role'),(s.group(1) if s and m.get('role')=='user' else c[:30]).replace(chr(10),' '))
"; }
monitors() { python3 -I -c "
import json
d=json.load(open('$P/mac-state/fleet-monitors/watched.json'))
ms=d['monitors']; ms=ms if isinstance(ms,list) else list(ms.values())
for m in sorted(ms,key=lambda m:m.get('createdAt','')): print(m['createdAt'][11:19],m.get('label'),'appliedSeq',m.get('appliedSeq'),'consumed',m.get('consumed'))
"; }
# Four children ending inside one held turn: the only way to pass the cap quickly.
burst() { hold_on; for l in "$@"; do child "$l" "$WHERE" 2; done; for l in "$@"; do wait_settled "$l"; echo "RESULT|$l|$WHERE|burst|settled=$(now)|status=$SETTLE_STATUS"; done; sleep 3; release; wait_idle; }
# A real workass run, on the host or here. Same cap, same fix.
job() { # job LABEL remote|local
  python3 -I -c "
import json,sys,random
d=dict(operation_id=sys.argv[1]+'-'+str(random.getrandbits(32)),label=sys.argv[1],cwd='/tmp',command=['sleep','3'])
d['machine_id']=sys.argv[3]
print(json.dumps(d))" "$1" "$2" "$([ "$2" = remote ] && echo $M || python3 -I -c "import json;print(json.load(open('$P/mac-state/machine-id.json'))['machineId'])")" > $P/.job.json; $P/tool workass_start_process_child "$(cat $P/.job.json)" | cut -c1-120; echo; }
