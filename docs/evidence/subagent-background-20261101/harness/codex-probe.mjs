// codex-probe.mjs [seconds]: real `codex app-server`, one thread, cheapest model. A command left running past its tool call;
// logs what the app-server says at turn end and after, whether the process survives while the thread stays open, and whether a turn starts by itself.
import {spawn,execSync} from 'node:child_process';import fs from 'node:fs';import readline from 'node:readline';
const SECS=Number(process.argv[2]||30),D=process.env.RIG_DIR||'/tmp/codex-probe',n='cxp'+Date.now().toString(36),script=`${D}/sweep-${n}.sh`,mark=`${D}/${n}.out`;
fs.writeFileSync(script,`#!/bin/sh\ni=0; while [ $i -lt ${SECS} ]; do sleep 1; i=$((i+1)); done\necho DONE-${n} > ${mark}\n`);
const t0=Date.now(),log=(...a)=>console.log(`+${String(Math.round((Date.now()-t0)/100)/10).padStart(5)}s`,...a);
const c=spawn('codex',['app-server'],{stdio:['pipe','pipe','ignore'],cwd:D});let id=0;const pend=new Map();
const req=(method,params)=>new Promise((r,j)=>{const i=++id;pend.set(i,{r,j});c.stdin.write(JSON.stringify({jsonrpc:'2.0',id:i,method,params})+'\n')});
let turns=0,turnDone=null;const alive=()=>execSync(`pgrep -f 'sweep-${n}.sh' || true`).toString().trim().length>0;
readline.createInterface({input:c.stdout}).on('line',l=>{let m;try{m=JSON.parse(l)}catch{return}
 if(m.id&&pend.has(m.id)&&!m.method){const p=pend.get(m.id);pend.delete(m.id);m.error?p.j(Error(JSON.stringify(m.error))):p.r(m.result);return}
 if(m.id&&m.method){log('server-request',m.method);c.stdin.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{decision:'approved'}})+'\n');return}
 const p=m.params||{};
 if(m.method==='turn/started'){turns++;log('turn/started',p.turn?.id,'turns='+turns)}
 else if(m.method==='turn/completed'){log('turn/completed',p.turn?.status,'process='+(alive()?'alive':'gone'));turnDone&&turnDone()}
 else if(m.method==='item/started'||m.method==='item/completed'){const it=p.item||{};if(it.type==='commandExecution'||it.type==='agentMessage')log(m.method,it.type,it.type==='commandExecution'?JSON.stringify({status:it.status,processId:it.processId,exit:it.exitCode,dur:it.durationMs,src:it.source}):JSON.stringify(String(it.text||'').slice(0,70)))}
 else if(/terminal|background/i.test(m.method||''))log(m.method,JSON.stringify(p).slice(0,200));
});
await req('initialize',{clientInfo:{name:'bgprobe',version:'0'},capabilities:{experimentalApi:true}});
c.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'initialized',params:{}})+'\n');
const th=await req('thread/start',{cwd:D,model:'gpt-5.6-luna',approvalPolicy:'never',sandbox:'danger-full-access'});const threadId=th.thread.id;log('thread',threadId);
const wait=()=>new Promise(r=>{turnDone=r});const w1=wait();
await req('turn/start',{threadId,input:[{type:'text',text:`Use your exec_command tool to start this exact command with yield_time_ms 1000 so it keeps running after the tool returns: sh ${script}\nThen end your turn at once with the sentence "It is running." Do not wait, poll or check on it.`}]});
await w1;log('first turn over; thread stays open; watching',SECS+25,'s');
let last=null;const end=Date.now()+(SECS+25)*1000;while(Date.now()<end){const a=alive();if(a!==last){log('process',a?'alive':'gone','marker',fs.existsSync(mark)?'written':'absent');last=a}await new Promise(r=>setTimeout(r,500))}
log(`RESULT turnsTotal=${turns} marker=${fs.existsSync(mark)}`);
const ww=wait();await req('thread/unsubscribe',{threadId}).catch(()=>{});log('unsubscribed');c.kill();process.exit(0);
