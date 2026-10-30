source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL status $(now) tree=$(git -C /Users/dukler/Workspace/workass rev-parse --short HEAD)+fix"
renderer_key; mesh_up; sleep 3; mockmodel; hold_on
echo -n "STATUS, nothing running: "; hstatus $M
W1=$(host_job f3-protected 40); W2=$(host_job f3-exposed 40 '{"WORKASS_RUN_SCOPED":"1"}'); W3=$(host_job f3-protected-2 40); echo "work $W1 $W2 $W3"; sleep 5
echo "scopes (protected, exposed, protected): $(ssh -o BatchMode=yes duklerver "systemctl --user is-active workass-job-$W1.scope; systemctl --user is-active workass-job-$W2.scope; systemctl --user is-active workass-job-$W3.scope" | tr '\n' ' ')"
echo -n "STATUS asked from the Mac: "; hstatus $M
hstatus $M > /dev/null; sleep 42
echo -n "STATUS after they ended: "; hstatus $M
echo "notices: $($P/notices | grep -c 'f3-')"

release; wait_idle; hold_on; echo "notices after the turn: $($P/notices | grep -c f3-)"; $P/notices | grep f3- | cut -c1-100; release
