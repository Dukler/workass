// rig.mjs BINARY LABEL: one daemon build on scratch state, loopback only, real Claude Haiku.
// A human turn spawns child A and starts a background shell. The shell ending makes Claude
// start a turn by itself, and that self-started turn spawns child B. The parent then messages
// both, with no turn running and inside a later human turn, then waits on and cancels both.
import {spawn,execFileSync} from 'node:child_process'; import fs from 'node:fs'; import os from 'node:os'; import path from 'node:path'; import crypto from 'node:crypto';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const [BIN_ARG,LABEL]=process.argv.slice(2); const BIN=path.resolve(BIN_ARG);
const ROOT=path.resolve(new URL('../../../..',import.meta.url).pathname);
const R=path.join(ROOT,'.dev/subagent-owner-rig',LABEL); fs.rmSync(R,{recursive:true,force:true});
// The chat's cwd sits outside the repository so the agents do not load its instructions.
const STATE=`${R}/state`,CWD=path.join(fs.realpathSync(os.tmpdir()),`wa-owner-rig-${LABEL}`),LOGS=`${R}/logs`,PORT=Number(process.env.RIG_PORT||28991);
fs.rmSync(CWD,{recursive:true,force:true});
for(const d of [STATE,CWD,LOGS])fs.mkdirSync(d,{recursive:true});
// The parent's selection carries an effort; the children name the model and leave effort to inherit.
const MODEL=process.env.RIG_MODEL||'haiku[low]',CHILD_MODEL=process.env.RIG_CHILD_MODEL||'haiku';
const say=(...a)=>console.log(`[${LABEL}]`,...a);
// The parent model runs this; the environment it runs in carries the chat's own tool context.
// A spawn with no cwd is what a chat normally sends. Refused, it is retried with an explicit cwd
// so the message path can still be tested on the unfixed build; both answers are logged.
fs.writeFileSync(`${R}/spawn.sh`,`#!/bin/sh
TASK="Use the Bash tool to run: ping -c 200 -i 1 127.0.0.1 > /dev/null (set its timeout to 300000; run it in the foreground). Then reply with only the word DONE."
BASE="\\"label\\":\\"$1\\",\\"task\\":\\"$TASK\\",\\"provider_id\\":\\"claude\\",\\"model_id\\":\\"${CHILD_MODEL}\\",\\"permission_intent\\":\\"full\\""
if printf '{"operation_id":"spawn-%s",%s}' "$1" "$BASE" | "$WORKASS_TOOLS_COMMAND" tools call workass_spawn_subagent > "${R}/spawn-$1.nocwd.log" 2>&1; then echo SPAWNED-NOCWD; exit 0; fi
printf '{"operation_id":"spawn-cwd-%s",%s,"cwd":"${CWD}"}' "$1" "$BASE" | "$WORKASS_TOOLS_COMMAND" tools call workass_spawn_subagent > "${R}/spawn-$1.cwd.log" 2>&1 && echo SPAWNED-WITH-CWD
`);
const env={HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',
 PATH:`${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`,WORKASS_PROFILE:'dev',WORKASS_DATA_ROOT:R,WORKASS_LOG_ROOT:LOGS,
 WORKASS_UPDATE_CHANNEL:'local',WORKASS_BROWSER_CONTROL_FILE:`${R}/bc.json`,WORKASS_CLAUDE_SDK_MODULE:process.env.RIG_SDK};
const log=fs.openSync(`${LOGS}/daemon.log`,'a');
const d=spawn(BIN,['--headless','--state-dir',STATE,'--port',String(PORT),'--bind','localhost','--beacon=false'],{cwd:ROOT,stdio:['ignore',log,log],env});
let finishing=false;const finish=code=>{if(finishing)return;finishing=true;try{d.kill('SIGTERM')}catch{};setTimeout(()=>{fs.rmSync(CWD,{recursive:true,force:true});process.exit(code)},2500)};
process.on('uncaughtException',e=>{say('RIG ERROR',e.message);finish(2)});process.on('unhandledRejection',e=>{say('RIG ERROR',String(e?.message||e));finish(2)});
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
setTimeout(()=>{say('RIG TIMEOUT');finish(2)},360000).unref();
let up=false;for(let i=0;i<120&&!up;i++){try{up=(await fetch(`https://127.0.0.1:${PORT}/workass/health`)).ok}catch{};if(!up)await sleep(500)}
if(!up){say('daemon never answered');finish(2)}
const sockets=execFileSync('/bin/sh',['-c',`lsof -nP -a -p ${d.pid} -iTCP -iUDP 2>/dev/null || true`]).toString().split('\n').slice(1).map(l=>l.split(/\s+/).slice(7,9).join(' ')).filter(Boolean);
const leak=sockets.filter(l=>!/127\.0\.0\.1|\[::1\]|\*:/.test(l));say('nonLoopbackSockets',JSON.stringify(leak));if(leak.length){say('rig reached the LAN');finish(2)}
const ws=new WebSocket(`wss://127.0.0.1:${PORT}/ws`);await new Promise((r,j)=>{ws.onopen=r;ws.onerror=()=>j(Error('ws'))});
let seq=0;const pend=new Map(),jobs=new Map();
ws.onmessage=e=>{const m=JSON.parse(e.data);
 if(m.t==='reply'){const p=pend.get(m.id);if(p){pend.delete(m.id);m.error?p.j(Error(typeof m.error==='string'?m.error:JSON.stringify(m.error))):p.r(m.result)}}
 else if(m.channel==='job:event'){const a=m.payload??m.args?.[0]??m.data;if(a?.job?.id&&(a.type==='start'||a.type==='end'))jobs.set(a.job.id,{type:a.type,status:a.job.status})}};
const inv=(channel,...args)=>new Promise((r,j)=>{const id='x'+(++seq);pend.set(id,{r,j});ws.send(JSON.stringify({t:'invoke',id,channel,args}));setTimeout(()=>{if(pend.delete(id))j(Error('timeout '+channel))},60000).unref()});
const until=async(what,ms,fn)=>{const end=Date.now()+ms;while(Date.now()<end){const v=await fn();if(v)return v;await sleep(500)}say('GAVE UP waiting for',what);throw Error(what)};
const stamp=Date.now().toString(36),tab='tab-'+stamp,chat='conv-'+stamp;
const jobId=op=>{const h=crypto.createHash('sha256');for(const p of ['workass-job-v1',chat,op])h.update(`${Buffer.byteLength(p)}:${p}`);return 'app-chat-'+h.digest('hex').slice(0,32)};
await until('chat created',120000,()=>inv('chat:create',{tabId:tab,chatId:chat,operationId:'create-'+chat,title:'owner rig',cwd:CWD,providerId:'claude',currentModelId:MODEL,currentModeId:'bypassPermissions',focus:false}).then(()=>true,()=>false));
const start=(op,prompt)=>until('job start '+op,120000,()=>inv('job:start',{tabId:tab,chatId:chat,operationId:op,userMessageId:'u-'+op,assistantMessageId:'a-'+op,prompt,cwd:CWD,permissionMode:'bypassPermissions',providerId:'claude',modelId:MODEL}).then(()=>true,e=>{say('start:',e.message.slice(0,120));return false}));
let ctx='';
const findCtx=()=>{if(!fs.existsSync(`${STATE}/tool-bootstrap`))return false;for(const dir of fs.readdirSync(`${STATE}/tool-bootstrap`,{withFileTypes:true})){if(!dir.isDirectory())continue;const f=`${STATE}/tool-bootstrap/${dir.name}/context.json`;try{if(JSON.parse(fs.readFileSync(f,'utf8')).chat_id===chat){ctx=f;return true}}catch{}}return false};
const tool=(name,input)=>new Promise(res=>{const p=spawn(BIN,['tools','--context',ctx,'call',name],{env:{HOME:env.HOME,PATH:env.PATH,TMPDIR:env.TMPDIR},stdio:['pipe','pipe','pipe']});let o='',er='';p.stdout.on('data',b=>o+=b);p.stderr.on('data',b=>er+=b);p.on('close',code=>res({code,out:o.trim(),err:er.trim()}));p.stdin.end(JSON.stringify(input))});
const pair=()=>({tab_id:tab,chat_id:chat});
const parse=r=>{try{return JSON.parse(r.out)}catch{return null}};
const oneLine=r=>{const p=parse(r);return p?JSON.stringify(p).slice(0,170):`exit ${r.code}: ${(r.err||r.out).split('\n').pop().slice(0,170)}`};
await start('t1-'+stamp,`Do exactly these steps, in order, and nothing else.
1. Bash: sh ${R}/spawn.sh human-child
2. Bash with run_in_background set to true: sleep 5; echo BG-FINISHED
3. Reply with only the word STARTED and end your turn; do not wait, poll or sleep.
Later, when you are told that background task finished, do this: Bash: sh ${R}/spawn.sh wake-child
Then reply with only the word SPAWNED and end your turn.`);
await until('tool context',60000,findCtx);
const rows=await until('both children running and no turn running',150000,async()=>{
 const list=parse(await tool('workass_list_subagents',pair()));const runs=list?.subagents||list?.runs||list?.items||[];
 const running=[...jobs.entries()].filter(([,v])=>v.type==='start');
 if(runs.filter(r=>r.status==='running').length>=2&&running.length===0&&[...jobs.keys()].some(k=>k.startsWith('harness-turn-')))return runs;return null});
const byLabel=l=>rows.find(r=>r.label===l);const A=byLabel('human-child'),B=byLabel('wake-child');
say('turns seen',JSON.stringify([...jobs.keys()].map(k=>k.replace(/^(app-chat|harness-turn)-.*/,'$1'))));
say('rows',JSON.stringify(rows.map(r=>({label:r.label,status:r.status,turn:String(r.rootJobId||'').replace(/^(app-chat|harness-turn)-.*/,'$1'),model:r.modelLabel}))));
const spawnNote=f=>{try{const t=fs.readFileSync(`${R}/spawn-${f}`,'utf8').trim();const j=JSON.parse(t);return j.error?`refused: ${j.error}`:`ok effort=${j.effort} cwd=${j.cwd===CWD?'chat cwd':j.cwd}`}catch{return 'not run'}};
say('spawn human-child no cwd:',spawnNote('human-child.nocwd.log'));say('spawn wake-child  no cwd:',spawnNote('wake-child.nocwd.log'));say('spawn wake-child  explicit cwd:',spawnNote('wake-child.cwd.log'));
await sleep(8000); // both children past session start, inside their ping.
const msg=async(run,tag)=>oneLine(await tool('workass_message_subagent',{...pair(),subagent_id:run.id,operation_id:`m-${tag}-${run.label}-${stamp}`,message:'Rig note: keep going, no reply needed.'}));
say('NO TURN   message human-child:',await msg(A,'idle'));
say('NO TURN   message wake-child: ',await msg(B,'idle'));
say('NO TURN   wait    wake-child: ',oneLine(await tool('workass_wait_subagent',{...pair(),subagent_id:B.id,operation_id:`w-idle-${stamp}`,timeout_ms:1000})));
const t2='t2-'+stamp;await start(t2,'Use the Bash tool to run: ping -c 30 -i 1 127.0.0.1 > /dev/null. Then reply with only the word OK.');
await until('second human turn running',60000,()=>jobs.get(jobId(t2))?.type==='start');await sleep(1500);
say('IN TURN   message human-child:',await msg(A,'turn'));
say('IN TURN   message wake-child: ',await msg(B,'turn'));
say('IN TURN   wait    wake-child: ',oneLine(await tool('workass_wait_subagent',{...pair(),subagent_id:B.id,operation_id:`w-turn-${stamp}`,timeout_ms:1000})));
say('IN TURN   cancel  wake-child: ',oneLine(await tool('workass_cancel_subagent',{...pair(),subagent_id:B.id,operation_id:`c-turn-${stamp}`})));
say('IN TURN   cancel  human-child:',oneLine(await tool('workass_cancel_subagent',{...pair(),subagent_id:A.id,operation_id:`c-turn-a-${stamp}`})));
const after=parse(await tool('workass_list_subagents',pair()));
say('final list',JSON.stringify((after?.subagents||after?.runs||after?.items||[]).map(r=>({label:r.label,status:r.status}))));
await inv('job:cancel',jobId(t2)).catch(()=>{});
finish(0);
