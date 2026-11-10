// rig.mjs BIN: bug-20261110-12916e73 live. Two real daemons on one Mac, each with its own state and
// identity (the owner on loopback, the host on the LAN address, reached only through the fleet mesh),
// as in docs/evidence/release-20261101/harness/rrig.mjs. A real Claude Haiku chat on the owner, at full
// access, issues a job-start credential. Then, exactly as a timer would, with NO chat context:
//   WORKASS_TOOL_CONTEXT=<token_file> workass tools call workass_start_process_child {machine_id:<host>,...}
// plus list / read / cancel, and the job's notice must land in the chat. Nothing touches an installed app.
import {spawn,execFileSync} from 'node:child_process';import fs from 'node:fs';import os from 'node:os';import path from 'node:path';
process.env.NODE_TLS_REJECT_UNAUTHORIZED='0';
const BIN=path.resolve(process.argv[2]);const HERE=path.dirname(new URL(import.meta.url).pathname),REPO=path.resolve(HERE,'../../../..');
const STAMP=`${Date.now().toString(36)}${process.pid.toString(36)}`,ROOT=`${REPO}/.dev/rig-12916e73/${STAMP}`,CWD=path.join(fs.realpathSync(os.tmpdir()),`wa-rig-${STAMP}`);
const OP=Number(process.env.RIG_OWNER_PORT||28891),HP=Number(process.env.RIG_HOST_PORT||28892);
for(const d of [`${ROOT}/o/logs`,`${ROOT}/h/logs`,CWD])fs.mkdirSync(d,{recursive:true});
const t0=Date.now(),say=(s,...a)=>{const line=`+${String(Math.round((Date.now()-t0)/1000)).padStart(4)}s [${s}] ${a.join(' ')}`;console.log(line);fs.appendFileSync(`${ROOT}/timeline.log`,line+'\n')};
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const envFor=n=>Object.fromEntries(Object.entries({HOME:process.env.HOME,USER:process.env.USER,LOGNAME:process.env.USER,TMPDIR:process.env.TMPDIR,LANG:'en_US.UTF-8',PATH:`${process.env.HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`,
 WORKASS_PROFILE:'dev',WORKASS_DATA_ROOT:`${ROOT}/${n}`,WORKASS_LOG_ROOT:`${ROOT}/${n}/logs`,WORKASS_UPDATE_CHANNEL:'local',WORKASS_BROWSER_CONTROL_FILE:`${ROOT}/${n}/bc.json`,
 WORKASS_CLAUDE_SDK_MODULE:process.env.RIG_SDK}).filter(([,v])=>v!==undefined));
const O=envFor('o'),H=envFor('h'),OS=`${ROOT}/o/state`,HS=`${ROOT}/h/state`;
const key=execFileSync(BIN,['fleet','key','--state-dir',OS],{env:O,encoding:'utf8'}).split('\n').map(l=>l.trim()).find(l=>/^[A-Za-z0-9_+/=-]{20,}$/.test(l));if(!key)throw Error('no fleet key');
execFileSync(BIN,['fleet','join','--state-dir',HS],{env:H,input:key+'\n',encoding:'utf8',stdio:['pipe','ignore','ignore']});
const boot=(n,env,state,port,bind)=>{const l=fs.openSync(`${ROOT}/${n}/logs/daemon.log`,'a');return spawn(BIN,['--headless','--state-dir',state,'--port',String(port),'--bind',bind,'--beacon=false'],{cwd:REPO,stdio:['ignore',l,l],env})};
const health=async p=>{for(let i=0;i<240;i++){try{if((await fetch(`https://127.0.0.1:${p}/workass/health`)).ok)return true}catch{}await sleep(250)}throw Error('no health '+p)};
let od=boot('o',O,OS,OP,'localhost'),hd=boot('h',H,HS,HP,'lan');
const bye=()=>{for(const p of [od,hd])try{p.kill('SIGTERM')}catch{}};process.on('exit',bye);process.on('SIGTERM',()=>process.exit(1));process.on('SIGINT',()=>process.exit(1));
await Promise.all([health(OP),health(HP)]);
const lan=execFileSync('/usr/sbin/ipconfig',['getifaddr','en0'],{encoding:'utf8'}).trim();
const call=(port,channel,...args)=>new Promise((resolve,reject)=>{const ws=new WebSocket(`wss://127.0.0.1:${port}/ws`);const timer=setTimeout(()=>{try{ws.close()}catch{};reject(Error('timeout '+channel))},45000);ws.onopen=()=>ws.send(JSON.stringify({t:'invoke',id:'x',channel,args}));ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.t==='reply'&&m.id==='x'){clearTimeout(timer);ws.close();m.error?reject(Error(JSON.stringify(m.error))):resolve(m.result)}};ws.onerror=()=>{clearTimeout(timer);reject(Error('ws '+channel))}});
const added=await call(OP,'machines:add',{address:`${lan}:${HP}`});say('rig','machine added',added.ok,added.error||'');if(!added.ok)throw Error('machine add failed');
{const gone=new Promise(res=>od.once('exit',res));od.kill('SIGTERM');await gone;od=boot('o',O,OS,OP,'localhost');await health(OP)}
const HID=JSON.parse(fs.readFileSync(`${HS}/machine-id.json`)).machineId;
const tab=`tab-${STAMP}`,chat=`conv-${STAMP}`;
for(let i=0;;i++){try{await call(OP,'chat:create',{tabId:tab,chatId:chat,operationId:`create-${STAMP}-${i}`,title:'bug 12916e73 rig',cwd:CWD,providerId:'claude',currentModelId:'haiku',currentModeId:'bypassPermissions',focus:true});break}catch(e){if(i>120)throw e;await sleep(1000)}}
const warm=`warm-${STAMP}`;for(let i=0;;i++){try{await call(OP,'job:start',{tabId:tab,chatId:chat,operationId:warm,userMessageId:'u-'+warm,assistantMessageId:'a-'+warm,prompt:'Reply with only the word READY.',cwd:CWD,permissionMode:'bypassPermissions',providerId:'claude',modelId:'haiku'});break}catch(e){if(i>120)throw e;await sleep(1000)}}
let ctx;for(let i=0;i<300&&!ctx;i++){const dir=`${OS}/tool-bootstrap`;try{for(const b of fs.readdirSync(dir)){try{const c=JSON.parse(fs.readFileSync(`${dir}/${b}/context.json`));if(c.chat_id===chat){ctx=`${ROOT}/ctx.json`;fs.copyFileSync(`${dir}/${b}/context.json`,ctx)}}catch{}}}catch{}if(!ctx)await sleep(300)}if(!ctx)throw Error('no tool context');
// One CLI call. context=null means the timer's case: only WORKASS_TOOL_CONTEXT, no chat at all.
const cli=(context,name,input={})=>new Promise(resolve=>{const env={...O};delete env.WORKASS_TOOL_CONTEXT;env.WORKASS_TOOL_CONTEXT=context;const c=spawn(BIN,['tools','call',name],{env,cwd:'/tmp',stdio:['pipe','pipe','pipe']});let out='',err='';c.stdout.on('data',d=>out+=d);c.stderr.on('data',d=>err+=d);c.on('close',code=>{let json=null;try{json=JSON.parse(out)}catch{}resolve({code,json,text:(out+err).trim()})});c.stdin.end(JSON.stringify(input))});
const asChat=(n,i)=>cli(ctx,n,i),R=j=>j?.result??j;
for(let i=0;i<240;i++){const r=await asChat('workass_read_chat',{tab_id:tab,chat_id:chat,limit:50});if((R(r.json)?.messages||[]).some(m=>m.role==='assistant'&&m.status==='done'))break;await sleep(500)}
for(let i=0;;i++){const c=await asChat('workass_agent_catalog',{machine_id:HID});if(c.code===0)break;if(i>30)throw Error('host unreachable: '+c.text);await sleep(1000)}
say('rig','chat ready; host',HID);
const issued=R((await asChat('workass_issue_job_credential',{label:'rig timer',operation_id:`issue-${STAMP}`})).json);const TF=issued.token_file;say('issue',issued.credential_id,'file',fs.existsSync(TF));
const timer=(n,i)=>cli(TF,n,i);const results={};const check=(k,ok,detail)=>{results[k]={ok:!!ok,detail};say(ok?'PASS':'FAIL',k,String(detail).slice(0,300))};
const release=`${CWD}/release`,cmd=['/bin/sh','-c',`i=0; while [ ! -e ${release} ] && [ $i -lt 1500 ]; do sleep 0.2; i=$((i+1)); done; echo rig-done`];
// 1. The reporter's exact failing call.
const s1=await timer('workass_start_process_child',{machine_id:HID,label:'rig remote nightly',command:cmd,cwd:CWD,operation_id:`nightly-${STAMP}`});
const work=R(s1.json)?.workId;check('remote start with the job-start credential',s1.code===0&&work,s1.text);
// 2. "Is it already running?"
let listed;for(let i=0;i<40;i++){listed=await timer('workass_list_process_children',{machine_id:HID});if((R(listed.json)?.children||[]).some(c=>c.work_id===work&&c.status==='running'))break;await sleep(500)}
check('list on the host shows it running',(R(listed.json)?.children||[]).some(c=>c.work_id===work&&c.status==='running'),listed.text);
const lu=await timer('workass_list_process_children',{});check('unaddressed list includes it',(R(lu.json)?.children||[]).some(c=>c.work_id===work),lu.text);
const rd=await timer('workass_read_process_child',{machine_id:HID,work_id:work});check('read',rd.code===0&&R(rd.json)?.child?.work_id===work,rd.text);
// 3. A second job, cancelled by the credential.
const s2=await timer('workass_start_process_child',{machine_id:HID,label:'rig cancel me',command:['/bin/sleep','120'],cwd:CWD,operation_id:`cancel-${STAMP}`});const w2=R(s2.json)?.workId;
await sleep(2000);const cx=await timer('workass_cancel_process_child',{machine_id:HID,work_id:w2,operation_id:`cancel-op-${STAMP}`});check('cancel',cx.code===0&&R(cx.json)?.ok,cx.text);
// 4. Still fenced.
for(const n of ['workass_list_chats','workass_send_chat_message','workass_spawn_subagent']){const r=await timer(n,{});check(`refused ${n}`,r.code!==0||R(r.json)?.error,r.text)}
const loc=await timer('workass_start_process_child',{label:'local',command:['/bin/true'],cwd:CWD,operation_id:`local-${STAMP}`,machine_id:JSON.parse(fs.readFileSync(`${OS}/machine-id.json`)).machineId});check('local start points at workass run',loc.code!==0&&loc.text.includes('workass run'),loc.text);
// 5. Finish job 1; its notice must reach the chat.
fs.writeFileSync(release,'');let note=null;for(let i=0;i<120&&!note;i++){const m=R((await asChat('workass_read_chat',{tab_id:tab,chat_id:chat,limit:100})).json)?.messages||[];note=m.find(x=>x.role==='user'&&String(x.content||x.text||'').includes('rig remote nightly'));if(!note)await sleep(1000)}
check('the job notice reached the issuing chat',note,note?String(note.content||note.text).slice(0,240):'none');
const fails=Object.entries(results).filter(([,v])=>!v.ok).map(([k])=>k);fs.writeFileSync(`${ROOT}/results.json`,JSON.stringify(results,null,1));
say('rig',fails.length?`FAILED: ${fails.join(', ')}`:'ALL PASSED','root',ROOT);process.exit(fails.length?1:0);
