source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f4
echo "### FIX-2 parent Stop with a local and a remote child side by side $(date -u +%T)"; ready; $P/turn
T='Run these two shell commands one at a time, in order: `sleep 45`, then `date`. Then reply with exactly: PS-DONE'
$P/spawn $LBL-r-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-60; echo; $P/spawn $LBL-l-$RANDOM "$LBL local" "$T" gpt-5.6-luna | cut -c1-60; echo
for n in $(seq 1 20); do [ -n "$(hs)" ] && [ -n "$(ls_local_sleep)" ] && break; sleep 2; done; $P/rows | grep "$LBL " | cut -c1-150; cards | mine; echo "host:"; hs; echo "mac:"; ls_local_sleep; $P/rail shot fix2-before >/dev/null
echo "=== STOP parent $(date -u +%T)"; stopbtn; sleep 5; $P/rows | grep "$LBL " | cut -c1-150; cards | mine; echo "host:"; hs; echo "mac:"; ls_local_sleep; $P/rail shot fix2-after-stop >/dev/null
wait_end 25; sleep 3; echo "-- both ended, no reload"; $P/rows | grep "$LBL " | cut -c1-210; cards | mine; $P/rail shot fix2-end >/dev/null
sleep 40; echo "-- 40s later, no reload"; cards | mine; $P/turn; sleep 3; echo "-- notices"; $P/notices | grep "$LBL "; echo "-- count $($P/notices | grep -c "$LBL ")"; echo "-- stored"; rawcard; stopbtn; sleep 3
