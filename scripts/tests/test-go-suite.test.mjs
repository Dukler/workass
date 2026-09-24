import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile, access } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { runGoSuite, parseTestList, anchoredTestPattern, allocateWorkers, partitionSerialCases, inspectCaseOutput } from '../test-go-suite.mjs';

const names = ['TestAlpha', 'TestBeta', 'ExampleWidget', 'FuzzParse'];

test('test listing keeps test, example, and fuzz roots; selection is anchored and escaped', () => {
  assert.deepEqual(parseTestList('TestAlpha\nExampleWidget\nFuzzParse\nBenchmarkIgnored\nTestAlpha\n'), ['ExampleWidget', 'FuzzParse', 'TestAlpha']);
  assert.equal(anchoredTestPattern('TestA.B'), '^TestA\\.B$');
  assert.deepEqual(inspectCaseOutput('TestParallel', 0, '=== RUN   TestParallel\n=== PAUSE TestParallel\n=== CONT  TestParallel\n=== RUN child\n--- PASS: child (0.01s)\n--- PASS: TestParallel (0.02s)\n'), { rootRuns: 1, rootPasses: 1, rootSkips: 0, rootFailures: 0, nestedRun: 1, nestedSkipped: 0, coverageError: false, outcome: 'pass' });
});

test('serial boundary is disjoint and retains all discovered names', () => {
  const all = [...names, 'TestStartupDetectionDoesNotRetryDevinNeedsLogin'];
  const split = partitionSerialCases(all);
  assert.deepEqual(split.serial, ['TestStartupDetectionDoesNotRetryDevinNeedsLogin']);
  assert.deepEqual([...split.parallel, ...split.serial].sort(), all.slice().sort());
});

test('worker allocation is deterministic and spreads measured costs', () => {
  const hints = new Map([['TestSlow', 9], ['TestFast', 1]]);
  const a = allocateWorkers(['TestZ', 'TestFast', 'TestSlow', 'TestA'], 2, hints);
  const b = allocateWorkers(['TestA', 'TestSlow', 'TestFast', 'TestZ'], 2, hints);
  assert.deepEqual(a, b);
  assert.equal(a.reduce((sum, worker) => sum + worker.names.length, 0), 4);
  assert.notEqual(a[0].weight, a[1].weight);
});

async function fixture(t, { fail = '', delay = '0.04', packageFail = false, childWait = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const logs = path.join(root, 'logs');
  await mkdir(path.join(root, 'internal', 'acp'), { recursive: true });
  await mkdir(logs);
  const binarySource = path.join(root, 'fake-acp-test.sh');
  const goSource = path.join(root, 'fake-go.sh');
  await writeFile(binarySource, `#!/bin/sh\nFAKE_CASE=""; PREV=""; for ARG in "$@"; do if [ "$PREV" = "-test.run" ]; then FAKE_CASE="$ARG"; fi; PREV="$ARG"; done; FAKE_CASE="$(printf %s "$FAKE_CASE" | tr -d '^$')"; case "$*" in\n  *-test.list*) printf '%s\\n' ${names.map(name => `'${name}'`).join(' ')} ;;\n  *) echo "=== RUN $FAKE_CASE"; echo '=== RUN TestNested/child'; echo "complete output for $FAKE_CASE"; echo '--- PASS: TestNested/child (0.01s)'; if [ "$FAKE_CASE" = '${fail}' ]; then echo 'deliberate failure detail' >&2; exit 7; fi; ${childWait ? 'sleep 30 & echo $! > "$CHILD_PID_FILE"; wait' : `sleep ${delay}`}; echo "--- PASS: $FAKE_CASE (0.01s)" ;;\nesac\n`, { mode: 0o755 });
  await writeFile(goSource, `#!/bin/sh\ncase "$*" in\n  *'test -c -o '* ) while [ "$1" != '-o' ]; do shift; done; shift; cp '${binarySource}' "$1"; chmod +x "$1" ;;\n  *'list ./...'*) printf '%s\\n' 'workass/internal/one' 'workass/internal/acp' 'workass/internal/two' ;;\n  *'test -json -count=1 -p=2'*) echo other-package-output; ${packageFail ? 'exit 9' : 'exit 0'} ;;\n  *) echo "unexpected go invocation: $*" >&2; exit 90 ;;\nesac\n`, { mode: 0o755 });
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

test('fresh matrix covers all discovered roots once, overlaps within the bound, and retains full logs', async t => {
  const f = await fixture(t);
  const tracker = trackingSpawn();
  const spawn = (command, args, options) => {
    if (String(command).includes('acp.test')) assert.equal(options.cwd, path.join(f.root, 'internal', 'acp'));
    return tracker.spawn(command, args, options);
  };
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 3, spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  assert.deepEqual(result.acp.cases.map(item => item.name).sort(), names.slice().sort());
  assert.equal(new Set(result.acp.cases.map(item => item.name)).size, names.length);
  assert.ok(tracker.maximum() > 1);
  assert.ok(tracker.maximum() <= 3);
  assert.equal(result.otherPackages.length, 2);
  const fullLog = await readFile(result.jsonlPath, 'utf8');
  assert.match(fullLog, /complete output for/);
  assert.match(fullLog, /other-package-output/);
  assert.match(fullLog, /TestNested\/child/);
  assert.equal(result.acp.cases.reduce((sum, item) => sum + item.nestedRun, 0), names.length);
});

test('case failures propagate after other cases run and preserve failure detail', async t => {
  const f = await fixture(t, { fail: 'TestBeta' });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.acp.failed, 1);
  assert.equal(result.acp.cases.length, names.length);
  assert.match(await readFile(result.jsonlPath, 'utf8'), /deliberate failure detail/);
});

test('other-package command failures propagate with complete output', async t => {
  const f = await fixture(t, { packageFail: true });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.commandFailure.label, 'other-go-packages');
  assert.match(await readFile(result.jsonlPath, 'utf8'), /other-package-output/);
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
