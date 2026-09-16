import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,readFile,writeFile,readdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import readline from 'node:readline';
const host=path.resolve('scripts/omp-native-host.mjs'), fixture=path.resolve('desktop/acp/mock-omp-sdk.mjs');
async function setup(t) {
  const dir=await mkdtemp(path.join(tmpdir(),'workass-omp-test-'));
  t.after(()=>rm(dir,{recursive:true,force:true}));
  return dir;
}
function run(t,dir,env={}) {
  const p=spawn(process.execPath,[process.env.WORKASS_TEST_INSTALLED_OMP ? path.resolve('scripts/omp-installed-host.mjs') : host],{env:{...process.env,WORKASS_OMP_SDK_MODULE:fixture,WORKASS_OMP_EXECUTABLE:path.resolve('desktop/acp/mock-omp-cli.mjs'),WORKASS_OMP_FIXTURE_DIR:dir,...env},stdio:['pipe','pipe','pipe']});
  t.after(()=>p.kill());
  const out=[], waiters=[];
  readline.createInterface({input:p.stdout}).on('line',line=>{
    const row=JSON.parse(line); out.push(row);
    for(const w of [...waiters]) if(w.predicate(row)) { waiters.splice(waiters.indexOf(w),1); clearTimeout(w.timer);w.resolve(row); }
  });
  function wait(predicate) {
    const found=out.find(predicate); if(found)return Promise.resolve(found);
    return new Promise((resolve,reject)=>{const w={predicate,resolve};w.timer=setTimeout(()=>{waiters.splice(waiters.indexOf(w),1);reject(Error('OMP host response timeout'))},3000);waiters.push(w)});
  }
  let n=0;
  const send=x=>p.stdin.write(JSON.stringify({jsonrpc:'2.0',...x})+'\n');
  async function call(method,params={}) { const id=++n;send({id,method,params});const row=await wait(x=>x.id===id); if(row.error)throw Object.assign(Error(row.error.message),row.error);return row.result; }
  return {p,out,wait,send,call};
}
test('native receipt follows durable user content; startup failure has no receipt; model failure is not success',async t=>{
  const dir=await setup(t),h=run(t,dir);
  const s=await h.call('session/new',{cwd:dir});
  await h.call('session/prompt',{sessionId:s.sessionId,prompt:'ok',clientUserMessageId:'accepted'});
  const receipt=await h.wait(x=>x.params?.update?.clientUserMessageId==='accepted');
  assert.equal(receipt.params.update.sessionUpdate,'_workass_input_consumed');
  const journal=await readFile(path.join(dir,`${s.sessionId}.jsonl`),'utf8');assert.match(journal,/"role":"user"/);
  await assert.rejects(h.call('session/prompt',{sessionId:s.sessionId,prompt:'FAIL',clientUserMessageId:'bad'}),/startup failure/);
  assert.ok(!h.out.some(x=>x.params?.update?.clientUserMessageId==='bad'));
  await assert.rejects(h.call('session/prompt',{sessionId:s.sessionId,prompt:'ERROR'}),/model failed/);
});
test('exact restart resume preserves journal and instructions; missing ID creates no replacement',async t=>{
  const dir=await setup(t),ins=path.join(dir,'instructions');await writeFile(ins,'WORKASS_APPEND');
  let h=run(t,dir,{WORKASS_INSTRUCTIONS_FILE:ins});const s=await h.call('session/new',{cwd:dir});
  await h.call('session/prompt',{sessionId:s.sessionId,prompt:'persist',clientUserMessageId:'m1'});
  await h.call('session/close',{sessionId:s.sessionId});h.p.kill();
  const before=await readFile(path.join(dir,`${s.sessionId}.jsonl`),'utf8');
  h=run(t,dir,{WORKASS_INSTRUCTIONS_FILE:ins});await h.call('session/resume',{cwd:dir,sessionId:s.sessionId});
  assert.equal(await readFile(path.join(dir,`${s.sessionId}.jsonl`),'utf8'),before);
  assert.equal(await readFile(path.join(dir,'prompt.txt'),'utf8'),'NATIVE DEFAULT\nWORKASS_APPEND');
  const files=await readdir(dir);await assert.rejects(h.call('session/resume',{cwd:dir,sessionId:'missing'}),/never materialized/);assert.deepEqual(await readdir(dir),files);
});
test('full access changes native approval, plan changes native state, and model IDs remain distinct',async t=>{
  const dir=await setup(t),h=run(t,dir),s=await h.call('session/new',{cwd:dir});
  assert.deepEqual(s.availableModels.map(x=>x.modelId),['fixture/same','other/same']);
  const modes=s.configOptions.find(x=>x.id==='mode').options;assert.ok(modes.some(x=>x.value==='yolo'&&x.name==='Yolo'));
  await assert.rejects(h.call('session/set_model',{sessionId:s.sessionId,modelId:'absent'}),/unavailable/);
  await h.call('session/set_model',{sessionId:s.sessionId,modelId:'other/same'});
  await assert.rejects(h.call('session/set_mode',{sessionId:s.sessionId,modeId:'unknown'}),/Unsupported/);
  for(const [mode,approval] of [['yolo','yolo'],['write','write'],['plan','always-ask']]) {
    await h.call('session/set_mode',{sessionId:s.sessionId,modeId:mode});
    const from=h.out.length;
    await h.call('session/prompt',{sessionId:s.sessionId,prompt:'STATE'});
    const state=JSON.parse(h.out.slice(from).find(x=>x.params?.update?.sessionUpdate==='agent_message_chunk').params.update.content.text);
    assert.equal(state.approval,approval);assert.equal(Boolean(state.plan?.enabled),mode==='plan');
  }
});
test('permissions reach Workass; full access skips ordinary approval; cancellation and native compaction are forwarded',async t=>{
  const dir=await setup(t),h=run(t,dir),s=await h.call('session/new',{cwd:dir});
  const prompt=h.call('session/prompt',{sessionId:s.sessionId,prompt:'PERMISSION'});
  const permission=await h.wait(x=>x.method==='session/request_permission');
  h.send({id:permission.id,result:{outcome:{outcome:'selected',optionId:'1'}}});await prompt;
  assert.ok(h.out.some(x=>x.params?.update?.content?.[0]?.content?.text.includes('Deny')));
  await h.call('session/set_mode',{sessionId:s.sessionId,modeId:'yolo'});
  const count=h.out.filter(x=>x.method==='session/request_permission').length;
  await h.call('session/prompt',{sessionId:s.sessionId,prompt:'PERMISSION'});assert.equal(h.out.filter(x=>x.method==='session/request_permission').length,count);
  const waiting=h.call('session/prompt',{sessionId:s.sessionId,prompt:'WAIT',clientUserMessageId:'waiting'});
  await h.wait(x=>x.params?.update?.clientUserMessageId==='waiting');h.send({method:'session/cancel',params:{sessionId:s.sessionId}});assert.equal((await waiting).stopReason,'cancelled');
  await h.call('session/prompt',{sessionId:s.sessionId,prompt:'COMPACT'});
  assert.ok(h.out.some(x=>x.params?.update?.sessionUpdate==='_workass_compaction'&&x.params.update.phase==='checkpoint'));
});


test('installed OMP commands preserve argument boundaries for binaries and Windows shims', async () => {
  const {installedOMPCommand} = await import('../omp-installed-host.mjs');
  const direct=installedOMPCommand('/installed/omp','/host/extension.mjs',{},'darwin');
  assert.equal(direct.command,'/installed/omp');
  assert.equal(direct.args.at(-1),'/host/extension.mjs');
  const executable=String.raw`C:\Users\A&B %name%!\omp.cmd`;
  const extension=String.raw`C:\Workass space\extension.mjs`;
  const shim=installedOMPCommand(executable,extension,{},'win32');
  assert.equal(shim.command,'cmd.exe');
  assert.deepEqual(shim.args.slice(0,4),['/d','/v:off','/s','/c']);
  assert.ok(!shim.args.at(-1).includes(executable));
  assert.equal(shim.env.WORKASS_OMP_EXECUTABLE,executable);
  assert.equal(shim.env.WORKASS_OMP_EXTENSION,extension);
  assert.throws(()=>installedOMPCommand('bad".cmd',extension,{},'win32'),/Invalid/);
});
