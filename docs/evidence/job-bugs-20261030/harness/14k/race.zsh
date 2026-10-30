#!/bin/zsh
# race.zsh BIN ROUNDS N ROOT: N `workass run` supervisors at once on a fresh outbox, ROUNDS times.
# Every job names --state-dir; each round gets a new state directory, removed after it is counted.
BIN=$1; ROUNDS=$2; N=$3; ROOT=$4
mkdir -p $ROOT/errs
total=0 refused=0 ran=0 bad_rounds=0
start=$(date +%s)
for r in $(seq 1 $ROUNDS); do
  d=$(mktemp -d $ROOT/r.XXXXXX); pids=()
  for i in $(seq 1 $N); do
    env -i PATH=/usr/bin:/bin HOME=$HOME $BIN run --unreported --quiet --state-dir $d/state --label j$i -- /usr/bin/true >$d/o$i 2>$d/e$i &
    pids+=($!)
  done
  rc=0; i=0; b=0
  for p in $pids; do i=$((i+1)); wait $p; c=$?; total=$((total+1))
    if [[ $c -ne 0 ]] || ! grep -q '"exitCode":0' $d/o$i; then refused=$((refused+1)); b=1; grep -v 'NO CHAT' $d/e$i >> $ROOT/errs/all.txt; echo "--exit=$c" >> $ROOT/errs/all.txt; else ran=$((ran+1)); fi
  done
  bad_rounds=$((bad_rounds+b))
  rm -rf $d
done
echo "RESULT bin=${BIN:t} rounds=$ROUNDS n=$N jobs=$total ran=$ran refused=$refused bad_rounds=$bad_rounds secs=$(( $(date +%s)-start ))"
sed -E 's#/[^ ]*process-outbox.db#OUTBOX#' $ROOT/errs/all.txt 2>/dev/null | grep -v '^--exit' | cut -c1-170 | sort | uniq -c | sort -rn | head -12
