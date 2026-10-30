source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE hosted bursts, started together $(now)"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
for r in 1 2 3 4; do burst_host_par bp$r- 12; compare bp$r-; release; wait_idle 280; hold_on; done
release; echo "### end $(now)"
