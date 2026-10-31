// rig.mjs BIN SCENARIO COUNT [--keep]  One real daemon, the real prepare/commit stop, a real restart, real `workass run` jobs.
// Rig only: every path is under .dev/nrig/run-*, every daemon has --state-dir.
import {spawn,execFileSync} from 'node:child_process'; import fs from 'node:fs'; import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const REPO='/Users/dev/Workspace/workass', [BIN,SCEN,COUNT]=process.argv.slice(2);
const PORT=28790+Math.floor(Math.random()*150);
const ROOT=`${REPO}/.dev/nrig/run-${SCEN}-${Date.now()}`, STATE=`${ROOT}/state`; fs.mkdirSync(`${ROOT}/logs`,{recursive:true});
const sleep=ms=>new Promise(r=>setTimeout(r,ms)); const log=(...a)=>console.log(new Date().toISOString().slice(11,23),...a);
const ENV={HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:'/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin',WORKASS_PROFILE:'dev',WORKASS_DATA_ROOT:`${ROOT}/data`,WORKASS_LOG_ROOT:`${ROOT}/logs`,WORKASS_BROWSER_CONTROL_FILE:`${ROOT}/browser-control.json`,WORKASS_MOCK_ACP_DELAY_MS:process.env.MOCKDELAY||'5'};
let daemon, exited;
async function up(){ const out=fs.openSync(`${ROOT}/logs/daemon.out`,'a'); daemon=spawn(`${REPO}/${BIN}`,['--headless','--state-dir',STATE,'--port',String(PORT),'--bind','localhost','--renderer-dir',`${REPO}/desktop/renderer2/dist`],{cwd:REPO,env:ENV,stdio:['ignore',out,out]}); exited=new Promise(r=>daemon.on('exit',(c,s)=>r({c,s})));
  for(let i=0;i<200;i++){ try{const r=await fetch(`https://127.0.0.1:${PORT}/workass/health`); if(r.ok) return;}catch{} await sleep(100);} throw new Error('daemon never healthy'); }
function wire(ch,...args){ return new Promise((res,rej)=>{ const ws=new WebSocket(`wss://127.0.0.1:${PORT}/ws`); const t=setTimeout(()=>{try{ws.close()}catch{};rej(new Error('timeout '+ch))},20000);
  ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel:ch,args})); ws.onmessage=e=>{const m=JSON.parse(e.data); if(m.t==='reply'&&m.id==='x'){clearTimeout(t);ws.close(); m.error?rej(new Error(JSON.stringify(m.error))):res(m.result)}}; ws.onerror=()=>{clearTimeout(t);rej(new Error('ws '+ch))}; }); }
const post=(p,body)=>fetch(`https://127.0.0.1:${PORT}${p}`,{method:'POST',body:JSON.stringify(body)}).then(async r=>({status:r.status,body:await r.text()}));
let seq=0; const newId=p=>`${p}-${Date.now().toString(36)}-${++seq}`;
async function sendTurn(tab,chat,text,model){ const op=newId('op'); return wire('job:start',{tabId:tab,chatId:chat,operationId:op,userMessageId:'u-'+op,assistantMessageId:'a-'+op,prompt:text,cwd:'/tmp',providerId:'mock',modelId:model,kind:'app-chat',title:'rig'}); }
async function read(tab,chat){ const ctx=ctxFile(chat); const r=execFileSync(`${REPO}/${BIN}`,['tools','--context',ctx,'call','workass_read_chat'],{input:JSON.stringify({tab_id:tab,chat_id:chat,limit:200}),env:ENV,cwd:'/tmp'}); return JSON.parse(r); }
function ctxFile(chat){ const dir=`${STATE}/tool-bootstrap`; for(const d of fs.readdirSync(dir).map(x=>path.join(dir,x)).sort((a,b)=>fs.statSync(b).mtimeMs-fs.statSync(a).mtimeMs)){ try{const c=JSON.parse(fs.readFileSync(`${d}/context.json`)); if(c.chat_id===chat){const f=`${ROOT}/ctx-${chat}.json`; fs.copyFileSync(`${d}/context.json`,f); return f;}}catch{} } const f=`${ROOT}/ctx-${chat}.json`; if(fs.existsSync(f)) return f; throw new Error('no ctx for '+chat); }
function job(label,seconds,ctx){ const p=spawn(`${REPO}/${BIN}`,['run','--state-dir',STATE,'--label',label,'--operation-id',newId(label),'--','sleep',String(seconds)],{cwd:'/tmp',env:{...ENV,WORKASS_TOOL_CONTEXT:ctx},stdio:['ignore',fs.openSync(`${ROOT}/logs/${label}.out`,'a'),fs.openSync(`${ROOT}/logs/${label}.out`,'a')],detached:true}); p.unref(); return p; }
function verdict(rows,label){ const notices=rows.filter(m=>m.role==='user'&&String(m.content||m.text||'').includes('<task-notification>')&&String(m.content||m.text||'').includes(`"${label}"`));
  const replies=[]; let fail=[]; for(const n of notices){ const i=rows.indexOf(n); const a=rows.slice(i+1).find(m=>m.role==='assistant'&&(m.status!==undefined)); const text=String(a?.content||a?.text||''); const ok=a&&a.status==='done'&&!/could not start|restarted before|connection ended/.test(text)&&text.length>0; replies.push(ok); if(!ok) fail.push((a?.status||'none')+': '+text.slice(0,110)); }
  return {notices:notices.length,replied:replies.filter(Boolean).length,fail,redelivered:notices.length>1}; }
async function main(){
 const results=[]; let total=0,okTotal=0,redelivered=0; await up(); const cat=await wire('app-chat:detect-acp',{}).catch(e=>({err:String(e)})); fs.writeFileSync(`${ROOT}/detect.json`,JSON.stringify(cat));
 for(let n=1;n<=Number(COUNT);n++){
  const tab=newId('tab'), chat=newId('conv'); const label=`job${n}`;
  await wire('chat:create',{tabId:tab,chatId:chat,operationId:newId('create'),title:'rig '+n,cwd:'/tmp',providerId:'mock',currentModelId:'mock-model',focus:false});
  await sendTurn(tab,chat,'hello','mock-model');
  let ctx; for(let i=0;i<80;i++){ try{ctx=ctxFile(chat);break}catch{await sleep(100)} } if(!ctx) throw new Error('no context');
  for(let i=0;i<100;i++){ const r=await read(tab,chat); if((r.messages||[]).some(m=>m.role==='assistant'&&m.status==='done')) break; await sleep(100); }
  const update=newId('upd0000000'); let labels=null;
  const stop=async(gap=300)=>{ await post('/workass/update/prepare',{updateId:update}); await sleep(gap); await post('/workass/update/commit',{updateId:update}); const e=await Promise.race([exited,sleep(30000).then(()=>'hung')]); return e; };
  let endAt=null;
  if(SCEN==='running-queued'){ // a turn is running; the notice queues behind it; the update stops the daemon
    await sendTurn(tab,chat,'[mock:hold-tools] work','mock-model'); await sleep(600); job(label,1,ctx); await sleep(2500); await stop(); }
  else if(SCEN==='idle-just-before'){ job(label,1,ctx); await sleep(1000+Number(process.env.GAP||150)); await stop(); }
  else if(SCEN==='running-in-drain'){ await sendTurn(tab,chat,'[mock:hold-tools] work','mock-model'); await sleep(600); job(label,2,ctx); await sleep(1500); await stop(1500); }
  else if(SCEN==='idle-in-drain'){ job(label,2,ctx); await sleep(1400); await stop(1500); }
  else if(SCEN==='idle-in-stop'){ job(label,1,ctx); await sleep(700); await stop(Number(process.env.GAP||320)); }
  else if(SCEN==='idle-while-down'){ job(label,2,ctx); await sleep(300); await stop(100); await sleep(3000); }
  else if(SCEN==='running-while-down'){ await sendTurn(tab,chat,'[mock:hold-tools] work','mock-model'); await sleep(600); job(label,2,ctx); await sleep(300); await stop(100); await sleep(3000); }
  else if(SCEN==='notice-slow-running'){ // the notice's own turn is mid-flight when the stop comes
    job(label,1,ctx); await sleep(1050); await stop(50); }
  else if(SCEN==='cancel-drain'){ // the update is called off: turns queued when it began wait for it, then run with no restart
    const ls=[0,1,2,3].map(k=>`c${n}x${k}`); for(const l of ls) job(l,0.2,ctx); await sleep(Number(process.env.BEFORE||3500));
    await post('/workass/update/prepare',{updateId:update}); await sleep(Number(process.env.HOLD||5000)); const before=(await read(tab,chat)).messages||[]; const bs=ls.map(l=>verdict(before,l)); await post('/workass/update/cancel',{updateId:update});
    let vs; for(let i=0;i<120;i++){ await sleep(250); const r=await read(tab,chat); vs=ls.map(l=>verdict(r.messages||[],l)); if(vs.every(v=>v.replied===1)) break; }
    const failedInDrain=bs.filter(v=>v.fail.some(f=>/could not start/.test(f))).length; const waiting=bs.filter(v=>v.notices===0).length; const pass=failedInDrain===0&&vs.every(v=>v.notices===1&&v.replied===1); results.push(pass); log(`RESULT ${SCEN} #${n} ${pass?'PASS':'FAIL'} waitingAtCancel=${waiting} failedInDrain=${failedInDrain} answered=${vs.filter(v=>v.replied===1).length}/4`); continue; }
  else if(SCEN.startsWith('sweep')){ const slow=SCEN.includes('slow'), running=SCEN.includes('running'); labels=[]; if(running){ await sendTurn(tab,chat,'[mock:hold-tools] work','mock-model'); await sleep(500);} const T=4000+Math.floor(Math.random()*500);
    for(let k=0;k<16;k++){ const l=`j${n}x${k}`+(slow?' [mock:slow]':''); labels.push(l); const ms=Math.max(200,T-3000+k*300); job(l,(ms/1000).toFixed(2),ctx);} await sleep(T-Number(process.env.DRAIN||1500)); await post('/workass/update/prepare',{updateId:update}); await sleep(Number(process.env.DRAIN||1500)); if(process.env.KILL9){ daemon.kill('SIGKILL'); } else { await post('/workass/update/commit',{updateId:update}); } await Promise.race([exited,sleep(30000)]); await sleep(1200); }
  else throw new Error('scenario');
  await up();
  if(labels){ let bad=[],lastSig='',same=0,r; for(let i=0;i<600;i++){ r=await read(tab,chat).catch(()=>null); if(r){ const vs=labels.map(l=>verdict(r.messages||[],l)); const sig=JSON.stringify(vs); same=sig===lastSig?same+1:0; lastSig=sig; if(vs.every(v=>v.notices>=1)&&same>=20) break; } await sleep(250); }
    fs.writeFileSync(`${ROOT}/chat-${n}.json`,JSON.stringify(r,null,1)); const vs=labels.map(l=>[l,verdict(r.messages||[],l)]); let good=0; for(const [l,v] of vs){ if(v.redelivered) redelivered++; if(v.replied===1&&(v.notices===1||(v.notices===2&&v.fail.length===1&&!/could not start/.test(v.fail[0])))) good++; else bad.push(`${l}:n=${v.notices},r=${v.replied},${v.fail.join('/').slice(0,90)}`);} daemon.kill('SIGTERM'); await Promise.race([exited,sleep(15000)]); await up(); await sleep(2500); const again=await read(tab,chat); const repeats=labels.filter(l=>{const a=verdict(again.messages||[],l),b=verdict(r.messages||[],l); return a.notices!==b.notices||a.replied!==b.replied}); if(repeats.length) bad.push('repeated after a further restart: '+repeats.join(',')); const pass=bad.length===0; results.push(pass); total+=vs.length; okTotal+=good; log(`RESULT ${SCEN} #${n} ${pass?'PASS':'FAIL'} ${good}/${vs.length} ${bad.join(' | ')}`); continue; }
  let v; for(let i=0;i<150;i++){ const r=await read(tab,chat).catch(()=>null); if(r){ v=verdict(r.messages||[],label); if(v.notices>=1&&v.replied>=1&&i>40) break; if(v.notices>=1&&v.fail.length&&i>80) break; } await sleep(100); }
  const r=await read(tab,chat); fs.writeFileSync(`${ROOT}/chat-${n}.json`,JSON.stringify(r,null,1)); v=verdict(r.messages||[],label);
  const pass=v.notices===1&&v.replied===1; results.push(pass); log(`RESULT ${SCEN} #${n} ${pass?'PASS':'FAIL'} notices=${v.notices} replied=${v.replied} ${v.fail.join(' | ')}`);
 }
 daemon.kill('SIGTERM'); await Promise.race([exited,sleep(10000)]);
 console.log(`SUMMARY ${SCEN} cycles ${results.filter(Boolean).length}/${results.length} notices ${okTotal}/${total} redelivered ${redelivered} root=${ROOT}`);
}
main().catch(e=>{console.log('RIG ERROR',e.stack);try{daemon.kill('SIGTERM')}catch{};process.exit(2)});
