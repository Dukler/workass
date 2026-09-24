#!/usr/bin/env node
import { spawn as nodeSpawn } from 'node:child_process';
import { createWriteStream } from 'node:fs';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { pathToFileURL } from 'node:url';
import { randomUUID } from 'node:crypto';

const HINTS = new Map([
  ['TestProviderDetectionAllowsFullInitializeAndSessionBudgets', 5.62],
  ['TestWorkassQuestionActorWireAnswerUnicodeIsolationAndReplay', 8.31],
  ['TestGlobalSessionStoreNoopReceiptIsBoundedAndUnchanged', 4.6],
  ['TestWireProvidersDetectInvokeEmitsAndEnablesStubs', 2.13],
  ['TestWireTraceHibernatedCheckpointKeepsTurnBaseline', 1.8],
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
const MAX_BATCH_SECONDS = 4;
const MAX_BATCH_CASES = 12;
const HEAVY_PACKAGES = ['./internal/acp', './cmd/workass'];
// Startup probes retain their real readiness deadlines and run in one explicit
// serial batch until their fixtures can be isolated.
const SERIAL_TESTS = new Set([
  'TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession',
  'TestStartupDetectionDoesNotRetryDevinNeedsLogin',
]);

export function parseTestList(output) {
  const names = [];
  for (const line of output.split(/\r?\n/)) {
    const name = line.trim();
    if (/^(?:Test|Example|Fuzz)\S*$/.test(name)) names.push(name);
  }
  return names;
}

export function anchoredTestPattern(name) {
  return `^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`;
}

export function inspectCaseOutput(name, code, output) {
  const lines = output.split(/\r?\n/);
  const runs = [];
  const terminals = [];
  for (const line of lines) {
    const run = line.trim().match(/^=== RUN\s+(.+)$/);
    if (run) runs.push(run[1]);
    const terminal = line.match(/^\s*--- (PASS|SKIP|FAIL): (.+?)(?: \(.*\))?$/);
    if (terminal) terminals.push({ action: terminal[1].toLowerCase(), name: terminal[2] });
  }
  const rootPasses = terminals.filter(item => item.name === name && item.action === 'pass').length;
  const rootSkips = terminals.filter(item => item.name === name && item.action === 'skip').length;
  const rootFailures = terminals.filter(item => item.name === name && item.action === 'fail').length;
  const rootRuns = runs.filter(item => item === name).length;
  const nestedNames = runs.filter(item => item.startsWith(`${name}/`));
  const nestedTerminals = terminals.filter(item => item.name.startsWith(`${name}/`));
  const nestedTerminalCounts = new Map();
  for (const terminal of nestedTerminals) nestedTerminalCounts.set(terminal.name, (nestedTerminalCounts.get(terminal.name) ?? 0) + 1);
  const nestedOutcomes = {
    pass: nestedTerminals.filter(item => item.action === 'pass').length,
    skip: nestedTerminals.filter(item => item.action === 'skip').length,
    fail: nestedTerminals.filter(item => item.action === 'fail').length,
  };
  return {
    rootRuns, rootPasses, rootSkips, rootFailures,
    nestedRun: nestedNames.length, nestedPassed: nestedOutcomes.pass, nestedSkipped: nestedOutcomes.skip, nestedFailed: nestedOutcomes.fail,
    coverageError: rootRuns !== 1 || rootPasses + rootSkips + rootFailures !== 1 || (rootFailures === 0) !== (rootPasses + rootSkips === 1) || nestedNames.some(item => nestedTerminalCounts.get(item) !== 1) || nestedTerminals.some(item => !nestedNames.includes(item.name)),
    outcome: rootPasses ? 'pass' : rootSkips ? 'skip' : 'fail',
  };
}

export function partitionSerialCases(names, serialNames = SERIAL_TESTS) {
  return { serial: names.filter(name => serialNames.has(name)), parallel: names.filter(name => !serialNames.has(name)) };
}

export function partitionBatches(names, hints = HINTS, { maxWeight = MAX_BATCH_SECONDS, maxCases = MAX_BATCH_CASES } = {}) {
  const sorted = [...names].sort((a, b) => (hints.get(b) ?? DEFAULT_SECONDS) - (hints.get(a) ?? DEFAULT_SECONDS) || a.localeCompare(b));
  const batches = [];
  for (const name of sorted) {
    const weight = hints.get(name) ?? DEFAULT_SECONDS;
    let batch = batches.find(item => item.names.length < maxCases && item.weight + weight <= maxWeight);
    if (!batch) { batch = { names: [], weight: 0 }; batches.push(batch); }
    batch.names.push(name);
    batch.weight += weight;
  }
  return batches.map((batch, id) => ({ id, names: batch.names, weight: batch.weight }));
}

function appendJsonLine(stream, value) { stream.write(`${JSON.stringify(value)}\n`); }

function spawnLogged(command, args, options, active) {
  return new Promise(resolve => {
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

function signalGroup(child, signal) {
  try { if (process.platform === 'win32') child.kill(signal); else process.kill(-child.pid, signal); }
  catch { try { child.kill(signal); } catch {} }
}

async function stopChildren(active) {
  const children = [...active];
  for (const child of children) signalGroup(child, 'SIGTERM');
  let escalationTimer;
  await Promise.race([
    Promise.all(children.map(child => new Promise(resolve => child.once('close', resolve)))),
    new Promise(resolve => { escalationTimer = setTimeout(resolve, 1000); }),
  ]);
  clearTimeout(escalationTimer);
  for (const child of [...active]) signalGroup(child, 'SIGKILL');
  await Promise.all([...active].map(child => new Promise(resolve => child.once('close', resolve))));
}

function parseGoJson(output) {
  const events = [];
  for (const line of output.split(/\r?\n/)) { try { events.push(JSON.parse(line)); } catch {} }
  return events;
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
  let jsonlError = null;
  const onInterrupt = signal => { interrupted ??= signal; void stopChildren(active); };
  jsonl.on('error', error => { jsonlError ??= error; onInterrupt('LOG_ERROR'); });
  if (signalHandlers) { process.on('SIGINT', onInterrupt); process.on('SIGTERM', onInterrupt); }
  const onAbort = () => onInterrupt('ABORT');
  abortSignal?.addEventListener('abort', onAbort, { once: true });
  const commandTemp = async label => {
    const dir = path.join(root, `command-${label}`);
    await mkdir(dir, { recursive: true });
    return dir;
  };
  const envFor = dir => ({ ...process.env, TMPDIR: dir, TMP: dir, TEMP: dir });
  const run = async (command, args, label, commandCwd = cwd) => {
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    appendJsonLine(jsonl, { event: 'command-start', label, command, args, cwd: commandCwd, at: new Date().toISOString() });
    const tempDir = await commandTemp(label);
    const result = await spawnLogged(command, args, { cwd: commandCwd, spawn, env: envFor(tempDir) }, active);
    appendJsonLine(jsonl, { event: 'command-end', label, ...result });
    if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label, text: result.stdout });
    if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label, text: result.stderr });
    if (result.code !== 0) throw Object.assign(new Error(`${label} failed with exit ${result.code}${result.signal ? ` (${result.signal})` : ''}${result.spawnError ? `: ${result.spawnError}` : ''}`), { result, label });
    return result;
  };
  const summary = {
    ok: false, interrupted: null, cwd, workers, race,
    heavyPackages: {}, otherPackages: [], packageOutcomes: [],
    otherGo: { tests: 0, topLevelTests: 0, nestedTests: 0, passed: 0, failed: 0, skipped: 0 },
    elapsedMs: 0, jsonlPath, summaryPath,
  };
  try {
    const raceFlag = race ? ['-race'] : [];
    const buildAndList = await Promise.all(HEAVY_PACKAGES.map(async (pkg, index) => {
      const label = index === 0 ? 'acp' : 'workass';
      const binary = path.join(root, `${label}.test`);
      const packageCwd = path.join(cwd, pkg.replace(/^\.\//, ''));
      await run(go, ['test', ...raceFlag, '-c', '-o', binary, pkg], `compile-${label}`);
      const listing = await run(binary, ['-test.list', '.'], `list-${label}`, packageCwd);
      const names = parseTestList(listing.stdout);
      if (!names.length) throw new Error(`${pkg} test binary discovered no Test, Example, or Fuzz cases`);
      if (new Set(names).size !== names.length) throw new Error(`${pkg} test listing contains duplicate case names`);
      return { pkg, label, binary, packageCwd, names };
    }));
    const packages = (await run(go, ['list', './...'], 'list-packages')).stdout.trim().split(/\r?\n/).filter(Boolean);
    const heavyImportPaths = new Set(buildAndList.map(item => packages.find(pkg => pkg.endsWith(item.pkg.slice(1))) ?? `workass/${item.pkg.slice(2)}`));
    const otherPackages = packages.filter(pkg => !heavyImportPaths.has(pkg));
    summary.otherPackages = otherPackages;
    const packageWorkers = otherPackages.length ? Math.min(2, Math.max(0, workers - 1)) : 0;
    const heavyWorkers = Math.max(1, workers - packageWorkers);
    const batches = [];
    for (const item of buildAndList) {
      const { serial, parallel } = partitionSerialCases(item.names);
      item.serialCases = serial;
      item.batches = partitionBatches(parallel);
      summary.heavyPackages[item.pkg] = {
        discovered: item.names.length, serialCases: serial,
        batches: item.batches.map(batch => ({ id: batch.id, weight: Number(batch.weight.toFixed(2)), cases: batch.names.length })),
        cases: [], passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedPassed: 0, nestedFailed: 0, nestedSkipped: 0, serialElapsedMs: 0,
      };
      for (const batch of item.batches) batches.push({ ...batch, item, boundary: 'weighted' });
      if (serial.length) batches.push({ id: `serial-${item.label}`, item, boundary: 'serial-lane', serialNames: serial });
    }
    const batchQueue = [...batches];
    let nextBatch = 0;
    const executeBatch = async (batch, worker) => {
      if (interrupted) return;
      const { item } = batch;
      const tempDir = path.join(root, `worker-${worker}-${item.label}-batch-${batch.id}`);
      await mkdir(tempDir, { recursive: true });
      if (interrupted) return;
      const pattern = `^(?:${batch.names.map(name => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})$`;
      const result = await spawnLogged(item.binary, ['-test.v', `-test.run=${pattern}`, '-test.count=1', '-test.parallel=1'], { cwd: item.packageCwd, spawn, env: envFor(tempDir) }, active);
      const target = summary.heavyPackages[item.pkg];
      if (batch.boundary === 'serial') target.serialElapsedMs += result.elapsedMs;
      appendJsonLine(jsonl, { event: 'batch-end', package: item.pkg, batch: batch.id, worker, names: batch.names, elapsedMs: result.elapsedMs, code: result.code, signal: result.signal, spawnError: result.spawnError, stdout: result.stdout, stderr: result.stderr });
      for (const name of batch.names) {
        const inspected = inspectCaseOutput(name, result.code, result.stdout);
        const record = { package: item.pkg, name, worker, batch: batch.id, boundary: batch.boundary, cwd: item.packageCwd, elapsedMs: result.elapsedMs, code: result.code, signal: result.signal, spawnError: result.spawnError, ...inspected };
        target.cases.push(record);
        appendJsonLine(jsonl, { event: 'case', ...record });
        if (!inspected.coverageError && inspected.rootPasses === 1) target.passed++;
        else if (!inspected.coverageError && inspected.rootSkips === 1) target.skipped++;
        else target.failed++;
        target.nestedSkipped += inspected.nestedSkipped;
        target.nestedRun += inspected.nestedRun;
        target.nestedPassed += inspected.nestedPassed;
        target.nestedFailed += inspected.nestedFailed;
      }
      if (result.code !== 0) throw Object.assign(new Error(`${item.pkg} batch ${batch.id} failed with exit ${result.code}`), { result, label: `batch-${item.label}-${batch.id}` });
    };
    const pool = Array.from({ length: Math.min(heavyWorkers, batchQueue.length) }, async (_, worker) => {
      while (!interrupted) {
        const index = nextBatch++;
        if (index >= batchQueue.length) return;
        const batch = batchQueue[index];
        if (batch.serialNames) {
          for (const [serialIndex, name] of batch.serialNames.entries()) {
            if (interrupted) return;
            await executeBatch({ id: `serial-${serialIndex}`, names: [name], item: batch.item, boundary: 'serial' }, worker);
          }
        } else await executeBatch(batch, worker);
      }
    });
    const runOtherPackages = async () => {
      if (!otherPackages.length) return;
      const tempDir = await commandTemp('other-packages');
      const flags = ['test', ...raceFlag, '-json', '-count=1', `-p=${Math.max(1, packageWorkers)}`, '-parallel=1', ...otherPackages];
      appendJsonLine(jsonl, { event: 'command-start', label: 'other-go-packages', command: go, args: flags, cwd, at: new Date().toISOString() });
      const result = await spawnLogged(go, flags, { cwd, spawn, env: envFor(tempDir) }, active);
      appendJsonLine(jsonl, { event: 'command-end', label: 'other-go-packages', ...result });
      if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label: 'other-go-packages', text: result.stdout });
      if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label: 'other-go-packages', text: result.stderr });
      const events = parseGoJson(result.stdout);
      const outcomes = events.filter(event => ['pass', 'fail', 'skip'].includes(event.Action));
      for (const event of outcomes) {
        if (event.Test) {
          summary.otherGo.tests++;
          if (event.Test.includes('/')) summary.otherGo.nestedTests++;
          else summary.otherGo.topLevelTests++;
          summary.otherGo[event.Action === 'pass' ? 'passed' : event.Action === 'fail' ? 'failed' : 'skipped']++;
        } else if (event.Package) summary.packageOutcomes.push({ package: event.Package, action: event.Action });
      }
      const packageOutcomes = new Set(summary.packageOutcomes.map(item => item.package));
      const missingPackages = otherPackages.filter(pkg => !packageOutcomes.has(pkg));
      if (missingPackages.length) throw new Error(`other Go package results missing: ${missingPackages.join(', ')}`);
      if (result.code !== 0) throw Object.assign(new Error(`other-go-packages failed with exit ${result.code}`), { result, label: 'other-go-packages' });
    };
    const otherPromise = packageWorkers ? runOtherPackages() : Promise.resolve();
    const settled = await Promise.allSettled([...pool, otherPromise]);
    const rejection = settled.find(item => item.status === 'rejected');
    if (rejection) throw rejection.reason;
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    if (!packageWorkers) await runOtherPackages();
    for (const item of buildAndList) {
      const result = summary.heavyPackages[item.pkg];
      result.cases.sort((a, b) => a.name.localeCompare(b.name));
      if (result.cases.length !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: discovered ${result.discovered}, ran ${result.cases.length}`);
      if (new Set(result.cases.map(test => test.name)).size !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: duplicate execution`);
      if (result.cases.some(test => test.coverageError)) throw new Error(`${item.pkg} test output did not contain exactly one terminal outcome per discovered case`);
    }
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    if (summary.packageOutcomes.some(item => item.action === 'fail')) throw new Error('one or more remaining Go packages failed');
    summary.ok = Object.values(summary.heavyPackages).every(item => item.failed === 0 && item.nestedFailed === 0) && summary.otherGo.failed === 0;
    if (!summary.ok) throw new Error('one or more Go cases failed or were missing');
  } catch (error) {
    summary.error = error.message;
    if (error.result) summary.commandFailure = { label: error.label, code: error.result.code, signal: error.result.signal, spawnError: error.result.spawnError, stdout: error.result.stdout, stderr: error.result.stderr };
    if (interrupted) summary.interrupted = interrupted;
    if (jsonlError) summary.logError = jsonlError.message;
    await stopChildren(active);
  } finally {
    if (signalHandlers) { process.off('SIGINT', onInterrupt); process.off('SIGTERM', onInterrupt); }
    abortSignal?.removeEventListener('abort', onAbort);
    await rm(root, { recursive: true, force: true });
    summary.elapsedMs = performance.now() - wallStart;
    if (!jsonlError) appendJsonLine(jsonl, { event: 'summary', ...summary });
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
  const packages = Object.values(summary.heavyPackages);
  const counts = packages.reduce((total, item) => ({ discovered: total.discovered + item.discovered, passed: total.passed + item.passed + item.nestedPassed, failed: total.failed + item.failed + item.nestedFailed, skipped: total.skipped + item.skipped + item.nestedSkipped, nestedRun: total.nestedRun + item.nestedRun, nestedSkipped: total.nestedSkipped + item.nestedSkipped }), { discovered: 0, passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedSkipped: 0 });
  counts.topLevelTests = counts.discovered + summary.otherGo.topLevelTests;
  counts.tests = counts.topLevelTests + counts.nestedRun + summary.otherGo.nestedTests;
  process.stdout.write(`${JSON.stringify({ ok: summary.ok, elapsedMs: Number(summary.elapsedMs.toFixed(1)), ...counts, otherGo: summary.otherGo, otherPackages: summary.otherPackages.length, jsonlPath: summary.jsonlPath, summaryPath: summary.summaryPath, error: summary.error }, null, 2)}\n`);
  return summary.ok ? 0 : 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().then(code => { process.exitCode = code; }).catch(error => { process.stderr.write(`${error.stack ?? error}\n`); process.exitCode = 1; });
}
