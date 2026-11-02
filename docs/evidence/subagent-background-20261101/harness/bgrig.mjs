// bgrig.mjs <before|after> <scenario> [seconds] [provider] [model]
// A real daemon, scratch state, loopback only, a real provider; a real tracked subagent that
// starts a background task and ends its turn. Prints the subagent's status, whether the process is
// alive, and what the parent chat is told. Scenarios: shell, monitor, abandon (see TASKS).
import {spawn,execFileSync,execSync} from 'node:child_process';import fs from 'node:fs';import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const [WHICH='before',SCEN='shell',SECS='120',PROVIDER='claude',MODEL='haiku']=process.argv.slice(2);
const M=path.resolve(new URL('../../../..',import.meta.url).pathname),R=`${M}/.dev/bgrig`,V=`${R}/${WHICH}`,BIN=`${V}/workass`,PORT=Number(process.env.RIG_PORT||28995),STAMP=`${Date.now().toString(36)}${process.pid.toString(36)}`,RUN=`${WHICH}-${SCEN}-${STAMP}`,A=`${R}/runs/${RUN}`,STATE=`${A}/state`,CWD=`${A}/cwd`;
fs.mkdirSync(STATE,{recursive:true});fs.mkdirSync(CWD,{recursive:true});
const nonce=`bgrig${STAMP}`,marker=`${CWD}/${nonce}.out`;
const t0=Date.now(),log=(...a)=>{const line=`+${String(Math.round((Date.now()-t0)/1000)).padStart(4)}s ${a.join(' ')}`;console.log(line);fs.appendFileSync(`${A}/timeline.log`,line+'\n')};
const ENV={HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:`${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`,
 WORKASS_PROFILE:'prod',WORKASS_DATA_ROOT:A,WORKASS_LOG_ROOT:`${A}/logs`,WORKASS_UPDATE_CHANNEL:'local',WORKASS_BROWSER_CONTROL_FILE:`${A}/browser-control.json`,
 WORKASS_CLAUDE_HOST:`${V}/hosts/claude-native-host.mjs`,WORKASS_CODEX_HOST:`${V}/hosts/codex-native-host.mjs`,WORKASS_CLAUDE_SDK_MODULE:`${M}/dist-bin/frontier-hosts/darwin-arm64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs`};
for(const[k,v]of Object.entries(process.env))if(k.startsWith('RIG_ENV_'))ENV[k.slice(8)]=v;
const dlog=fs.openSync(`${A}/daemon.log`,'a');
let d=spawn(BIN,['--prod','--headless','--state-dir',STATE,'--port',String(PORT),'--bind','localhost','--beacon=false'],{cwd:CWD,stdio:['ignore',dlog,dlog],env:ENV});
const stop=()=>{try{d.kill('SIGTERM')}catch{}};process.on('exit',stop);process.on('SIGTERM',()=>process.exit(1));
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
let up=false;for(let i=0;i<120&&!up;i++){try{up=(await fetch(`https://127.0.0.1:${PORT}/workass/health`)).ok}catch{}if(!up)await sleep(500)}if(!up)throw Error('no health');
const leak=execSync(`lsof -nP -a -p ${d.pid} -iTCP -iUDP 2>/dev/null || true`).toString().split('\n').slice(1).map(l=>l.split(/\s+/).slice(7,9).join(' ')).filter(Boolean).filter(l=>!/127\.0\.0\.1|\[::1\]|\*:/.test(l));if(leak.length){stop();throw Error('rig reached the LAN '+leak)}
const ws=new WebSocket(`wss://127.0.0.1:${PORT}/ws`);await new Promise((r,j)=>{ws.onopen=r;ws.onerror=()=>j(Error('ws'))});
let seq=0;const pend=new Map();ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'){const p=pend.get(m.id);if(p){pend.delete(m.id);m.error?p.j(Error(JSON.stringify(m.error))):p.r(m.result)}}};
const inv=(channel,...args)=>new Promise((r,j)=>{const id='x'+(++seq);pend.set(id,{r,j});ws.send(JSON.stringify({t:'invoke',id,channel,args}));setTimeout(()=>{if(pend.delete(id))j(Error('timeout '+channel))},60000).unref()});
const tab=`tab-${nonce}`,chat=`conv-${nonce}`;
for(let i=0;;i++){try{await inv('chat:create',{tabId:tab,chatId:chat,operationId:`create-${nonce}-${i}`,title:'bg rig',cwd:CWD,providerId:'claude',currentModelId:'haiku',currentModeId:'bypassPermissions',focus:false});break}catch(e){if(i>120)throw e;await sleep(1000)}}
const op=`warm-${nonce}`;for(let i=0;;i++){try{await inv('job:start',{tabId:tab,chatId:chat,operationId:op,userMessageId:'u-'+op,assistantMessageId:'a-'+op,prompt:'Reply with only the word READY.',cwd:CWD,permissionMode:'bypassPermissions',providerId:'claude',modelId:'haiku'});break}catch(e){if(i>120)throw e;await sleep(1000)}}
let ctx;for(let i=0;i<200&&!ctx;i++){const dir=`${STATE}/tool-bootstrap`;try{for(const b of fs.readdirSync(dir)){try{const c=JSON.parse(fs.readFileSync(`${dir}/${b}/context.json`));if(c.chat_id===chat){ctx=`${A}/ctx.json`;fs.copyFileSync(`${dir}/${b}/context.json`,ctx)}}catch{}}}catch{}if(!ctx)await sleep(300)}if(!ctx)throw Error('no tool context');
const tool=(name,input)=>JSON.parse(execFileSync(BIN,['tools','--context',ctx,'call',name],{input:JSON.stringify(input),env:ENV,cwd:'/tmp',encoding:'utf8',timeout:60000}));
const script=`${CWD}/sweep-${nonce}.sh`,hb=`${CWD}/${nonce}.hb`;
fs.writeFileSync(script,`#!/bin/sh
echo "started pid=$$ $(date +%T)" >> ${hb}
for sig in TERM HUP INT; do trap "echo signal $sig at \\$(date +%T) >> ${hb}; exit 143" $sig; done
i=0; while [ $i -lt ${SECS} ]; do sleep 1; i=$((i+1)); date +%T > ${hb}.tick; done
echo SWEEP-DONE-${nonce} > ${marker}
`);
const CMD=`sh ${script}`;
const END=`End your turn at once with exactly this sentence: "The sweep is running and the notification will report when it finishes." Do not wait, poll, sleep or check on it in that turn.`;
const LATER=`If you are later told the task finished, read ${marker} and reply with "RESULT:" followed by its contents.`;
const alive=()=>{try{return execSync(`pgrep -f 'sweep-${nonce}.sh' || true`).toString().trim().split('\n').filter(Boolean).length>0}catch{return false}};
const TASKS={
 shell:`Use your Bash tool with run_in_background set to true to start this exact command: ${CMD}\n${END}\n${LATER}`,
 monitor:`Use your Monitor tool (load it with ToolSearch if needed) with timeout_ms 600000 to watch this exact command, which prints one line when it is done: ${CMD} ; echo FINISHED-${nonce}\n${END}\n${LATER}`,
 codexyield:`Use your exec_command tool to start this exact command with yield_time_ms 1000 so it keeps running after the tool returns: ${CMD}\n${END}\n${LATER}`,
 codexamp:`Start this exact command detached with your shell tool, so the tool returns at once: nohup ${CMD} >/dev/null 2>&1 &\n${END}\n${LATER}`,
 abandon:`Use your Bash tool with run_in_background set to true to start this exact command: ${CMD}\n${END}\nIf you are later told anything about that task, reply only "NOTED".`,
};
const spawnInput={operation_id:`sub-${nonce}`,label:`bg ${SCEN}`,task:TASKS[SCEN],provider_id:PROVIDER,model_id:MODEL,permission_intent:'full',...(process.env.RIG_EFFORT?{effort:process.env.RIG_EFFORT}:{})};
for(let i=0;i<240;i++){const ms=(()=>{try{const x=tool('workass_read_chat',{tab_id:tab,chat_id:chat,limit:20});return x.result?.messages||x.messages||[]}catch{return[]}})();if(ms.some(m=>m.role==='assistant'&&m.status==='done'))break;await sleep(500)}
if(process.env.RIG_CONTROL==='chat'){const ctl=`ctl-${nonce}`;await inv('job:start',{tabId:tab,chatId:chat,operationId:ctl,userMessageId:'u-'+ctl,assistantMessageId:'a-'+ctl,prompt:TASKS[SCEN],cwd:CWD,permissionMode:'bypassPermissions',providerId:'claude',modelId:'haiku'}).catch(e=>log('ctl start',e.message));
 let l='',seen=false,end=Date.now()+(Number(SECS)+150)*1000;while(Date.now()<end){const p=alive(),f=fs.existsSync(marker);const ms=(()=>{try{const x=tool('workass_read_chat',{tab_id:tab,chat_id:chat,limit:30});return x.result?.messages||x.messages||[]}catch{return[]}})();const last=ms.filter(m=>m.role==='assistant').at(-1);const txt=String(last?.content||last?.text||'');const line=`CONTROL process=${p?'alive':'gone'} marker=${f?'written':'absent'} lastReply=${last?.status}:${JSON.stringify(txt.slice(0,90))}`;if(line!==l){log(line);l=line}if(txt.includes('SWEEP-DONE-'+nonce)){seen=true;break}await sleep(1000)}
 log('HEARTBEAT',fs.existsSync(hb)?fs.readFileSync(hb,'utf8').trim():'(none)');log(`CONTROL VERDICT selfWoke=${seen} workFinished=${fs.existsSync(marker)}`);stop();await sleep(1500);if(alive())try{execSync(`pkill -f 'sweep-${nonce}.sh'`)}catch{};process.exit(0)}
const spawned=tool('workass_spawn_subagent',spawnInput);const out=spawned.result||spawned;const sid=out.id||out.subagent?.id||out.subagentId||out.receiptId;if(!sid)throw Error('no id '+JSON.stringify(spawned).slice(0,300));log(`spawned ${sid} provider=${PROVIDER} model=${MODEL} background=${SECS}s nonce=${nonce}`);
const subagent=()=>{const l=tool('workass_list_subagents',{});const rows=l.result?.subagents||l.subagents||l.result||[];return (Array.isArray(rows)?rows:[]).find(x=>x.id===sid)||null};
const ACTION=process.env.RIG_ACTION||'';let acted=false;
const hostOf=pid=>{for(let p=pid,i=0;i<8&&p>1;i++){const line=execSync(`ps -o ppid=,command= -p ${p} || true`).toString().trim();if(!line)return 0;if(/claude-native-host|codex-native-host/.test(line))return p;p=Number(line.split(/\s+/)[0])}return 0};
const act=async()=>{if(ACTION==='kill-engine'){const pid=Number(execSync(`pgrep -f 'sweep-${nonce}.sh' | head -1`).toString().trim());const host=hostOf(pid);log(`ACTION kill-engine: SIGKILL host ${host} of sweep ${pid}`);if(!host)throw Error('no host');process.kill(host,'SIGKILL')}
 else if(ACTION==='restart'){log('ACTION restart: SIGTERM daemon');const gone=new Promise(r=>d.once('exit',r));d.kill('SIGTERM');await gone;d=spawn(BIN,['--prod','--headless','--state-dir',STATE,'--port',String(PORT),'--bind','localhost','--beacon=false'],{cwd:CWD,stdio:['ignore',dlog,dlog],env:ENV});let ok=false;for(let i=0;i<120&&!ok;i++){try{ok=(await fetch(`https://127.0.0.1:${PORT}/workass/health`)).ok}catch{}if(!ok)await sleep(500)}log('daemon back');await sleep(4000)}};
let last='',settledAt=0,notice='';const CAP=Number(process.env.RIG_CAP||Number(SECS)+240)*1000,GRACE=Number(process.env.RIG_GRACE||30)*1000;
for(;;){let s=null;try{s=subagent()}catch(e){log('list error',String(e.message).slice(0,160))}
 const p=alive(),f=fs.existsSync(marker);const line=`status=${s?.status} phase=${s?.phase} activity=${JSON.stringify(s?.latestActivity||'')} process=${p?'alive':'gone'} marker=${f?'written':'absent'}`;
 if(line!==last){log(line);last=line}
 if(ACTION&&!acted&&s?.phase==='waiting_background'&&p){acted=true;await sleep(1500);await act();if(ACTION==='restart')break}
 if(s&&s.status!=='running'&&!settledAt)settledAt=Date.now();
 if(settledAt&&p&&Date.now()-settledAt>GRACE){log('STILL MATCHING AFTER SETTLE:',execSync(`ps -axo pid,ppid,command | grep 'sweep-${nonce}' | grep -v grep || true`).toString().replace(/\n/g,' | ').slice(0,400));break}
 if(settledAt&&Date.now()-settledAt>GRACE&&!p)break;if(Date.now()-t0>CAP){log('CAP reached');break}await sleep(500)}
const receipts=tool('workass_list_subagent_receipts',{});const rl=receipts.result?.receipts||receipts.receipts||[];const rec=rl.find(x=>x.subagentId===sid||x.receiptId===sid);
log('RECEIPT',JSON.stringify({status:rec?.status,stopReason:rec?.stopReason,elapsedMs:rec?.elapsedMs,result:String(rec?.result||'').slice(0,400),error:rec?.error,backgroundWork:rec?.backgroundWork}));
const read=tool('workass_read_chat',{tab_id:tab,chat_id:chat,limit:50});const msgs=read.result?.messages||read.messages||[];
for(const m of msgs){const t=String(m.content||m.text||'');if(t.includes('<task-notification>'))log('PARENT TOLD:',t.replace(/\n/g,' ').slice(0,900))}
const rd=f=>{try{return fs.readFileSync(f,'utf8').trim().replace(/\n/g,' | ')}catch{return'(none)'}};log('HEARTBEAT',rd(hb),'lastTick='+rd(hb+'.tick'));
const lost=!fs.existsSync(marker);log(`VERDICT parentSawStatus=${rec?.status} workFinished=${!lost} childReadResult=${String(rec?.result||'').includes('SWEEP-DONE-'+nonce)}`);
stop();await sleep(1500);if(alive())try{execSync(`pkill -f 'sweep-${nonce}.sh'`)}catch{};process.exit(0);
