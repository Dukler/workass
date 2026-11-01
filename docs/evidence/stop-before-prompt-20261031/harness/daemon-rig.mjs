// daemon-rig.mjs before|after ROUNDS [PROVIDER] [MODEL]: sealed bundle's real daemon, headless, scratch state, real provider; Stop raced against the prompt over the frozen wire.
import {spawn,execSync} from 'node:child_process'; import crypto from 'node:crypto'; import fs from 'node:fs'; import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const [WHICH,ROUNDS='3',PROVIDER='claude',MODEL='haiku']=process.argv.slice(2);
const R='/Users/dev/Workspace/workass/.dev/stop-race/rig',BIN=`${R}/${WHICH}.app/Contents/Resources/runtime/workass`,PORT=28990,STATE=`${R}/state-${WHICH}-${Date.now()}`,CWD=`${R}/cwd`;
fs.mkdirSync(STATE,{recursive:true});fs.mkdirSync(CWD,{recursive:true});
const log=fs.openSync(`${R}/logs/daemon-${WHICH}.log`,'a');
const d=spawn(BIN,['--prod','--headless','--state-dir',STATE,'--port',String(PORT),'--bind','localhost','--beacon=false'],{cwd:CWD,stdio:['ignore',log,log],env:{HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:`${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`,WORKASS_PROFILE:'prod',...(process.env.HOLD==='1'?{NODE_OPTIONS:'--import=/Users/dev/Workspace/workass/.dev/stop-race/stdin-hold.mjs',RIG_HOLD_LOG:`${R}/logs/hold-${WHICH}.log`}:{}),WORKASS_DATA_ROOT:R,WORKASS_LOG_ROOT:`${R}/logs`,WORKASS_UPDATE_CHANNEL:'local',WORKASS_BROWSER_CONTROL_FILE:`${R}/browser-control.json`}});
const stop=()=>{try{d.kill('SIGTERM')}catch{}};process.on('exit',stop);
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
for(let i=0;i<120;i++){try{const r=await fetch(`https://127.0.0.1:${PORT}/workass/health`);if(r.ok){console.log('health',JSON.stringify(await r.json()).slice(0,90));break}}catch{}await sleep(500)}
const lsof=execSync(`lsof -nP -a -p ${d.pid} -iTCP -iUDP 2>/dev/null || true`).toString().split('\n').slice(1).map(l=>l.split(/\s+/).slice(7,9).join(' ')).filter(Boolean);const leak=lsof.filter(l=>!/127\.0\.0\.1|\[::1\]|\*:/.test(l));console.log('nonLoopbackSockets',JSON.stringify(leak));if(leak.length){stop();throw Error('rig reached the LAN')}
const ws=new WebSocket(`wss://127.0.0.1:${PORT}/ws`);await new Promise((r,j)=>{ws.onopen=r;ws.onerror=()=>j(Error('ws'))});
let seq=0;const pend=new Map(),ends=new Map(),waiters=[];
ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'){const p=pend.get(m.id);if(p){pend.delete(m.id);m.error?p.j(Error(typeof m.error==='string'?m.error:JSON.stringify(m.error))):p.r(m.result)}}
 else if(m.channel==='job:event'){const a=m.payload??m.args?.[0]??m.data;if(a?.type==='end'&&a.job?.id){ends.set(a.job.id,a.job);for(const w of [...waiters])if(w.id===a.job.id){waiters.splice(waiters.indexOf(w),1);w.r(a.job)}}}};
const inv=(channel,...args)=>new Promise((r,j)=>{const id='x'+(++seq);pend.set(id,{r,j});ws.send(JSON.stringify({t:'invoke',id,channel,args}));setTimeout(()=>{if(pend.delete(id))j(Error('timeout '+channel))},60000).unref()});
const waitEnd=(id,ms)=>new Promise(r=>{if(ends.has(id))return r(ends.get(id));const w={id,r};waiters.push(w);setTimeout(()=>{r(null)},ms).unref()});
const jobId=(chat,op)=>{const h=crypto.createHash('sha256');for(const p of ['workass-job-v1',chat,op])h.update(`${Buffer.byteLength(p)}:${p}`);return 'app-chat-'+h.digest('hex').slice(0,32)};
let tab='tab-'+Date.now().toString(36),chat='conv-'+Date.now().toString(36);
for(let i=0;;i++){try{await inv('chat:create',{tabId:tab,chatId:chat,operationId:'create-'+chat+'-'+i,title:'stop rig',cwd:CWD,providerId:PROVIDER,currentModelId:MODEL,currentModeId:'bypassPermissions',focus:false});console.log('chat created after tries',i);break}catch(e){if(i>120)throw e;if(i===0||i%20===0)console.log('create:',e.message);await sleep(1000)}}
const start=(op,prompt)=>inv('job:start',{tabId:tab,chatId:chat,operationId:op,userMessageId:'u-'+op,assistantMessageId:'a-'+op,prompt,cwd:CWD,permissionMode:'bypassPermissions',providerId:PROVIDER,modelId:MODEL});
const holdLog=`${R}/logs/hold-${WHICH}.log`;const holds=()=>{try{return (fs.readFileSync(holdLog,'utf8').match(/holding/g)||[]).length}catch{return 0}};
if(process.env.NEWCHAT==='1'){const out={ranAfterAcceptedStop:0,stopped:0,finishedFirst:0,noEnd:0,other:0};let okFollow=0,N=Number(ROUNDS);
 for(let k=1;k<=N;k++){tab=`tab-${k}-${Date.now().toString(36)}`;chat=`conv-${k}-${Date.now().toString(36)}`;
  await inv('chat:create',{tabId:tab,chatId:chat,operationId:'create-'+chat,title:'stop rig',cwd:CWD,providerId:PROVIDER,currentModelId:MODEL,currentModeId:'bypassPermissions',focus:false});
  const before=holds(),word=`RAN${k}Z`,o=`n${k}-${Date.now().toString(36)}`,id=jobId(chat,o);let st;for(let i=0;i<120;i++){try{st=await start(o,`Reply with only the word ${word}.`);break}catch(e){await sleep(1000)}}
  for(let i=0;i<6000&&holds()===before;i++)await sleep(5);const held=holds()>before;
  const c=await inv('job:cancel',id).catch(e=>({error:e.message}));const e=await waitEnd(id,60000);const text=String(e?.result||''),ran=text.includes(word),acc=!!c?.cancelled;await sleep(500);
  const kind=!e?'noEnd':ran&&acc?'ranAfterAcceptedStop':ran?'finishedFirst':(e.stopReason==='cancelled'||e.interrupted)?'stopped':'other';out[kind]++;
  const fo=`f${k}-${Date.now().toString(36)}`;await start(fo,'Reply with only the word ALIVE.');const f=await waitEnd(jobId(chat,fo),90000);const alive=f?.status==='done'&&String(f?.result||'').includes('ALIVE');if(alive)okFollow++;
  console.log(JSON.stringify({k,held,kind,cancel:c?.reason,pre:c?.preAdmission,status:e?.status,stop:e?.stopReason,ran,follow:f?.status,alive}),String(f?.error||e?.error||'').slice(0,100))}
 console.log(`RESULT ${WHICH} newchat ${PROVIDER}/${MODEL} n=${N} ${JSON.stringify(out)} followUpsAlive=${okFollow}/${N}`);stop();await sleep(1500);try{execSync(`pkill -f 'stop-race/rig/${WHICH}.app' || true`)}catch{};process.exit(0)}
let op;for(let i=0;;i++){op='warm-'+i+'-'+Date.now().toString(36);try{await start(op,'Reply with only the word READY.');console.log('warm start after tries',i);break}catch(e){if(i>120)throw e;await sleep(1000)}}const w=await waitEnd(jobId(chat,op),120000);console.log('warm',w?.status,w?.stopReason,String(w?.result||'').slice(0,40));
const hostPid=()=>{const o=execSync(`pgrep -P ${d.pid} -f ${PROVIDER}-native-host || pgrep -P ${d.pid} -f ${PROVIDER} || true`).toString().trim().split('\n').filter(Boolean);return o.map(Number)};
const FROZEN=process.env.FROZEN==='1',HOLD=process.env.HOLD==='1';
const DELAYS=FROZEN||HOLD?[0,5,20,60]:[0,0,0,2,5,10,20,40,80,150,300,600];let tally={ranAfterAcceptedStop:0,stopped:0,finishedFirst:0,noEnd:0,other:0};const rows=[];let n=0;
for(let round=1;round<=Number(ROUNDS);round++)for(const delay of DELAYS){n++;op=`t${n}-${Date.now().toString(36)}`;const id=jobId(chat,op),word=`RAN${n}Z`;
 const hp=FROZEN?hostPid():[];if(FROZEN&&hp.length!==1)throw Error('host pids '+hp);if(FROZEN)process.kill(hp[0],'SIGSTOP');
 const started=start(op,`Reply with only the word ${word}.`).catch(e=>({startError:e.message}));if(delay)await sleep(delay);
 if(FROZEN){await started;await sleep(150);setTimeout(()=>{process.kill(hp[0],'SIGCONT')},150)}
 const t0=Date.now();let c,tries=0;do{c=await inv('job:cancel',id).catch(e=>({error:e.message}));if(c?.reason==='unknown'||c?.Reason==='unknown')await sleep(3);tries++}while((c?.reason==='unknown'||c?.Reason==='unknown')&&tries<400);
 await started;let end=await waitEnd(id,45000);const text=String(end?.result||'');await sleep(400);
 const ran=text.includes(word),accepted=!!(c?.cancelled||c?.Cancelled),reason=c?.reason||c?.Reason||'';
 let kind;if(!end)kind='noEnd';else if(ran&&accepted)kind='ranAfterAcceptedStop';else if(ran)kind='finishedFirst';else if(end.stopReason==='cancelled'||end.interrupted)kind='stopped';else kind='other';tally[kind]++;
 rows.push({n,delay,kind,cancel:reason,pre:c?.preAdmission,status:end?.status,stop:end?.stopReason,ran,ms:Date.now()-t0});console.log(JSON.stringify(rows.at(-1)),String(end?.error||'').slice(0,80));}
op='after-'+Date.now().toString(36);await start(op,'Reply with only the word ALIVE.');const a=await waitEnd(jobId(chat,op),120000);
console.log(`RESULT ${WHICH} ${PROVIDER}/${MODEL} n=${n} ${JSON.stringify(tally)} followUp=${a?.status}/${a?.stopReason} ${String(a?.result||'').slice(0,20)}`);
fs.writeFileSync(`${R}/rows-${WHICH}-${PROVIDER}-${Date.now()}.json`,JSON.stringify(rows));
stop();await sleep(1500);try{execSync(`pkill -f 'stop-race/rig/${WHICH}.app' || true`)}catch{};process.exit(0);
