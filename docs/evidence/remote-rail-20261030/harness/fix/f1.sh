source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f5
echo "### FIX-1 owner restart, child succeeds $(date -u +%T)"; ready; $P/turn
T='Run these two shell commands one at a time, in order: `sleep 60`, then `date`. Then reply with exactly: RS-DONE'
$P/spawn $LBL-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-60; echo
sleep 16; $P/rows | grep "$LBL " | cut -c1-175; cards | mine; hs; $P/rail shot fix1-before >/dev/null
owner_bounce; echo "-- after restart"; $P/rows | grep "$LBL " | cut -c1-175; cards | mine; hs; $P/rail shot fix1-after-restart >/dev/null
wait_end; sleep 40; echo "-- child ended, 40s later, no reload"; cards | mine; $P/rail shot fix1-end >/dev/null
curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; echo "-- after reload"; cards | mine; $P/rail shot fix1-after-reload >/dev/null
owner_bounce; echo "-- after a second restart"; cards | mine; $P/rows | grep "$LBL " | cut -c1-120; $P/rail shot fix1-after-second-restart >/dev/null
$P/turn; sleep 2; echo "-- notices"; $P/notices | grep "$LBL "; echo "-- count $($P/notices | grep -c "$LBL ")"; stopbtn; sleep 2
