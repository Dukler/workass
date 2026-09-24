#!/usr/bin/env node
import { spawn as nodeSpawn } from 'node:child_process';
import { createWriteStream } from 'node:fs';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { pathToFileURL } from 'node:url';
import { randomUUID } from 'node:crypto';

const HINTS = new Map([
  ['TestProviderDetectionAllowsFullInitializeAndSessionBudgets', 5.62],
  ['TestNextTurnListsAndWaitsOnAdoptedSubagent', 3.18],
  ['TestSubagentSurvivesCancelledParentSettlesAndWritesReceipt', 2.68],
  ['TestMockBurstStreamsAtDisplayCadenceWithoutDroppingText', 2.39],
  ['TestSubagentLatchedPermissionAttentionSurfacesToAdoptingTurn', 1.83],
  ['TestStopSpawnedWorkKillsARealProcessThatIgnoresSIGTERM', 1.83],
  ['TestProviderUpdateInvokeRejectsDoubleUnknownAndNoPending', 1.59],
  ['TestPiNativeSDKProviderContext', 1.57],
  ['TestDetectProvidersLocalServerRegistersNativeProviderAndStreamsThroughAgent', 1.47],
  ['TestProviderRegistryCatalogToggleFailureAndConcurrentIsolation', 1.39],
  ['TestCancelInPromptPreparationGap', 1.37],
  ['TestMockInitializeSessionPromptCancelErrorAndReuse', 1.32],
  ['TestMockSteerMidSlowTurnReflectedInOutput', 1.30],
  ['TestProviderUpdateInvokeFailureKeepsCardWithRedactedTail', 1.28],
  ['TestQwenStandaloneUpdateUsesBundledUpdaterAtCompatibleRelease', 1.27],
  ['TestProviderUpdateZeroExitWithoutVersionAdvanceFailsVerification', 1.26],
  ['TestACPEffortSelectionBeforeAxisDiscovery', 1.25],
  ['TestProviderUpdateInvokeProgressNoProcRegistryAndReplay', 1.25],
]);
const DEFAULT_SECONDS = 0.25;
// These provider-startup probes have two-second readiness deadlines and launch
// several child processes. Both missed under six-way process pressure and pass
// together serially, so this explicit boundary keeps their assertions intact.
const SERIAL_TESTS = new Set([
  'TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession',
  'TestStartupDetectionDoesNotRetryDevinNeedsLogin',
]);

export function parseTestList(output) {
  const names = [];
  for (const line of output.split(/\r?\n/)) {
    const name = line.trim();
    if (/^(?:Test|Example|Fuzz)[A-Za-z0-9_]*$/.test(name)) names.push(name);
  }
  return [...new Set(names)].sort();
}

export function anchoredTestPattern(name) {
  return `^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`;
}


export function inspectCaseOutput(name, code, output) {
  const lines = output.split(/\r?\n/);
  const runs = lines.flatMap(line => {
    const match = line.trim().match(/^=== RUN\s+(.+)$/);
    return match ? [match[1]] : [];
  });
  const terminals = lines.flatMap(line => {
    const match = line.match(/^--- (PASS|SKIP|FAIL): (.+?)(?: \(.*\))?$/);
    return match ? [{ action: match[1].toLowerCase(), name: match[2] }] : [];
  });
  const rootPasses = terminals.filter(item => item.name === name && item.action === 'pass').length;
  const rootSkips = terminals.filter(item => item.name === name && item.action === 'skip').length;
  const rootFailures = terminals.filter(item => item.name === name && item.action === 'fail').length;
  const rootRuns = runs.filter(item => item === name).length;
  return {
    rootRuns, rootPasses, rootSkips, rootFailures,
    nestedRun: runs.filter(item => item !== name).length,
    nestedSkipped: terminals.filter(item => item.name !== name && item.action === 'skip').length,
    coverageError: rootRuns !== 1 || rootPasses + rootSkips + rootFailures !== 1 || (code === 0) !== (rootPasses + rootSkips === 1),
    outcome: rootPasses ? 'pass' : rootSkips ? 'skip' : 'fail',
  };
}

export function allocateWorkers(names, workerCount, hints = HINTS) {
  if (!Number.isInteger(workerCount) || workerCount < 1) throw new Error('workerCount must be a positive integer');
  const workers = Array.from({ length: workerCount }, (_, id) => ({ id, weight: 0, names: [] }));
  const sorted = [...names].sort((a, b) => (hints.get(b) ?? DEFAULT_SECONDS) - (hints.get(a) ?? DEFAULT_SECONDS) || a.localeCompare(b));
  for (const name of sorted) {
    const worker = workers.reduce((best, item) => item.weight < best.weight ? item : best);
    worker.names.push(name);
    worker.weight += hints.get(name) ?? DEFAULT_SECONDS;
  }
  return workers;
}

export function partitionSerialCases(names, serialNames = SERIAL_TESTS) {
  return { serial: names.filter(name => serialNames.has(name)), parallel: names.filter(name => !serialNames.has(name)) };
}

function appendJsonLine(stream, value) { stream.write(`${JSON.stringify(value)}\n`); }

function spawnLogged(command, args, options, active) {
  return new Promise((resolve) => {
    const started = performance.now();
    let stdout = '', stderr = '', spawnError = null;
    const child = (options.spawn ?? nodeSpawn)(command, args, {
      cwd: options.cwd, env: options.env ?? process.env,
      stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32',
    });
    active.add(child);
    child.stdout?.on('data', chunk => { stdout += chunk; });
    child.stderr?.on('data', chunk => { stderr += chunk; });
    child.on('error', error => { spawnError = error; });
    child.on('close', (code, signal) => {
      active.delete(child);
      resolve({ code: code ?? (spawnError ? 127 : 1), signal, spawnError: spawnError?.message ?? null, stdout, stderr, elapsedMs: performance.now() - started });
    });
  });
}

async function stopChildren(active) {
  const children = [...active];
  for (const child of children) {
    try {
      if (process.platform === 'win32') child.kill('SIGTERM');
      else process.kill(-child.pid, 'SIGTERM');
    } catch { try { child.kill('SIGTERM'); } catch {} }
  }
  await Promise.race([Promise.all(children.map(child => new Promise(resolve => child.once('close', resolve)))), new Promise(resolve => setTimeout(resolve, 1000))]);
  for (const child of [...active]) {
    try { if (process.platform === 'win32') child.kill('SIGKILL'); else process.kill(-child.pid, 'SIGKILL'); } catch { try { child.kill('SIGKILL'); } catch {} }
  }
}

export async function runGoSuite({ cwd = process.cwd(), logDir, workers = 6, race = false, go = 'go', spawn = nodeSpawn, signalHandlers = true, abortSignal } = {}) {
  if (!Number.isInteger(workers) || workers < 1) throw new Error('workers must be a positive integer');
  workers = Math.min(workers, 6);
  const wallStart = performance.now();
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-matrix-'));
  const logs = logDir ?? path.join(os.tmpdir(), `workass-go-matrix-logs-${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}`);
  await mkdir(logs, { recursive: true });
  const runId = `${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}-${process.pid}-${randomUUID()}`;
  const jsonlPath = path.join(logs, `go-matrix-${runId}.jsonl`);
  const summaryPath = path.join(logs, `go-matrix-${runId}.json`);
  const jsonl = createWriteStream(jsonlPath, { flags: 'wx' });
  const active = new Set();
  let interrupted = null;
  const onInterrupt = signal => { interrupted ??= signal; void stopChildren(active); };
  if (signalHandlers) { process.on('SIGINT', onInterrupt); process.on('SIGTERM', onInterrupt); }
  const onAbort = () => onInterrupt('ABORT');
  abortSignal?.addEventListener('abort', onAbort, { once: true });
  const run = async (command, args, label, commandCwd = cwd) => {
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    appendJsonLine(jsonl, { event: 'command-start', label, command, args, cwd: commandCwd, at: new Date().toISOString() });
    const tempDir = path.join(root, `command-${label}`);
    await mkdir(tempDir, { recursive: true });
    const result = await spawnLogged(command, args, { cwd: commandCwd, spawn, env: { ...process.env, TMPDIR: tempDir, TMP: tempDir, TEMP: tempDir } }, active);
    appendJsonLine(jsonl, { event: 'command-end', label, ...result });
    if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label, text: result.stdout });
    if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label, text: result.stderr });
    if (result.code !== 0) throw Object.assign(new Error(`${label} failed with exit ${result.code}${result.signal ? ` (${result.signal})` : ''}${result.spawnError ? `: ${result.spawnError}` : ''}`), { result, label });
    return result;
  };
  const summary = { ok: false, interrupted: null, cwd, workers, acp: { discovered: 0, passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedSkipped: 0, cases: [] }, otherPackages: [], otherGo: { tests: 0, passed: 0, failed: 0, skipped: 0 }, elapsedMs: 0, jsonlPath, summaryPath };
  try {
    const goFlags = race ? ['-race'] : [];
    await run(go, [...goFlags, 'test', '-c', '-o', path.join(root, 'acp.test'), './internal/acp'], 'compile-acp');
    const binary = path.join(root, 'acp.test');
    const packageCwd = path.join(cwd, 'internal', 'acp');
    const listing = await run(binary, ['-test.list', '.'], 'list-acp', packageCwd);
    const names = parseTestList(listing.stdout);
    if (!names.length) throw new Error('ACP test binary discovered no Test, Example, or Fuzz cases');
    const { parallel, serial } = partitionSerialCases(names);
    const allocations = allocateWorkers(parallel, workers);
    summary.acp.discovered = names.length;
    summary.acp.serialCases = serial;
    summary.acp.workerLoads = allocations.map(w => ({ worker: w.id, hintedSeconds: Number(w.weight.toFixed(2)), cases: w.names.length }));
    const runCase = async (name, worker, boundary) => {
      if (interrupted) return;
      const tempDir = path.join(root, `worker-${worker}-${name.replace(/[^A-Za-z0-9_-]/g, '_')}`);
      await mkdir(tempDir, { recursive: true });
      if (interrupted) return;
      const result = await spawnLogged(binary, ['-test.v', '-test.run', anchoredTestPattern(name), '-test.count=1', '-test.parallel=2'], { cwd: packageCwd, spawn, env: { ...process.env, TMPDIR: tempDir, TMP: tempDir, TEMP: tempDir } }, active);
      const inspected = inspectCaseOutput(name, result.code, result.stdout);
      const record = { name, worker, boundary, cwd: packageCwd, elapsedMs: result.elapsedMs, code: result.code, signal: result.signal, spawnError: result.spawnError, ...inspected, stdout: result.stdout, stderr: result.stderr };
      summary.acp.cases.push(record);
      appendJsonLine(jsonl, { event: 'case', ...record });
      if (result.code === 0 && !inspected.coverageError && inspected.rootSkips === 0) summary.acp.passed++;
      else if (result.code === 0 && !inspected.coverageError && inspected.rootSkips === 1) summary.acp.skipped++;
      else summary.acp.failed++;
      summary.acp.nestedSkipped += inspected.nestedSkipped;
      summary.acp.nestedRun += inspected.nestedRun;
    };
    const pool = allocations.map(async allocation => {
      for (const name of allocation.names) {
        if (interrupted) break;
        await runCase(name, allocation.id, 'pooled');
      }
    });
    await Promise.all(pool);
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    for (const name of serial) await runCase(name, 'serial', 'serial');
    if (summary.acp.cases.length !== names.length) throw new Error(`ACP coverage mismatch: discovered ${names.length}, ran ${summary.acp.cases.length}`);
    const packages = (await run(go, [...goFlags, 'list', './...'], 'list-packages')).stdout.trim().split(/\r?\n/).filter(pkg => pkg && pkg !== 'workass/internal/acp');
    summary.otherPackages = packages;
    if (packages.length) {
      const tempDir = path.join(root, 'other-packages-tmp');
      await mkdir(tempDir, { recursive: true });
      if (interrupted) throw new Error(`interrupted by ${interrupted}`);
      const result = await spawnLogged(go, [...goFlags, 'test', '-json', '-count=1', '-p=2', ...packages], { cwd, spawn, env: { ...process.env, TMPDIR: tempDir, TMP: tempDir, TEMP: tempDir } }, active);
      appendJsonLine(jsonl, { event: 'command-end', label: 'other-go-packages', ...result });
      if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label: 'other-go-packages', text: result.stdout });
      if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label: 'other-go-packages', text: result.stderr });
      for (const line of result.stdout.split(/\r?\n/)) {
        try { const item = JSON.parse(line); if (item.Test && ['pass', 'fail', 'skip'].includes(item.Action)) { summary.otherGo.tests++; summary.otherGo[item.Action === 'pass' ? 'passed' : item.Action === 'fail' ? 'failed' : 'skipped']++; } } catch {}
      }
      if (result.code !== 0) throw Object.assign(new Error(`other-go-packages failed with exit ${result.code}`), { result, label: 'other-go-packages' });
    }
    summary.ok = summary.acp.failed === 0 && summary.otherGo.failed === 0;
    if (!summary.ok) throw new Error(`${summary.acp.failed} ACP case process(es) failed`);
  } catch (error) {
    summary.error = error.message;
    if (error.result) summary.commandFailure = { label: error.label, code: error.result.code, signal: error.result.signal, spawnError: error.result.spawnError, stdout: error.result.stdout, stderr: error.result.stderr };
    if (interrupted) summary.interrupted = interrupted;
    await stopChildren(active);
  } finally {
    if (signalHandlers) { process.off('SIGINT', onInterrupt); process.off('SIGTERM', onInterrupt); }
    abortSignal?.removeEventListener('abort', onAbort);
    await rm(root, { recursive: true, force: true });
    summary.elapsedMs = performance.now() - wallStart;
    appendJsonLine(jsonl, { event: 'summary', ...summary });
    await new Promise(resolve => jsonl.end(resolve));
    await writeFile(summaryPath, `${JSON.stringify(summary, null, 2)}\n`);
  }
  return summary;
}

async function main() {
  const args = process.argv.slice(2);
  const options = { workers: 6, cwd: process.cwd() };
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--workers') options.workers = Number(args[++i]);
    else if (args[i] === '--cwd') options.cwd = path.resolve(args[++i]);
    else if (args[i] === '--log-dir') options.logDir = path.resolve(args[++i]);
    else if (args[i] === '--race') options.race = true;
    else if (args[i] === '--help') { process.stdout.write('Usage: node scripts/test-go-suite.mjs [--workers N] [--race] [--cwd DIR] [--log-dir DIR]\n'); return 0; }
    else throw new Error(`unknown argument: ${args[i]}`);
  }
  const summary = await runGoSuite(options);
  process.stdout.write(`${JSON.stringify({ ok: summary.ok, elapsedMs: Number(summary.elapsedMs.toFixed(1)), discovered: summary.acp.discovered, passed: summary.acp.passed, failed: summary.acp.failed, skipped: summary.acp.skipped, otherGo: summary.otherGo, otherPackages: summary.otherPackages.length, jsonlPath: summary.jsonlPath, summaryPath: summary.summaryPath, error: summary.error }, null, 2)}\n`);
  return summary.ok ? 0 : 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().then(code => { process.exitCode = code; }).catch(error => { process.stderr.write(`${error.stack ?? error}\n`); process.exitCode = 1; });
}
