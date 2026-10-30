#!/bin/zsh
# Cases 1, 3, 4, 2a, 2b, then 2c as far as taking the owner down.
P=${0:a:h}; M=$1
id() { python3 -I -c "import sys,json;print(json.loads(sys.stdin.read())['id'])"; }
curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 9
$P/rail js "document.querySelector('.rail')?1:document.querySelector('.tico[title=\"Mostrar/ocultar panel\"]').click()" >/dev/null
$P/turn
#T1 parent-turn check is run after case 2c
T1='Run these three shell commands one at a time, in order: `ls /tmp | head -3`, then `sleep 25`, then `date`. Then reply with exactly: PROOF-DONE'
echo "### CASE 1,3,4 $(date -u +%T)"
R1=$($P/spawn r1-remote-00001 'c1 remoto' "$T1" gpt-5.6-luna $M | id); L1=$($P/spawn r1-local-000001 'c1 local' "$T1" gpt-5.6-luna | id)
R3=$($P/spawn r3-nomodel-0001 'c3 sin modelo' 'Run `date` then reply with exactly: NOMODEL-OK' - $M | id); R4=$($P/spawn r4-stop-0000001 'c4 detener' 'Run `sleep 300` and then reply DONE' gpt-5.6-luna $M | id)
echo "R1=$R1 L1=$L1 R3=$R3 R4=$R4"
for i in 1 2 3; do sleep 6; echo "-- t$i"; $P/rail shot case1-t$i >/dev/null; $P/rows | grep -E "c1 |c3|c4" | cut -c1-175; done
HS='for p in $(pgrep -x sleep); do grep -q "app.slice/workass-scratch.service" /proc/$p/cgroup && echo "scratch sleep $p $(tr "\0" " " </proc/$p/cmdline)"; done; true'
echo "-- c4 host before:"; ssh -o BatchMode=yes duklerver "$HS"
$P/rail js "(()=>{const b=document.querySelector('[data-work-id=\"$R4\"] .bgr-stop');if(!b)return 'no button';b.click();return 'clicked stop'})()"; echo
sleep 5; $P/rail shot case4-after-stop >/dev/null; echo "-- c4 host after:"; ssh -o BatchMode=yes duklerver "$HS"
sleep 18; $P/rail shot case1-end >/dev/null; $P/rows | grep -E "c1 |c3|c4" | cut -c1-230
echo "### CASE 2a"; $P/spawn r2a-badmodel-01 'c2a modelo malo' 'reply OK' gpt-6.1-sol $M | cut -c1-200; echo
echo "### CASE 2b"; R2B=$($P/spawn r2b-dies-000001 'c2b muere' 'Run `sleep 120` then reply DONE' gpt-5.6-luna $M | id); sleep 6; $P/rail shot case2b-running >/dev/null
ssh -o BatchMode=yes duklerver 'MP=$(systemctl --user show -p MainPID --value workass-scratch.service); for c in $(pgrep -P $MP); do kill -9 $c; done; echo "killed the provider of the scratch daemon"'
T0=$(date +%s); for i in $(seq 1 15); do sleep 1; S=$($P/rows | grep 'c2b muere' | grep -v running | cut -c1-260); [ -n "$S" ] && { echo "settled after $(( $(date +%s)-T0 ))s: $S"; break; }; done; $P/rail shot case2b-failed >/dev/null
ssh -o BatchMode=yes duklerver 'for p in $(pgrep -x sleep); do grep -q "app.slice/workass-scratch.service" /proc/$p/cgroup && kill $p; done; true'
echo "### CASE 2c"; R2C=$($P/spawn r2c-linkdown-01 'c2c enlace caido' 'Run `sleep 20` then reply with exactly: LINKDOWN-OK' gpt-5.6-luna $M | id); echo "$R1 $L1 $R3 $R4 $R2B $R2C" > $P/ids
sleep 6; $P/rows | grep c2c | cut -c1-140; $P/rail shot case2c-running >/dev/null
pkill -f "workass-mac --headless --state-dir"; echo "owner down $(date -u +%T)"; sleep 35; ssh -o BatchMode=yes duklerver "cut -c1-140 /data/workass-scratch/state/fleet-logs/*$R2C*"
