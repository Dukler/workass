// rrig-message.mjs BIN: release-20261101's rrig.mjs with one more scenario, `message`:
// workass_message_subagent from the owner's parent to a child hosted on the other daemon, once in
// the middle of a turn and once parked on its background work.
// Two real daemons on one Mac, each with its own state and identity. The
// owner stays on loopback; the host listens on the LAN address and is reached only through the
// fleet mesh. The parent is real Claude Haiku on the owner; children and jobs run on the host.
// Scenarios (comma separated): claude, codex, killed, ownerdown, hold, rail.
// Nothing touches an installed app or daemon. Background tasks write a heartbeat and any signal.
import {spawn,execFileSync,execSync} from 'node:child_process';import fs from 'node:fs';import os from 'node:os';import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const [BIN_ARG,SCEN_ARG='message']=process.argv.slice(2);const BIN=path.resolve(BIN_ARG),SCENS=SCEN_ARG.split(',');
const HERE=path.dirname(new URL(import.meta.url).pathname),REPO=path.resolve(HERE,'../../../..');
const STAMP=`${Date.now().toString(36)}${process.pid.toString(36)}`,ROOT=`${REPO}/.dev/rrig/${STAMP}`,CWD=path.join(fs.realpathSync(os.tmpdir()),`wa-rrig-${STAMP}`);
const OP=Number(process.env.RIG_OWNER_PORT||28881),HP=Number(process.env.RIG_HOST_PORT||28882),SECS=Number(process.env.RIG_SECS||45);
for(const d of [`${ROOT}/o/logs`,`${ROOT}/h/logs`,`${ROOT}/codex-home`,CWD])fs.mkdirSync(d,{recursive:true});
fs.symlinkSync(`${os.homedir()}/.codex/auth.json`,`${ROOT}/codex-home/auth.json`);
const t0=Date.now(),say=(s,...a)=>{const line=`+${String(Math.round((Date.now()-t0)/1000)).padStart(4)}s [${s}] ${a.join(' ')}`;console.log(line);fs.appendFileSync(`${ROOT}/timeline.log`,line+'\n')};
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const envFor=n=>({HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:`${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`,
 WORKASS_PROFILE:'dev',WORKASS_DATA_ROOT:`${ROOT}/${n}`,WORKASS_LOG_ROOT:`${ROOT}/${n}/logs`,WORKASS_UPDATE_CHANNEL:'local',WORKASS_BROWSER_CONTROL_FILE:`${ROOT}/${n}/bc.json`,
 WORKASS_CLAUDE_SDK_MODULE:process.env.RIG_SDK,CODEX_HOME:`${ROOT}/codex-home`});
const O=envFor('o'),H=envFor('h'),OS=`${ROOT}/o/state`,HS=`${ROOT}/h/state`;
const key=execFileSync(BIN,['fleet','key','--state-dir',OS],{env:O,encoding:'utf8'}).split('\n').map(l=>l.trim()).find(l=>/^[A-Za-z0-9_+/=-]{20,}$/.test(l));if(!key)throw Error('no fleet key');
execFileSync(BIN,['fleet','join','--state-dir',HS],{env:H,input:key+'\n',encoding:'utf8',stdio:['pipe','ignore','ignore']});
const boot=(n,env,state,port,bind)=>{const l=fs.openSync(`${ROOT}/${n}/logs/daemon.log`,'a');return spawn(BIN,['--headless','--state-dir',state,'--port',String(port),'--bind',bind,'--beacon=false'],{cwd:REPO,stdio:['ignore',l,l],env})};
const health=async p=>{for(let i=0;i<240;i++){try{if((await fetch(`https://127.0.0.1:${p}/workass/health`)).ok)return true}catch{}await sleep(250)}throw Error('no health '+p)};
let od=boot('o',O,OS,OP,'localhost'),hd=boot('h',H,HS,HP,'lan');
const procs=[];const bye=()=>{for(const p of [od,hd,...procs])try{p.kill('SIGTERM')}catch{}};process.on('exit',bye);process.on('SIGTERM',()=>process.exit(1));process.on('SIGINT',()=>process.exit(1));
await Promise.all([health(OP),health(HP)]);
const lan=execFileSync('/usr/sbin/ipconfig',['getifaddr','en0'],{encoding:'utf8'}).trim();
const call=(port,channel,...args)=>new Promise((resolve,reject)=>{const ws=new WebSocket(`wss://127.0.0.1:${port}/ws`);const timer=setTimeout(()=>{try{ws.close()}catch{};reject(Error('timeout '+channel))},45000);ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel,args}));ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'&&m.id==='x'){clearTimeout(timer);ws.close();m.error?reject(Error(JSON.stringify(m.error))):resolve(m.result)}};ws.onerror=()=>{clearTimeout(timer);reject(Error('ws '+channel))}});
const added=await call(OP,'machines:add',{address:`${lan}:${HP}`});say('rig','machine added',added.ok,added.error||'');if(!added.ok)throw Error('machine add failed');
// A daemon reads the book into its mesh at boot and after a network scan, not after machines:add.
{const gone=new Promise(res=>od.once('exit',res));od.kill('SIGTERM');await gone;od=boot('o',O,OS,OP,'localhost');await health(OP)}
const HID=JSON.parse(fs.readFileSync(`${HS}/machine-id.json`)).machineId;
const tab=`tab-${STAMP}`,chat=`conv-${STAMP}`;
for(let i=0;;i++){try{await call(OP,'chat:create',{tabId:tab,chatId:chat,operationId:`create-${STAMP}-${i}`,title:'remote rig',cwd:CWD,providerId:'claude',currentModelId:'haiku',currentModeId:'bypassPermissions',focus:true});break}catch(e){if(i>120)throw e;await sleep(1000)}}
const warm=`warm-${STAMP}`;for(let i=0;;i++){try{await call(OP,'job:start',{tabId:tab,chatId:chat,operationId:warm,userMessageId:'u-'+warm,assistantMessageId:'a-'+warm,prompt:'Reply with only the word READY.',cwd:CWD,permissionMode:'bypassPermissions',providerId:'claude',modelId:'haiku'});break}catch(e){if(i>120)throw e;await sleep(1000)}}
let ctx;for(let i=0;i<300&&!ctx;i++){const dir=`${OS}/tool-bootstrap`;try{for(const b of fs.readdirSync(dir)){try{const c=JSON.parse(fs.readFileSync(`${dir}/${b}/context.json`));if(c.chat_id===chat){ctx=`${ROOT}/ctx.json`;fs.copyFileSync(`${dir}/${b}/context.json`,ctx)}}catch{}}}catch{}if(!ctx)await sleep(300)}if(!ctx)throw Error('no tool context');
const tool=(name,input={})=>new Promise((resolve,reject)=>{const c=spawn(BIN,['tools','--context',ctx,'call',name],{env:O,cwd:'/tmp',stdio:['pipe','pipe','pipe']});let out='',err='';c.stdout.on('data',d=>out+=d);c.stderr.on('data',d=>err+=d);const t=setTimeout(()=>{c.kill();reject(Error('tool timeout '+name))},60000);c.on('close',code=>{clearTimeout(t);if(code!==0)return reject(Error(`${name}: ${(err||out).slice(0,400)}`));try{resolve(JSON.parse(out))}catch{resolve({raw:out})}});c.stdin.end(JSON.stringify(input))});
const messages=async()=>{const x=await tool('workass_read_chat',{tab_id:tab,chat_id:chat,limit:200});return x.result?.messages||x.messages||[]};
const notices=async label=>(await messages()).filter(m=>m.role==='user').map(m=>String(m.content||m.text||'')).filter(t=>t.includes('<task-notification>')&&t.includes(`"${label}"`)||t.includes('<task-notification>')&&t.includes(label));
for(let i=0;i<240;i++){const ms=await messages().catch(()=>[]);if(ms.some(m=>m.role==='assistant'&&m.status==='done'))break;await sleep(500)}
say('rig','parent ready, host machine',HID.slice(0,6));
for(let i=0;;i++){const c=await tool('workass_agent_catalog',{machine_id:HID}).catch(e=>({error:e.message}));if(!c.error){say('rig',`host catalog read over the mesh after ${i} tries:`,(c.result?.providers||c.providers||[]).map(p=>p.id||p.provider_id).join(','));break}if(i>30)throw Error('host unreachable: '+c.error);await sleep(1000)}
say('rig','links',execFileSync(BIN,['fleet','links','--state-dir',OS],{env:O,encoding:'utf8'}).replace(/\s+/g,' ').slice(0,240));
const alive=n=>{try{return execSync(`pgrep -f 'sweep-${n}.sh' || true`).toString().trim().split('\n').filter(Boolean)}catch{return[]}};
const hostOf=pid=>{for(let p=pid,i=0;i<8&&p>1;i++){const line=execSync(`ps -o ppid=,command= -p ${p} || true`).toString().trim();if(!line)return 0;if(/claude-native-host|codex-native-host/.test(line))return p;p=Number(line.split(/\s+/)[0])}return 0};
const results={};
async function hosted(S,{provider='claude',model='haiku',task,secs=SECS,onWaiting}){
 const nonce=`${S}${STAMP}`,label=`rig ${S}`,marker=`${CWD}/${nonce}.out`,hb=`${CWD}/${nonce}.hb`,script=`${CWD}/sweep-${nonce}.sh`;
 fs.writeFileSync(script,`#!/bin/sh\necho "started pid=$$ $(date +%T)" >> ${hb}\nfor sig in TERM HUP INT; do trap "echo signal $sig at \\$(date +%T) >> ${hb}; exit 143" $sig; done\ni=0; while [ $i -lt ${secs} ]; do sleep 1; i=$((i+1)); date +%T > ${hb}.tick; done\necho SWEEP-DONE-${nonce} > ${marker}\n`);
 const CMD=`sh ${script}`,END=`End your turn at once with exactly this sentence: "The sweep is running and the notification will report when it finishes." Do not wait, poll, sleep or check on it in that turn.`,LATER=`If you are later told the task finished, read ${marker} and reply with "RESULT:" followed by its contents.`;
 const prompt=provider==='codex'?`Use your exec_command tool to start this exact command with yield_time_ms 1000 so it keeps running after the tool returns: ${CMD}\n${END}\n${LATER}`:`Use your Bash tool with run_in_background set to true to start this exact command: ${CMD}\n${END}\n${LATER}`;
 const spawned=await tool('workass_spawn_subagent',{operation_id:`sub-${nonce}`,label,task:task||prompt,provider_id:provider,model_id:model,permission_intent:'full',cwd:CWD,machine_id:HID,...(provider==='codex'?{effort:'low'}:{})});
 const sid=(spawned.result||spawned).id||(spawned.result||spawned).subagent?.id||(spawned.result||spawned).subagentId;if(!sid)throw Error('no id '+JSON.stringify(spawned).slice(0,300));say(S,'spawned on host',sid,provider,model,`background=${secs}s`);
 const r={sawWaiting:false,settled:null,workAliveAtSettle:null,markerAtSettle:null,message:null};let last='',settledAt=0,acted=false;const cap=Date.now()+(secs+Number(process.env.RIG_CAP||300))*1000;
 for(;;){let row=null;try{const l=await tool('workass_list_subagents',{});const rows=l.result?.subagents||l.subagents||l.result||[];row=(Array.isArray(rows)?rows:[]).find(x=>x.id===sid)||null}catch(e){if(!r.down)say(S,'list error',String(e.message).slice(0,120))}
  const pids=alive(nonce),f=fs.existsSync(marker);const line=`status=${row?.status} phase=${row?.phase} activity=${JSON.stringify((row?.latestActivity||'').slice(0,70))} process=${pids.length?'alive':'gone'} marker=${f?'written':'absent'}`;if(line!==last){say(S,line);last=line}
  if(row?.phase==='waiting_background')r.sawWaiting=true;
  if(!acted&&row?.phase==='waiting_background'&&pids.length){acted=true;if(onWaiting)await onWaiting({S,sid,pids,r,nonce})}
  if(row&&row.status!=='running'&&!settledAt){settledAt=Date.now();r.settled=row.status;r.workAliveAtSettle=pids.length>0;r.markerAtSettle=f}
  if(settledAt&&Date.now()-settledAt>12000)break;if(Date.now()>cap){say(S,'CAP');break}await sleep(1000)}
 const rc=await tool('workass_list_subagent_receipts',{}).catch(e=>({error:e.message}));const rec=(rc.result?.receipts||rc.receipts||[]).find(x=>x.subagentId===sid||x.receiptId===sid);
 r.receipt={status:rec?.status,stopReason:rec?.stopReason,elapsedMs:rec?.elapsedMs,result:String(rec?.result||'').slice(0,200),error:rec?.error,backgroundWork:rec?.backgroundWork||rec?.backgroundStopped};
 r.finished=fs.existsSync(marker);r.childRead=String(rec?.result||'').includes('SWEEP-DONE-'+nonce);r.heartbeat=fs.existsSync(hb)?fs.readFileSync(hb,'utf8').trim().replace(/\n/g,' | ').replace(/pid=\d+ /,''):'';
 const n=await notices(label);r.notices=n.length;r.noticeStatus=n.map(t=>(t.match(/<status>(.*?)<\/status>/)||[])[1]);r.backgroundStoppedInNotice=n.some(t=>t.includes('<background-stopped>'));
 say(S,'RESULT',JSON.stringify(r));results[S]=r;if(alive(nonce).length)try{execSync(`pkill -f 'sweep-${nonce}.sh'`)}catch{}
}
// The real renderer the owner serves, in a hidden window that never takes focus (remote-rail harness).
const RAIL=Number(process.env.RIG_RAIL_PORT||28883),SHOTS=`${ROOT}/shots`;fs.mkdirSync(SHOTS,{recursive:true});
const railEval=async js=>{const r=await fetch(`http://127.0.0.1:${RAIL}/eval`,{method:'POST',body:js});const j=await r.json();if(j.error)throw Error(j.error);return j.value};
const railUp=async()=>{const el=spawn(process.env.RIG_ELECTRON,[path.resolve(HERE,'../../remote-rail-20261030/harness/rail-harness.cjs')],{env:{...process.env,RAIL_URL:`https://127.0.0.1:${OP}/`,RAIL_SHOTS:SHOTS,RAIL_PORT:String(RAIL)},stdio:'ignore'});procs.push(el);for(let i=0;i<80;i++){try{if(await railEval('!!(window.api&&window.api.lanTakeControl)'))return true}catch{}await sleep(500)}return false};
const railCheck=async({S,sid})=>{try{await sleep(2000);await railEval('window.api.lanTakeControl().then(x=>x.controller)');await sleep(1500);await railEval(`(()=>{const b=document.querySelector('button[title="Mostrar/ocultar panel"]');if(b&&!b.classList.contains('on'))b.click();return 1})()`);await sleep(1500);const list=await railEval(`window.api.spawnedWorkList(${JSON.stringify(tab)},${JSON.stringify(chat)}).then(r=>JSON.stringify((r.items||r).map(i=>({label:i.label,status:i.status,phase:i.lastToolName,host:!!i.hostMachine}))))`);say(S,'rail row',list);await railEval(`document.querySelectorAll('details.r-sa').forEach(d=>{d.open=true});1`);await sleep(500);const dom=await railEval(`JSON.stringify((()=>{const open=[...document.querySelectorAll('.bgr,.r-sa')].map(e=>e.innerText.replace(/\\s+/g,' ').slice(0,120));return {rows:open,oldLabel:document.body.innerText.includes('Esperando su trabajo'),waitText:document.body.innerText.includes('Waiting for background work')}})())`);say(S,'rail dom',dom);results.rail=JSON.parse(dom);results.rail.list=JSON.parse(list);await fetch(`http://127.0.0.1:${RAIL}/shot?name=waiting`)}catch(e){say(S,'rail check failed',e.message)}};
if(process.env.RIG_ELECTRON)say('rig','rail window',await railUp());
const jobs=[];
if(SCENS.includes('claude'))jobs.push(hosted('claude',{onWaiting:process.env.RIG_ELECTRON?railCheck:undefined}));
if(SCENS.includes('codex'))jobs.push(hosted('codex',{provider:'codex',model:process.env.RIG_CODEX_MODEL||'gpt-5.6-luna'}));
if(SCENS.includes('killed'))jobs.push(hosted('killed',{secs:SECS+200,onWaiting:async({S,pids})=>{await sleep(1500);const host=hostOf(Number(pids[0]));say(S,`SIGKILL host engine ${host?'found':'NOT FOUND'}`);if(host)process.kill(host,'SIGKILL')}}));
async function holdRow(id){const l=await tool('workass_list_process_children',{machine_id:HID}).catch(e=>({error:e.message}));const rows=l.result?.children||l.children||[];return rows.find(x=>x.work_id===id||x.workId===id)||null}
async function holdCase(){const S='hold',start=async(label,sec,extra={})=>{const v=await tool('workass_start_process_child',{label:`rig ${label}`,cwd:CWD,operation_id:`${label}-${STAMP}`,machine_id:HID,resource:'rig-gpu',command:['/bin/sh','-c',`sleep ${sec}; echo done-${label}`],...extra});return (v.result||v).workId};
 const holder=await start('holder',40,{max_hold_ms:600000});say(S,'holder',holder);await sleep(4000);const waiter=await start('waiter',8,{max_hold_ms:15000,stale_after_ms:3600000});say(S,'waiter queued behind it with max_hold 15s, waits about 35s, holds 8s');const over=await start('overholder',22,{max_hold_ms:6000,stale_after_ms:3600000});
 const seen={};let last='';for(let i=0;i<150;i++){const rows=await Promise.all([waiter,over].map(holdRow));const line=rows.map((r,i)=>`${['waiter','over'][i]}:${r?.status}/q${r?.queue_position??'-'}/stalled=${r?.stalled??'-'}`).join(' ');if(line!==last){say(S,line);last=line}rows.forEach((r,i)=>{const k=['waiter','over'][i];if(r?.stalled)seen[k]=true});rows.forEach((r,i)=>{if(r)seen['saw'+i]=true});if(rows.every((r,i)=>seen['saw'+i]&&(!r||!['running','queued'].includes(r.status))))break;await sleep(1000)}
 const mw=await notices('rig waiter'),mo=await notices('rig overholder'),st=t=>(t.match(/<status>(.*?)<\/status>/)||[])[1];
 const stall=t=>/past the|still holding|stalled/i.test(t);const r={waiterStalledFlag:!!seen.waiter,waiterNotices:mw.map(st),waiterFalseStall:mw.some(stall),overStalledFlag:!!seen.over,overNotices:mo.map(st),overStallNotice:mo.some(stall)};say(S,'RESULT',JSON.stringify(r));results.hold=r;if(mw[0])say(S,'waiter notice',mw.at(-1).replace(/\n/g,' ').slice(0,420));}
if(SCENS.includes('hold'))jobs.push(holdCase());

const msgRow=async sid=>{const l=await tool('workass_list_subagents',{});const rows=l.result?.subagents||l.subagents||l.result||[];return (Array.isArray(rows)?rows:[]).find(x=>x.id===sid)};
const sendMsg=async(S,sid,text)=>{const op=`msg-${S}-${STAMP}`;try{const a=await tool('workass_message_subagent',{operation_id:op,subagent_id:sid,message:text});say(S,'MESSAGE ANSWER',JSON.stringify(a.result||a).slice(0,300));return a.result||a}catch(e){say(S,'MESSAGE REFUSED',e.message.slice(0,300));return{refused:e.message}}};
async function messageLive(){const S='live',nonce=`${S}${STAMP}`,token=`PONG-${nonce}`;
 const script=`${CWD}/fg-${nonce}.sh`;fs.writeFileSync(script,`#!/bin/sh\ni=0\nwhile [ $i -lt ${SECS} ]; do sleep 1; i=$((i+1)); done\necho finished\n`);
 const spawned=await tool('workass_spawn_subagent',{operation_id:`sub-${nonce}`,label:`rig ${S}`,task:`Use your Bash tool to run exactly this command in the foreground: sh ${script}\nWhen it returns, end your turn with one short sentence, following anything your coordinator has told you meanwhile.`,provider_id:'claude',model_id:'haiku',permission_intent:'full',cwd:CWD,machine_id:HID});
 const sid=(spawned.result||spawned).id||(spawned.result||spawned).subagent?.id||(spawned.result||spawned).subagentId;if(!sid)throw Error('no id '+JSON.stringify(spawned));say(S,'spawned',sid);
 for(let i=0;i<60;i++){const row=await msgRow(sid).catch(()=>null);if(row&&/Bash|sleep|tool/i.test(`${row.latestActivity||''} ${row.phase||''}`))break;await sleep(1000)}
 await sleep(3000);const answer=await sendMsg(S,sid,`Your final sentence must contain this exact token: ${token}`);
 let row;const cap=Date.now()+(SECS+180)*1000;for(;Date.now()<cap;await sleep(2000)){row=await msgRow(sid).catch(()=>null);if(row&&row.status!=='running')break}
 if(row?.status==='running'||!row)await tool('workass_cancel_subagent',{operation_id:`cancel-${nonce}`,subagent_id:sid}).catch(()=>{});
 await sleep(4000);const rc=await tool('workass_list_subagent_receipts',{}).catch(e=>({error:e.message}));const rec=(rc.result?.receipts||rc.receipts||[]).find(x=>x.subagentId===sid||x.id===sid);
 const r={answer:answer.delivery||answer.refused?.slice(0,120)||answer.error,status:rec?.status,tokenInResult:String(rec?.result||'').includes(token),result:String(rec?.result||'').slice(0,200)};say(S,'RESULT',JSON.stringify(r));results[S]=r}
async function messageParked(){const S='parked',token=`PONG-parked${STAMP}`;let answered;await hosted(S,{secs:Math.min(SECS,30),onWaiting:async({S,sid})=>{await sleep(1500);answered=await sendMsg(S,sid,`When your background command has finished, your final message must also contain this exact token: ${token}`)}});
 const rc=await tool('workass_list_subagent_receipts',{}).catch(e=>({error:e.message}));const label=`rig ${S}`;const rec=(rc.result?.receipts||rc.receipts||[]).filter(x=>x.label===label).at(-1);
 const r={answer:answered?.delivery||answered?.refused?.slice(0,120)||answered?.error,strategy:answered?.strategy,status:rec?.status,tokenInResult:String(rec?.result||'').includes(token),result:String(rec?.result||'').slice(0,200)};say(S,'MESSAGE RESULT',JSON.stringify(r));results[S+'-message']=r}
if(SCENS.includes('message')){jobs.push(messageLive());jobs.push(messageParked())}
await Promise.all(jobs.map(p=>p.catch(e=>say('rig','SCENARIO ERROR',e.message))));
if(SCENS.includes('ownerdown')){await hosted('ownerdown',{secs:Math.min(SECS,30),onWaiting:async({S,r})=>{r.down=true;say(S,'owner daemon stopping while the child waits on the host');const gone=new Promise(res=>od.once('exit',res));od.kill('SIGTERM');await gone;const m=`${CWD}/${S}${STAMP}.out`;for(let i=0;i<120&&!fs.existsSync(m);i++)await sleep(1000);await sleep(25000);say(S,`work finished while the owner was down: ${fs.existsSync(m)}`);od=boot('o',O,OS,OP,'localhost');await health(OP);say(S,'owner back');await sleep(4000)}}).catch(e=>say('rig','SCENARIO ERROR',e.message))}
// A chat takes one notice per turn, so the last may arrive a while after its row settled.
{const names=Object.keys(results).filter(k=>k!=='hold'&&k!=='rail'&&k!=='parked-message');for(let i=0;i<90;i++){const counts=await Promise.all(names.map(async n=>[n,(await notices(`rig ${n}`).catch(()=>[])).length]));if(counts.every(([,c])=>c>=1)||i===89){for(const [n,c] of counts){results[n].notices=c;const texts=await notices(`rig ${n}`).catch(()=>[]);results[n].noticeStatus=texts.map(t=>(t.match(/<status>(.*?)<\/status>/)||[])[1]);results[n].noticeSummary=texts.map(t=>(t.match(/<summary>(.*?)<\/summary>/)||[])[1])}say('rig','NOTICES',JSON.stringify(Object.fromEntries(names.map(n=>[n,results[n].noticeSummary]))),`after ${i*2}s`);break}await sleep(2000)}}
fs.writeFileSync(`${ROOT}/results.json`,JSON.stringify(results,null,1));say('rig','DONE',ROOT.replace(REPO,''));bye();await sleep(1500);fs.rmSync(CWD,{recursive:true,force:true});
const proj=`${os.homedir()}/.claude/projects/${CWD.replace(/[^A-Za-z0-9]/g,'-')}`;fs.rmSync(proj,{recursive:true,force:true});process.exit(0);
