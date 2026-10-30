source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f7
echo "### FIX-2b parent Stop, THEN owner restart, then the remote child ends $(date -u +%T)"; ready; $P/turn
T='Run these two shell commands one at a time, in order: `sleep 50`, then `date`. Then reply with exactly: PS-DONE'
$P/spawn $LBL-r-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-60; echo; sleep 13; hs; stopbtn; sleep 3; cards | mine
owner_bounce; cards | mine; wait_end; sleep 2; cards | mine; $P/rail shot fix2b-end >/dev/null
$P/turn; sleep 3; echo "-- count $($P/notices | grep -c "$LBL ")"; echo "-- stored"; rawcard; stopbtn; sleep 3
