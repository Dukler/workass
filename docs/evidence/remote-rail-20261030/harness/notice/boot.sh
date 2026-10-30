source $P/lib.sh
source $P/notice-lib.sh
exec > >(tee -a $P/boot.log) 2>&1
echo "M=$M"; link; mockmodel; hold_on; $P/rail js "document.querySelector('.modelsel')?.textContent"; echo
$P/rail js "window.api.machinesList?window.api.machinesList().then(x=>JSON.stringify(x).slice(0,400)):'none'"; echo
child probe-r remote 2; child probe-l local 2; wait_settled probe-r; echo "r=$SETTLE_STATUS $(now)"; wait_settled probe-l; echo "l=$SETTLE_STATUS $(now)"; busy && echo turn-still-held; $P/rows | cut -c1-200; $P/rail shot boot >/dev/null; release; wait_idle; echo idle $(now); sleep 2; tally probe-r probe-l
