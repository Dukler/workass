#!/bin/bash
# race-linux.sh BIN ROUNDS N: as race.zsh, under /tmp only, low priority.
BIN=$1; ROUNDS=$2; N=$3; ROOT=/tmp/outbox-race-rig/run.$$; mkdir -p $ROOT; : > $ROOT/errs
total=0 refused=0 start=$(date +%s)
for r in $(seq 1 $ROUNDS); do
  d=$(mktemp -d $ROOT/r.XXXXXX); pids=()
  for i in $(seq 1 $N); do
    env -i PATH=/usr/bin:/bin HOME=/tmp/outbox-race-rig $BIN run --unreported --quiet --state-dir $d/state --label j$i -- /usr/bin/true >$d/o$i 2>$d/e$i & pids+=($!)
  done
  i=0
  for p in "${pids[@]}"; do i=$((i+1)); wait $p; c=$?; total=$((total+1))
    if [ $c -ne 0 ] || ! grep -q '"exitCode":0' $d/o$i; then refused=$((refused+1)); grep -v 'NO CHAT' $d/e$i >> $ROOT/errs; fi
  done
  rm -rf $d
done
echo "RESULT bin=$(basename $BIN) rounds=$ROUNDS n=$N jobs=$total refused=$refused secs=$(( $(date +%s)-start ))"
sed -E 's#/[^ ]*process-outbox.db#OUTBOX#' $ROOT/errs | cut -c1-170 | sort | uniq -c | sort -rn | head -6
rm -rf $ROOT
