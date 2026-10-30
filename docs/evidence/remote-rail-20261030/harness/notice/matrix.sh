# TAG names the run; SHOT prefixes the screenshots. Every case is one real child.
source $P/lib.sh
source $P/notice-lib.sh
exec > >(tee -a $P/$TAG.log) 2>&1
echo "### $TAG $(now) $($P/workass-mac --version 2>&1 | head -1)"; mesh_up; mockmodel
L=$TAG
mid $L-r-mid remote; last $L-r-last remote; none $L-r-none remote; justafter $L-r-after remote
mid $L-l-mid local; last $L-l-last local; none $L-l-none local; justafter $L-l-after local
$P/rail shot $L-after-moments >/dev/null
WHERE=remote burst $L-r5 $L-r6 $L-r7 $L-r8
WHERE=local burst $L-l5 $L-l6 $L-l7 $L-l8
sleep 3; $P/rail shot $L-after-burst >/dev/null
hold_on; job $L-job-r remote; job $L-job-l local; sleep 10; $P/rows | grep -E "$L-job" | cut -c1-150; release; wait_idle
tally $L-r-mid $L-r-last $L-r-none $L-r-after $L-l-mid $L-l-last $L-l-none $L-l-after $L-r5 $L-r6 $L-r7 $L-r8 $L-l5 $L-l6 $L-l7 $L-l8 $L-job-r $L-job-l
$P/rows | grep -E "'$L-r[78]'" | cut -c1-170
sleep 2; $P/rail shot $L-end >/dev/null
