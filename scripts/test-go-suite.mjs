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
  ["TestACPCatalogDiscoversEffortBeforeFirstPrompt", 0.92],
  ["TestACPEffortSelectionBeforeAxisDiscovery", 1.66],
  ["TestActorDeleteCrashRecoveryCompletesNativeCleanupFromTombstone", 1.21],
  ["TestActorEnvironmentProjectionSurvivesRuntimeRestartAndRejectsWrongTab", 0.72],
  ["TestActorNativeChatProjectsAfterRestartFromCanonicalStorage", 0.84],
  ["TestActorRendererSessionSnapshotParityAcrossRestart", 4.36],
  ["TestAdapterSessionRefreshCannotOverwriteActorFromStaleAttachment", 1.08],
  ["TestAgentChatSendQueuedOwnershipAndImmutableRetries", 1.20],
  ["TestAgentChatSendSteerDerivedOperationRejectsChangedMessageAndDelivery", 2.15],
  ["TestAgentControlCodexOwnerCanRegisterExternalHandoff", 1.73],
  ["TestAgentControlCreatedChatSurvivesStaleSessionSaveBeforeSend", 0.53],
  ["TestAgentControlHostsArtifactsOnlyFromTheCallingAgentWorkspace", 0.90],
  ["TestAgentControlRejectsDeletedActorBeforeLiveManagerAuthorization", 0.62],
  ["TestAgentControlTurnlessSpawnNeverUsesLegacySessionMirrorAsOwner", 0.55],
  ["TestAgentStatelessMCPFencesDeletedActorBeforeOwnerValidation", 1.56],
  ["TestArtifactValidationFailureIsTerminalAndRetryDoesNotInspectSource", 0.67],
  ["TestBrowserReadReleasesActorLockDuringShellHTTP", 1.61],
  ["TestBrowserStatelessMCPMutationJournalReadbackConflictAndActorFence", 3.22],
  ["TestBrowserStatelessMCPRejectsLiveManagerOwnerAfterActorDeletion", 1.58],
  ["TestBrowserStatelessMCPUnreadyControlDoesNotClaimActorMutation", 1.97],
  ["TestCancelInPromptPreparationGap", 1.37],
  ["TestChatCheckpointRotationAndLargeRepoSkip", 0.75],
  ["TestChatCheckpointsDiffRewindAndOutsideGuard", 1.40],
  ["TestChatControlInvalidOperationCannotCancelOrDeleteRunningTurn", 2.41],
  ["TestChatControlVisibleMutationRefreshesAreImmediate", 0.68],
  ["TestChatDiagnosticsExactActorAndNoMutation", 2.10],
  ["TestChatDiagnosticsToolRemoteRoute", 1.67],
  ["TestChatEnvTracksRepoChangesAfterTurn", 0.63],
  ["TestChatEnvTruncationFlags", 2.46],
  ["TestChatListToolPreservesLocalChatsWithMountedRemote", 1.13],
  ["TestChatLifecycleDoesNotRunAutomaticGit", 3.2],
  ["TestClaudeUpdateReresolvesTransientShimAndAtomicInstallSwap", 4.65],
  ["TestCodexNativeGoalThroughActorAndExactResume", 5.32],
  ["TestCodexRuntimeDiagnosticsThroughActorAndRestart", 1.63],
  ["TestCodexServiceTierSurvivesExactResumeAndClearsExplicitly", 5.11],
  ["TestCompositeModelCreateValidation", 0.56],
  ["TestDeclaredWorkIsNeverReclassifiedByInference", 0.75],
  ["TestDeferredCodexCreatesAgainOnlyForAProvablyEmptyLane", 0.71],
  ["TestDeferredDevinCandidateAbsencePreservesCommittedThreadProtection", 1.03],
  ["TestDeletedActorDoesNotBlockStartupReconciliation", 0.67],
  ["TestDeletedActorRejectsOriginalCreateReplay", 0.53],
  ["TestDetectProvidersLocalServerRegistersNativeProviderAndStreamsThroughAgent", 3.30],
  ["TestDetectProvidersOMLXAuthenticatesQwenAndNativeProviderWithoutPersistingKey", 3.00],
  ["TestDevinStopAndSendUsesDurableQueueAndExactCancellation", 2.82],
  ["TestDreamSubagentCatalogProgressMessageWaitManyAndDurableReceipt", 0.56],
  ["TestExplicitParentStopDropsOnlyItsQueuedSubagentCompletion", 2.50],
  ["TestForkProviderFailureCommitsChildBeforeSelectionAndRetryIsDurable", 0.59],
  ["TestForkRetryAfterChildActorCommitAttachesExactlyOnce", 1.22],
  ["TestForkRetryAfterChildCommitDoesNotReadSourceOrRecreateLane", 1.39],
  ["TestImmediateStopPublishesCommittedTerminalBeforeReply", 2.30],
  ["TestLifecyclePinnedNeverReapedTinyTTL", 0.52],
  ["TestLifecycleRaceReapAbortedByArrivingPrompt", 0.61],
  ["TestLifecycleWithoutExactResumeFailsClosedAfterHibernation", 0.58],
  ["TestMockBurstStreamsAtDisplayCadenceWithoutDroppingText", 2.40],
  ["TestMockClaudeProviderForwardsSpawnedWorkWithoutAgentCooperation", 0.55],
  ["TestMockInitializeSessionPromptCancelErrorAndReuse", 1.39],
  ["TestMockNativeSessionLoadAttachesTheExactThreadWithoutPublishingReplay", 1.09],
  ["TestMockNativeSessionNeverResumesAfterConversationIdentityChanges", 0.71],
  ["TestMockNativeSessionResumeAfterHibernationDoesNotCollide", 0.74],
  ["TestMockNativeSessionResumesExactThreadAcrossManagerRestart", 1.24],
  ["TestMockNativeSessionUnseenWorkassHistoryDoesNotGovernExactResume", 1.40],
  ["TestMockSteerMidSlowTurnReflectedInOutput", 1.31],
  ["TestMoveWorkspaceReceiptReplayFinishesCrashWindowWithoutNewEpoch", 1.36],
  ["TestMoveWorkspaceReceiptRetryDoesNotCloseProviderHostAgain", 1.28],
  ["TestNativeCodexBackgroundChildOutlivesParentAndRetainsReceipt", 0.59],
  ["TestNativeCodexBackgroundLifecycleIsOwnedByChatActor", 2.15],
  ["TestNegotiatedACPSteeringSurvivesProviderLaneSelection", 1.29],
  ["TestNextTurnListsAndWaitsOnAdoptedSubagent", 3.31],
  ["TestOMPInstalledHostContract", 1.09],
  ["TestOMPNativeHostContract", 1.52],
  ["TestPermissionWaitRemainsOwnedByHarness", 0.51],
  ["TestPhaseCManagerPublicationWaitsForDurableActorState", 0.55],
  ["TestPiDiscoveryUsesOfficialSDKHost", 0.52],
  ["TestPiNativeHostContract", 1.33],
  ["TestPiNativeSDKProviderContext", 2.45],
  ["TestPreTurnCheckpointCapturesWorktreeOnce", 3.04],
  ["TestPresentationWithoutDraftNeverMutatesLegacyDraft", 0.63],
  ["TestProviderChatAgentReadProjectsActorBackgroundState", 1.38],
  ["TestProviderChatAgentWaitChangedIntentWinsWhenTargetIsMissing", 1.31],
  ["TestProviderChatAgentWaitFencesStalePairBeforeOwnerManager", 0.80],
  ["TestProviderChatAgentWaitManyUsesTerminalActorRows", 1.14],
  ["TestProviderChatAgentWaitObservationRaceReservesOneReceipt", 1.11],
  ["TestProviderChatAgentWaitUsesDurableObservationReceipt", 1.41],
  ["TestProviderChatCloseSessionDetachesCurrentAttachmentPreservingThread", 0.50],
  ["TestProviderChatCloseSessionRetryCannotCloseExactResumedAttachment", 0.69],
  ["TestProviderChatRuntimeResumesExactLaneAcrossActorAndTabRestart", 1.89],
  ["TestProviderChatRuntimeSwitchesAndReturnsThroughVerifiedContextImport", 1.27],
  ["TestProviderChatSteerRejectedInputDoesNotPersistAttachmentSidecar", 0.80],
  ["TestProviderChatSteerRejectsAfterForegroundEndWithoutTakingOwnership", 0.68],
  ["TestProviderChatSteerRejectsStaleDurableAttachmentBeforeManagerOrSidecars", 0.95],
  ["TestProviderDetectionAllowsFullInitializeAndSessionBudgets", 5.36],
  ["TestProviderLaneSelectionIsReadOnlyUntilAtomicReceiptCommit", 1.15],
  ["TestProviderLaneSelectionRetryCreatesAfterOldZeroThreadFailure", 1.91],
  ["TestProviderNativeCompactionBypassesWorkassFallback", 0.51],
  ["TestProviderRegistryCatalogToggleFailureAndConcurrentIsolation", 1.48],
  ["TestProviderUpdateAvailabilityUsesCardWithoutNotify", 0.50],
  ["TestQuestionWaitsForTheUserWhileAPermissionStillExpires", 0.62],
  ["TestRejectedSteerDoesNotEndParentOrPrematurelyAdoptRunningSubagents", 0.70],
  ["TestReplaceStagedQueueStaleRevisionDoesNotPersistAttachmentSidecar", 0.77],
  ["TestRuntimeControlsCommitToActorBeforeProviderAndApplyOnlyAtTurnBoundary", 2.17],
  ["TestRuntimeDiagnosticsCoalescedFailureGetsTrailingCheckpoint", 1.06],
  ["TestSaveProviderConfigsConcurrentWritersUseDistinctTemps", 0.74],
  ["TestSpawnedWorkListCarriesTheObligation", 2.00],
  ["TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession", 1.44],
  ["TestStatelessMCPMutationsRequireCallerStableOperationID", 0.67],
  ["TestStatelessMCPRoutesTaggedRemoteReadWithoutExposingOwner", 1.45],
  ["TestStatelessMCPRoutesUpdaterStatusAndAuthorizedApplyWithoutExposingOwner", 1.08],
  ["TestStatelessMCPSpawnsAndWaitsForTrackedSubagent", 3.15],
  ["TestStopDoesNotWaitForUnrelatedProviderAttachment", 1.82],
  ["TestStopSpawnedWorkKillsARealProcessThatIgnoresSIGTERM", 2.17],
  ["TestSubagentCancelClearsPermissionAndValidOwnerIsolation", 0.70],
  ["TestSubagentEventOnlyWaitCancellationDoesNotCancelChild", 0.54],
  ["TestSubagentLatchedPermissionAttentionSurfacesToAdoptingTurn", 2.08],
  ["TestSubagentSurvivesCancelledParentSettlesAndWritesReceipt", 2.76],
  ["TestSubagentTurnlessOwnerListsWaitsAndSpawnsBornAdoptedWithOptionalVisibleHint", 1.23],
  ["TestT3ExternalDoneFileSettlesAndWritesRedactedReceiptTail", 0.80],
  ["TestT7AgentControlExternalSettleIsIdempotentAndOwnerValidated", 1.79],
  ["TestToolAPIRefusesPlaintextAndBrowserOrigin", 0.70],
  ["TestToolsCLICrossProviderDelegationAndCrossChatMessaging", 6.56],
  ["TestTrackedSubagentCompletionDoesNotResurrectDeletedChat", 0.78],
  ["TestTrackedSubagentCompletionQueuesBehindUnrelatedForegroundTurn", 0.69],
  ["TestTrackedSubagentCompletionRunsOnOwningMockCoordinator", 1.98],
  ["TestTrackedSubagentCompletionUsesExactActorAndReceiptIdempotency", 1.14],
  ["TestTrackedSubagentTerminalStatusesSettleWithoutSyntheticWake", 0.87],
  ["TestTurnDiagnosticsMockCancellationRecordsWireBoundaries", 1.06],
  ["TestWireBusyStartQueuesCapabilityAwareFollowUpWithoutFailedTranscript", 1.39],
  ["TestWireDaemonQueueDrainsWithoutControllerAndReplaysPermissionOnAttach", 1.65],
  ["TestWireE2EAppChatAssertsJobEventChannel", 1.56],
  ["TestWireFreshProviderGetsHistorySeedAndEstablishedLaneUsesSafeImport", 3.88],
  ["TestWireJobStartReplyGateBlocksProviderAndProjectsFailureAfterReceipt", 1.30],
  ["TestWireMockBurstReachesClientAtDisplayCadence", 1.6],
  ["TestWireProviderCatalogConnectBeforeDetectionGetsSingleBroadcast", 2.68],
  ["TestWireProvidersDetectInvokeEmitsAndEnablesStubs", 5.31],
  ["TestWireReconnectRestoresLiveSessionControlsAndPendingPermission", 3.58],
  ["TestWireSessionRecoversTurnCompletedWithoutRenderer", 2.16],
  ["TestWireTraceAppChatSteer", 3.01],
  ["TestWireTraceChatEnvNumstat", 0.80],
  ["TestWireTraceForkChatSeedsPrefixAndDiverges", 4.83],
  ["TestWireTraceGroupedCatalogAndInterleavedProviders", 3.19],
  ["TestWireTraceHibernatedCheckpointKeepsTurnBaseline", 5.78],
  ["TestWireTraceMockCrashNeverReplaysAndNextDistinctPromptRuns", 2.70],
  ["TestWireTraceMockEngineCrashTerminalizesThenNextPromptResumesExactThread", 2.08],
  ["TestWireTraceMockPermissionTurn", 2.31],
  ["TestWireTraceNativeLocalProviderColdStartAndTurn", 5.19],
  ["TestWireTraceNotifyControllerOnlyRedactionAndNoTurnEndBacklog", 1.16],
  ["TestWireWorkspaceMoveCommitsBeforeInvalidationAndStaleReconnectUsesTargetCWD", 2.16],
  ["TestWorkassQuestionActorWireAnswerUnicodeIsolationAndReplay", 1.85],
  ["TestWorkspaceMoveCreatesFreshEpochWithoutTranscriptReplay", 0.60],
  ["TestWorkspaceReturnCreatesCurrentRevisionLaneAndAcceptsNextTurn", 5.02],
]);
const DEFAULT_SECONDS = 0.1;
const DEFAULT_WORKERS = os.availableParallelism();
const MAX_BATCH_SECONDS = 2;
const MAX_BATCH_CASES = 8;
const HEAVY_PACKAGES = ['./internal/acp', './cmd/workass', './internal/chat', './internal/appinstall'];
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
  const rank = item => item.kind === 'compile' ? -1 : item.package === slowPackage ? 0 : item.kind === 'build' ? 1 : item.kind === 'heavy' ? 2 : 3;
  return [...work].sort((a, b) => rank(a) - rank(b) ||
    (b.weight ?? DEFAULT_SECONDS) - (a.weight ?? DEFAULT_SECONDS) ||
    String(a.package ?? '').localeCompare(String(b.package ?? '')) || String(a.id ?? '').localeCompare(String(b.id ?? '')));
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

export async function runGoSuite({ cwd = process.cwd(), logDir, cacheDir, fixtureRoot = process.env.WORKASS_TEST_FIXTURE_ROOT, workers = DEFAULT_WORKERS, race = false, go = 'go', spawn = nodeSpawn, signalHandlers = true, abortSignal } = {}) {
  if (!Number.isInteger(workers) || workers < 1) throw new Error('workers must be a positive integer');
  const wallStart = performance.now();
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-matrix-'));
  let fixtureTempRoot;
  if (fixtureRoot) {
    await mkdir(fixtureRoot, { recursive: true });
    fixtureTempRoot = await mkdtemp(path.join(fixtureRoot, 'workass-go-fixtures-'));
  }
  const repoKey = createHash('sha256').update(realpathSync(cwd)).digest('hex').slice(0, 24);
  const binaryCache = cacheDir ?? (spawn === nodeSpawn
    ? path.join(os.tmpdir(), 'workass-go-test-binaries', repoKey, race ? 'race' : 'normal')
    : path.join(root, 'test-binaries'));
  await mkdir(binaryCache, { recursive: true });
  const cacheMarker = path.join(binaryCache, '.workass-go-test-cache');
  const markerValue = `workass-go-test-cache-v1\n${repoKey}\n${race ? 'race' : 'normal'}\n`;
  if (existsSync(cacheMarker)) {
    if (await readFile(cacheMarker, 'utf8') !== markerValue) throw new Error(`Go test binary cache ownership mismatch: ${binaryCache}`);
  } else {
    for (const pkg of HEAVY_PACKAGES) {
      const label = path.posix.basename(pkg);
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
  const commandTemp = async (label, { testBinary = false } = {}) => {
    if (testBinary && fixtureTempRoot) return mkdtemp(path.join(fixtureTempRoot, `${label}-`));
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
    const tempDir = await commandTemp(label, { testBinary });
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
    const packageBasenames = new Map();
    for (const pkg of packageMetadata) {
      const basename = path.posix.basename(pkg.importPath);
      const previous = packageBasenames.get(basename);
      if (previous) throw new Error(`Go test binary basename collision: ${previous} and ${pkg.importPath} both produce ${basename}.test`);
      packageBasenames.set(basename, pkg.importPath);
    }
    const buildAndList = [];
    const failures = [];
    const groups = [
      { id: 'acp', packages: packageMetadata.filter(pkg => pkg.importPath === 'workass/internal/acp') },
      { id: 'workass', packages: packageMetadata.filter(pkg => pkg.importPath === 'workass/cmd/workass') },
      { id: 'remaining', packages: packageMetadata.filter(pkg => pkg.importPath !== 'workass/internal/acp' && pkg.importPath !== 'workass/cmd/workass') },
    ];
    const workForGroup = group => [
      ...otherPackages.filter(pkg => group.packages.includes(pkg)).map(pkg => ({ kind: 'package', package: pkg.importPath, dir: pkg.dir, hasTests: pkg.hasTests, id: pkg.importPath, weight: pkg.importPath.endsWith('/machinebook') ? 10 : DEFAULT_SECONDS })),
      ...HEAVY_PACKAGES.map(pkg => ({ pkg, package: packages.find(name => name.endsWith(pkg.slice(1))) ?? `workass/${pkg.slice(2)}` })).filter(item => group.packages.some(meta => meta.importPath === item.package)).map(item => ({ kind: 'build', ...item, weight: DEFAULT_SECONDS })),
    ];
    const workQueue = orderWorkByWeight(groups.map(group => ({ kind: 'compile', group, id: `compile-${group.id}`, weight: DEFAULT_SECONDS })));
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
      const { pkg } = work;
      const importPath = packages.find(name => name.endsWith(pkg.slice(1))) ?? `workass/${pkg.slice(2)}`;
      const label = path.posix.basename(importPath);
      const cachedBinary = path.join(binaryCache, `${path.posix.basename(importPath)}.test`);
      const binary = path.join(root, `${label}.test`);
      const packageCwd = path.join(cwd, pkg.replace(/^\.\//, ''));
      if (!packageMetadata.find(item => item.importPath === importPath)?.hasTests) throw new Error(`${pkg} was unexpectedly classified as having no test files`);
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
    const executeCompile = async work => {
      const { group } = work;
      const packageIds = group.packages.map(pkg => pkg.importPath);
      const compilerParallelism = Math.max(1, Math.floor(workers / 3));
      await run(go, ['test', ...raceFlag, '-p', String(compilerParallelism), '-c', '-o', `${binaryCache}${path.sep}`, ...packageIds], `compile-${group.id}`);
      for (const task of workForGroup(group)) enqueue(task);
    };
    const executeBatch = async (batch, worker) => {
      const { item } = batch;
      const tempDir = await commandTemp(`worker-${worker}-${item.label}-batch-${batch.id}`, { testBinary: true });
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
      if (!hasTests) {
        appendJsonLine(jsonl, { event: 'package-test', package: work.package, cwd: packageCwd, code: 0, noTestFiles: true });
        summary.packageOutcomes.push({ package: work.package, action: 'pass', noTestFiles: true });
        return;
      }
      const cachePath = path.join(binaryCache, `${path.posix.basename(work.package)}.test`);
      const tempDir = await commandTemp(`other-${work.package.replace(/[^a-zA-Z0-9_-]/g, '-')}`, { testBinary: true });
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
    const pool = Array.from({ length: workers }, async (_, worker) => {
      while (!interrupted) {
        const work = await takeWork();
        if (!work) return;
        try {
          if (work.kind === 'compile') await executeCompile(work);
          else if (work.kind === 'build') await executeBuild(work);
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
    if (fixtureTempRoot) await rm(fixtureTempRoot, { recursive: true, force: true });
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
