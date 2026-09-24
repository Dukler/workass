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
    { name: 'go_tests', command: 'node', args: [path.join(repo, 'scripts/test-go-suite.mjs'), '--workers', '6', '--cwd', repo], cwd: repo },
    { name: 'script_tests', command: 'node', args: ['--test', ...files(path.join(repo, 'scripts/tests'), /\.test\.mjs$/)], cwd: repo },
  ];
}

function files(dir, pattern) {
  return fs.readdirSync(dir).filter(name => pattern.test(name)).sort().map(name => path.join(dir, name));
}

export function testSummary(output) {
  const lines = output.split('\n');
  let jsonSummary = lines.map(line => { try { return JSON.parse(line); } catch { return null; } }).find(event => event && typeof event.ok === 'boolean' && Number.isInteger(event.discovered) && event.otherGo);
  if (!jsonSummary) {
    try {
      const parsed = JSON.parse(output.trim());
      if (parsed && typeof parsed.ok === 'boolean' && Number.isInteger(parsed.discovered) && parsed.otherGo) jsonSummary = parsed;
    } catch {}
  }
  if (jsonSummary) {
    const tests = jsonSummary.tests ?? (jsonSummary.discovered + (jsonSummary.nestedRun ?? 0) + (jsonSummary.otherGo.tests ?? 0));
    const passed = jsonSummary.passed + (jsonSummary.otherGo.passed ?? 0);
    const failed = jsonSummary.failed + (jsonSummary.otherGo.failed ?? 0);
    const skipped = jsonSummary.skipped + (jsonSummary.otherGo.skipped ?? 0);
    const subtests = (jsonSummary.nestedRun ?? 0) + (jsonSummary.otherGo.nestedTests ?? 0);
    return { tests, topLevelTests: jsonSummary.topLevelTests ?? (tests - subtests), subtests, passed, failed, skipped };
  }
  const events = lines.flatMap(line => { try { return [JSON.parse(line)]; } catch { return []; } });
  if (events.some(event => event && typeof event.Action === 'string')) {
    const outcomes = events.filter(event => event.Test && ['pass', 'fail', 'skip'].includes(event.Action));
    const top = outcomes.filter(event => !event.Test.includes('/'));
    const subs = outcomes.filter(event => event.Test.includes('/'));
    return { tests: outcomes.length || null, topLevelTests: top.length, subtests: subs.length,
      passed: outcomes.filter(e => e.Action === 'pass').length,
      failed: outcomes.filter(e => e.Action === 'fail').length,
      skipped: outcomes.filter(e => e.Action === 'skip').length };
  }
  const totals = Object.fromEntries([...output.matchAll(/^(?:#|ℹ)\s*(tests|pass|fail|cancelled|skipped)\s+(\d+)\s*$/gm)].map(m => [m[1], Number(m[2])]));
  if (totals.tests !== undefined) {
    const topLevelTests = lines.filter(line => /^# Subtest:/.test(line)).length || lines.filter(line => /^(?:✔ |✖ |﹣ ).+ \([\d.]+(?:ms|s)\)/.test(line)).length;
    const allSubtests = lines.filter(line => /^(?:\s+# Subtest:|\s+✔ |\s+✖ |\s+﹣ )/.test(line)).length;
    const skipped = totals.skipped ?? 0;
    const failed = totals.fail ?? 0;
    return { tests: totals.tests, topLevelTests: topLevelTests || null,
      subtests: allSubtests || (topLevelTests ? Math.max(0, totals.tests - topLevelTests) : null),
      passed: totals.pass ?? Math.max(0, totals.tests - failed - skipped), failed, skipped };
  }
  return { tests: null, topLevelTests: null, subtests: null, passed: null, failed: null, skipped: 0 };
}

function failureDetail(output) {
  const lines = output.slice(-32_000).split(/\r?\n/);
  const details = [];
  for (const line of lines) {
    try {
      const event = JSON.parse(line);
      if (event.Action === 'fail' && event.Test) details.push(`Go test failed: ${event.Test}`);
      else if (event.Action === 'output' && event.Test && /(?:FAIL|Error|panic|assert)/i.test(event.Output ?? '')) details.push(`${event.Test}: ${(event.Output ?? '').trim()}`);
      continue;
    } catch {}
    if (/(?:not ok|✖|failed|failure|assert(?:ion)?|error|exception|timed out)/i.test(line)) details.push(line);
  }
  return (details.length ? details.slice(-12) : lines.filter(Boolean).slice(-6)).join('\n').slice(-2_000);
}

export async function runSuiteMatrix(commands, { logDir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-test-suite-')), budgetMs = limitMs, startedAt = performance.now() } = {}) {
  fs.mkdirSync(logDir, { recursive: true });
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
    let sinkError;
    let child;
    const stopChildren = () => {
      for (const running of children) {
        try { if (running.pid && process.platform !== 'win32') process.kill(-running.pid, 'SIGTERM'); else running.kill('SIGTERM'); }
        catch { running.kill('SIGTERM'); }
      }
    };
    const append = chunk => {
      output = (output + chunk.toString()).slice(-32_000);
      if (!sinkError) log.write(chunk, error => { if (error) { sinkError = error; stopChildren(); } });
    };
    log.on('error', error => { sinkError = error; stopChildren(); });
    try {
      child = spawn(spec.command, spec.args ?? [], { cwd: spec.cwd ?? root, env: spec.env ?? process.env, stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32' });
      children.add(child);
      child.stdout.on('data', append);
      child.stderr.on('data', append);
      child.on('error', error => { spawnError = error; });
      child.on('close', (code, signal) => {
        children.delete(child);
        const finish = () => {
          let completeOutput = output;
          try { completeOutput = fs.readFileSync(logPath, 'utf8'); } catch {}
          const summary = testSummary(completeOutput);
          resolve({ name: spec.name, code: sinkError ? 1 : (code ?? 127), signal, elapsedMs: performance.now() - childStarted, ...summary, logPath, spawnError: spawnError?.message, sinkError: sinkError?.message, failureDetail: code === 0 && !sinkError ? undefined : failureDetail(completeOutput) });
        };
        if (sinkError) { log.destroy(); finish(); }
        else log.end(finish);
      });
    } catch (error) {
      spawnError = error;
      log.end(() => resolve({ name: spec.name, code: 127, signal: null, elapsedMs: performance.now() - childStarted, ...testSummary(output), logPath, spawnError: error.message, failureDetail: failureDetail(output) }));
    }
  })));
  process.removeListener('SIGINT', onInterrupt);
  process.removeListener('SIGTERM', onInterrupt);
  const elapsedMs = performance.now() - startedAt;
  const correctness = !interrupted && results.every(result => result.code === 0 && !result.spawnError && !result.sinkError);
  const performanceStatus = elapsedMs <= budgetMs ? 'within_budget' : 'over_budget';
  return { results, elapsedMs, correctness, performanceStatus, logDir, interrupted };
}

function printReport(report) {
  for (const result of report.results) {
    const state = result.code === 0 && !result.spawnError ? 'passed' : 'failed';
    console.log(`WORKASS_TEST_SUITE name=${result.name} status=${state} exit=${result.code} seconds=${(result.elapsedMs / 1000).toFixed(3)} tests=${result.tests ?? 'unknown'} top_level=${result.topLevelTests ?? 'unknown'} subtests=${result.subtests ?? 'unknown'} passed=${result.passed ?? 'unknown'} failed=${result.failed ?? 'unknown'} skipped=${result.skipped ?? 0} log=${result.logPath}`);
    if (state === 'failed') console.error(`WORKASS_TEST_SUITE_FAILURE name=${result.name} exit=${result.code} signal=${result.signal ?? 'none'}${result.spawnError ? ` spawn_error=${result.spawnError}` : ''}${result.sinkError ? ` log_sink_error=${result.sinkError}` : ''}${result.failureDetail ? `\n${result.failureDetail}` : ''} log=${result.logPath}`);
  }
  console.log(`WORKASS_TEST_SUITE_TOTAL correctness=${report.correctness ? 'passed' : 'failed'} performance=${report.performanceStatus} seconds=${(report.elapsedMs / 1000).toFixed(3)} budget_seconds=10.000 logs=${report.logDir}`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const startedAt = performance.now();
  const commands = fullSuiteCommands();
  const report = await runSuiteMatrix(commands, { startedAt });
  printReport(report);
  process.exitCode = report.correctness ? 0 : 1;
}
