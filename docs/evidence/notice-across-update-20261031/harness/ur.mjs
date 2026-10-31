// ur.mjs FROM TO SCEN COUNT: real click-the-card update in the sealed rig, a real Haiku owner, real `workass run` jobs.
import {spawn,execFileSync,execSync} from 'node:child_process'; import fs from 'node:fs'; import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const RR='/Users/dev/Workspace/workass',U=`${RR}/.dev/urig`,APP=`${process.env.HOME}/Applications/WorkassUpdateRig.app`,BIN=`${APP}/Contents/Resources/runtime/workass`;
const [FROM,TO,TOVER,SCEN,COUNT]=process.argv.slice(2);
const ENV={HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:'/usr/bin:/bin:/usr/sbin:/sbin'};
const sleep=ms=>new Promise(r=>setTimeout(r,ms)); const log=(...a)=>console.log(new Date().toISOString().slice(11,23),...a);
function rigProcs(){ try{return execSync(`pgrep -f 'WorkassUpdateRig' || true`).toString().trim().split('\n').filter(Boolean)}catch{return[]} }
async function down(){ try{execSync(`launchctl bootout gui/$(id -u)/com.workass.daemon.urig 2>/dev/null || true`)}catch{}; for(const p of rigProcs()){ try{process.kill(Number(p),'SIGTERM')}catch{} } for(let i=0;i<40&&rigProcs().length;i++) await sleep(250); for(const p of rigProcs()){ try{process.kill(Number(p),'SIGKILL')}catch{} } await sleep(500); }
function wire(ch,...args){ return new Promise((res,rej)=>{ const ws=new WebSocket(`wss://127.0.0.1:28990/ws`); const t=setTimeout(()=>{try{ws.close()}catch{};rej(new Error('timeout '+ch))},20000); ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel:ch,args})); ws.onmessage=e=>{const m=JSON.parse(e.data); if(m.t==='reply'&&m.id==='x'){clearTimeout(t);ws.close(); m.error?rej(new Error(JSON.stringify(m.error))):res(m.result)}}; ws.onerror=()=>{clearTimeout(t);rej(new Error('ws '+ch))}; }); }
const health=async()=>{ try{const r=await fetch('https://127.0.0.1:28990/workass/health'); return (await r.json()).version}catch{return null} };
function cdp(expr){ return execFileSync('node',[`${U}/scripts/cdp.mjs`,'28992',expr],{encoding:'utf8',timeout:15000}).trim(); }
let seq=0; const newId=p=>`${p}-${Date.now().toString(36)}-${++seq}`; const STATE=`${U}/a/state`;
function ctxFile(chat){ const f=`${U}/ctx-${chat}.json`; const dir=`${STATE}/tool-bootstrap`; for(const d of fs.readdirSync(dir).map(x=>path.join(dir,x))){ try{const c=JSON.parse(fs.readFileSync(`${d}/context.json`)); if(c.chat_id===chat){fs.copyFileSync(`${d}/context.json`,f); return f;}}catch{} } if(fs.existsSync(f)) return f; throw new Error('no ctx'); }
const read=(tab,chat)=>JSON.parse(execFileSync(BIN,['tools','--context',ctxFile(chat),'call','workass_read_chat'],{input:JSON.stringify({tab_id:tab,chat_id:chat,limit:200}),env:ENV,cwd:'/tmp'}));
const sendTurn=(tab,chat,text,model)=>{ const op=newId('op'); return wire('job:start',{tabId:tab,chatId:chat,operationId:op,userMessageId:'u-'+op,assistantMessageId:'a-'+op,prompt:text,cwd:'/tmp',permissionMode:'bypassPermissions',providerId:'claude',modelId:model,kind:'app-chat',title:'rig'}); };
function job(label,seconds,ctx){ const o=fs.openSync(`${U}/a-logs/${label}.out`,'a'); const p=spawn(BIN,['run','--state-dir',STATE,'--label',label,'--operation-id',newId(label),'--','sleep',String(seconds)],{cwd:'/tmp',env:{...ENV,WORKASS_TOOL_CONTEXT:ctxFile.cur},stdio:['ignore',o,o],detached:true}); p.unref(); }
function verdict(ms,label){ const ns=ms.filter(m=>m.role==='user'&&String(m.content||m.text||'').includes('<task-notification>')&&String(m.content||m.text||'').includes(`"${label}"`)); let done=0,fail=[]; for(const n of ns){ const a=ms.slice(ms.indexOf(n)+1).find(m=>m.role==='assistant'); if(a&&a.status==='done') done++; else fail.push((a?.status||'no-reply')+': '+String(a?.content||a?.text||'').slice(0,100)); } return {n:ns.length,done,fail}; }
const verdictPass=v=>v.done===1&&(v.n===1||(v.n===2&&v.fail.length===1&&!/could not start/.test(v.fail[0])));
const CLICK=`(async()=>{const w=ms=>new Promise(r=>setTimeout(r,ms));const f=()=>document.querySelector('.updcard');let b=f();if(!b)return 'nocard';const t0=b.textContent;b.click();await w(700);b=f();const t1=b?b.textContent:'gone';if(b&&/igual/.test(t1)){b.click();return 'armed+second: '+t1}return 'one: '+t0+' -> '+t1})()`;
async function cycle(n){
  await down(); execSync(`rm -rf "${APP}" "${process.env.HOME}/Applications/".Workass.app.incoming-* ; ditto "${U}/bundles/${FROM}.app" "${APP}"; find "${U}/a" -mindepth 1 -maxdepth 1 ! -name update-feed -exec rm -rf {} +`); execSync(`"${U}/feed.sh" "${U}/bundles/${TO}.app" ${TOVER} 20261031235959`,{stdio:'ignore'});
  const app=spawn(`${APP}/Contents/MacOS/Workass`,['--remote-debugging-port=28992'],{cwd:'/tmp',env:ENV,stdio:'ignore',detached:true}); app.unref();
  let v0; for(let i=0;i<120&&!(v0=await health());i++) await sleep(500); if(!v0) throw new Error('no health');
  let cards=false; for(let i=0;i<120;i++){ try{ if(cdp(`!!document.querySelector('.updcard')`)==='true'){cards=true;break} }catch{} await sleep(1000);} if(!cards) throw new Error('no update card');
  const tab=newId('tab'),chat=newId('conv'); const model=process.env.RIG_MODEL||'haiku';
  await wire('chat:create',{tabId:tab,chatId:chat,operationId:newId('create'),title:'owner',cwd:'/tmp',providerId:'claude',currentModelId:model,currentModeId:'bypassPermissions',focus:false});
  await sendTurn(tab,chat,'Reply with only the word READY.',model);
  let ok=false; for(let i=0;i<120;i++){ try{ const ms=read(tab,chat).messages||[]; if(ms.some(m=>m.role==='assistant'&&m.status==='done')){ok=true;break} }catch{} await sleep(500);} if(!ok) throw new Error('owner never replied: first turn');
  ctxFile.cur=ctxFile(chat);
  const labels=[]; const clickAt=2500; const spread=Number(process.env.SPREAD||6);
  if(SCEN==='running'){ await sendTurn(tab,chat,`Run this shell command with your shell tool with timeout 600000 and wait for it: python3 -c 'import time;time.sleep(${30+n})'. Then reply DONE.`,model); for(let i=0;i<60;i++){ if(execSync(`pgrep -f 'time.sleep\\(${30+n}\\)' || true`).toString().trim()) break; await sleep(500);} }
  if(SCEN==='subagent'){ for(const [l,task] of [[`sfast${n}`,'Reply with only the word DONE-SUB.'],[`sslow${n}`,'Run this shell command with your shell tool, timeout 300000: python3 -c "import time;time.sleep(45)" then reply DONE-SUB.']]){ labels.push(l); const out=execFileSync(BIN,['tools','--context',ctxFile.cur,'call','workass_spawn_subagent'],{input:JSON.stringify({operation_id:newId('sub'),label:l,task,permission_intent:'full'}),env:ENV,cwd:'/tmp',encoding:'utf8'}); log('spawned',l,out.slice(0,90)); } }
  else   for(let k=0;k<spread;k++){ const l=`j${Date.now().toString(36)}x${k}`; labels.push(l); job(l,((clickAt-1500+k*600)/1000).toFixed(2)); }
  await sleep(clickAt); const click=cdp(CLICK); log(`cycle ${n} click: ${click.slice(0,90)}`);
  let v1; const t0=Date.now(); for(let i=0;i<200;i++){ v1=await health(); if(v1===TOVER) break; await sleep(500);} log(`cycle ${n} healthy ${v1} after ${((Date.now()-t0)/1000).toFixed(1)}s`);
  let last='',same=0,ms=[]; for(let i=0;i<Number(process.env.WAIT||240);i++){ try{ ms=read(tab,chat).messages||[]; const vs=labels.map(l=>verdict(ms,l)); const sig=JSON.stringify(vs); same=sig===last?same+1:0; last=sig; if(vs.every(v=>v.n>=1&&(v.done+v.fail.length)>=v.n&&v.done>=1)&&same>=8) break; }catch{} await sleep(1000);}
  fs.writeFileSync(`${U}/out-${SCEN}-${FROM}-${n}-${Date.now()}.json`,JSON.stringify(ms,null,1));
  const vs=labels.map(l=>[l,verdict(ms,l)]); const good=vs.filter(([,v])=>verdictPass(v)).length; const redo=vs.filter(([,v])=>v.n>1).length; const bad=vs.filter(([,v])=>!verdictPass(v)).map(([l,v])=>`${l}:n=${v.n},done=${v.done},${v.fail.join('/').slice(0,120)}`);
  let rec={}; try{rec=JSON.parse(fs.readFileSync(`${U}/a/updates/receipt.json`))}catch{} log(`RESULT ${SCEN} ${FROM}->${TO} #${n} ${good===vs.length?'PASS':'FAIL'} ${good}/${vs.length} redelivered=${redo} receipt=${rec.phase}/${rec.installedVersion}/rb=${rec.rolledBack} ${bad.join(' | ')}`);
  return [good,vs.length,redo];
}
(async()=>{ let g=0,t=0,r=0,p=0; for(let n=1;n<=Number(COUNT);n++){ try{ const [a,b,c]=await cycle(n); g+=a;t+=b;r+=c; if(a===b)p++; }catch(e){ log('CYCLE ERROR',n,e.message.slice(0,300)); } } await down(); console.log(`SUMMARY ${SCEN} ${FROM}->${TO} cycles ${p}/${COUNT} notices ${g}/${t} redelivered ${r}`); })();
