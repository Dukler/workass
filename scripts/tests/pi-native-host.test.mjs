import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,readFile,writeFile,readdir,rm,mkdir,symlink,realpath} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import readline from 'node:readline';
import {resolvePiSDK} from '../pi-native-host.mjs';
const host=path.resolve('scripts/pi-native-host.mjs'),fixture=path.resolve('desktop/acp/mock-pi-sdk.mjs');
async function setup(t) {
  const dir=await mkdtemp(path.join(tmpdir(),'workass-pi-test-'));
  t.after(()=>rm(dir,{recursive:true,force:true}));return dir;
}
function run(t,dir,env={}) {
  const p=spawn(process.execPath,[host],{env:{...process.env,WORKASS_INSTRUCTIONS_FILE:'',WORKASS_PI_SDK_MODULE:fixture,WORKASS_PI_FIXTURE_DIR:dir,...env},stdio:['pipe','pipe','pipe']});
  t.after(()=>p.kill());const out=[],waiters=[];let stderr='';
  p.stderr.on('data',d=>stderr+=d);
  readline.createInterface({input:p.stdout}).on('line',line=>{
    const row=JSON.parse(line);out.push(row);
    for(const w of [...waiters])if(w.predicate(row)){waiters.splice(waiters.indexOf(w),1);clearTimeout(w.timer);w.resolve(row)}
  });
  function wait(predicate) {
    const found=out.find(predicate);if(found)return Promise.resolve(found);
    return new Promise((resolve,reject)=>{const w={predicate,resolve};w.timer=setTimeout(()=>{waiters.splice(waiters.indexOf(w),1);reject(Error(`Pi host timeout: ${stderr}`))},3000);waiters.push(w)});
  }
  let n=0;const send=x=>p.stdin.write(JSON.stringify({jsonrpc:'2.0',...x})+'\n');
  async function call(method,params={}){const id=++n;send({id,method,params});const row=await wait(x=>x.id===id);if(row.error)throw Object.assign(Error(row.error.message),row.error);return row.result}
  return {p,out,wait,send,call};
}
test('official import-only SDK resolves from npm symlink and Windows shim; unrelated installs fail',async t=>{
  const dir=await setup(t),pkg=path.join(dir,'node_modules/@earendil-works/pi-coding-agent');
  await mkdir(path.join(pkg,'dist/bundle'),{recursive:true});
  await writeFile(path.join(pkg,'package.json'),JSON.stringify({name:'@earendil-works/pi-coding-agent',exports:{'.':{import:'./dist/index.js'}}}));
  await writeFile(path.join(pkg,'dist/index.js'),'export {}');await writeFile(path.join(pkg,'dist/bundle/cli.js'),'');
  const shim=path.join(dir,'pi.cmd');await writeFile(shim,'');assert.equal(resolvePiSDK(shim),await realpath(path.join(pkg,'dist/index.js')));
  if(process.platform!=='win32'){const bin=path.join(dir,'pi');await symlink(path.join(pkg,'dist/bundle/cli.js'),bin);assert.equal(resolvePiSDK(bin),await realpath(path.join(pkg,'dist/index.js')))}
  const wrong=await setup(t);await writeFile(path.join(wrong,'pi'),'');assert.throws(()=>resolvePiSDK(path.join(wrong,'pi')),/official SDK/);
});
test('deferred journal receipt, exact restart resume, instructions and write failures',async t=>{
  const dir=await setup(t),ins=path.join(dir,'instructions');await writeFile(ins,'WORKASS_APPEND');
  let h=run(t,dir,{WORKASS_INSTRUCTIONS_FILE:ins}),s=await h.call('session/new',{cwd:dir});
  assert.ok(!(await readdir(dir)).some(f=>f.endsWith('.jsonl')));
  await h.call('session/prompt',{sessionId:s.sessionId,prompt:'ok',clientUserMessageId:'accepted'});
  assert.equal(h.out.filter(x=>x.params?.update?.clientUserMessageId==='accepted').length,1);
  await h.call('session/close',{sessionId:s.sessionId});h.p.kill();
  const file=path.join(dir,`${s.sessionId}.jsonl`),before=await readFile(file,'utf8');
  h=run(t,dir,{WORKASS_INSTRUCTIONS_FILE:ins});await h.call('session/resume',{cwd:dir,sessionId:s.sessionId});
  assert.equal(await readFile(file,'utf8'),before);
  assert.equal(await readFile(path.join(dir,'prompt.txt'),'utf8'),'NATIVE DEFAULT\nPROJECT INSTRUCTIONS\nWORKASS_APPEND');
  for(const [prompt,error] of [['FAIL',/startup failure/],['DISK_FAIL',/journal write failed/]]) {
    await assert.rejects(h.call('session/prompt',{sessionId:s.sessionId,prompt,clientUserMessageId:prompt}),error);
    assert.ok(!h.out.some(x=>x.params?.update?.clientUserMessageId===prompt));
  }
  const files=await readdir(dir);await assert.rejects(h.call('session/resume',{cwd:dir,sessionId:'missing'}),e=>e.code===-32044);assert.deepEqual(await readdir(dir),files);
  const other=run(t,dir);await assert.rejects(other.call('session/resume',{cwd:path.dirname(dir),sessionId:s.sessionId}),/identity mismatch/);
});
test('single native steer accepts text/images without materializing, restarting or queueing the turn',async t=>{
  const dir=await setup(t),h=run(t,dir),init=await h.call('initialize');assert.equal(init._meta.workassPiSteerRequest,true);
  const s=await h.call('session/new',{cwd:dir}),sessionId=s.sessionId;
  const params={sessionId,prompt:[{type:'text',text:'one direction'},{type:'image',data:'aW1hZ2U=',mimeType:'image/png'}]};
  await assert.rejects(h.call('_workass/pi/steer',params),/no active turn/);
  let ended=false;const active=h.call('session/prompt',{sessionId,prompt:'WAIT',clientUserMessageId:'initial'}).then(r=>{ended=true;return r});
  await h.wait(x=>x.params?.update?.sessionUpdate==='agent_thought_chunk');
  assert.ok(!h.out.some(x=>x.params?.update?.clientUserMessageId==='initial'));
  const first=await h.call('_workass/pi/steer',params);assert.ok(first.turnId);
  assert.equal(ended,false);
  const calls=(await readFile(path.join(dir,'calls.jsonl'),'utf8')).trim().split('\n').map(JSON.parse);
  assert.deepEqual(calls,[{method:'steer',text:'one direction',images:[{type:'image',data:'aW1hZ2U=',mimeType:'image/png'}]}]);
  await assert.rejects(h.call('_workass/pi/steer',{sessionId,prompt:'REJECT'}),/steer rejected/);
  await assert.rejects(h.call('session/prompt',{sessionId,prompt:'duplicate'}),/active prompt/);
  assert.equal((await h.call('_workass/pi/steer',{sessionId,prompt:'second'})).turnId,first.turnId);
  h.send({method:'session/cancel',params:{sessionId}});assert.equal((await active).stopReason,'cancelled');
  assert.equal(h.out.filter(x=>x.params?.update?.clientUserMessageId==='initial').length,1);
  await assert.rejects(h.call('_workass/pi/steer',params),/no active turn/);
});
test('native model/effort controls, extension tools/guards, compaction, usage and model errors',async t=>{
  const dir=await setup(t),h=run(t,dir),s=await h.call('session/new',{cwd:dir}),sessionId=s.sessionId;
  assert.deepEqual(s.availableModels.map(m=>m.modelId),['fixture/same','other/same']);
  assert.deepEqual(s.configOptions.find(x=>x.id==='mode').options,[{value:'native',name:'Pi defaults'}]);
  await assert.rejects(h.call('session/set_mode',{sessionId,modeId:'read'}),/Unsupported/);
  await h.call('session/set_model',{sessionId,modelId:'other/same'});
  await h.call('session/set_config_option',{sessionId,configId:'effort',value:'high'});
  await h.call('session/prompt',{sessionId,prompt:'STATE'});
  const state=JSON.parse(h.out.find(x=>x.params?.update?.sessionUpdate==='agent_message_chunk').params.update.content.text);
  assert.equal(state.model.provider,'other');assert.equal(state.effort,'high');
  const prompt=h.call('session/prompt',{sessionId,prompt:'PERMISSION'});
  const permission=await h.wait(x=>x.method==='session/request_permission');h.send({id:permission.id,result:{outcome:{outcome:'selected',optionId:'1'}}});await prompt;
  assert.ok(h.out.some(x=>x.params?.update?.content?.[0]?.content?.text==='Deny'));
  await h.call('session/prompt',{sessionId,prompt:'TOOL'});
  assert.deepEqual(h.out.filter(x=>x.params?.update?.toolCallId==='subagent-1').map(x=>x.params.update.status),['in_progress','in_progress','completed']);
  await h.call('session/prompt',{sessionId,prompt:'COMPACT'});
  assert.ok(h.out.some(x=>x.params?.update?.phase==='checkpoint'));
  assert.ok(h.out.some(x=>x.params?.update?.used===42&&x.params?.update?.size===4096));
  await assert.rejects(h.call('session/prompt',{sessionId,prompt:'ERROR'}),/model failed/);
});
