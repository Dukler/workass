source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL install over the old build $(now). Owner and host both unmodified 6bbb68a2."
read -r -A ALL < $P/.realchats; C=($ALL[1] $ALL[2])
renderer_key; mesh_up; sleep 2; mockmodel; hold_on
echo "old jobs from the first baseline, after every fixed boot so far:"; count_notices $C[1] $C[2] rb-claude-fg; count_notices $C[1] $C[2] rb-claude-bg
R() { WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac-base run --state-dir $P/mac-state "$@" >/dev/null 2>>$P/f4-jobs.err; }
echo "-- old build: 12 jobs at once, rows 5s after they end"
for n in 1 2 3 4 5 6 7 8 9 10 11 12; do R --label ob-burst$n -- sleep 2 & done; sleep 7; $P/rows | grep ob-burst | python3 -I -c "
import sys,ast,collections
print('OLD BUILD ROW STATUSES',collections.Counter(ast.literal_eval(l)['status'] for l in sys.stdin))"
R --label up-ended -- sleep 1 & R --label up-running -- sleep 35 & sleep 4
monitors_all | grep -E "up-(ended|running)"
echo "-- replace the owner with the fixed build while up-running runs"; owner_down; rm -f $P/workass-mac; cp $P/workass-mac-fix $P/workass-mac; owner_up; renderer_key; sleep 3; mesh_up; sleep 2; hold_on
monitors_all | grep -E "up-(ended|running)"
sleep 36; release; wait_idle 200; hold_on; $P/notices > $P/.n; for l in up-ended up-running ob-burst1; do echo "NOTICES|$l|$(grep -c "\"$l\"" $P/.n)"; done
monitors_all | grep -E "up-(ended|running)"
echo "== fixed Mac, host still on the old build"
W=$(host_job mx-job 20); sleep 3; echo -n "STATUS asked from the Mac: "; hstatus $M; sleep 19; release; wait_idle 200; hold_on; $P/notices | grep -c '"mx-job"'
R2() { WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state "$@" >/dev/null 2>>$P/f4-jobs.err; }
echo "-- fixed build: 12 jobs at once, rows 5s after they end"
for n in 1 2 3 4 5 6 7 8 9 10 11 12; do R2 --label nb-burst$n -- sleep 2 & done; sleep 7; $P/rows | grep nb-burst | python3 -I -c "
import sys,ast,collections
print('FIXED BUILD ROW STATUSES',collections.Counter(ast.literal_eval(l)['status'] for l in sys.stdin))"
release; wait_idle 200; hold_on; $P/notices > $P/.n; n=0; for i in 1 2 3 4 5 6 7 8 9 10 11 12; do n=$((n+$(grep -c "\"nb-burst$i\"" $P/.n))); done; echo "fixed-build burst notices: $n of 12"; release
