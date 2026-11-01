// host-rig.mjs HOST COUNT [real]: one write holding session/prompt + session/cancel, against a real host process.
// Counts: did the prompt answer cancelled, did the provider produce an answer anyway.
import {spawn} from 'node:child_process'; import readline from 'node:readline'; import path from 'node:path'; import fs from 'node:fs'; import os from 'node:os';
const [HOST,COUNT='20',MODE='fixture']=process.argv.slice(2); const R=process.cwd();
const scratch=fs.mkdtempSync(path.join(os.tmpdir(),'stop-race-'));
const envs={
 claude:{script:'scripts/claude-native-host.mjs',env:MODE==='real'?{WORKASS_CLAUDE_SDK_MODULE:process.env.REAL_SDK,WORKASS_CLAUDE_EXECUTABLE:process.env.REAL_CLAUDE}:{WORKASS_CLAUDE_SDK_MODULE:path.join(R,'desktop/acp/mock-claude-agent-sdk.mjs'),WORKASS_CLAUDE_EXECUTABLE:'/fixture/claude'}},
 codex:{script:'scripts/codex-native-host.mjs',env:{WORKASS_CODEX_EXECUTABLE:process.execPath,WORKASS_CODEX_APP_SERVER_ARGS:JSON.stringify([path.join(R,'desktop/acp/mock-codex-app-server.mjs')])}},
 pi:{script:'scripts/pi-native-host.mjs',env:{WORKASS_INSTRUCTIONS_FILE:'',WORKASS_PI_SDK_MODULE:path.join(R,'desktop/acp/mock-pi-sdk.mjs'),WORKASS_PI_FIXTURE_DIR:scratch}},
 omp:{script:'scripts/omp-native-host.mjs',env:{WORKASS_OMP_SDK_MODULE:path.join(R,'desktop/acp/mock-omp-sdk.mjs'),WORKASS_OMP_EXECUTABLE:path.join(R,'desktop/acp/mock-omp-cli.mjs'),WORKASS_OMP_FIXTURE_DIR:scratch}},
}[HOST];
const p=spawn(process.execPath,[path.join(R,envs.script)],{cwd:scratch,env:{...process.env,...envs.env},stdio:['pipe','pipe','pipe']});
let stderr='';p.stderr.on('data',d=>stderr+=d);
const out=[],waiters=[];readline.createInterface({input:p.stdout}).on('line',l=>{let m;try{m=JSON.parse(l)}catch{return};out.push(m);for(const w of [...waiters])if(w.f(m)){waiters.splice(waiters.indexOf(w),1);w.r(m)}});
const wait=(f,ms=20000)=>new Promise((r,j)=>{const x=out.find(f);if(x)return r(x);const w={f,r};waiters.push(w);setTimeout(()=>j(Error('timeout; stderr='+stderr.slice(-400))),ms).unref()});
const line=x=>JSON.stringify({jsonrpc:'2.0',...x})+'\n';let n=0;
const call=async(method,params={})=>{const id=++n;p.stdin.write(line({id,method,params}));const m=await wait(x=>x.id===id);if(m.error)throw Error(m.error.message);return m.result};
const text=t=>[{type:'text',text:t}];
const answers=(from)=>out.slice(from).filter(m=>m.params?.update?.sessionUpdate==='agent_message_chunk').length;
await call('initialize',{});const s=await call('session/new',{cwd:scratch,mcpServers:[]});const sid=s.sessionId;
if(MODE==='real'){const m=(s.configOptions||[]).find(o=>o.id==='model');const h=m?.options?.find(o=>/haiku/i.test(o.value));if(h)await call('session/set_config_option',{sessionId:sid,configId:'model',value:h.value});console.log('model',h?.value)}
const sendTogether=(prompt,cuid)=>{const id=++n;p.stdin.write(line({id,method:'session/prompt',params:{sessionId:sid,prompt:text(prompt),clientUserMessageId:cuid}})+line({method:'session/cancel',params:{sessionId:sid}}));return id};
let lost=0,cancelled=0,other=0;
for(let i=1;i<=Number(COUNT);i++){
  const from=out.length;const id=sendTogether(MODE==='real'?`Reply with only the word RAN${i}.`:'[stop race] answer',`op-${i}`);
  const m=await wait(x=>x.id===id,MODE==='real'?60000:15000).catch(e=>({timedOut:e.message}));
  await new Promise(r=>setTimeout(r,MODE==='real'?1500:40));
  const a=answers(from);const stop=m.result?.stopReason||(m.error?'error:'+m.error.message.slice(0,60):m.timedOut?'no-reply':'?');
  if(stop==='cancelled'&&a===0)cancelled++;else if(a>0||stop==='end_turn')lost++;else other++;
  if(MODE==='real'||i===1)console.log(`#${i} stop=${stop} answerChunks=${a}`);
}
// The session must still work after all that.
const from=out.length;const id=++n;p.stdin.write(line({id,method:'session/prompt',params:{sessionId:sid,prompt:text(MODE==='real'?'Reply with only the word ALIVE.':'follow up'),clientUserMessageId:'after'}}));
const after=await wait(x=>x.id===id,MODE==='real'?90000:15000).catch(e=>({timedOut:e.message}));
console.log(`RESULT ${HOST}/${MODE} n=${COUNT} stoppedClean=${cancelled} ranAnyway=${lost} other=${other} followUp=${after.result?.stopReason||JSON.stringify(after.error||after.timedOut).slice(0,160)} followUpChunks=${answers(from)}`);
p.kill();fs.rmSync(scratch,{recursive:true,force:true});process.exit(0);
