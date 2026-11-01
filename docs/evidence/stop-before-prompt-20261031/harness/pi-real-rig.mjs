// pi-real-rig.mjs BIN_HOST COUNT DELAY_MS: real Pi SDK through the real host, loopback fake OpenAI model; counts model requests.
import {spawn} from 'node:child_process'; import readline from 'node:readline'; import http from 'node:http'; import fs from 'node:fs'; import os from 'node:os'; import path from 'node:path';
const [HOST='scripts/pi-native-host.mjs',COUNT='20',DELAY='0',KIND='pi']=process.argv.slice(2);
let hits=0,finished=0;const srv=http.createServer((q,r)=>{if(q.url.endsWith('/chat/completions')){hits++;let closed=false;r.on('close',()=>closed=true);r.writeHead(200,{'Content-Type':'text/event-stream'});
 const c=(o)=>r.write('data: '+JSON.stringify({id:'x',object:'chat.completion.chunk',created:1,model:'fake-model',choices:[o]})+'\n\n');
 c({index:0,delta:{role:'assistant',content:'RAN'}});setTimeout(()=>{if(!closed){c({index:0,delta:{},finish_reason:'stop'});finished++;r.end('data: [DONE]\n\n')}},300);return}
 r.writeHead(200,{'Content-Type':'application/json'});r.end(JSON.stringify({object:'list',data:[{id:'fake-model'}]}))});
await new Promise(r=>srv.listen(0,'127.0.0.1',r));const port=srv.address().port;
const dir=fs.mkdtempSync(path.join(os.tmpdir(),'stop-race-'+KIND+'-'));const agent=path.join(dir,'agent');fs.mkdirSync(agent,{recursive:true});
fs.writeFileSync(path.join(agent,'models.json'),JSON.stringify({providers:{fake:{baseUrl:`http://127.0.0.1:${port}/v1`,api:'openai-completions',apiKey:'fake-key-not-secret',models:[{id:'fake-model',name:'Fake',input:['text'],contextWindow:32000,maxTokens:1000,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}}]}}}));
if(KIND==='omp'){const o=path.join(dir,'.omp/agent');fs.mkdirSync(o,{recursive:true});fs.cpSync(path.join(agent,'models.json'),path.join(o,'models.json'));fs.writeFileSync(path.join(o,'config.yml'),'modelRoles:\n  default: fake/fake-model\n')}
fs.writeFileSync(path.join(agent,'settings.json'),JSON.stringify({defaultProvider:'fake',defaultModel:'fake-model',packages:[],extensions:[]}));
const env={...process.env,HOME:dir,PI_CODING_AGENT_DIR:agent,PI_OFFLINE:'1',WORKASS_INSTRUCTIONS_FILE:'',WORKASS_PI_EXECUTABLE:'/opt/homebrew/bin/pi',WORKASS_OMP_EXECUTABLE:'/opt/homebrew/bin/omp'};
const p=spawn(process.execPath,[path.resolve(HOST)],{cwd:dir,env,stdio:['pipe','pipe','pipe']});let err='';p.stderr.on('data',d=>err+=d);
const out=[],ws=[];readline.createInterface({input:p.stdout}).on('line',l=>{let m;try{m=JSON.parse(l)}catch{return}out.push(m);for(const w of [...ws])if(w.f(m)){ws.splice(ws.indexOf(w),1);w.r(m)}});
const wait=(f,ms=20000)=>new Promise((r,j)=>{const x=out.find(f);if(x)return r(x);ws.push({f,r});setTimeout(()=>j(Error('timeout stderr='+err.slice(-500))),ms).unref()});
const line=x=>JSON.stringify({jsonrpc:'2.0',...x})+'\n';let n=0;const call=async(method,params={})=>{const id=++n;p.stdin.write(line({id,method,params}));const m=await wait(x=>x.id===id);if(m.error)throw Error(m.error.message);return m.result};
await call('initialize',{});const s=await call('session/new',{cwd:dir,mcpServers:[]});const sid=s.sessionId;console.log('session',!!sid,'models',JSON.stringify((s.configOptions||[]).find(o=>o.id==='model')?.options?.map(o=>o.value)));
let ranModel=0,clean=0,other=0,replied=[];
for(let i=1;i<=Number(COUNT);i++){const h0=hits,f0=finished,f=out.length,id=++n;const P=line({id,method:'session/prompt',params:{sessionId:sid,prompt:[{type:'text',text:'say something '+i}],clientUserMessageId:'op'+i}}),C=line({method:'session/cancel',params:{sessionId:sid}});
 if(Number(DELAY)===0)p.stdin.write(P+C);else{p.stdin.write(P);await new Promise(r=>setTimeout(r,Number(DELAY)));p.stdin.write(C)}
 const m=await wait(x=>x.id===id).catch(e=>({timedOut:e.message}));await new Promise(r=>setTimeout(r,450));
 const stop=m.result?.stopReason||(m.error?'error:'+m.error.message.slice(0,70):'no-reply');replied.push(stop);const ch=out.slice(f).filter(x=>x.params?.update?.sessionUpdate==='agent_message_chunk').length;
 if(finished>f0)ranModel++;else if(stop==='cancelled')clean++;else other++}
const h1=hits,id=++n;p.stdin.write(line({id,method:'session/prompt',params:{sessionId:sid,prompt:[{type:'text',text:'after'}],clientUserMessageId:'after'}}));const a=await wait(x=>x.id===id).catch(e=>({timedOut:e.message}));
console.log(`RESULT ${KIND} delay=${DELAY} n=${COUNT} stopped=${clean} modelAnswerFinishedAnyway=${ranModel} requestsSeen=${hits} other=${other} replies=${JSON.stringify([...new Set(replied)])} followUp=${a.result?.stopReason||JSON.stringify(a.error||a.timedOut).slice(0,200)} followUpModelCalls=${hits-h1}`);
p.kill();srv.close();fs.rmSync(dir,{recursive:true,force:true});process.exit(0);
