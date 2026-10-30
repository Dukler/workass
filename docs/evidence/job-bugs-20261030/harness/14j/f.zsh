source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh; source $P/rig2.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL local bursts $TAG $(now) tree=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD)+fix"
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
for r in 1 2 3 4; do burst_local $TAG$r- 24; compare $TAG$r-; release; wait_idle 400; hold_on; done
$P/notices > $P/.n; echo "NOTICES last round: $(grep -c "\"$TAG"4- $P/.n) lines for 24 jobs; statuses: $(grep "\"$TAG"4- $P/.n | cut -d' ' -f2 | sort | uniq -c | tr '\n' ' ')"
release; echo "### end $(now)"
