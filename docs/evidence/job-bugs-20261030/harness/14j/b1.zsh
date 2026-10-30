source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE orphaned bursts $(now) head=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD) dirty=$(git -C /Users/dukler/Workspace/workass status --short | wc -l)"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
for r in 1 2 3 4 5 6 7 8; do
  burst_local bl$r- 24; compare bl$r-
  release; wait_idle 400; hold_on
done
for r in 1 2 3; do burst_host bh$r- 12; compare bh$r-; release; wait_idle 400; hold_on; done
compare bl; compare bh
release; wait_idle 400; echo "### end $(now)"
