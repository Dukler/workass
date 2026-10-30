source $P/lib.sh; source $P/notice-lib.sh; source $P/jobs-lib.zsh
exec > >(tee -a $P/final.log) 2>&1
echo "### FINAL what a person sees $(now), fixed owner and host"
renderer_key; mesh_up; sleep 2; mockmodel
$P/rail js "document.querySelector('.rail')?1:document.querySelector('.tico[title=\"Mostrar/ocultar panel\"]')?.click()" >/dev/null; sleep 1
hold_on; R() { WORKASS_TOOL_CONTEXT=$P/ctx.json $P/workass-mac run --state-dir $P/mac-state "$@" >/dev/null 2>>$P/f5.err; }
R --label sv-ok -- sleep 2 & R --label sv-fail -- sh -c 'sleep 2; exit 3' & W=$(host_job sv-remote 4); sleep 9; release; wait_idle 200; sleep 2
$P/rail js "JSON.stringify([...document.querySelectorAll('.note')].map(e=>e.textContent.trim().slice(0,60)).filter(t=>/sv-/.test(t)))"; echo
$P/rail js "(()=>{const a=[...document.querySelectorAll('.note')].filter(e=>/sv-/.test(e.textContent));a.length&&a[0].scrollIntoView({block:'start'});return a.length})()"; echo; sleep 1
$P/rail shot job-notices-final
