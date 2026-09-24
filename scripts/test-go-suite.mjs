#!/usr/bin/env node
import { spawn as nodeSpawn } from 'node:child_process';
import { createWriteStream, existsSync, realpathSync } from 'node:fs';
import { mkdtemp, mkdir, readFile, rm, unlink, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { fileURLToPath } from 'node:url';
import { createHash, randomUUID } from 'node:crypto';
import { link } from 'node:fs/promises';

const HINTS = new Map([
  ["TestACPEffortSelectionBeforeAxisDiscovery", 1.3],
  ["TestBrowserStatelessMCPMutationJournalReadbackConflictAndActorFence", 0.6],
  ["TestCancelInPromptPreparationGap", 1.35],
  ["TestChatCheckpointsDiffRewindAndOutsideGuard", 0.63],
  ["TestChatEnvTruncationFlags", 0.95],
  ["TestChatLifecycleDoesNotRunAutomaticGit", 1.54],
  ["TestClaudeUpdateReresolvesTransientShimAndAtomicInstallSwap", 2.64],
  ["TestCodexNativeGoalThroughActorAndExactResume", 1.29],
  ["TestCodexServiceTierSurvivesExactResumeAndClearsExplicitly", 1.07],
  ["TestDeclaredWorkIsNeverReclassifiedByInference", 0.65],
  ["TestDeferredCodexCreatesAgainOnlyForAProvablyEmptyLane", 0.56],
  ["TestDetectFrontierProvidersNeedsLogin", 1.25],
  ["TestDetectFrontierProvidersReadyWithNativeProtocolFixtures", 1.26],
  ["TestDetectProvidersExplicitDisableSurvivesRedetection", 0.9],
  ["TestDetectProvidersLocalServerRegistersNativeProviderAndStreamsThroughAgent", 1.93],
  ["TestDetectProvidersOMLXAuthenticatesQwenAndNativeProviderWithoutPersistingKey", 1.49],
  ["TestDetectProviderStoresRedactedCLIVersionRaw", 1.26],
  ["TestDevinAuthenticationFailureBecomesNeedsLoginWithoutRetryLoop", 1.29],
  ["TestDevinStopAndSendUsesDurableQueueAndExactCancellation", 0.57],
  ["TestFailedDetectionDisablesPreviouslyReadyProviderWithoutUserDisable", 0.86],
  ["TestImmediateStopPublishesCommittedTerminalBeforeReply", 0.6],
  ["TestLegacyDevinNeedsLoginRecoveryFailureDoesNotLoopAcrossRestart", 1.07],
  ["TestMockBurstStreamsAtDisplayCadenceWithoutDroppingText", 2.35],
  ["TestMockClaudeProviderKeepsUnnotifiedBackgroundWorkRunningViaOutputOwner", 0.59],
  ["TestMockInitializeSessionPromptCancelErrorAndReuse", 1.29],
  ["TestMockSteerMidSlowTurnReflectedInOutput", 1.3],
  ["TestNextTurnListsAndWaitsOnAdoptedSubagent", 3.19],
  ["TestOMPInstalledHostContract", 1.07],
  ["TestPiNativeSDKProviderContext", 1.72],
  ["TestProviderChatRuntimeResumesExactLaneAcrossActorAndTabRestart", 0.59],
  ["TestProviderChatRuntimeSwitchesAndReturnsThroughVerifiedContextImport", 0.8],
  ["TestProviderCLIExecutableRefreshesValidCacheFromPATH", 1.08],
  ["TestProviderDetectionAllowsFullInitializeAndSessionBudgets", 6.14],
  ["TestProviderRegistryCatalogToggleFailureAndConcurrentIsolation", 1.37],
  ["TestProviderUpdateCheckFakeRegistry", 1.82],
  ["TestProviderUpdateInvokeFailureKeepsCardWithRedactedTail", 2.26],
  ["TestProviderUpdateInvokeProgressNoProcRegistryAndReplay", 2.81],
  ["TestProviderUpdateInvokeRejectsDoubleUnknownAndNoPending", 3.81],
  ["TestProviderUpdatePostRecheckAllFailKeepsEntryWithRecheckError", 1.31],
  ["TestProviderUpdatePostRecheckRetriesUntilVersionLands", 1.35],
  ["TestProviderUpdateRunsResolvedProviderExecutable", 2.15],
  ["TestProviderUpdateTerminalReceiptDoesNotWaitForRegistryRefresh", 1.18],
  ["TestProviderUpdateZeroExitWithoutVersionAdvanceFailsVerification", 2.11],
  ["TestQuestionWaitsForTheUserWhileAPermissionStillExpires", 0.62],
  ["TestQwenStandaloneUpdateUsesBundledUpdaterAtCompatibleRelease", 2.79],
  ["TestRuntimeDiagnosticsCoalescedFailureGetsTrailingCheckpoint", 1.01],
  ["TestStartupDetectionRecoversLegacyDevinNeedsLoginOnceUnderSanitizedLaunch", 0.67],
  ["TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession", 1.08],
  ["TestStartupDetectProvidersRetriesOnlyStatusErrors", 0.88],
  ["TestStatelessMCPSpawnsAndWaitsForTrackedSubagent", 0.89],
  ["TestStopSpawnedWorkKillsARealProcessThatIgnoresSIGTERM", 2.02],
  ["TestSubagentLatchedPermissionAttentionSurfacesToAdoptingTurn", 1.84],
  ["TestSubagentSurvivesCancelledParentSettlesAndWritesReceipt", 2.68],
  ["TestT7AgentControlExternalSettleIsIdempotentAndOwnerValidated", 0.66],
  ["TestToolsCLICrossProviderDelegationAndCrossChatMessaging", 1.22],
  ["TestTurnDiagnosticsMockCancellationRecordsWireBoundaries", 1.05],
  ["TestWireBusyStartQueuesCapabilityAwareFollowUpWithoutFailedTranscript", 0.54],
  ["TestWireDaemonQueueDrainsWithoutControllerAndReplaysPermissionOnAttach", 0.52],
  ["TestWireE2EAppChatAssertsJobEventChannel", 1.37],
  ["TestWireFreshProviderGetsHistorySeedAndEstablishedLaneUsesSafeImport", 0.52],
  ["TestWireMockBurstReachesClientAtDisplayCadence", 1],
  ["TestWireProviderCatalogConnectBeforeDetectionGetsSingleBroadcast", 0.62],
  ["TestWireProvidersDetectInvokeEmitsAndEnablesStubs", 1.88],
  ["TestWireReconnectRestoresLiveSessionControlsAndPendingPermission", 1.72],
  ["TestWireSessionRecoversTurnCompletedWithoutRenderer", 1.49],
  ["TestWireTraceAppChatSteer", 1.49],
  ["TestWireTraceForkChatSeedsPrefixAndDiverges", 0.82],
  ["TestWireTraceGroupedCatalogAndInterleavedProviders", 1.64],
  ["TestWireTraceHibernatedCheckpointKeepsTurnBaseline", 1.81],
  ["TestWireTraceMockCrashNeverReplaysAndNextDistinctPromptRuns", 0.83],
  ["TestWireTraceMockEngineCrashTerminalizesThenNextPromptResumesExactThread", 0.7],
  ["TestWireTraceNativeLocalProviderColdStartAndTurn", 0.78],
  ["TestWireTraceNotifyControllerOnlyRedactionAndNoTurnEndBacklog", 0.7],
  ["TestWireWorkspaceMoveCommitsBeforeInvalidationAndStaleReconnectUsesTargetCWD", 0.63],
  ["TestWorkspaceReturnCreatesCurrentRevisionLaneAndAcceptsNextTurn", 1.01],
]);
const DEFAULT_SECONDS = 0.1;
const DEFAULT_WORKERS = os.availableParallelism();
const MAX_BATCH_SECONDS = 2;
const MAX_BATCH_CASES = 24;
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

export function orderWorkByWeight(work, { slowPackage = 'workass/internal/machinebook' } = {}) {
  const rank = item => item.package === slowPackage ? 0 : item.kind === 'build' ? 1 : item.kind === 'heavy' ? 2 : 3;
  return [...work].sort((a, b) => rank(a) - rank(b) ||
    (b.weight ?? DEFAULT_SECONDS) - (a.weight ?? DEFAULT_SECONDS) ||
    String(a.package ?? '').localeCompare(String(b.package ?? '')) || String(a.id ?? '').localeCompare(String(b.id ?? '')));
}

function cacheLabel(pkg) { return `pkg-${createHash('sha256').update(pkg).digest('hex').slice(0, 24)}.test`; }

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

export async function runGoSuite({ cwd = process.cwd(), logDir, cacheDir, workers = DEFAULT_WORKERS, race = false, go = 'go', spawn = nodeSpawn, signalHandlers = true, abortSignal } = {}) {
  if (!Number.isInteger(workers) || workers < 1) throw new Error('workers must be a positive integer');
  const wallStart = performance.now();
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-matrix-'));
  const repoKey = createHash('sha256').update(path.resolve(cwd)).digest('hex').slice(0, 24);
  const binaryCache = cacheDir ?? (spawn === nodeSpawn
    ? path.join(os.tmpdir(), 'workass-go-test-binaries', repoKey, race ? 'race' : 'normal')
    : path.join(root, 'test-binaries'));
  await mkdir(binaryCache, { recursive: true });
  const cacheMarker = path.join(binaryCache, '.workass-go-test-cache');
  const markerValue = `workass-go-test-cache-v1\n${repoKey}\n${race ? 'race' : 'normal'}\n`;
  if (existsSync(cacheMarker)) {
    if (await readFile(cacheMarker, 'utf8') !== markerValue) throw new Error(`Go test binary cache ownership mismatch: ${binaryCache}`);
  } else {
    for (const label of ['acp', 'workass']) {
      if (existsSync(path.join(binaryCache, `${label}.test`))) throw new Error(`Refusing to overwrite an unowned Go test binary: ${path.join(binaryCache, `${label}.test`)}`);
    }
    const markerTemp = path.join(binaryCache, `.workass-go-test-cache-${randomUUID()}`);
    await writeFile(markerTemp, markerValue, { flag: 'wx' });
    try { await link(markerTemp, cacheMarker); }
    catch (error) {
      if (error.code !== 'EEXIST' || await readFile(cacheMarker, 'utf8') !== markerValue) throw error;
    } finally { await unlink(markerTemp).catch(() => {}); }
  }
  const logs = logDir ?? path.join(os.tmpdir(), `workass-go-matrix-logs-${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}`);
  await mkdir(logs, { recursive: true });
  const runId = `${new Date().toISOString().replaceAll(':', '').replaceAll('.', '')}-${process.pid}-${randomUUID()}`;
  const jsonlPath = path.join(logs, `go-matrix-${runId}.jsonl`);
  const summaryPath = path.join(logs, `go-matrix-${runId}.json`);
  const jsonl = createWriteStream(jsonlPath, { flags: 'wx' });
  const active = new Set();
  let interrupted = null;
  let jsonlError = null;
  let wakeWorkers = () => {};
  const onInterrupt = signal => { interrupted ??= signal; wakeWorkers(); void stopChildren(active); };
  jsonl.on('error', error => { jsonlError ??= error; onInterrupt('LOG_ERROR'); });
  if (signalHandlers) { process.on('SIGINT', onInterrupt); process.on('SIGTERM', onInterrupt); }
  const onAbort = () => onInterrupt('ABORT');
  abortSignal?.addEventListener('abort', onAbort, { once: true });
  const commandTemp = async label => {
    const dir = path.join(root, `command-${label}`);
    await mkdir(dir, { recursive: true });
    return dir;
  };
  const envFor = (dir, { testBinary = false } = {}) => ({
    ...process.env,
    TMPDIR: dir, TMP: dir, TEMP: dir,
    ...(testBinary ? { GOMAXPROCS: '1' } : {}),
  });
  const run = async (command, args, label, commandCwd = cwd, { testBinary = false } = {}) => {
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    appendJsonLine(jsonl, { event: 'command-start', label, command, args, cwd: commandCwd, at: new Date().toISOString() });
    const tempDir = await commandTemp(label);
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    const result = await spawnLogged(command, args, { cwd: commandCwd, spawn, env: envFor(tempDir, { testBinary }) }, active);
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
    const packageMetadata = (await run(go, ['list', '-f', '{{.ImportPath}}\t{{.Dir}}\t{{if or .TestGoFiles .XTestGoFiles}}true{{else}}false{{end}}', './...'], 'list-packages')).stdout.trim().split(/\r?\n/).filter(Boolean).map(line => {
      const [importPath, dir, hasTests] = line.split('\t');
      if (!importPath || !dir || (hasTests !== 'true' && hasTests !== 'false')) throw new Error(`invalid Go package metadata: ${line}`);
      return { importPath, dir, hasTests: hasTests === 'true' };
    });
    const packages = packageMetadata.map(pkg => pkg.importPath);
    const heavyImportPaths = new Set(HEAVY_PACKAGES.map(pkg => packages.find(name => name.endsWith(pkg.slice(1))) ?? `workass/${pkg.slice(2)}`));
    const otherPackages = packageMetadata.filter(pkg => !heavyImportPaths.has(pkg.importPath));
    summary.otherPackages = otherPackages.map(pkg => pkg.importPath);
    const buildAndList = [];
    const failures = [];
    const workQueue = orderWorkByWeight([
      ...otherPackages.map(pkg => ({ kind: 'package', package: pkg.importPath, dir: pkg.dir, hasTests: pkg.hasTests, id: pkg.importPath, weight: pkg.importPath.endsWith('/machinebook') ? 10 : DEFAULT_SECONDS })),
      ...HEAVY_PACKAGES.map((pkg, index) => ({ kind: 'build', pkg, index, package: packages.find(name => name.endsWith(pkg.slice(1))) ?? `workass/${pkg.slice(2)}`, weight: DEFAULT_SECONDS })),
    ]);
    let pendingTasks = workQueue.length;
    const wakeups = new Set();
    const wakeAll = () => { for (const wake of wakeups) wake(); wakeups.clear(); };
    wakeWorkers = wakeAll;
    const enqueue = work => { pendingTasks++; workQueue.push(work); wakeAll(); };
    const takeWork = async () => {
      while (true) {
        if (interrupted) return null;
        if (workQueue.length) {
          const selected = orderWorkByWeight(workQueue)[0];
          return workQueue.splice(workQueue.indexOf(selected), 1)[0];
        }
        if (pendingTasks === 0) return null;
        await new Promise(resolve => wakeups.add(resolve));
      }
    };
    const executeBuild = async work => {
      const { pkg, index } = work;
      const label = index === 0 ? 'acp' : 'workass';
      const cachedBinary = path.join(binaryCache, `${label}.test`);
      const binary = path.join(root, `${label}.test`);
      const packageCwd = path.join(cwd, pkg.replace(/^\.\//, ''));
      await run(go, ['test', ...raceFlag, '-c', '-o', cachedBinary, pkg], `compile-${label}`);
      // Pin this invocation to the compiled inode. Go replaces changed outputs
      // atomically; the hard link keeps an older binary runnable during another
      // suite's compiler pass without copying its startup cost onto each batch.
      await link(cachedBinary, binary);
      const listing = await run(binary, ['-test.list', '.'], `list-${label}`, packageCwd, { testBinary: true });
      const names = parseTestList(listing.stdout);
      if (!names.length) throw new Error(`${pkg} test binary discovered no Test, Example, or Fuzz cases`);
      if (new Set(names).size !== names.length) throw new Error(`${pkg} test listing contains duplicate case names`);
      const item = { pkg, label, binary, packageCwd, names };
      buildAndList.push(item);
      const { serial, parallel } = partitionSerialCases(names);
      item.serialCases = serial;
      item.batches = partitionBatches(parallel);
      summary.heavyPackages[pkg] = {
        discovered: names.length, serialCases: serial,
        batches: item.batches.map(batch => ({ id: batch.id, weight: Number(batch.weight.toFixed(2)), cases: batch.names.length })),
        cases: [], passed: 0, failed: 0, skipped: 0, nestedRun: 0, nestedPassed: 0, nestedFailed: 0, nestedSkipped: 0, serialElapsedMs: 0,
      };
      for (const batch of item.batches) enqueue({ kind: 'heavy', ...batch, item, boundary: 'weighted', package: work.package });
      for (const [serialIndex, name] of serial.entries()) enqueue({ kind: 'heavy', id: `serial-${serialIndex}`, names: [name], weight: HINTS.get(name) ?? DEFAULT_SECONDS, item, boundary: 'serial', package: work.package });
    };
    const executeBatch = async (batch, worker) => {
      const { item } = batch;
      const tempDir = path.join(root, `worker-${worker}-${item.label}-batch-${batch.id}`);
      await mkdir(tempDir, { recursive: true });
      if (interrupted) return;
      const pattern = `^(?:${batch.names.map(name => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})$`;
      const result = await spawnLogged(item.binary, ['-test.v', `-test.run=${pattern}`, '-test.count=1', '-test.parallel=2'], { cwd: item.packageCwd, spawn, env: envFor(tempDir, { testBinary: true }) }, active);
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
      if (result.code !== 0) failures.push(`${item.pkg} batch ${batch.id} failed with exit ${result.code}`);
    };
    const executePackage = async work => {
      const { hasTests } = work;
      const packageCwd = work.dir;
      const label = `package-${createHash('sha256').update(work.package).digest('hex').slice(0, 16)}`;
      const cachePath = path.join(binaryCache, cacheLabel(work.package));
      await run(go, ['test', ...raceFlag, '-c', '-o', cachePath, work.package], `compile-${label}`);
      if (!hasTests) {
        appendJsonLine(jsonl, { event: 'package-test', package: work.package, cwd: packageCwd, code: 0, noTestFiles: true });
        summary.packageOutcomes.push({ package: work.package, action: 'pass', noTestFiles: true });
        return;
      }
      const tempDir = await commandTemp(`other-${work.package.replace(/[^a-zA-Z0-9_-]/g, '-')}`);
      if (interrupted) return;
      const binary = path.join(root, `${label}-run.test`);
      await link(cachePath, binary);
      const listing = await run(binary, ['-test.list', '.'], `list-${label}`, packageCwd, { testBinary: true });
      const names = parseTestList(listing.stdout);
      if (!names.length || new Set(names).size !== names.length) throw new Error(`${work.package} test binary has an empty or duplicate listing`);
      const result = await spawnLogged(binary, ['-test.v', '-test.count=1', '-test.parallel=2'], { cwd: packageCwd, spawn, env: envFor(tempDir, { testBinary: true }) }, active);
      const inspected = names.map(name => ({ name, ...inspectCaseOutput(name, result.code, result.stdout) }));
      let packageOutcome = result.code === 0 && inspected.every(item => !item.coverageError && !item.rootFailures) ? 'pass' : 'fail';
      for (const item of inspected) {
        summary.otherGo.topLevelTests++;
        summary.otherGo.tests++;
        summary.otherGo.nestedTests += item.nestedRun;
        summary.otherGo.tests += item.nestedRun;
        summary.otherGo.passed += item.rootPasses + item.nestedPassed;
        summary.otherGo.failed += item.rootFailures + item.nestedFailed + (item.coverageError ? 1 : 0);
        summary.otherGo.skipped += item.rootSkips + item.nestedSkipped;
      }
      appendJsonLine(jsonl, { event: 'package-test', package: work.package, cwd: packageCwd, elapsedMs: result.elapsedMs, code: result.code, stdout: result.stdout, stderr: result.stderr });
      if (result.stdout) appendJsonLine(jsonl, { event: 'stdout', label: `other-go-${work.package}`, text: result.stdout });
      if (result.stderr) appendJsonLine(jsonl, { event: 'stderr', label: `other-go-${work.package}`, text: result.stderr });
      if (inspected.some(item => item.coverageError || item.rootFailures || item.nestedFailed)) packageOutcome = 'fail';
      summary.packageOutcomes.push({ package: work.package, action: packageOutcome });
      if (result.code !== 0) { summary.commandFailure ??= { label: `other-go-${work.package}`, code: result.code, signal: result.signal, spawnError: result.spawnError, stdout: result.stdout, stderr: result.stderr }; failures.push(`${work.package} failed with exit ${result.code}`); }
      if (packageOutcome === 'fail' && result.code === 0) failures.push(`${work.package} test output was incomplete or failed`);
    };
    const pool = Array.from({ length: Math.min(workers, workQueue.length) }, async (worker) => {
      while (!interrupted) {
        const work = await takeWork();
        if (!work) return;
        try {
          if (work.kind === 'build') await executeBuild(work);
          else if (work.kind === 'heavy') await executeBatch(work, worker);
          else await executePackage(work);
        } catch (error) {
          failures.push(error.message);
          if (error.result && !summary.commandFailure) summary.commandFailure = { label: error.label, code: error.result.code, signal: error.result.signal, spawnError: error.result.spawnError, stdout: error.result.stdout, stderr: error.result.stderr };
        } finally {
          pendingTasks--;
          wakeAll();
        }
      }
    });
    const settled = await Promise.allSettled(pool);
    const rejection = settled.find(item => item.status === 'rejected');
    if (rejection) throw rejection.reason;
    if (interrupted) throw new Error(`interrupted by ${interrupted}`);
    if (failures.length) throw new Error(failures.join('; '));
    for (const item of buildAndList) {
      const result = summary.heavyPackages[item.pkg];
      result.cases.sort((a, b) => a.name.localeCompare(b.name));
      if (result.cases.length !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: discovered ${result.discovered}, ran ${result.cases.length}`);
      if (new Set(result.cases.map(test => test.name)).size !== result.discovered) throw new Error(`${item.pkg} coverage mismatch: duplicate execution`);
      if (result.cases.some(test => test.coverageError)) throw new Error(`${item.pkg} test output did not contain exactly one terminal outcome per discovered case`);
    }
    if (jsonlError) throw new Error(`Go matrix log write failed: ${jsonlError.message}`);
    if (summary.packageOutcomes.some(item => item.action === 'fail')) throw new Error('one or more remaining Go packages failed');
    if (failures.length) throw new Error(failures.join('; '));
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
  const options = { workers: DEFAULT_WORKERS, cwd: process.cwd() };
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

export function isMainModule(argvPath = process.argv[1], modulePath = fileURLToPath(import.meta.url)) {
  if (!argvPath) return false;
  try { return realpathSync(argvPath) === realpathSync(modulePath); }
  catch { return false; }
}

if (isMainModule()) {
  main().then(code => { process.exitCode = code; }).catch(error => { process.stderr.write(`${error.stack ?? error}\n`); process.exitCode = 1; });
}
