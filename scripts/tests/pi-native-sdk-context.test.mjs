// The installed official SDK is the integration boundary. A loopback fixture
// captures actual provider requests and returns fixed protocol responses; no
// model inference, vendor account, or user Pi profile is used as an oracle.
import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,mkdir,readFile,writeFile,rm,access} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import readline from 'node:readline';

function hostClient(env, cwd) {
  const child = spawn(process.execPath, [path.resolve('scripts/pi-native-host.mjs')], {env,cwd,stdio:['pipe','pipe','pipe']});
  const pending = new Map();
  const updates = [];
  let sequence = 0;
  // Only the isolated fixture's diagnostics are collected. Never inherit the
  // caller's provider credentials or private Workass context into this child.
  let stderr = '';
  child.stderr.on('data', data => { stderr += data; });
  readline.createInterface({input:child.stdout}).on('line', line => {
    const row = JSON.parse(line);
    if (row.params?.update) updates.push(row.params.update);
    const waiter = pending.get(row.id);
    if (!waiter) return;
    pending.delete(row.id);
    clearTimeout(waiter.timer);
    if (row.error) waiter.reject(new Error(row.error.message));
    else waiter.resolve(row.result);
  });
  const exited = new Promise(resolve => child.once('exit', resolve));
  child.on('exit', () => {
    for (const waiter of pending.values()) {
      clearTimeout(waiter.timer);
      waiter.reject(new Error('Pi fixture host exited before replying'));
    }
    pending.clear();
  });
  return {
    updates,
    call(method, params = {}) {
      const id = ++sequence;
      return new Promise((resolve,reject) => {
        const timer = setTimeout(() => { pending.delete(id); reject(new Error(`Pi fixture timeout: ${method}`)); }, 15000);
        pending.set(id,{resolve,reject,timer});
        child.stdin.write(JSON.stringify({jsonrpc:'2.0',id,method,params})+'\n');
      });
    },
    async stop() {
      if (child.exitCode === null && child.signalCode === null) child.kill();
      await exited;
      assert.doesNotMatch(stderr, /extension failed to load/i);
    },
  };
}

test('official Pi SDK preserves provider instructions and tool deltas across fresh input and exact host-restart resume', {
  skip: !process.env.WORKASS_TEST_PI_EXECUTABLE && 'Run through TestPiNativeSDKProviderContext with an installed official Pi SDK',
  timeout: 40000,
}, async t => {
  const root = await mkdtemp(path.join(tmpdir(),'workass-pi-context-'));
  let host, server;
  t.after(async () => {
    try { await host?.stop(); }
    finally {
      if (server?.listening) {
        server.closeAllConnections();
        await new Promise(resolve => server.close(resolve));
      }
      await rm(root,{recursive:true,force:true});
    }
  });
  const cwd = path.join(root,'project'), agent = path.join(root,'agent');
  await mkdir(cwd); await mkdir(agent);
  const instructions = path.join(root,'workass-instructions.md');
  const catalog = JSON.parse(await readFile('internal/agenttext/catalog.json','utf8'));
  const bootstrap = catalog['native.bootstrap'].trim();
  assert.ok(bootstrap.includes('WORKASS_TOOLS_COMMAND'));
  await writeFile(instructions,bootstrap);
  await writeFile(path.join(cwd,'AGENTS.md'),'PROJECT_CONTEXT_FIXTURE: preserve this project instruction.\n');

  const requests = [];
  let serverError;
  server = createServer(async (req,res) => {
    try {
      assert.equal(req.method,'POST');
      assert.equal(req.url,'/v1/chat/completions');
      let bytes = '';
      for await (const chunk of req) bytes += chunk;
      const body = JSON.parse(bytes);
      assert.equal(body.model,'fixed');
      requests.push(body);
      const tool = requests.length % 2 === 1;
      const delta = tool
        ? {role:'assistant',tool_calls:[{index:0,id:`fixture-call-${requests.length}`,type:'function',function:{name:'fixture_swap',arguments:'{}'}}]}
        : {role:'assistant',content:'fixture complete'};
      res.writeHead(200,{'content-type':'text/event-stream'});
      for (const choice of [{index:0,delta,finish_reason:null},{index:0,delta:{},finish_reason:tool ? 'tool_calls':'stop'}]) {
        res.write(`data: ${JSON.stringify({id:'fixture',object:'chat.completion.chunk',model:'fixed',choices:[choice]})}\n\n`);
      }
      res.end('data: [DONE]\n\n');
    } catch (error) {
      serverError = error;
      res.writeHead(500); res.end('invalid fixture request');
    }
  });
  server.listen(0,'127.0.0.1'); await once(server,'listening');
  const settings = {
    defaultProvider:'workass-context-fixture',defaultModel:'fixed',defaultThinkingLevel:'off',
    retry:{enabled:false},
    extensions:[path.resolve('desktop/acp/mock-pi-context-extension.mjs')],
  };
  const settingsFile = path.join(agent,'settings.json');
  await writeFile(settingsFile,JSON.stringify(settings));
  await writeFile(path.join(agent,'models.json'),JSON.stringify({providers:{'workass-context-fixture':{
    baseUrl:`http://127.0.0.1:${server.address().port}/v1`,api:'openai-completions',apiKey:'fixture',
    models:[{id:'fixed',contextWindow:32768,maxTokens:1024}],
  }}}));
  const env = {};
  for (const name of ['PATH','HOME','USERPROFILE','SystemRoot','WINDIR','COMSPEC','TEMP','TMP']) {
    if (process.env[name]) env[name] = process.env[name];
  }
  Object.assign(env,{
    PI_CODING_AGENT_DIR:agent,WORKASS_PI_EXECUTABLE:process.env.WORKASS_TEST_PI_EXECUTABLE,
    WORKASS_INSTRUCTIONS_FILE:instructions,WORKASS_TOOLS_COMMAND:path.join(root,'fixture-workass'),
  });
  host = hostClient(env,cwd);
  await host.call('initialize');
  const fresh = await host.call('session/new',{cwd});
  assert.ok(fresh.availableModels.some(model => model.modelId === 'workass-context-fixture/fixed'));
  await host.call('session/prompt',{sessionId:fresh.sessionId,prompt:'fixture fresh',clientUserMessageId:'fresh-input'});
  assert.ifError(serverError);
  assert.equal(requests.length,2,'fresh turn must reach the provider, execute the fixture tool, and finish');
  assert.equal(host.updates.filter(update => update.clientUserMessageId === 'fresh-input').length,1);
  await host.call('session/close',{sessionId:fresh.sessionId});
  await host.stop();

  // User-owned Pi tool preferences must survive a new host. No Workass
  // allowlist may remove extension tools or replace the chosen shell tool.
  settings.defaultTools = ['read','powershell','edit','write'];
  await writeFile(settingsFile,JSON.stringify(settings));
  host = hostClient(env,cwd);
  const resumed = await host.call('session/resume',{cwd,sessionId:fresh.sessionId});
  assert.equal(resumed.sessionId,fresh.sessionId,'resume must retain the exact Pi journal');
  await host.call('session/prompt',{sessionId:fresh.sessionId,prompt:'fixture resumed',clientUserMessageId:'resumed-input'});
  assert.ifError(serverError);
  assert.equal(requests.length,4,'resume must reach the provider and finish after the fixture tool');
  assert.equal(host.updates.filter(update => update.clientUserMessageId === 'resumed-input').length,1);
  await host.call('session/close',{sessionId:fresh.sessionId});

  for (const [index,request] of requests.entries()) {
    const system = request.messages.filter(message => ['system','developer'].includes(message.role)).map(message => message.content).join('\n');
    assert.ok(system.includes(bootstrap),`request ${index}: central Workass bootstrap was dropped`);
    assert.ok(system.includes('PROJECT_CONTEXT_FIXTURE'),`request ${index}: project instructions were dropped`);
    assert.ok(system.includes('tools guide'),`request ${index}: Workass CLI discovery was dropped`);
    assert.ok(Array.isArray(request.tools),`request ${index}: provider tool schemas were dropped`);
    const tools = new Map(request.tools.map(tool => [tool.function.name,tool.function]));
    for (const name of ['read','edit','write','fixture_swap']) {
      assert.ok(tools.get(name)?.description && tools.get(name)?.parameters?.type === 'object',`request ${index}: ${name} schema missing`);
    }
    if (index === 0) assert.ok(tools.has('bash'),'fresh session must retain Pi defaults');
    else assert.ok(tools.has('powershell'),'added/configured PowerShell schema must reach the provider');
    if (index % 2 === 1) {
      assert.equal(tools.has('bash'),false,'removed built-in tool survived transcript delta');
      assert.equal(tools.has('fixture_removed'),false,'removed extension tool survived transcript delta');
      assert.ok(request.messages.some(message => message.role === 'tool' && message.content.includes('fixture tool state changed')));
    } else assert.ok(tools.has('fixture_removed'),'fresh/resumed extension discovery lost a tool');
    assert.equal(request.messages.filter(message => message.role === 'user').some(message => JSON.stringify(message.content).includes('WORKASS_TOOLS_COMMAND')),false,'bootstrap leaked into user transcript guidance');
  }
  const resumedUsers = requests[2].messages.filter(message => message.role === 'user').map(message => JSON.stringify(message.content));
  assert.ok(resumedUsers.some(text => text.includes('fixture fresh')),'exact resume lost prior user input');
  assert.ok(resumedUsers.some(text => text.includes('fixture resumed')),'resumed input did not reach provider');
});

test('official Pi SDK repairs an interrupted extension tool result before resumed provider request', {
  skip: !process.env.WORKASS_TEST_PI_EXECUTABLE && 'Run through TestPiNativeSDKProviderContext with an installed official Pi SDK',
  timeout: 30000,
}, async t => {
  const root = await mkdtemp(path.join(tmpdir(),'workass-pi-repair-'));
  let host,server;
  t.after(async () => {
    try { await host?.stop(); }
    finally {
      if (server?.listening) {
        server.closeAllConnections();
        await new Promise(resolve => server.close(resolve));
      }
      await rm(root,{recursive:true,force:true});
    }
  });
  const cwd=path.join(root,'project'),agent=path.join(root,'agent');
  await mkdir(cwd);await mkdir(agent);
  const marker=path.join(root,'tool-started'),extension=path.join(root,'crash-extension.mjs');
  await writeFile(extension,`import {writeFileSync} from 'node:fs';
function stream(){const queue=[],waiters=[];let done=false,final;return {push(event){if(event.type==='done'){done=true;final=event.message;}const waiter=waiters.shift();waiter?waiter({value:event,done:false}):queue.push(event);},end(){done=true;while(waiters.length)waiters.shift()({done:true});},result(){return Promise.resolve(final);},async *[Symbol.asyncIterator](){while(true){if(queue.length){yield queue.shift();continue;}if(done)return;const next=await new Promise(resolve=>waiters.push(resolve));if(next.done)return;yield next.value;}}};}
export default function(pi){
 pi.registerTool({name:'fixture_crash',label:'Fixture crash',description:'Leaves a persisted call without a result.',parameters:{type:'object',properties:{},additionalProperties:false},async execute(){writeFileSync(${JSON.stringify(marker)},'started');return new Promise(()=>{});}});
 pi.registerProvider('workass-repair-fixture',{api:'openai-completions',apiKey:'fixture',baseUrl:${JSON.stringify('__ENDPOINT__')},models:[{id:'fixed',name:'Repair fixture',api:'openai-completions',reasoning:false,input:['text'],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:32768,maxTokens:1024}],streamSimple(model,context){const output={role:'assistant',content:[],api:model.api,provider:model.provider,model:model.id,usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:'pending',timestamp:Date.now()};const result=stream();(async()=>{try{const response=await fetch(${JSON.stringify('__ENDPOINT__')},{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({messages:context.messages})});const receipt=await response.json();result.push({type:'start',partial:output});if(receipt.turn===1){result.push({type:'done',reason:'toolUse',message:{...output,content:[{type:'toolCall',id:'fixture-crash-call',name:'fixture_crash',arguments:{}}],stopReason:'toolUse'}});}else result.push({type:'done',reason:'stop',message:{...output,content:[{type:'text',text:'resumed after interrupted tool'}],stopReason:'stop'}});}catch(error){result.push({type:'error',error});}finally{result.end();}})();return result;}});
}`);
  const requests=[];
  server=createServer(async(req,res)=>{
    try {
      let bytes='';for await(const chunk of req)bytes+=chunk;
      requests.push(JSON.parse(bytes));
      res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({turn:requests.length}));
    } catch(error) { res.writeHead(500);res.end('invalid fixture request'); }
  });
  server.listen(0,'127.0.0.1');await once(server,'listening');
  const source=await readFile(extension,'utf8');
  await writeFile(extension,source.replaceAll('__ENDPOINT__',`http://127.0.0.1:${server.address().port}/capture`));
  const settingsFile=path.join(agent,'settings.json');
  await writeFile(settingsFile,JSON.stringify({defaultProvider:'workass-repair-fixture',defaultModel:'fixed',defaultThinkingLevel:'off',retry:{enabled:false},extensions:[extension]}));
  await writeFile(path.join(agent,'models.json'),JSON.stringify({providers:{'workass-repair-fixture':{baseUrl:`http://127.0.0.1:${server.address().port}/capture`,api:'openai-completions',apiKey:'fixture',models:[{id:'fixed',contextWindow:32768,maxTokens:1024}]}}}));
  const env={};
  for(const name of ['PATH','HOME','USERPROFILE','SystemRoot','WINDIR','COMSPEC','TEMP','TMP'])if(process.env[name])env[name]=process.env[name];
  Object.assign(env,{PI_CODING_AGENT_DIR:agent,WORKASS_PI_EXECUTABLE:process.env.WORKASS_TEST_PI_EXECUTABLE,WORKASS_INSTRUCTIONS_FILE:''});
  host=hostClient(env,cwd);
  await host.call('initialize');
  const session=await host.call('session/new',{cwd});
  const interrupted=host.call('session/prompt',{sessionId:session.sessionId,prompt:'start fixture tool'});
  const until=Date.now()+10000;
  while(Date.now()<until){try{await access(marker);break;}catch{await new Promise(resolve=>setTimeout(resolve,25));}}
  await access(marker);
  await host.stop();host=undefined;
  await assert.rejects(interrupted);

  host=hostClient(env,cwd);
  const resumed=await host.call('session/resume',{cwd,sessionId:session.sessionId});
  assert.equal(resumed.sessionId,session.sessionId);
  await host.call('session/prompt',{sessionId:session.sessionId,prompt:'resume exact session'});
  assert.equal(requests.length,2,'resume must reach the extension-backed fixture provider');
  const repaired=requests[1].messages.find(message=>message.role==='toolResult'&&JSON.stringify(message.content).includes('Tool execution outcome is unknown'));
  assert.ok(repaired,'resumed provider request must receive an explicit unknown-outcome tool result');
  assert.ok(requests[1].messages.some(message=>message.role==='assistant'&&message.content?.some(block=>block.type==='toolCall'&&block.id==='fixture-crash-call')),'resumed provider context must retain the original assistant tool call');
});
