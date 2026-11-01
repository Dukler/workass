import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { formatReport, fullSuiteCommands, runSuiteLifecycle } from '../test-suite.mjs';
import { failedNodeTests, MAX_RERUNS, oneLineCause } from '../test-rerun.mjs';
import { failureLines, flakeLines } from '../release/lib/failure-summary.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const node = process.execPath;
const nativeSuite = path.resolve(here, '../test-native-host-suite.mjs');

function temp(t) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'workass-rerun-test-')));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function run(t, specs, options = {}) {
  const dir = temp(t);
  return runSuiteLifecycle({
    commandSpecs: () => specs,
    createFixture: async () => ({ root: dir, cleanup: async () => {} }),
    rerunFailed: true,
    signalTarget: new EventEmitter(),
    ...options,
  }).then(result => {
    t.after(() => fs.rmSync(result.report.logDir, { recursive: true, force: true }));
    return result.report;
  });
}

function nodeSuite(name, file) {
  return { name, command: node, args: ['--test', '--test-concurrency=4', file], cwd: path.dirname(file), requireTestReport: true, nodeEvents: true };
}

// A test file that records each run of each test, and fails on the attempts named.
function testFile(dir, body) {
  const file = path.join(dir, 'fixture.test.mjs');
  const runs = path.join(dir, 'runs.log');
  fs.writeFileSync(file, `import test from 'node:test';
import fs from 'node:fs';
const runs = ${JSON.stringify(runs)};
const attempt = name => { fs.appendFileSync(runs, name + '\\n'); return fs.readFileSync(runs, 'utf8').split('\\n').filter(line => line === name).length; };
${body}
`);
  return { file, runCount: name => fs.readFileSync(runs, 'utf8').split('\n').filter(line => line === name).length };
}

// Stands in for scripts/test-go-suite.mjs: reports failed cases in the real
// summary format and points them at a stub test binary.
function goStub(dir, { names, binaryBody, unattributed = [] }) {
  const binary = path.join(dir, 'pkg.test');
  fs.writeFileSync(binary, `#!/bin/sh\n${binaryBody}\n`, { mode: 0o755 });
  const summaryPath = path.join(dir, 'summary.json');
  fs.writeFileSync(summaryPath, JSON.stringify({
    ok: false,
    failedCases: names.map(name => ({ package: 'workass/internal/pkg', name, binary, cwd: dir, first: 'failed', cause: 'pkg_test.go:9: load made this slow' })),
    unattributedFailures: unattributed,
    agentFixture: null,
  }));
  const report = JSON.stringify({ ok: false, discovered: names.length, tests: names.length, passed: 0, failed: names.length, skipped: 0, otherGo: { tests: 0, passed: 0, failed: 0, skipped: 0 }, summaryPath }, null, 2);
  return { name: 'go_stub', command: node, args: ['-e', `console.log(${JSON.stringify(report)}); process.exitCode = 1`], cwd: dir, requireTestReport: true, requireGoReport: true };
}

const goPassing = name => `echo "=== RUN   ${name}"; echo "--- PASS: ${name} (0.00s)"; echo PASS; exit 0`;

test('a failed test that passes alone does not fail the gate and is named as a flake', async t => {
  const dir = temp(t);
  const { file, runCount } = testFile(dir, `
test('stable', () => { attempt('stable'); });
test('flaky once', () => { if (attempt('flaky once') === 1) throw new Error('timed out under load'); });
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)]);
  assert.equal(report.correctness, true);
  assert.deepEqual(report.flakes.map(flake => flake.name), ['flaky once']);
  assert.equal(runCount('flaky once'), 2);
  assert.equal(runCount('stable'), 1, 'only the failed test runs again');
  const { errors, lines } = formatReport(report);
  assert.deepEqual(errors, []);
  assert.ok(lines.some(line => line.startsWith('WORKASS_TEST_FLAKE name="flaky once" suite=fixture_suite') && line.includes('timed out under load')));
  const suiteLine = lines.find(line => line.startsWith('WORKASS_TEST_SUITE name=fixture_suite'));
  assert.match(suiteLine, /status=flaky/);
  assert.match(suiteLine, /exit=1/, 'the first run stays on the record');
  assert.match(suiteLine, /failed=1/);
  assert.match(lines.at(-1), /^WORKASS_TEST_SUITE_TOTAL correctness=passed .* reruns=1 flakes=1 /);
});

test('a test that fails twice fails the gate and leads the report', async t => {
  const dir = temp(t);
  const { file, runCount } = testFile(dir, `
test('stable', () => { attempt('stable'); });
test('really broken', () => { attempt('really broken'); throw new Error('wrong answer\\nsecond line'); });
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)]);
  assert.equal(report.correctness, false);
  assert.deepEqual(report.flakes, []);
  assert.equal(runCount('really broken'), 2);
  const { errors, lines } = formatReport(report);
  assert.match(errors[0], /^WORKASS_TEST_FAILED name="really broken" suite=fixture_suite .*rerun=failed_again cause="wrong answer" log=/);
  assert.doesNotMatch(errors.join('\n'), /second line/);
  assert.match(lines.at(-1), /correctness=failed/);
});

test('one flake and one real failure in a suite still fail it', async t => {
  const dir = temp(t);
  const { file } = testFile(dir, `
test('flaky once', () => { if (attempt('flaky once') === 1) throw new Error('slow'); });
test('really broken', () => { attempt('really broken'); throw new Error('wrong'); });
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)]);
  assert.equal(report.correctness, false);
  assert.deepEqual(report.flakes.map(flake => flake.name), ['flaky once']);
  assert.deepEqual(report.failedTests.map(failure => failure.name), ['really broken']);
});

test('a plain run reruns nothing and says so', async t => {
  const dir = temp(t);
  const { file, runCount } = testFile(dir, `
test('flaky once', () => { if (attempt('flaky once') === 1) throw new Error('slow'); });
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)], { rerunFailed: false });
  assert.equal(report.correctness, false);
  assert.equal(runCount('flaky once'), 1);
  const { errors, lines } = formatReport(report);
  assert.match(errors[0], /^WORKASS_TEST_FAILED name="flaky once" .*rerun=off cause=slow/);
  assert.match(lines.at(-1), / reruns=off /);
});

test('a rerun that runs no test is not a pass', async t => {
  const dir = temp(t);
  // The first run fails under one name; on the second that name no longer
  // exists, so the pattern matches nothing and node:test exits 0.
  const { file } = testFile(dir, `
const first = attempt('marker') === 1;
test(first ? 'old name' : 'new name', () => { if (first) throw new Error('slow'); });
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)]);
  assert.equal(report.correctness, false);
  assert.match(report.failedTests[0].cause, /did not report this test as passed/);
});

test('a file that crashes outside any test is rerun whole and must report a real pass', async t => {
  const dir = temp(t);
  const { file } = testFile(dir, `
if (attempt('load') === 1) throw new Error('could not load under pressure');
test('in file', () => {});
`);
  const report = await run(t, [nodeSuite('fixture_suite', file)]);
  assert.equal(report.correctness, true);
  assert.deepEqual(report.flakes.map(flake => flake.name), ['fixture.test.mjs']);
});

test('a suite that dies with no named test is not rerun', async t => {
  const dir = temp(t);
  const marker = path.join(dir, 'ran');
  const report = await run(t, [{ name: 'no_names', command: node, args: ['-e', `require('node:fs').appendFileSync(${JSON.stringify(marker)}, 'x'); process.exitCode = 3`], cwd: dir }]);
  assert.equal(report.correctness, false);
  assert.equal(fs.readFileSync(marker, 'utf8'), 'x');
  assert.deepEqual(report.reruns, []);
  assert.match(formatReport(report).errors[0], /^WORKASS_TEST_SUITE_FAILURE name=no_names exit=3 /);
});

test('a Go case that passes alone is a flake, proven by its own terminal line', async t => {
  const dir = temp(t);
  const report = await run(t, [goStub(dir, { names: ['TestFlaky'], binaryBody: goPassing('TestFlaky') })]);
  assert.equal(report.correctness, true);
  assert.match(formatReport(report).lines[0], /^WORKASS_TEST_FLAKE name=TestFlaky suite=go_stub where=workass\/internal\/pkg first=failed /);
});

test('a Go case that fails twice fails, and its second cause is the one reported', async t => {
  const dir = temp(t);
  const report = await run(t, [goStub(dir, { names: ['TestBroken'], binaryBody: 'echo "=== RUN   TestBroken"; echo "    pkg_test.go:12: still wrong"; echo "--- FAIL: TestBroken (0.00s)"; exit 1' })]);
  assert.equal(report.correctness, false);
  assert.match(formatReport(report).errors[0], /^WORKASS_TEST_FAILED name=TestBroken .*rerun=failed_again cause="pkg_test\.go:12: still wrong"/);
});

test('a Go exit 0 with no terminal line, or a skip, does not clear a failure', async t => {
  for (const binaryBody of ['echo PASS; exit 0', 'echo "=== RUN   TestGone"; echo "--- SKIP: TestGone (0.00s)"; exit 0']) {
    const dir = temp(t);
    const report = await run(t, [goStub(dir, { names: ['TestGone'], binaryBody })]);
    assert.equal(report.correctness, false, binaryBody);
    assert.deepEqual(report.flakes, []);
  }
});

test('a Go failure no test name accounts for fails without running anything again', async t => {
  const dir = temp(t);
  const marker = path.join(dir, 'binary-ran');
  const report = await run(t, [goStub(dir, { names: ['TestFlaky'], binaryBody: `touch ${marker}; ${goPassing('TestFlaky')}`, unattributed: ['compile-acp failed with exit 1'] })]);
  assert.equal(report.correctness, false);
  assert.equal(fs.existsSync(marker), false);
  assert.match(formatReport(report).errors.join('\n'), /unattributed="compile-acp failed with exit 1"/);
});

test('more failures than the limit are a broken tree: no reruns, and they say why', async t => {
  const dir = temp(t);
  const marker = path.join(dir, 'binary-ran');
  const names = Array.from({ length: MAX_RERUNS + 1 }, (_, index) => `TestN${index}`);
  const report = await run(t, [goStub(dir, { names, binaryBody: `touch ${marker}; exit 0` })]);
  assert.equal(report.correctness, false);
  assert.equal(fs.existsSync(marker), false);
  const { errors } = formatReport(report);
  assert.equal(errors.filter(line => line.startsWith('WORKASS_TEST_FAILED ')).length, names.length);
  assert.ok(errors.includes(`WORKASS_TEST_RERUN_SKIPPED reason=too_many_failures count=${names.length} limit=${MAX_RERUNS}`));
});

test('a shell contract reruns as the whole contract', async t => {
  const dir = temp(t);
  const script = path.join(dir, 'contract.test.sh');
  fs.writeFileSync(script, `#!/bin/sh\necho x >> ${path.join(dir, 'attempts')}\n[ "$(wc -l < ${path.join(dir, 'attempts')})" -gt 1 ] || { echo "contract broke under load"; exit 1; }\n`);
  const report = await run(t, [{ name: 'script_contract_fixture', command: 'sh', args: [script], cwd: dir, rerunWhole: true }]);
  assert.equal(report.correctness, true);
  assert.equal(report.flakes[0].name, 'script_contract_fixture');
});

test('an interrupt during a rerun stops it and fails the run', async t => {
  const dir = temp(t);
  const signals = new EventEmitter();
  const started = path.join(dir, 'rerun-started');
  const pending = run(t, [goStub(dir, { names: ['TestFlaky'], binaryBody: `touch ${started}; exec sleep 30` })], { signalTarget: signals });
  while (!fs.existsSync(started)) await new Promise(resolve => setTimeout(resolve, 10));
  signals.emit('SIGTERM');
  const report = await pending;
  assert.equal(report.correctness, false);
});

test('only the top-level failure is a rerun target and it keeps the deepest cause', () => {
  const file = '/repo/a.test.mjs';
  assert.deepEqual(failedNodeTests([
    { outcome: 'fail', name: 'child', nesting: 1, file, cause: 'the real reason' },
    { outcome: 'fail', name: 'parent', nesting: 0, file, cause: '1 subtest failed' },
    { outcome: 'fail', name: 'wanted later', nesting: 0, file, todo: true, cause: 'todo' },
    { outcome: 'pass', name: 'fine', nesting: 0, file },
    { outcome: 'fail', name: 'a.test.mjs', nesting: 0, file, cause: 'test failed' },
  ]), [
    { name: 'parent', file, wholeFile: false, cause: 'the real reason' },
    { name: 'a.test.mjs', file, wholeFile: true, cause: 'test failed' },
  ]);
});

test('causes are one bounded line with secret-shaped values removed', () => {
  assert.equal(oneLineCause('\n  first line\nsecond'), 'first line');
  assert.equal(oneLineCause('bad token=abc123 here'), 'bad token=[REDACTED] here');
  assert.equal(oneLineCause('Authorization: Bearer abc123'), 'Authorization: Bearer [REDACTED]');
  assert.equal(oneLineCause(undefined), 'no detail');
  assert.equal(oneLineCause('x'.repeat(500)).length, 200);
});

test('the native host runner honors the rerun pattern and writes test events', t => {
  const dir = temp(t);
  const file = path.join(dir, 'host.test.mjs');
  fs.writeFileSync(file, "import test from 'node:test';\ntest('wanted', () => {});\ntest('other', () => { throw new Error('must not run'); });\n");
  const events = path.join(dir, 'events.jsonl');
  const { NODE_TEST_CONTEXT: _outerRunner, ...env } = process.env;
  const result = spawnSync(node, ['--test-name-pattern=^wanted$', nativeSuite, file], { encoding: 'utf8', env: { ...env, WORKASS_TEST_EVENTS_FILE: events, WORKASS_TEST_CONCURRENCY: '1' } });
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
  const lines = fs.readFileSync(events, 'utf8').trim().split('\n').map(line => JSON.parse(line));
  assert.ok(lines.some(line => line.outcome === 'pass' && line.name === 'wanted' && line.nesting === 0));
  assert.ok(!lines.some(line => line.name === 'other'));
});

test('every test command of the real matrix can name its failures', () => {
  for (const spec of fullSuiteCommands()) {
    assert.ok(spec.nodeEvents || spec.nodeEventsEnv || spec.requireGoReport || spec.rerunWhole, spec.name);
  }
});

test('the release summary follows nested logs and prints only what failed', t => {
  const dir = temp(t);
  const inner = path.join(dir, 'repository_gate.log');
  const outer = path.join(dir, 'prepare.log');
  const ship = path.join(dir, 'ship.log');
  fs.writeFileSync(inner, [
    'WORKASS_TEST_FLAKE name=TestSlow suite=go_tests where=workass/internal/acp first=failed rerun_seconds=1.000 cause=slow log=/l',
    'WORKASS_TEST_FAILED name=TestBroken suite=go_tests where=workass/internal/acp rerun=failed_again cause="wrong" log=/l',
    'WORKASS_TEST_SUITE_FAILURE name=go_tests exit=1 signal=none log=/l',
    'WORKASS_TEST_SUITE name=shell_tests status=passed exit=0 seconds=1.000 log=/l',
    'WORKASS_TEST_SUITE_TOTAL correctness=failed performance=over_budget',
  ].join('\n'));
  fs.writeFileSync(outer, `WORKASS_RELEASE_PHASE_START name=repository_gate epoch=1\nrelease phase failed: repository_gate (full output: ${inner})\n`);
  fs.writeFileSync(ship, `WORKASS_RELEASE_PHASE_LOG name=prepare log=${outer}\nrelease phase failed: prepare (full output: ${outer})\nverbose passing line\n`);
  assert.deepEqual(failureLines(ship), [
    `release phase failed: prepare (full output: ${outer})`,
    `release phase failed: repository_gate (full output: ${inner})`,
    'WORKASS_TEST_FAILED name=TestBroken suite=go_tests where=workass/internal/acp rerun=failed_again cause="wrong" log=/l',
    'WORKASS_TEST_FLAKE name=TestSlow suite=go_tests where=workass/internal/acp first=failed rerun_seconds=1.000 cause=slow log=/l',
  ]);
  assert.deepEqual(flakeLines(ship), ['WORKASS_TEST_FLAKE name=TestSlow suite=go_tests where=workass/internal/acp first=failed rerun_seconds=1.000 cause=slow log=/l']);
});

test('a release failure that is not a test shows the last meaningful lines of the deepest log', t => {
  const dir = temp(t);
  const inner = path.join(dir, 'stage.log');
  const ship = path.join(dir, 'ship.log');
  fs.writeFileSync(inner, ['WORKASS_GATE_PHASE name=go_build status=passed seconds=1', 'codesign: identity not found', ''].join('\n'));
  fs.writeFileSync(ship, `release phase failed: stage (full output: ${inner})\n`);
  assert.deepEqual(failureLines(ship), [`release phase failed: stage (full output: ${inner})`, 'codesign: identity not found']);
});
