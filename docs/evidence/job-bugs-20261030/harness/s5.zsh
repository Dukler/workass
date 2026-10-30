source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE refused delivery $(now) binary built at 6bbb68a2"
renderer_key; mesh_up; sleep 3; mockmodel; hold_on
W3=$(host_job rb2b-remote 40); echo "work $W3"; sleep 2; monitor_of $W3
owner_down; lane_gone $W3; owner_up; renderer_key
for i in $(seq 1 60); do grep -a "could not deliver" $P/mac-logs/daemon.log | grep -q . && break; sleep 1; done
grep -a "could not deliver\|held a" $P/mac-logs/daemon.log | tail -3 | cut -c1-220
monitor_of $W3
echo "second owner restart: is it offered again?"; owner_down; owner_up; renderer_key; sleep 8
hold_on; echo "notices for rb2b-remote: $($P/notices | grep -c rb2b-remote)"; monitor_of $W3; grep -a "could not deliver" $P/mac-logs/daemon.log | wc -l; release
