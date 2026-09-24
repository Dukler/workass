import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { runSuiteMatrix } from '../test-suite.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const node = process.execPath;
const fixture = (name, source) => ({ name, command: node, args: ['-e', source] });
const temp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'workass-suite-test-'));

test('suite matrix runs commands concurrently and retains complete logs', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const source = mark => `const fs=require('node:fs'),p=${JSON.stringify(dir)};fs.writeFileSync(p+'/${mark}','ready');const end=Date.now()+5000;const timer=setInterval(()=>{if(fs.existsSync(p+'/one-ready')&&fs.existsSync(p+'/two-ready')){clearInterval(timer);console.log('both-ready');process.exit(0)}if(Date.now()>end){clearInterval(timer);console.error('concurrency timeout');process.exit(8)}},10)`;
  const report = await runSuiteMatrix([
    fixture('one', source('one-ready')),
    fixture('two', source('two-ready')),
  ], { logDir: dir });
  assert.equal(report.correctness, true);
  assert.match(fs.readFileSync(path.join(dir, 'one.log'), 'utf8'), /both-ready/);
  assert.match(fs.readFileSync(path.join(dir, 'two.log'), 'utf8'), /both-ready/);
  assert.ok(report.results.every(result => result.code === 0));
});

test('nonzero exit and spawn errors are reported without losing the other suite', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const report = await runSuiteMatrix([
    { name: 'missing', command: path.join(dir, 'does-not-exist'), args: [] },
    fixture('failure', "console.log('assertion detail'); process.exitCode = 7"),
    fixture('success', "console.log('survived')"),
  ], { logDir: dir });
  assert.equal(report.correctness, false);
  assert.ok(report.results.find(result => result.name === 'missing').spawnError);
  assert.equal(report.results.find(result => result.name === 'failure').code, 7);
  assert.match(fs.readFileSync(path.join(dir, 'failure.log'), 'utf8'), /assertion detail/);
  assert.equal(report.results.find(result => result.name === 'success').code, 0);
});

test('over-budget suites finish before reporting the performance miss', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const report = await runSuiteMatrix([fixture('slow', "setTimeout(() => console.log('finished'), 80)")], { logDir: dir, budgetMs: 10 });
  assert.equal(report.correctness, true);
  assert.equal(report.performanceStatus, 'over_budget');
  assert.match(fs.readFileSync(path.join(dir, 'slow.log'), 'utf8'), /finished/);
});

test('interrupt is forwarded to running children and waits for cleanup', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const harness = `import {runSuiteMatrix} from ${JSON.stringify(new URL('../test-suite.mjs', import.meta.url).href)};\n` +
    `const r=await runSuiteMatrix([{name:'child',command:process.execPath,args:['-e',"console.log('ready');process.on('SIGINT',()=>{console.log('cleaned');process.exit(0)});setInterval(()=>{},1000)"]}],{logDir:${JSON.stringify(dir)}});\n` +
    `console.log(JSON.stringify({interrupted:r.interrupted,code:r.results[0].code}));`;
  const child = spawn(node, ['--input-type=module', '-e', harness], { stdio: ['ignore', 'pipe', 'pipe'] });
  let output = ''; child.stdout.on('data', chunk => output += chunk.toString());
  const finished = new Promise((resolve, reject) => { child.on('close', code => resolve(code)); child.on('error', reject); });
  const log = path.join(dir, 'child.log');
  const readyBy = Date.now() + 5000;
  while ((!fs.existsSync(log) || !fs.readFileSync(log, 'utf8').includes('ready')) && Date.now() < readyBy) {
    await new Promise(resolve => setTimeout(resolve, 20));
  }
  assert.ok(fs.existsSync(log) && fs.readFileSync(log, 'utf8').includes('ready'), 'child reached observable ready state');
  child.kill('SIGINT');
  assert.equal(await finished, 0);
  assert.match(output, /"interrupted":true/);
  assert.match(fs.readFileSync(path.join(dir, 'child.log'), 'utf8'), /cleaned/);
});
