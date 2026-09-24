import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile, access } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { runGoSuite, parseTestList, anchoredTestPattern, partitionBatches, partitionSerialCases, inspectCaseOutput } from '../test-go-suite.mjs';

const names = ['TestAlpha', 'TestBeta', 'ExampleWidget', 'FuzzParse'];

test('test listing keeps test, example, and fuzz roots; selection is anchored and escaped', () => {
  assert.deepEqual(parseTestList('TestAlpha\nExampleWidget\nFuzzParse\nBenchmarkIgnored\nTest字\nTestAlpha\n'), ['TestAlpha', 'ExampleWidget', 'FuzzParse', 'Test字', 'TestAlpha']);
  assert.equal(anchoredTestPattern('TestA.B'), '^TestA\\.B$');
  assert.deepEqual(inspectCaseOutput('TestParallel', 0, '=== RUN   TestParallel\n=== PAUSE TestParallel\n=== CONT  TestParallel\n=== RUN TestParallel/child\n    --- PASS: TestParallel/child (0.01s)\n--- PASS: TestParallel (0.02s)\n'), { rootRuns: 1, rootPasses: 1, rootSkips: 0, rootFailures: 0, nestedRun: 1, nestedPassed: 1, nestedSkipped: 0, nestedFailed: 0, coverageError: false, outcome: 'pass' });
});

test('serial boundary is disjoint and retains all discovered names', () => {
  const all = [...names, 'TestStartupDetectionDoesNotRetryDevinNeedsLogin'];
  const split = partitionSerialCases(all);
  assert.deepEqual(split.serial, ['TestStartupDetectionDoesNotRetryDevinNeedsLogin']);
  assert.deepEqual([...split.parallel, ...split.serial].sort(), all.slice().sort());
});

test('weighted batches stay bounded and retain every discovered root once', () => {
  const hints = new Map([['TestSlow', 9], ['TestFast', 1]]);
  const batches = partitionBatches(['TestZ', 'TestFast', 'TestSlow', 'TestA'], hints, { maxWeight: 3, maxCases: 2 });
  const batched = batches.flatMap(batch => batch.names);
  assert.equal(batched.length, 4);
  assert.deepEqual([...batched].sort(), ['TestA', 'TestFast', 'TestSlow', 'TestZ']);
  assert.ok(batches.every(batch => batch.names.length <= 2));
  assert.ok(batches.every(batch => batch.weight <= 3 || batch.names.length === 1));
});

async function fixture(t, { fail = '', delay = '0.04', packageFail = false, childWait = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const logs = path.join(root, 'logs');
  await mkdir(path.join(root, 'internal', 'acp'), { recursive: true });
  await mkdir(path.join(root, 'cmd', 'workass'), { recursive: true });
  await mkdir(logs);
  const binarySource = path.join(root, 'fake-acp-test.sh');
  const goSource = path.join(root, 'fake-go.sh');
  const namesShell = names.map(name => `'${name}'`).join(' ');
  await writeFile(binarySource, `#!/bin/sh\nRUN=""; for ARG in "$@"; do case "$ARG" in -test.run=*) RUN=$(printf '%s' "$ARG" | sed 's/^-test.run=//') ;; esac; done; case "$*" in\n  *-test.list*) printf '%s\\n' ${namesShell} ;;\n  *) for CASE in ${namesShell}; do case "$RUN" in *"$CASE"*) echo "=== RUN $CASE"; echo "=== RUN $CASE/child"; echo "complete output for $CASE"; echo "--- PASS: $CASE/child (0.01s)"; if [ "$CASE" = '${fail}' ]; then echo '--- FAIL: $CASE (0.01s)'; echo 'deliberate failure detail' >&2; else echo "--- PASS: $CASE (0.01s)"; fi ;; esac; done; ${childWait ? 'sleep 30 & echo $! > "$CHILD_PID_FILE"; wait' : `sleep ${delay}`}; [ '${fail}' = '' ] || case "$RUN" in *'${fail}'*) exit 7 ;; esac ;;\nesac\n`, { mode: 0o755 });
  await writeFile(goSource, `#!/bin/sh\ncase "$*" in\n  *' -c -o '* ) while [ "$1" != '-o' ]; do shift; done; shift; cp '${binarySource}' "$1"; chmod +x "$1" ;;\n  *'list ./...'*) printf '%s\\n' 'workass/internal/one' 'workass/internal/acp' 'workass/cmd/workass' 'workass/internal/two' ;;\n  *'test -json'*|*'test -race -json'*) for PACKAGE in "$@"; do case "$PACKAGE" in workass/*) echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\"}"; echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\",\\\"Test\\\":\\\"TestOther\\\"}" ;; esac; done; echo other-package-output; ${packageFail ? 'exit 9' : 'exit 0'} ;;\n  *) echo "unexpected go invocation: $*" >&2; exit 90 ;;\nesac\n`, { mode: 0o755 });
  return { root, logs, go: goSource };
}

function trackingSpawn() {
  const { spawn } = awaitImportChildProcess;
  let active = 0, maximum = 0;
  return {
    spawn(command, args, options) {
      const child = spawn(command, args, options);
      child.once('spawn', () => { active++; maximum = Math.max(maximum, active); });
      child.once('close', () => { active--; });
      return child;
    },
    maximum: () => maximum,
  };
}
import * as awaitImportChildProcess from 'node:child_process';

test('fresh matrix covers both heavy packages once, overlaps within the bound, and retains full logs', async t => {
  const f = await fixture(t);
  const tracker = trackingSpawn();
  const spawn = (command, args, options) => {
    if (String(command).includes('acp.test')) assert.equal(options.cwd, path.join(f.root, 'internal', 'acp'));
    return tracker.spawn(command, args, options);
  };
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 3, spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  for (const pkg of ['./internal/acp', './cmd/workass']) {
    assert.deepEqual(result.heavyPackages[pkg].cases.map(item => item.name).sort(), names.slice().sort());
    assert.equal(new Set(result.heavyPackages[pkg].cases.map(item => item.name)).size, names.length);
    assert.equal(result.heavyPackages[pkg].nestedPassed, names.length);
  }
  assert.ok(tracker.maximum() > 1);
  assert.ok(tracker.maximum() <= 3);
  assert.equal(result.otherPackages.length, 2);
  const fullLog = await readFile(result.jsonlPath, 'utf8');
  assert.match(fullLog, /complete output for/);
  assert.match(fullLog, /other-package-output/);
  assert.match(fullLog, /TestAlpha\/child/);
  const completedBatches = fullLog.split(/\r?\n/).filter(Boolean).map(line => JSON.parse(line)).filter(event => event.event === 'batch-end');
  assert.equal(completedBatches.length, 2);
  assert.ok(completedBatches.every(batch => batch.names.length > 1));
  assert.equal(Object.values(result.heavyPackages).reduce((sum, pkg) => sum + pkg.cases.reduce((n, item) => n + item.nestedRun, 0), 0), names.length * 2);
});

test('case failures propagate after other cases run and preserve failure detail', async t => {
  const f = await fixture(t, { fail: 'TestBeta' });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.heavyPackages['./internal/acp'].failed, 1);
  assert.equal(result.heavyPackages['./internal/acp'].cases.length, names.length);
  assert.match(await readFile(result.jsonlPath, 'utf8'), /deliberate failure detail/);
});

test('other-package command failures propagate with complete output', async t => {
  const f = await fixture(t, { packageFail: true });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.commandFailure.label, 'other-go-packages');
  assert.match(await readFile(result.jsonlPath, 'utf8'), /other-package-output/);
});

test('race flags follow the Go subcommand for compiled and remaining packages', async t => {
  const f = await fixture(t, { delay: '0.001' });
  const invocations = [];
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, race: true, workers: 2, signalHandlers: false, spawn: (command, args, options) => {
    if (command === f.go) invocations.push(args);
    return awaitImportChildProcess.spawn(command, args, options);
  } });
  assert.equal(result.ok, true, result.error);
  assert.ok(invocations.some(args => args[0] === 'test' && args[1] === '-race' && args[2] === '-c'));
  assert.ok(invocations.some(args => args[0] === 'test' && args[1] === '-race' && args.includes('-json')));
});

test('abort terminates process group and records interruption', async t => {
  const f = await fixture(t, { childWait: true });
  const pidFile = path.join(f.root, 'child.pid');
  const controller = new AbortController();
  const promise = runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 1, abortSignal: controller.signal, signalHandlers: false, spawn: (command, args, options) => {
    options = { ...options, env: { ...options.env, CHILD_PID_FILE: pidFile } };
    return awaitImportChildProcess.spawn(command, args, options);
  } });
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    try { await access(pidFile); break; } catch { await new Promise(resolve => setTimeout(resolve, 10)); }
  }
  await access(pidFile);
  controller.abort();
  const result = await promise;
  assert.equal(result.ok, false);
  assert.equal(result.interrupted, 'ABORT');
  const pid = Number((await readFile(pidFile, 'utf8')).trim());
  if (process.platform !== 'win32') {
    await new Promise(resolve => setTimeout(resolve, 30));
    try { process.kill(pid, 0); assert.fail(`helper child ${pid} survived interruption`); } catch (error) { assert.equal(error.code, 'ESRCH'); }
  }
});

test('spawn errors are returned and written to the command log', async t => {
  const f = await fixture(t);
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: path.join(f.root, 'missing-go'), workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.commandFailure.label, 'compile-acp');
  assert.match(result.commandFailure.spawnError, /ENOENT/);
  assert.match(await readFile(result.jsonlPath, 'utf8'), /ENOENT/);
});
