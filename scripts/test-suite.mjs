#!/usr/bin/env node
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { performance } from 'node:perf_hooks';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const limitMs = 10_000;

export function fullSuiteCommands(repo = root) {
  return [
    { name: 'renderer_tests', command: 'npm', args: ['test', '--silent'], cwd: path.join(repo, 'desktop/renderer2') },
    { name: 'shell_tests', command: 'node', args: ['--test', ...files(path.join(repo, 'desktop/shell'), /\.test\.js$/)], cwd: repo },
    { name: 'go_tests', command: 'go', args: ['test', './...', '-count=1', '-p=2', '-parallel=2', '-json'], cwd: repo },
    { name: 'script_tests', command: 'node', args: ['--test', ...files(path.join(repo, 'scripts/tests'), /\.test\.mjs$/)], cwd: repo },
  ];
}

function files(dir, pattern) {
  return fs.readdirSync(dir).filter(name => pattern.test(name)).sort().map(name => path.join(dir, name));
}

function testSummary(output) {
  const summaries = [...output.matchAll(/(?:# )?(\d+) (?:sub)?tests?\b/g)].map(m => Number(m[1]));
  const skipped = [...output.matchAll(/# skipped (\d+)/g)].reduce((sum, m) => sum + Number(m[1]), 0);
  if (summaries.length) return { tests: summaries.at(-1), skipped };
  const events = output.split('\n').flatMap(line => { try { return [JSON.parse(line)]; } catch { return []; } });
  const goTests = events.filter(event => event.Action === 'pass' && /^(Test|Example|Fuzz)/.test(event.Test ?? '')).length;
  const goSkipped = events.filter(event => event.Action === 'skip' && event.Test).length;
  return { tests: goTests || null, skipped: goSkipped };
}

export async function runSuiteMatrix(commands, { logDir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-test-suite-')), budgetMs = limitMs } = {}) {
  fs.mkdirSync(logDir, { recursive: true });
  const started = performance.now();
  let interrupted = false;
  const children = new Set();
  const onInterrupt = () => {
    interrupted = true;
    for (const child of children) {
      try { if (child.pid && process.platform !== 'win32') process.kill(-child.pid, 'SIGINT'); else child.kill('SIGINT'); }
      catch { child.kill('SIGINT'); }
    }
  };
  process.on('SIGINT', onInterrupt);
  process.on('SIGTERM', onInterrupt);
  const results = await Promise.all(commands.map(spec => new Promise(resolve => {
    const logPath = path.join(logDir, `${spec.name}.log`);
    const log = fs.createWriteStream(logPath, { flags: 'w' });
    const childStarted = performance.now();
    let output = '';
    let spawnError;
    let child;
    try {
      child = spawn(spec.command, spec.args ?? [], { cwd: spec.cwd ?? root, env: spec.env ?? process.env, stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32' });
      children.add(child);
      child.stdout.on('data', chunk => { log.write(chunk); output += chunk.toString(); });
      child.stderr.on('data', chunk => { log.write(chunk); output += chunk.toString(); });
      child.on('error', error => { spawnError = error; });
      child.on('close', (code, signal) => {
        children.delete(child);
        const summary = testSummary(output);
        log.end(() => resolve({ name: spec.name, code: code ?? 127, signal, elapsedMs: performance.now() - childStarted, ...summary, logPath, spawnError: spawnError?.message }));
      });
    } catch (error) {
      spawnError = error;
      log.end(() => resolve({ name: spec.name, code: 127, signal: null, elapsedMs: performance.now() - childStarted, tests: null, logPath, spawnError: error.message }));
    }
  })));
  process.removeListener('SIGINT', onInterrupt);
  process.removeListener('SIGTERM', onInterrupt);
  const elapsedMs = performance.now() - started;
  const correctness = !interrupted && results.every(result => result.code === 0 && !result.spawnError);
  const performanceStatus = elapsedMs <= budgetMs ? 'within_budget' : 'over_budget';
  return { results, elapsedMs, correctness, performanceStatus, logDir, interrupted };
}

function printReport(report) {
  for (const result of report.results) {
    const state = result.code === 0 && !result.spawnError ? 'passed' : 'failed';
    console.log(`WORKASS_TEST_SUITE name=${result.name} status=${state} exit=${result.code} seconds=${(result.elapsedMs / 1000).toFixed(3)} tests=${result.tests ?? 'unknown'} skipped=${result.skipped ?? 0} log=${result.logPath}`);
    if (state === 'failed') console.error(`WORKASS_TEST_SUITE_FAILURE name=${result.name} exit=${result.code} signal=${result.signal ?? 'none'}${result.spawnError ? ` spawn_error=${result.spawnError}` : ''} log=${result.logPath}`);
  }
  console.log(`WORKASS_TEST_SUITE_TOTAL correctness=${report.correctness ? 'passed' : 'failed'} performance=${report.performanceStatus} seconds=${(report.elapsedMs / 1000).toFixed(3)} budget_seconds=10.000 logs=${report.logDir}`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const report = await runSuiteMatrix(fullSuiteCommands());
  printReport(report);
  process.exitCode = report.correctness ? 0 : 1;
}
