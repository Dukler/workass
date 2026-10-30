source $P/lib.sh
exec > >(tee -a $P/baseline.log) 2>&1
LBL=${LBL:-r3}
echo "### owner restart [$LBL] $(date -u +%T)"; ready; $P/turn
T='Run these two shell commands one at a time, in order: `sleep 60`, then `date`. Then reply with exactly: RS-DONE'
$P/spawn $LBL-r-$RANDOM "$LBL remoto" "$T" gpt-5.6-luna $M | cut -c1-90; echo
sleep 16; $P/rows | grep "$LBL " | cut -c1-170; cards; hs; $P/rail shot $LBL-before >/dev/null
pkill -f "workass-mac --headless --state-dir"; echo "owner down $(date -u +%T)"; sleep 3
daemon; curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6
$P/rail js "window.api.lanTakeControl().then(x=>x.controller)" >/dev/null
echo "-- after restart $(date -u +%T)"; $P/rows | grep "$LBL " | cut -c1-170; cards; $P/rail shot $LBL-after-restart >/dev/null
for n in 1 2 3 4 5 6 7 8 9 10; do sleep 8; S=$($P/rows | grep "$LBL " | cut -c1-170); echo "$(date -u +%T)"; echo "$S"; echo "$S" | grep -q running || break; done
sleep 4; echo "-- child ended"; cards; $P/rail shot $LBL-end >/dev/null
curl -s http://127.0.0.1:18992/reload >/dev/null; sleep 6; echo "-- after reload"; cards; $P/rail shot $LBL-after-reload >/dev/null
$P/turn; sleep 2; echo "-- notices"; $P/notices | grep "$LBL "; echo "-- notices end"; cards; stopbtn
