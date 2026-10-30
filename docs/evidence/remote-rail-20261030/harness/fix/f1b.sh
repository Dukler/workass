source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f1b
echo "### FIX-1b restart, then a NEW turn is running when the child ends $(date -u +%T)"; ready; $P/turn
T='Run these two shell commands one at a time, in order: `sleep 50`, then `date`. Then reply with exactly: RS-DONE'
$P/spawn $LBL-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-60; echo; sleep 14; cards | mine; hs
owner_bounce; $P/turn; echo "-- new turn running"; cards | mine
wait_end; sleep 2; cards | mine; $P/rail shot fix1b-end >/dev/null
stopbtn; sleep 3; curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; echo "-- after reload"; cards | mine
echo "-- stored"; rawcard
