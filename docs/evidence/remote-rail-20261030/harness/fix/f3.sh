source $P/lib.sh
exec > >(tee -a $P/final.log) 2>&1
export LBL=f3
echo "### finished before stop [$LBL] $(date -u +%T)"; ready; $P/turn
T='Run the shell command `date`. Then reply with exactly: Q-DONE'
$P/spawn $LBL-r-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-90; echo; $P/spawn $LBL-l-$RANDOM "$LBL local" "$T" gpt-5.6-luna | cut -c1-90; echo
for n in $(seq 1 15); do sleep 3; S=$($P/rows | grep "$LBL "); echo "$S" | grep -q running || break; done; echo "$S" | cut -c1-150; sleep 3
$P/notices | grep "$LBL "; echo "=== STOP parent $(date -u +%T)"; stopbtn; sleep 10
$P/rail js "JSON.stringify(window.api?1:0)" >/dev/null
$P/turn; sleep 2; echo "-- notices"; $P/notices | grep "$LBL "; echo "-- notices end"; cards | mine; stopbtn; $P/rail shot fix3-end >/dev/null
