// Runs no prompts or inference; uses the user's executable with isolated state.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import readline from 'node:readline';
const executable = process.argv[2];
if (!executable) throw new Error('Pass the installed OMP executable');
const temp = await fs.mkdtemp(path.join(os.tmpdir(), 'workass-installed-omp-'));
const instructions = path.join(temp,'instructions.txt');
await fs.writeFile(instructions,'Workass SDK smoke instructions');
let child;
const start = () => {
  child = spawn(process.execPath,[path.resolve('scripts/omp-installed-host.mjs')], {cwd:temp,env:{...process.env,HOME:temp,USERPROFILE:temp,OMP_PROFILE:'',PI_PROFILE:'',PI_CODING_AGENT_DIR:path.join(temp,'agent'),OMP_AUTH_BROKER_URL:'',WORKASS_OMP_EXECUTABLE:executable,WORKASS_INSTRUCTIONS_FILE:instructions},stdio:['pipe','pipe','pipe']});
  const waiting = new Map(); let seq=0;
  child.stderr.resume();
  readline.createInterface({input:child.stdout}).on('line', line=>{
    const row=JSON.parse(line), p=waiting.get(row.id);
    if(p){waiting.delete(row.id);clearTimeout(p.timer);row.error?p.reject(new Error(row.error.message)):p.resolve(row.result);}
  });
  child.on('exit',()=>{for(const p of waiting.values()){clearTimeout(p.timer);p.reject(new Error('Installed OMP exited'));}waiting.clear();});
  return (method,params={}) => new Promise((resolve,reject)=>{
    const id=++seq;
    const timer=setTimeout(()=>{waiting.delete(id);reject(new Error(`Timeout: ${method}`));},15000);
    waiting.set(id,{resolve,reject,timer});child.stdin.write(JSON.stringify({jsonrpc:'2.0',id,method,params})+'\n');
  });
};
const stop=async()=>{if(child.exitCode!==null)return;const done=new Promise(resolve=>child.once('exit',resolve));child.stdin.end();await done;};
try {
  let call=start();
  const init=await call('initialize'); assert.equal(init.agentInfo.name,'oh-my-pi');
  const session=await call('session/new',{cwd:temp}); assert.ok(session.sessionId);
  for(const mode of ['always-ask','write','yolo','plan','default']) {
    const result=await call('session/set_mode',{sessionId:session.sessionId,modeId:mode});
    assert.equal(result.configOptions.find(x=>x.id==='mode').currentValue,mode);
  }
  await call('session/close',{sessionId:session.sessionId}); await stop();
  call=start();await call('initialize');
  await call('session/resume',{sessionId:session.sessionId,cwd:temp});
  await assert.rejects(call('session/resume',{sessionId:'missing-session',cwd:temp}),/never materialized/);
  await call('session/close',{sessionId:session.sessionId});await stop();
  console.log('PASS: installed OMP SDK, all modes, process restart/exact resume, missing session rejection; no inference');
} finally {child?.kill();await fs.rm(temp,{recursive:true,force:true});}
