import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { createHash } from 'node:crypto';
import { PassThrough } from 'node:stream';
import { watch, writeFileSync } from 'node:fs';
import { spawn as realSpawn, spawnSync } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile, access, symlink } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { runGoSuite, parseTestList, anchoredTestPattern, partitionBatches, partitionSerialCases, inspectCaseOutput, orderWorkByWeight, isMainModule } from '../test-go-suite.mjs';

const names = ['TestAlpha', 'TestBeta', 'ExampleWidget', 'FuzzParse'];

test('symlinked Go runner executes its main entry and help without starting suites', async t => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'workass-go-entry-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const source = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../test-go-suite.mjs');
  const alias = path.join(dir, 'runner.mjs');
  await symlink(source, alias);
  assert.equal(isMainModule(alias, source), true);
  const result = spawnSync(process.execPath, [alias, '--help'], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /Usage: node scripts\/test-go-suite\.mjs/);
  assert.doesNotMatch(result.stdout, /"ok"\s*:/, 'help exits before Go suite execution');
});

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

test('default heavy batches target two seconds and cap at eight cases', () => {
  const testNames = Array.from({ length: 60 }, (_, index) => `TestBatch${String(index).padStart(2, '0')}`);
  const batches = partitionBatches(testNames);
  assert.equal(batches.flatMap(batch => batch.names).length, testNames.length);
  assert.ok(batches.every(batch => batch.names.length <= 8));
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
  let executions = 0;
  let releaseExecutions;
  const executionBarrier = new Promise(resolve => { releaseExecutions = resolve; });
  const tracker = inProcessGoSpawn({ testExecutionGate: () => {
    executions++;
    if (executions === 4) releaseExecutions();
    return executionBarrier;
  } });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 8, spawn: tracker.spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  assert.equal(result.workers, 8);
  assert.ok(tracker.maximum() > 3, 'workers beyond the initial three compile jobs take dynamically enqueued tests');
  assert.ok(tracker.maximum() <= 8);
});

test('only test binary listing and execution cap GOMAXPROCS; Go commands inherit it', async t => {
  const f = await fixture(t);
  const prior = process.env.GOMAXPROCS;
  process.env.GOMAXPROCS = '18';
  t.after(() => {
    if (prior === undefined) delete process.env.GOMAXPROCS;
    else process.env.GOMAXPROCS = prior;
  });
  const invocations = [];
  const fake = inProcessGoSpawn({ record: (command, args, options) => invocations.push({ command, args, env: options.env }) });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 2, spawn: fake.spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  const goCommands = invocations.filter(item => item.command === f.go);
  assert.ok(goCommands.length > 0);
  assert.ok(goCommands.every(item => item.env.GOMAXPROCS === '18'), 'compiler and package-list commands keep the inherited Go setting');
  const binaryCommands = invocations.filter(item => String(item.command).endsWith('.test'));
  assert.ok(binaryCommands.some(item => item.args[0] === '-test.list'));
  assert.ok(binaryCommands.some(item => item.args[0] === '-test.v'));
  assert.ok(binaryCommands.every(item => item.env.GOMAXPROCS === '1'), 'listing and test execution use one Go runtime CPU');
  assert.ok(binaryCommands.filter(item => item.args.includes('-test.v')).every(item => item.args.includes('-test.parallel=2')));
});

test('optional fixture root routes only Go test binary temp files there and removes its invocation subtree', async t => {
  const f = await fixture(t);
  const fixtureRoot = path.join(f.root, 'ram-fixtures');
  await mkdir(fixtureRoot);
  const invocations = [];
  const fake = inProcessGoSpawn({ record: (command, args, options) => invocations.push({ command, args, env: options.env }) });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRoot, go: f.go, workers: 2, spawn: fake.spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  const binaries = invocations.filter(item => String(item.command).endsWith('.test'));
  assert.ok(binaries.length > 0);
  assert.ok(binaries.every(item => item.env.TMPDIR.startsWith(fixtureRoot + path.sep)));
  assert.ok(binaries.every(item => item.env.TMPDIR === item.env.TMP && item.env.TMPDIR === item.env.TEMP));
  assert.ok(invocations.filter(item => item.command === f.go).every(item => !item.env.TMPDIR.startsWith(fixtureRoot + path.sep)));
  assert.deepEqual(await import('node:fs/promises').then(fs => fs.readdir(fixtureRoot)), []);
});

test('metadata and all compile groups run while fixture is pending; test binaries wait for its root', async t => {
  const f = await fixture(t);
  let provideRoot;
  const fixtureRootPromise = new Promise(resolve => { provideRoot = resolve; });
  const events = [];
  const fake = inProcessGoSpawn({
    record: (command, args) => events.push(String(command).endsWith('.test') ? `binary:${args[0]}` : args[0] === 'test' && args.includes('-c') ? 'compile' : args[0]),
    onCompileFinish: () => events.push('compile-finished'),
    onTestBinaryStart: () => events.push('binary-started'),
  });
  const running = runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise, go: f.go, workers: 3, spawn: fake.spawn, signalHandlers: false });
  const deadline = Date.now() + 2000;
  while (events.filter(event => event === 'compile-finished').length < 3 && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(events.filter(event => event === 'compile-finished').length, 3, 'three compile groups completed before fixture delivery');
  assert.equal(events.includes('binary-started'), false, 'listing and execution binaries remain gated on the fixture root');
  const fixtureRoot = path.join(f.root, 'delivered-fixture');
  await mkdir(fixtureRoot);
  provideRoot(fixtureRoot);
  const result = await running;
  assert.equal(result.ok, true, result.error);
  assert.equal(events.filter(event => event === 'binary-started').length > 0, true);
  assert.equal(events.filter(event => event === 'compile-finished').length, 3);
});

test('concurrent Go workers share one asynchronously provisioned fixture subtree', async t => {
  const f = await fixture(t);
  let provideRoot;
  const fixtureRootPromise = new Promise(resolve => { provideRoot = resolve; });
  const binaryTemps = [];
  let compileFinishes = 0;
  const fake = inProcessGoSpawn({ record: (command, _args, options) => {
    if (String(command).endsWith('.test')) binaryTemps.push(options.env.TMPDIR);
  }, onCompileFinish: () => { compileFinishes++; }});
  const running = runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise, go: f.go, workers: 18, spawn: fake.spawn, signalHandlers: false });
  while (compileFinishes < 3) await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(binaryTemps.length, 0, 'no test binary starts while fixture provisioning is held');
  const fixtureRoot = path.join(f.root, 'shared-fixture');
  await mkdir(fixtureRoot);
  provideRoot(fixtureRoot);
  const result = await running;
  assert.equal(result.ok, true, result.error);
  assert.ok(binaryTemps.length > 2, 'multiple workers reached fixture temp creation');
  assert.equal(new Set(binaryTemps.map(dir => path.dirname(dir))).size, 1, 'all test commands use one memoized owned fixture root');
  assert.deepEqual(await import('node:fs/promises').then(fs => fs.readdir(fixtureRoot)), [], 'the single owned fixture subtree was removed');
});

test('abort while waiting for fixture delivery settles workers and joins active compilers', async t => {
  const f = await fixture(t);
  const controller = new AbortController();
  const fixtureRootPromise = new Promise(() => {});
  const spawned = [];
  let compileStarts = 0;
  const fake = inProcessGoSpawn({ record: (_command, args) => spawned.push(args[0]), onCompileStart: () => { compileStarts++; }, compileDelayMs: 500 });
  const running = runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise, go: f.go, workers: 3, spawn: fake.spawn, signalHandlers: false, abortSignal: controller.signal });
  while (compileStarts < 3) await new Promise(resolve => setTimeout(resolve, 5));
  controller.abort();
  const result = await running;
  assert.equal(result.ok, false);
  assert.equal(result.interrupted, 'ABORT');
  assert.equal(spawned.includes('-test.v'), false, 'no Go test binary ran without fixture delivery');
  assert.equal(fake.active(), 0, 'the prestarted compile process groups are joined before return');
});

test('pre-aborted fixture wait returns an interrupted summary and cleans up', async t => {
  const f = await fixture(t);
  const controller = new AbortController();
  controller.abort();
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise: new Promise(() => {}), go: f.go, workers: 3, spawn: inProcessGoSpawn().spawn, signalHandlers: false, abortSignal: controller.signal });
  assert.equal(result.ok, false);
  assert.equal(result.interrupted, 'ABORT');
  assert.match(result.error, /interrupted by ABORT/);
});

test('abort after compilation while workers wait for fixture delivery joins all children', async t => {
  const f = await fixture(t);
  const controller = new AbortController();
  let compileFinishes = 0;
  const fake = inProcessGoSpawn({ onCompileFinish: () => { compileFinishes++; } });
  const running = runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise: new Promise(() => {}), go: f.go, workers: 3, spawn: fake.spawn, signalHandlers: false, abortSignal: controller.signal });
  while (compileFinishes < 3) await new Promise(resolve => setTimeout(resolve, 5));
  controller.abort();
  const result = await running;
  assert.equal(result.interrupted, 'ABORT');
  assert.equal(fake.active(), 0);
});

test('process signal after compilation rejects fixture wait and joins all children', async t => {
  const f = await fixture(t);
  let compileFinishes = 0;
  const fake = inProcessGoSpawn({ onCompileFinish: () => { compileFinishes++; } });
  const running = runGoSuite({ cwd: f.root, logDir: f.logs, fixtureRootPromise: new Promise(() => {}), go: f.go, workers: 3, spawn: fake.spawn });
  while (compileFinishes < 3) await new Promise(resolve => setTimeout(resolve, 5));
  const signalHandler = process.listeners('SIGTERM').at(-1);
  assert.equal(typeof signalHandler, 'function', 'runner installs its SIGTERM handler while active');
  signalHandler('SIGTERM');
  const result = await running;
  assert.equal(result.interrupted, 'SIGTERM');
  assert.equal(fake.active(), 0);
});

async function fixture(t, { fail = '', delay = '0.04', packageFail = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const logs = path.join(root, 'logs');
  await mkdir(path.join(root, 'internal', 'acp'), { recursive: true });
  await mkdir(path.join(root, 'internal', 'chat'), { recursive: true });
  await mkdir(path.join(root, 'internal', 'appinstall'), { recursive: true });
  await mkdir(path.join(root, 'cmd', 'workass'), { recursive: true });
  await mkdir(logs);
  const binarySource = path.join(root, 'fake-acp-test.sh');
  const goSource = path.join(root, 'fake-go.sh');
  const namesShell = names.map(name => `'${name}'`).join(' ');
  await writeFile(binarySource, `#!/bin/sh\nRUN=""; for ARG in "$@"; do case "$ARG" in -test.run=*) RUN=$(printf '%s' "$ARG" | sed 's/^-test.run=//') ;; esac; done; case "$*" in\n  *-test.list*) printf '%s\\n' ${namesShell} ;;\n  *) for CASE in ${namesShell}; do case "$RUN" in *"$CASE"*) echo "=== RUN $CASE"; echo "=== RUN $CASE/child"; echo "complete output for $CASE"; echo "--- PASS: $CASE/child (0.01s)"; if [ "$CASE" = '${fail}' ]; then echo '--- FAIL: $CASE (0.01s)'; echo 'deliberate failure detail' >&2; else echo "--- PASS: $CASE (0.01s)"; fi ;; esac; done; sleep ${delay}; [ '${fail}' = '' ] || case "$RUN" in *'${fail}'*) exit 7 ;; esac ;;\nesac\n`, { mode: 0o755 });
  await writeFile(goSource, `#!/bin/sh\ncase "$*" in\n  *' -c -o '* ) while [ "$1" != '-o' ]; do shift; done; shift; cp '${binarySource}' "$1"; chmod +x "$1" ;;\n  *'list ./...'*) printf '%s\\n' 'workass/internal/one' 'workass/internal/acp' 'workass/cmd/workass' 'workass/internal/two' ;;\n  *'test -json'*|*'test -race -json'*) for PACKAGE in "$@"; do case "$PACKAGE" in workass/*) echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\"}"; echo "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"$PACKAGE\\\",\\\"Test\\\":\\\"TestOther\\\"}" ;; esac; done; echo other-package-output; ${packageFail ? 'exit 9' : 'exit 0'} ;;\n  *) echo "unexpected go invocation: $*" >&2; exit 90 ;;\nesac\n`, { mode: 0o755 });
  return { root, logs, go: goSource };
}

function inProcessGoSpawn({ packages = ['workass/internal/one', 'workass/internal/acp', 'workass/cmd/workass', 'workass/internal/chat', 'workass/internal/appinstall', 'workass/internal/two'], noTestPackages = [], failCase = '', packageFail = false, record = () => {}, compileDelayMs = 0, compileDelays = {}, failCompile = '', onCompileStart = () => {}, onCompileFinish = () => {}, onPackageStart = () => {}, onTestBinaryStart = () => {}, testExecutionGate = () => Promise.resolve() } = {}) {
  let active = 0, maximum = 0;
  const spawn = (command, args, options) => {
    const child = new EventEmitter();
    child.stdout = new PassThrough(); child.stderr = new PassThrough();
    let closed = false;
    active++; maximum = Math.max(maximum, active);
    const finish = (code, stdout = '', stderr = '') => {
      if (closed) return;
      closed = true;
      if (stdout) child.stdout.end(stdout);
      else child.stdout.end();
      if (stderr) child.stderr.end(stderr);
      else child.stderr.end();
      setImmediate(() => { active--; child.emit('close', code, null); });
    };
    child.kill = () => { finish(null); return true; };
    setImmediate(() => child.emit('spawn'));
    record(command, args, options);
    if (String(command).endsWith('.test')) {
      onTestBinaryStart(path.basename(String(command)));
      if (args[0] === '-test.list') { finish(0, `${names.join('\n')}\n`); return child; }
      const pattern = args.find(arg => arg.startsWith('-test.run='))?.slice('-test.run='.length) ?? '';
      const selected = names.filter(name => new RegExp(pattern).test(name));
      const out = selected.flatMap(name => [`=== RUN ${name}`, `=== RUN ${name}/child`, `complete output for ${name}`, `--- PASS: ${name}/child (0.01s)`, name === failCase ? `--- FAIL: ${name} (0.01s)` : `--- PASS: ${name} (0.01s)`]).join('\n') + '\n';
      const otherPackageFailure = packageFail && options.cwd?.endsWith('/internal/one');
      Promise.resolve(testExecutionGate()).then(() => finish(failCase && selected.includes(failCase) ? 7 : otherPackageFailure ? 9 : 0, `${out}other-package-output\n`, failCase && selected.includes(failCase) || otherPackageFailure ? 'deliberate failure detail\n' : ''));
      return child;
    }
    if (command === 'go' || path.basename(String(command)).startsWith('fake-go')) {
      if (args[0] === 'test' && args.includes('-c')) {
        const outputDir = args[args.indexOf('-o') + 1];
        const selectedPackages = args.slice(args.indexOf('-o') + 2);
        const group = selectedPackages.join(',');
        for (const pkg of selectedPackages) if (packages.includes(pkg) && pkg !== failCompile && !noTestPackages.includes(pkg)) writeFileSync(path.join(outputDir, `${path.posix.basename(pkg)}.test`), 'fake test binary');
        onCompileStart(group);
        setTimeout(() => { onCompileFinish(group); finish(failCompile && selectedPackages.includes(failCompile) ? 8 : 0); }, compileDelays[group] ?? compileDelayMs);
        return child;
      }
      if (args[0] === 'list') { finish(0, `${packages.map(pkg => `${pkg}\t${path.join(options.cwd, pkg.replace(/^workass\//, ''))}\t${noTestPackages.includes(pkg) ? 'false' : 'true'}`).join('\n')}\n`); return child; }
      if (args[0] === 'test' && args.includes('-json')) {
        const pkg = args.at(-1);
        onPackageStart(pkg);
        const events = [{ Action: packageFail ? 'fail' : 'pass', Package: pkg }, { Action: 'pass', Package: pkg, Test: 'TestOther' }];
        finish(packageFail ? 9 : 0, `${events.map(event => JSON.stringify(event)).join('\n')}\nother-package-output\n`); return child;
      }
    }
    finish(90, '', `unexpected fake invocation: ${command} ${args.join(' ')}\n`); return child;
  };
  return { spawn, maximum: () => maximum, active: () => active };
}

test('fresh matrix covers all four heavy packages once, with unique labels and paths, within the bound, and retains full logs', async t => {
  const f = await fixture(t);
  const executions = [];
  const tracker = inProcessGoSpawn({ record: (command, args, options) => {
    if (['acp.test', 'workass.test', 'chat.test', 'appinstall.test'].includes(path.basename(String(command))) && args[0] === '-test.v') executions.push({ binary: String(command), args, cwd: options.cwd });
  } });
  const spawn = (command, args, options) => {
    if (String(command).includes('acp.test')) assert.equal(options.cwd, path.join(f.root, 'internal', 'acp'));
    return tracker.spawn(command, args, options);
  };
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 3, spawn, signalHandlers: false });
  assert.equal(result.ok, true, result.error);
  const heavyPackages = ['./internal/acp', './cmd/workass', './internal/chat', './internal/appinstall'];
  for (const pkg of heavyPackages) {
    assert.deepEqual(result.heavyPackages[pkg].cases.map(item => item.name).sort(), names.slice().sort());
    assert.equal(new Set(result.heavyPackages[pkg].cases.map(item => item.name)).size, names.length);
    assert.ok(result.heavyPackages[pkg].cases.every(item => item.nestedRun === 1 && item.nestedPassed === 1), `${pkg} subtests execute exactly once`);
    assert.equal(result.heavyPackages[pkg].nestedPassed, names.length);
  }
  const byBinary = new Map();
  for (const execution of executions) byBinary.set(execution.binary, (byBinary.get(execution.binary) ?? 0) + execution.args[1].split('|').length);
  assert.equal(byBinary.size, 4, 'each heavy package has its own pinned test binary path');
  assert.deepEqual([...byBinary.keys()].map(binary => path.basename(binary, '.test')).sort(), ['acp', 'appinstall', 'chat', 'workass']);
  assert.equal([...byBinary.values()].reduce((sum, count) => sum + count, 0), names.length * 4, 'every heavy-package test is assigned and executed once');
  assert.deepEqual(new Set(executions.map(item => item.cwd)).size, 4, 'each package executes from its own package directory');
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
  assert.equal(completedBatches.length, 4);
  assert.ok(completedBatches.every(batch => batch.names.length > 1));
  assert.equal(Object.values(result.heavyPackages).reduce((sum, pkg) => sum + pkg.cases.reduce((n, item) => n + item.nestedRun, 0), 0), names.length * 4);
});

test('three disjoint bounded compiles cover every package once on every invocation', async t => {
  const f = await fixture(t);
  const cacheDir = path.join(f.root, 'stable-cache');
  const compiles = [];
  const invocations = [];
  const fake = inProcessGoSpawn({ record: (command, args) => {
    if (command === f.go) {
      invocations.push(args);
      if (args[0] === 'test' && args.includes('-c')) compiles.push({ pkg: args.slice(args.indexOf('-o') + 2).join(','), output: args[args.indexOf('-o') + 1] });
    }
  } });
  for (let invocation = 0; invocation < 2; invocation++) {
    const result = await runGoSuite({ cwd: f.root, logDir: f.logs, cacheDir, go: f.go, workers: 3, spawn: fake.spawn, signalHandlers: false });
    assert.equal(result.ok, true, result.error);
  }
  assert.equal(compiles.length, 6, 'each of three groups compiles once per invocation');
  assert.deepEqual(compiles.slice(0, 3).map(item => item.pkg).sort(), ['workass/cmd/workass', 'workass/internal/acp', 'workass/internal/one,workass/internal/chat,workass/internal/appinstall,workass/internal/two'].sort());
  assert.ok(compiles.every(item => item.output === `${cacheDir}${path.sep}`));
  const compileArgs = invocations.filter(args => args[0] === 'test' && args.includes('-c'));
  assert.equal(compileArgs.length, 6);
  assert.ok(compileArgs.every(args => args.includes('-p') && args[args.indexOf('-p') + 1] === '1'));
  for (let i = 0; i < 2; i++) assert.equal(new Set(compileArgs.slice(i * 3, (i + 1) * 3).flatMap(args => args.slice(args.indexOf('-o') + 2))).size, 6);
});

test('three group compiles retain stale no-test artifacts but metadata prevents execution', async t => {
  const f = await fixture(t);
  const cacheDir = path.join(f.root, 'stable-cache');
  await mkdir(cacheDir);
  const stale = path.join(cacheDir, 'one.test');
  await writeFile(stale, 'stale binary');
  const compiles = [], executions = [];
  const fake = inProcessGoSpawn({ noTestPackages: ['workass/internal/one'], record: (command, args) => {
    if (command === f.go && args[0] === 'test' && args.includes('-c')) compiles.push(args.slice(args.indexOf('-o') + 2).join(','));
    if (String(command).endsWith('.test')) executions.push(command);
  } });
  for (let invocation = 0; invocation < 2; invocation++) {
    const result = await runGoSuite({ cwd: f.root, logDir: f.logs, cacheDir, go: f.go, workers: 3, spawn: fake.spawn, signalHandlers: false });
    assert.equal(result.ok, true, result.error);
    assert.equal(result.packageOutcomes.find(item => item.package === 'workass/internal/one')?.noTestFiles, true);
  }
  assert.equal(compiles.length, 6, 'all groups including no-test packages compile on every invocation');
  assert.equal(compiles.filter(group => group.includes('workass/internal/one')).length, 2, 'no-test package remains in the remaining compiler group on each invocation');
  assert.equal(await readFile(stale, 'utf8'), 'stale binary', 'the compiler may leave stale outputs when a package has no test files');
  const staleRunner = path.join(f.root, `package-${createHash('sha256').update('workass/internal/one').digest('hex').slice(0, 16)}-run.test`);
  assert.ok(!executions.includes(staleRunner), 'current no-test metadata must prevent listing or executing any cached binary');
});

test('a group waits for its own compile while ready ACP tests overlap another blocked group compiler', async t => {
  const f = await fixture(t);
  const events = [];
  const fake = inProcessGoSpawn({
    compileDelays: { 'workass/internal/one,workass/internal/chat,workass/internal/appinstall,workass/internal/two': 100 },
    onCompileStart: pkg => events.push(`compile-start:${pkg}`),
    onCompileFinish: pkg => events.push(`compile-finish:${pkg}`),
    onTestBinaryStart: label => events.push(`test-start:${label}`),
  });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, workers: 3, signalHandlers: false, spawn: fake.spawn });
  assert.equal(result.ok, true, result.error);
  const remainingGroup = 'workass/internal/one,workass/internal/chat,workass/internal/appinstall,workass/internal/two';
  const remainingStart = events.indexOf(`compile-start:${remainingGroup}`);
  const remainingFinish = events.indexOf(`compile-finish:${remainingGroup}`);
  const acpStart = events.indexOf('compile-start:workass/internal/acp');
  const acpFinish = events.indexOf('compile-finish:workass/internal/acp');
  const acpTest = events.findIndex(event => event.startsWith('test-start:acp'));
  assert.ok(acpStart >= 0 && acpFinish > acpStart && acpTest > acpFinish, events.join(', '));
  assert.ok(remainingStart >= 0 && remainingFinish > remainingStart && acpTest < remainingFinish, events.join(', '));
});

test('failed group compilation never lists or runs its stale cached binary', async t => {
  const f = await fixture(t);
  const cacheDir = path.join(f.root, 'stable-cache');
  let fail = false;
  const executions = [];
  const fake = inProcessGoSpawn({ failCompile: '', record: (command, args) => {
    if (String(command).endsWith('.test')) executions.push(String(command));
  } });
  const run = () => runGoSuite({ cwd: f.root, logDir: f.logs, cacheDir, go: f.go, workers: 3, spawn: (command, args, options) => {
    if (fail && command === f.go && args[0] === 'test' && args.includes('-c') && args.includes('workass/internal/acp')) {
      const failing = inProcessGoSpawn({ failCompile: 'workass/internal/acp' });
      return failing.spawn(command, args, options);
    }
    return fake.spawn(command, args, options);
  }, signalHandlers: false });
  assert.equal((await run()).ok, true);
  executions.length = 0;
  const stale = path.join(cacheDir, 'acp.test');
  await writeFile(stale, 'stale ACP binary');
  fail = true;
  const result = await run();
  assert.equal(result.ok, false);
  assert.match(result.error, /compile-acp failed with exit 8/);
  assert.equal(await readFile(stale, 'utf8'), 'stale ACP binary');
  assert.ok(!executions.some(command => command.endsWith(`${path.sep}acp.test`)), 'the failed group does not inspect or execute the old artifact');
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
  const binaryInvocations = [];
  const fake = inProcessGoSpawn({ record: (command, args, options) => {
    if (command === f.go) invocations.push(args);
    else if (String(command).endsWith('.test')) binaryInvocations.push({ args, env: options.env });
  } });
  const result = await runGoSuite({ cwd: f.root, logDir: f.logs, go: f.go, race: true, workers: 2, signalHandlers: false, spawn: fake.spawn });
  assert.equal(result.ok, true, result.error);
  const compile = invocations.filter(args => args[0] === 'test' && args.includes('-c'));
  assert.equal(compile.length, 3);
  assert.ok(compile.every(args => args.includes('-race')));
  assert.ok(compile.some(args => args.at(-1) === 'workass/internal/acp'));
  assert.ok(compile.some(args => args.at(-1) === 'workass/cmd/workass'));
  assert.ok(binaryInvocations.length > 0 && binaryInvocations.every(item => item.env.GOMAXPROCS === '1'));
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
    if (args[0] === 'list') { finish(0, ['workass/internal/one', 'workass/internal/acp', 'workass/cmd/workass', 'workass/internal/chat', 'workass/internal/appinstall'].map(pkg => `${pkg}\t${path.join(f.root, pkg.replace('workass/', ''))}\ttrue`).join('\n') + '\n'); return child; }
    if (args[0] === 'test' && args.includes('-c')) {
      const outputDir = args[args.indexOf('-o') + 1];
      for (const pkg of ['workass/internal/one', 'workass/internal/acp', 'workass/cmd/workass', 'workass/internal/chat', 'workass/internal/appinstall', 'workass/internal/two']) writeFileSync(path.join(outputDir, `${path.posix.basename(pkg)}.test`), 'fake test binary');
      finish(0); return child;
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
