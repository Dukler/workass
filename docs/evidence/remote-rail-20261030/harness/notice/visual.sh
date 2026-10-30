# The picture of it: eight remote children, the rail open, what a person sees.
source $P/lib.sh
source $P/notice-lib.sh
exec > >(tee -a $P/$TAG-visual.log) 2>&1
L=$TAG-v; echo "### $TAG visual $(now)"; mesh_up; mockmodel
$P/rail js "document.querySelector('.rail')?1:document.querySelector('.tico[title=\"Mostrar/ocultar panel\"]')?.click()" >/dev/null; sleep 1
WHERE=remote; hold_on; for n in 1 2 3 4 5 6 7 8; do child $L$n remote 2; done
for n in 1 2 3 4 5 6 7 8; do wait_settled $L$n; done; sleep 1; $P/rail shot notice-$TAG-rail-settled >/dev/null
release; wait_idle; sleep 2
$P/rail js "JSON.stringify([...document.querySelectorAll('.note')].map(e=>e.textContent.trim().slice(0,50)).filter(t=>t.includes('$L')))"; echo
$P/rail js "(()=>{const a=[...document.querySelectorAll('.note')].filter(e=>e.textContent.includes('$L'));a.length&&a[0].scrollIntoView({block:'start'});return a.length})()"; echo; sleep 1
$P/rail shot notice-$TAG-transcript >/dev/null
tally ${L}1 ${L}2 ${L}3 ${L}4 ${L}5 ${L}6 ${L}7 ${L}8
