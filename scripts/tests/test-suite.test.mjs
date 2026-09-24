import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { fileURLToPath } from 'node:url';
import { runSuiteMatrix, runSuiteLifecycle, testSummary, fullSuiteCommands, isMainModule } from '../test-suite.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const node = process.execPath;
const fixture = (name, source) => ({ name, command: node, args: ['-e', source] });
const temp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'workass-suite-test-'));

test('summarizes Node TAP reporter totals and keeps top-level and nested tests distinct', () => {
  const tap = `TAP version 13\n# Subtest: outer\n    # Subtest: inner\n    ok 1 - inner\n    1..1\nok 1 - outer\n# tests 2\n# pass 2\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n`;
  assert.deepEqual(testSummary(tap), { tests: 2, topLevelTests: 1, subtests: 1, passed: 2, failed: 0, skipped: 0 });
});

test('summarizes Node spec reporter totals from complete output', () => {
  const spec = `✔ outer (2ms)\n  ✔ inner (1ms)\n✔ parent suite\nℹ tests 2\nℹ suites 1\nℹ pass 2\nℹ fail 0\nℹ cancelled 0\nℹ skipped 0\nℹ todo 0\n`;
  assert.deepEqual(testSummary(spec), { tests: 2, topLevelTests: 1, subtests: 1, passed: 2, failed: 0, skipped: 0 });
});

test('full suite partitions every renderer test across six sequential isolated Node processes', () => {
  const commands = fullSuiteCommands();
  const renderer = commands.filter(command => /^renderer_tests_[1-6]$/.test(command.name));
  const rendererFiles = fs.readdirSync(path.join(root, 'desktop/renderer2/tests')).filter(name => name.endsWith('.test.ts')).sort();
  const shell = commands.find(command => command.name === 'shell_tests');
  const scripts = commands.find(command => command.name === 'script_tests');
  const codexHost = commands.find(command => command.name === 'codex_native_host_tests');
  const go = commands.find(command => command.name === 'go_tests');
  const shellContracts = commands.filter(command => command.name.startsWith('script_contract_'));
  assert.deepEqual(renderer.map(command => command.name), ['renderer_tests_1', 'renderer_tests_2', 'renderer_tests_3', 'renderer_tests_4', 'renderer_tests_5', 'renderer_tests_6']);
  const rendererGroups = renderer.map(command => {
    assert.deepEqual(command.args.slice(0, 4), ['--experimental-strip-types', '--test', '--test-isolation=none', '--test-concurrency=1']);
    assert.equal(command.cwd, path.join(root, 'desktop/renderer2'));
    return command.args.slice(4).map(file => path.relative(path.join(root, 'desktop/renderer2/tests'), file));
  });
  const assignedRendererFiles = rendererGroups.flat();
  assert.equal(new Set(assignedRendererFiles).size, rendererFiles.length);
  assert.deepEqual(assignedRendererFiles.slice().sort(), rendererFiles);
  assert.ok(rendererGroups.every(group => group.length > 0));
  assert.equal(shell.args[1], '--test-concurrency=4');
  assert.equal(shell.args.filter(arg => arg.endsWith('.test.js')).length, fs.readdirSync(path.join(root, 'desktop/shell')).filter(name => name.endsWith('.test.js')).length);
  assert.equal(scripts.args[1], '--test-concurrency=6');
  const scriptTestDir = path.join(root, 'scripts/tests');
  const inventory = fs.readdirSync(scriptTestDir).filter(name => name.endsWith('.test.mjs')).sort();
  const measuredHeavy = ['release-pipeline-gates.test.mjs', 'release-pipeline-ship.test.mjs', 'release-pipeline-receipts.test.mjs', 'release-pipeline.test.mjs'];
  const scheduled = scripts.args.slice(2).map(file => path.basename(file));
  const codexHostFile = 'codex-native-host.test.mjs';
  assert.deepEqual(scheduled, [...measuredHeavy, ...inventory.filter(name => !measuredHeavy.includes(name) && name !== codexHostFile)]);
  assert.ok(codexHost, 'Codex native host tests have their own command');
  assert.deepEqual(codexHost.args, [path.join(root, 'scripts/test-native-host-suite.mjs'), path.join(scriptTestDir, codexHostFile)]);
  assert.equal(codexHost.cwd, root);
  assert.equal(codexHost.requireTestReport, true);
  const allScheduledScriptTests = [...scheduled, ...codexHost.args.filter(arg => arg.endsWith('.test.mjs')).map(file => path.basename(file))];
  assert.equal(new Set(allScheduledScriptTests).size, inventory.length, 'every script test is scheduled exactly once across both commands');
  assert.deepEqual(allScheduledScriptTests.slice().sort(), inventory, 'both commands cover the complete script test inventory');
  assert.equal(go.args.includes('--workers'), false, 'Go suite selects its host-aware default worker count');
  assert.equal(go.requireTestReport, true);
  assert.equal(go.requireGoReport, true);
  assert.equal(scripts.requireTestReport, true);
  assert.equal(scripts.args.filter(arg => arg.endsWith('.test.mjs')).length + codexHost.args.filter(arg => arg.endsWith('.test.mjs')).length, inventory.length);
  const shellFiles = fs.readdirSync(path.join(root, 'scripts/tests')).filter(name => name.endsWith('.test.sh')).sort();
  assert.equal(shellContracts.length, shellFiles.length);
  assert.deepEqual(shellContracts.map(command => path.basename(command.args[0])).sort(), shellFiles);
  assert.ok(shellContracts.every(command => command.command === 'sh' && command.args.length === 1 && command.cwd === root));
});

test('counts Go JSON pass, fail, skip outcomes and separates nested tests', () => {
  const go = [
    { Action: 'pass', Test: 'TestOuter' },
    { Action: 'fail', Test: 'TestOuter/child' },
    { Action: 'skip', Test: 'TestSkipped' },
    { Action: 'fail', Package: 'example/pkg' },
  ].map(event => JSON.stringify(event)).join('\n');
  assert.deepEqual(testSummary(go), { tests: 3, topLevelTests: 2, subtests: 1, passed: 1, failed: 1, skipped: 1 });
});

test('counts the grouped Go runner summary instead of an output tail', () => {
  const result = { ok: true, discovered: 20, topLevelTests: 25, tests: 34, nestedRun: 5, passed: 20, failed: 0, skipped: 0, otherGo: { tests: 9, topLevelTests: 5, nestedTests: 4, passed: 9, failed: 0, skipped: 0 } };
  assert.deepEqual(testSummary(JSON.stringify(result, null, 2)), { tests: 34, topLevelTests: 25, subtests: 9, passed: 29, failed: 0, skipped: 0 });
});

test('canonical main detection accepts a symlinked entry and safely rejects missing paths', t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const alias = path.join(dir, 'runner.mjs');
  fs.symlinkSync(path.join(root, 'scripts/test-suite.mjs'), alias);
  assert.equal(isMainModule(alias, path.join(root, 'scripts/test-suite.mjs')), true);
  assert.equal(isMainModule(path.join(dir, 'missing'), path.join(root, 'scripts/test-suite.mjs')), false);
  assert.equal(isMainModule(undefined, path.join(root, 'scripts/test-suite.mjs')), false);
});

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

test('required test command with exit zero and no report fails while generic fixtures remain valid', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const report = await runSuiteMatrix([
    { ...fixture('missing_report', "console.log('no tests ran')"), requireTestReport: true },
    fixture('generic_fixture', "console.log('fixture succeeded')"),
  ], { logDir: dir });
  assert.equal(report.correctness, false);
  assert.equal(report.results.find(result => result.name === 'missing_report').code, 1);
  assert.match(report.results.find(result => result.name === 'missing_report').reportError, /missing/);
  assert.equal(report.results.find(result => result.name === 'generic_fixture').code, 0);
  assert.equal(report.results.find(result => result.name === 'generic_fixture').reportError, undefined);
  assert.match(fs.readFileSync(path.join(dir, 'missing_report.log'), 'utf8'), /no tests ran/);
});

test('required Go report with ok false fails even when its output parses', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const output = JSON.stringify({ ok: false, discovered: 3, tests: 3, passed: 3, failed: 0, otherGo: { tests: 1, passed: 1, failed: 0 } }, null, 2);
  const report = await runSuiteMatrix([{ ...fixture('false_go', `console.log(${JSON.stringify(output)})`), requireTestReport: true, requireGoReport: true }], { logDir: dir });
  assert.equal(report.correctness, false);
  assert.equal(report.results[0].tests, 3);
  assert.match(report.results[0].reportError, /failed/);
  assert.match(fs.readFileSync(path.join(dir, 'false_go.log'), 'utf8'), /"ok": false/);
});

test('counts TAP totals after output has exceeded the former 32 KB tail', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const source = `process.stdout.write('x'.repeat(40000)+'\\n# tests 1\\n# pass 1\\n# fail 0\\n# skipped 0\\n')`;
  const report = await runSuiteMatrix([fixture('long_tap', source)], { logDir: dir });
  assert.equal(report.correctness, true);
  assert.equal(report.results[0].tests, 1);
  assert.equal(report.results[0].passed, 1);
  assert.equal(fs.statSync(path.join(dir, 'long_tap.log')).size > 40000, true);
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

test('log sink errors become controlled suite failures and stop sibling children', async t => {
  const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.mkdirSync(path.join(dir, 'broken.log'));
  const report = await runSuiteMatrix([
    fixture('broken', "setInterval(() => console.log('still running'), 5)"),
    fixture('sibling', "setInterval(() => {}, 1000)"),
  ], { logDir: dir });
  assert.equal(report.correctness, false);
  assert.ok(report.results.find(result => result.name === 'broken').sinkError);
  assert.ok(report.results.every(result => result.code !== 0));
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

test('interrupt during fixture provisioning joins the prestarted matrix and cleans the allocated fixture', async () => {
  const signals = new EventEmitter();
  let finishProvisioning;
  let cleaned = false;
  let matrixStarted = false;
  const lifecycle = runSuiteLifecycle({
    signalTarget: signals,
    createFixture: () => new Promise(resolve => { finishProvisioning = resolve; }),
    runMatrix: async (_commands, { fixturePromise }) => {
      matrixStarted = true;
      await fixturePromise;
      return { results: [], correctness: false, performanceStatus: 'over_budget', elapsedMs: 0, logDir: '(test)' };
    },
  });
  await new Promise(resolve => setImmediate(resolve));
  signals.emit('SIGTERM');
  finishProvisioning({ root: '/unused-test-fixture', cleanup: async () => { cleaned = true; } });
  const result = await lifecycle;
  assert.equal(matrixStarted, true);
  assert.equal(cleaned, true);
  assert.equal(result.interrupted, true);
  assert.equal(result.report.correctness, false);
  assert.equal(result.report.results[0].name, 'fixture_volume_interrupted');
  assert.equal(signals.listenerCount('SIGINT'), 0);
  assert.equal(signals.listenerCount('SIGTERM'), 0);
});

test('fixture failure and interruption both kill and join the prestarted Go child', async t => {
  for (const mode of ['fixture-failure', 'interrupt']) {
    const dir = temp(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const controller = new AbortController();
    let rejectFixture;
    const fixturePromise = new Promise((resolve, reject) => { rejectFixture = reject; });
    let killed = false, closed = false;
    const report = runSuiteMatrix([{ name: 'go_tests', command: 'node', fixtureRootIpc: true, requireTestReport: true }], {
      logDir: dir, fixturePromise, abortSignal: controller.signal,
      spawnCommand: () => {
        const child = new EventEmitter();
        child.stdout = new PassThrough(); child.stderr = new PassThrough(); child.stdio = [null, child.stdout, child.stderr, new PassThrough()];
        child.kill = () => { killed = true; setImmediate(() => { closed = true; child.emit('close', null, 'SIGINT'); }); return true; };
        setImmediate(() => child.emit('spawn'));
        child.on('close', () => { closed = true; });
        return child;
      },
    });
    await new Promise(resolve => setImmediate(resolve));
    if (mode === 'fixture-failure') rejectFixture(new Error('provision failed'));
    else controller.abort();
    await report;
    assert.equal(killed, true, `${mode} cancels the prestarted Go child`);
    assert.equal(closed, true, `${mode} joins the Go child before returning`);
  }
});

test('interrupt during fixture cleanup waits for cleanup before returning a failed report', async () => {
  const signals = new EventEmitter();
  let finishCleanup;
  let cleanupStarted = false;
  const lifecycle = runSuiteLifecycle({
    signalTarget: signals,
    createFixture: async () => ({ root: '/unused-test-fixture', cleanup: () => {
      cleanupStarted = true;
      return new Promise(resolve => { finishCleanup = resolve; });
    } }),
    runMatrix: async () => ({ results: [], correctness: true, performanceStatus: 'within_budget', elapsedMs: 0, logDir: '(test)' }),
  });
  while (!cleanupStarted) await new Promise(resolve => setImmediate(resolve));
  signals.emit('SIGINT');
  let returned = false;
  lifecycle.then(() => { returned = true; });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(returned, false);
  finishCleanup();
  const result = await lifecycle;
  assert.equal(result.interrupted, true);
  assert.equal(result.report.correctness, false);
  assert.equal(signals.listenerCount('SIGINT'), 0);
  assert.equal(signals.listenerCount('SIGTERM'), 0);
});
