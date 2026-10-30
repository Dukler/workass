source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh; source $P/status.zsh
exec > >(tee -a $P/baseline.log) 2>&1
echo "### BASELINE hosted bursts + status $(now) head=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD), binaries from before the fix"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
status_case bs
release; sleep 100; hold_on
burst_host bh 12; compare bh-
release; echo "### end $(now)"
