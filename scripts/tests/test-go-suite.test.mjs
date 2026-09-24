import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { watch, writeFileSync } from 'node:fs';
import { spawn as realSpawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile, access } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { runGoSuite, parseTestList, anchoredTestPattern, partitionBatches, partitionSerialCases, inspectCaseOutput, orderWorkByWeight } from '../test-go-suite.mjs';

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

test('default heavy batches target two seconds and cap at 24 cases', () => {
  const testNames = Array.from({ length: 60 }, (_, index) => `TestBatch${String(index).padStart(2, '0')}`);
  const batches = partitionBatches(testNames);
  assert.equal(batches.flatMap(batch => batch.names).length, testNames.length);
  assert.ok(batches.every(batch => batch.names.length <= 24));
  assert.ok(batches.every(batch => batch.weight <= 2));
});

test('scheduler orders measured heavy batches globally and starts machinebook first', () => {
  const work = orderWorkByWeight([
    { kind: 'heavy', package: 'workass/cmd/workass', id: 1, weight: 3 },
    { kind: 'package', package: 'workass/internal/fast', id: 'fast', weight: 0.25 },
    { kind: 'package', package: 'workass/internal/machinebook', id: 'machinebook', weight: 10 },
    { kind: 'build', package: 'workass/internal/acp', id: 'build-acp', weight: 0.1 },
    { kind: 'heavy', package: 'workass/internal/acp', id: 2, weight: 4 },
  ]);
  assert.deepEqual(work.map(item => item.id), ['machinebook', 'build-acp', 2, 1, 'fast']);
  assert.deepEqual(partitionBatches(['TestFallback']).map(batch => batch.weight), [0.1]);
});

test('requested worker capacity is honored without the legacy six-worker clamp', async t => {
  const f = await fixture(t);
  const tracker = inProcessGoSpawn();
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 8, spawn: tracker.spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  assert.equal(result.workers, 8);
  assert.ok(tracker.maximum() >= 1);
  assert.ok(tracker.maximum() <= 8);
});

async function fixture(t, { fail = '', delay = '0.04', packageFail = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const logs = path.join(root, 'logs');
  await mkdir(path.join(root, 'internal', 'acp'), { recursive: true });
  await mkdir(path.join(root, 'cmd', 'workass'), { recursive: true });
  await mkdir(logs);
  const binarySource = path.join(root, 'fake-acp-test.sh');
  const goSource = path.join(root, 'fake-go.sh');
  const namesShell = names.map(name => `'${name}'`).join(' ');
  await writeFile(binarySource, `#!/bin/sh\nRUN=""; for ARG in "$@"; do case "$ARG" in -test.run=*) RUN=$(printf '%s' "$ARG" | sed 's/^-test.run=//') ;; esac; done; case "$*" in\n  *-test.list*) printf '%s\\n' ${namesShell} ;;\n  *) for CASE in ${namesShell}; do case "$RUN" in *"$CASE"*) echo "=== RUN $CASE"; echo "=== RUN $CASE/child"; echo "complete output for $CASE"; echo "--- PASS: $CASE/child (0.01s)"; if [ "$CASE" = '${fail}' ]; then echo '--- FAIL: $CASE (0.01s)'; echo 'deliberate failure detail' >&2; else echo "--- PASS: $CASE (0.01s)"; fi ;; esac; done; sleep ${delay}; [ '${fail}' = '' ] || case "$RUN" in *'${fail}'*) exit 7 ;; esac ;;\nesac\n`, { mode: 0o755 });
  await writeFile(goSource, `#!/bin/sh\ncase "$*" in\n  *' -c -o '* ) while [ "$1" != '-o' ]; do shift; done; shift; cp '${binarySource}' "$1"; chmod +x "$1" ;;\n  *'list ./...'*) printf '%s\\n' 'workass/internal/one' 'workass/internal/acp' 'workass/cmd/workass' 'workass/internal/two' ;;\n  *'test -json'*|*'test -race -json'*) for PACKAGE in "$@"; do case "$PACKAGE" in workass/*) echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\"}"; echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\",\\\"Test\\\":\\\"TestOther\\\"}" ;; esac; done; echo other-package-output; ${packageFail ? 'exit 9' : 'exit 0'} ;;\n  *) echo "unexpected go invocation: $*" >&2; exit 90 ;;\nesac\n`, { mode: 0o755 });
  return { root, logs, go: goSource };
}

function inProcessGoSpawn({ packages = ['workass/internal/one', 'workass/internal/acp', 'workass/cmd/workass', 'workass/internal/two'], failCase = '', packageFail = false, record = () => {}, compileDelayMs = 0, onCompileStart = () => {}, onCompileFinish = () => {}, onPackageStart = () => {} } = {}) {
  let active = 0, maximum = 0;
  const spawn = (command, args, options) => {
    const child = new EventEmitter();
    child.stdout = new PassThrough(); child.stderr = new PassThrough();
    child.kill = () => { setImmediate(() => child.emit('close', null, 'SIGTERM')); return true; };
    active++; maximum = Math.max(maximum, active);
    const finish = (code, stdout = '', stderr = '') => {
      if (stdout) child.stdout.end(stdout);
      else child.stdout.end();
      if (stderr) child.stderr.end(stderr);
      else child.stderr.end();
      setImmediate(() => { active--; child.emit('close', code, null); });
    };
    setImmediate(() => child.emit('spawn'));
    record(command, args, options);
    if (String(command).endsWith('.test')) {
      if (args[0] === '-test.list') { finish(0, `${names.join('\n')}\n`); return child; }
      const pattern = args.find(arg => arg.startsWith('-test.run='))?.slice('-test.run='.length) ?? '';
      const selected = names.filter(name => new RegExp(pattern).test(name));
      const out = selected.flatMap(name => [`=== RUN ${name}`, `=== RUN ${name}/child`, `complete output for ${name}`, `--- PASS: ${name}/child (0.01s)`, name === failCase ? `--- FAIL: ${name} (0.01s)` : `--- PASS: ${name} (0.01s)`]).join('\n') + '\n';
      const otherPackageFailure = packageFail && options.cwd?.endsWith('/internal/one');
      finish(failCase && selected.includes(failCase) ? 7 : otherPackageFailure ? 9 : 0, `${out}other-package-output\n`, failCase && selected.includes(failCase) || otherPackageFailure ? 'deliberate failure detail\n' : ''); return child;
    }
    if (command === 'go' || path.basename(String(command)).startsWith('fake-go')) {
      if (args[0] === 'test' && args.includes('-c')) {
        const binary = args[args.indexOf('-o') + 1];
        writeFileSync(binary, 'fake test binary');
        onCompileStart(args.at(-1));
        setTimeout(() => { onCompileFinish(args.at(-1)); finish(0); }, compileDelayMs);
        return child;
      }
      if (args[0] === 'list') { finish(0, `${packages.join('\n')}\n`); return child; }
      if (args[0] === 'test' && args.includes('-json')) {
        const pkg = args.at(-1);
        onPackageStart(pkg);
        const events = [{ Action: packageFail ? 'fail' : 'pass', Package: pkg }, { Action: 'pass', Package: pkg, Test: 'TestOther' }];
        finish(packageFail ? 9 : 0, `${events.map(event => JSON.stringify(event)).join('\n')}\nother-package-output\n`); return child;
      }
    }
    finish(90, '', `unexpected fake invocation: ${command} ${args.join(' ')}\n`); return child;
  };
  return { spawn, maximum: () => maximum };
}

test('fresh matrix covers both heavy packages once, overlaps within the bound, and retains full logs', async t => {
  const f = await fixture(t);
  const tracker = inProcessGoSpawn();
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
  assert.equal(result.otherGo.tests, 16);
  assert.equal(result.otherGo.passed, 16, 'remaining package totals include nested passing outcomes');
  const fullLog = await readFile(result.jsonlPath, 'utf8');
  assert.match(fullLog, /complete output for/);
  assert.match(fullLog, /other-package-output/);
  assert.match(fullLog, /TestAlpha\/child/);
  const completedBatches = fullLog.split(/\r?\n/).filter(Boolean).map(line => JSON.parse(line)).filter(event => event.event === 'batch-end');
  assert.equal(completedBatches.length, 2);
  assert.ok(completedBatches.every(batch => batch.names.length > 1));
  assert.equal(Object.values(result.heavyPackages).reduce((sum, pkg) => sum + pkg.cases.reduce((n, item) => n + item.nestedRun, 0), 0), names.length * 2);
});

test('remaining package work starts while heavy test binaries are still compiling', async t => {
  const f = await fixture(t);
  const events = [];
  const fake = inProcessGoSpawn({
    compileDelayMs: 60,
    onCompileStart: pkg => events.push(`compile-start:${pkg}`),
    onCompileFinish: pkg => events.push(`compile-finish:${pkg}`),
  });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 3, signalHandlers: false, spawn: fake.spawn });
  assert.equal(result.ok, true, result.error);
  const compileStarted = events.findIndex(event => event.startsWith('compile-start:'));
  const packageStarted = events.findIndex(event => event.startsWith('compile-start:workass/'));
  const firstCompileFinished = events.findIndex(event => event.startsWith('compile-finish:'));
  assert.ok(compileStarted >= 0 && packageStarted >= 0 && packageStarted < firstCompileFinished, events.join(', '));
});

test('case failures propagate after other cases run and preserve failure detail', async t => {
  const f = await fixture(t, { fail: 'TestBeta' });
  const fake = inProcessGoSpawn({ failCase: 'TestBeta' });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false, spawn: fake.spawn });
  assert.equal(result.ok, false);
  assert.equal(result.heavyPackages['./internal/acp'].failed, 1);
  assert.equal(result.heavyPackages['./internal/acp'].cases.length, names.length);
  assert.equal(result.heavyPackages['./cmd/workass'].cases.length, names.length, 'assigned work drains after a worker sees a failed batch');
  assert.equal(result.otherGo.tests, 16, 'remaining package work also drains after a heavy batch failure');
  assert.match(await readFile(result.jsonlPath, 'utf8'), /deliberate failure detail/);
});

test('other-package command failures propagate with complete output', async t => {
  const f = await fixture(t, { packageFail: true });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, signalHandlers: false, spawn: inProcessGoSpawn({ packageFail: true }).spawn });
  assert.equal(result.ok, false);
  assert.match(result.commandFailure.label, /^other-go-workass\/internal\//);
  assert.match(await readFile(result.jsonlPath, 'utf8'), /other-package-output/);
});

test('race flags follow the Go subcommand for compiled and remaining packages', async t => {
  const f = await fixture(t);
  const invocations = [];
  const fake = inProcessGoSpawn({ record: (command, args) => { if (command === f.go) invocations.push(args); } });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, race: true, workers: 2, signalHandlers: false, spawn: fake.spawn });
  assert.equal(result.ok, true, result.error);
  assert.ok(invocations.some(args => args[0] === 'test' && args[1] === '-race' && args[2] === '-c'));
  assert.ok(invocations.some(args => args[0] === 'test' && args[1] === '-race' && args.includes('-c') && args.at(-1).startsWith('workass/')));
});

test('abort terminates process group and records interruption', async t => {
  const f = await fixture(t);
  const pidFile = path.join(f.root, 'child.pid');
  const controller = new AbortController();
  let promise;
  t.after(async () => {
    controller.abort();
    if (promise) await promise;
  });
  const fakeGo = inProcessGoSpawn();
  const spawn = (command, args, options) => {
    if (String(command).endsWith('.test')) {
      if (args[0] === '-test.list') return fakeGo.spawn(command, args, options);
      return realSpawn('/bin/sh', ['-c', 'sleep 30 & echo "$!" > "$CHILD_PID_FILE"; wait'], options);
    }
    return fakeGo.spawn(command, args, options);
  };
  promise = runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 1, abortSignal: controller.signal, signalHandlers: false, spawn: (command, args, options) => {
    options = { ...options, env: { ...options.env, CHILD_PID_FILE: pidFile } };
    return spawn(command, args, options);
  } });
  await new Promise((resolve, reject) => {
    const watcher = watch(f.root, (event, filename) => {
      if (filename?.toString() === path.basename(pidFile)) {
        watcher.close(); clearTimeout(timer); resolve();
      }
    });
    const timer = setTimeout(() => { watcher.close(); reject(new Error('real process fixture did not publish its child pid')); }, 5000);
    access(pidFile).then(() => { watcher.close(); clearTimeout(timer); resolve(); }, () => {});
  });
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

test('in-process abort releases a waiting worker and leaves queued batches unspawned', async t => {
  const f = await fixture(t);
  const controller = new AbortController();
  const testNames = Array.from({ length: 70 }, (_, index) => `TestQueued${String(index).padStart(2, '0')}`);
  let heavySpawns = 0;
  let active = 0;
  const spawn = (command, args) => {
    const child = new EventEmitter();
    child.stdout = new PassThrough(); child.stderr = new PassThrough();
    let closed = false;
    let counted = false;
    const finish = (code, stdout = '') => {
      if (closed) return;
      closed = true;
      child.stdout.end(stdout); child.stderr.end();
      setImmediate(() => { if (counted) active--; child.emit('close', code, null); });
    };
    child.kill = () => { finish(null); return true; };
    setImmediate(() => child.emit('spawn'));
    if (args[0] === 'list') { finish(0, 'workass/internal/one\nworkass/internal/acp\nworkass/cmd/workass\n'); return child; }
    if (args[0] === 'test' && args.includes('-c')) {
      writeFileSync(args[args.indexOf('-o') + 1], 'fake test binary'); finish(0); return child;
    }
    if (String(command).endsWith('.test')) {
      if (args[0] === '-test.list') { finish(0, `${testNames.join('\n')}\n`); return child; }
      heavySpawns++; active++; counted = true;
      if (heavySpawns === 3) setImmediate(() => controller.abort());
      return child;
    }
    if (args[0] === 'test' && args.includes('-json')) { finish(0); return child; }
    finish(90);
    return child;
  };
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, workers: 3, abortSignal: controller.signal, signalHandlers: false, spawn });
  assert.equal(result.interrupted, 'ABORT');
  assert.equal(result.ok, false);
  assert.equal(heavySpawns, 3, 'the queued fourth batch must not spawn after interruption');
  assert.equal(active, 0, 'all acquired in-process children must be cleaned up');
});

test('spawn errors are returned and written to the command log', async t => {
  const f = await fixture(t);
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: path.join(f.root, 'missing-go'), workers: 2, signalHandlers: false });
  assert.equal(result.ok, false);
  assert.equal(result.commandFailure.label, 'list-packages');
  assert.match(result.commandFailure.spawnError, /ENOENT/);
  assert.match(await readFile(result.jsonlPath, 'utf8'), /ENOENT/);
});
