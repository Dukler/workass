source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f6
echo "### FIX-1c restart, then the child really fails $(date -u +%T)"; ready; $P/turn
T='Run the shell command `sleep 120` and then reply DONE'
$P/spawn $LBL-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-60; echo; sleep 14; cards | mine; hs
owner_bounce; cards | mine
ssh -o BatchMode=yes duklerver 'MP=$(systemctl --user show -p MainPID --value workass-scratch.service); for c in $(pgrep -P $MP); do kill -9 $c; done; echo killed the provider of the scratch daemon; for p in $(pgrep -x sleep); do grep -q "app.slice/workass-scratch.service" /proc/$p/cgroup && kill $p; done; true'
wait_end 5; sleep 2; echo "-- end"; cards | mine; $P/rail shot fix1c-end >/dev/null
curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; echo "-- after reload"; cards | mine
echo "-- stored"; rawcard; $P/turn; sleep 2; echo "-- notices"; $P/notices | grep "$LBL " | cut -c1-200; stopbtn; sleep 2
