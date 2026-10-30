# Eight real `workass run` jobs on this machine, past the old cap. TAG names the run.
# --state-dir always: a copied credential does not state one, and run would fall back to the INSTALLED daemon's state.
source $P/lib.sh
source $P/notice-lib.sh
exec > >(tee -a $P/$TAG-jobs.log) 2>&1
echo "### $TAG local jobs $(now)"; mesh_up; mockmodel; hold_on
for n in 1 2 3 4 5 6 7 8; do WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state --label $TAG-j$n -- sleep 2 >/dev/null 2>>$P/$TAG-jobs.err & done
sleep 10; release; wait_idle; tally $TAG-j1 $TAG-j2 $TAG-j3 $TAG-j4 $TAG-j5 $TAG-j6 $TAG-j7 $TAG-j8
