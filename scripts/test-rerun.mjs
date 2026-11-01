// Names what failed in a test matrix and, for the repository gate only, runs
// each failed test once more by itself after the whole matrix has finished.
//
// The rules the gate relies on:
// - A test passes its rerun only on positive proof: the exact test reported a
//   pass. Exit 0 alone proves nothing, a name pattern can match no test at all.
// - A failure no test name accounts for (a compile error, a killed runner, a
//   crash with no report) is never rerun and always fails.
// - A test that passes its rerun is reported as a flake, it never disappears.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { fileURLToPath } from 'node:url';
import { anchoredTestPattern, goFailureCause, inspectCaseOutput } from './test-go-suite.mjs';

const reporterUrl = new URL('./test-events-reporter.mjs', import.meta.url).href;
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// More failures than this is a broken tree or a broken machine, not flakes.
export const MAX_RERUNS = 25;
export const RERUN_TIMEOUT_MS = 5 * 60_000;

export function nodeEventReporterArgs(eventsPath) {
  return ['--test-reporter=spec', '--test-reporter-destination=stdout', `--test-reporter=${reporterUrl}`, `--test-reporter-destination=${eventsPath}`];
}

// Causes reach the terminal and release logs, so secret-shaped values go first.
export function oneLineCause(text) {
  const line = String(text ?? '').replace(/\u001b\[[0-9;]*m/g, '').split(/\r?\n/).map(item => item.trim()).find(Boolean) ?? 'no detail';
  return line.replace(/\b(api[_-]?key|token|secret|password|credential|bearer)\b(\s*[:=]?\s*)\S+/gi, '$1$2[REDACTED]').slice(0, 200);
}

export function field(key, value) {
  const text = String(value);
  return /^[^\s"=]+$/.test(text) ? `${key}=${text}` : `${key}=${JSON.stringify(text)}`;
}

function readEvents(eventsPath) {
  try {
    return fs.readFileSync(eventsPath, 'utf8').split('\n').flatMap(line => {
      try { return line ? [JSON.parse(line)] : []; } catch { return []; }
    });
  } catch { return []; }
}

// A file that crashed outside any test reports under its own path, as it was
// spelled on the command line.
function isFileEvent(event) {
  return Boolean(event.file) && (event.name === event.file || event.file.endsWith(`${path.sep}${event.name}`));
}

// The failing top-level tests of a node:test run, with the deepest cause known.
export function failedNodeTests(events) {
  const tests = [];
  const seen = new Set();
  const leafCause = new Map();
  for (const event of events) {
    if (event.outcome !== 'fail' || event.todo) continue;
    if (event.nesting > 0) {
      if (!leafCause.has(event.file) && event.failureType !== 'cancelledByParent') leafCause.set(event.file, event.cause);
      continue;
    }
    const cause = leafCause.get(event.file) ?? event.cause;
    leafCause.delete(event.file);
    const key = `${event.file}\0${event.name}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const wholeFile = isFileEvent(event);
    tests.push({ name: wholeFile ? path.basename(event.file) : event.name, file: event.file, wholeFile, cause });
  }
  return tests;
}

function goSummaryFromLog(logPath) {
  try {
    const raw = fs.readFileSync(logPath, 'utf8').match(/"summaryPath":\s*("(?:[^"\\]|\\.)*")/)?.[1];
    return raw ? JSON.parse(fs.readFileSync(JSON.parse(raw), 'utf8')) : null;
  } catch { return null; }
}

function suiteFailed(result) {
  return result.code !== 0 || Boolean(result.spawnError || result.sinkError || result.reportError);
}

function relative(file) {
  return file && path.isAbsolute(file) ? path.relative(repoRoot, file) : file;
}

function nodeRerunRoute(spec, test, eventsPath) {
  const pattern = test.wholeFile ? null : anchoredTestPattern(test.name);
  if (spec.nodeEventsEnv) {
    return {
      // With isolation 'none' node:test reads the pattern from the process's own
      // flags; run()'s testNamePatterns option does not reach in-process files.
      command: spec.command, args: [...(pattern ? [`--test-name-pattern=${pattern}`] : []), ...spec.args], cwd: spec.cwd, eventsPath,
      env: { WORKASS_TEST_EVENTS_FILE: eventsPath, WORKASS_TEST_CONCURRENCY: '1' },
    };
  }
  const flags = spec.args.filter(arg => arg.startsWith('-')).map(arg => arg.startsWith('--test-concurrency=') ? '--test-concurrency=1' : arg);
  return {
    command: spec.command,
    args: [...nodeEventReporterArgs(eventsPath), ...flags, ...(pattern ? [`--test-name-pattern=${pattern}`] : []), test.file],
    cwd: spec.cwd, eventsPath, env: {},
  };
}

// What failed in one suite, by test name. `unattributed` is why the suite fails
// no matter how its named tests behave on a rerun.
export function namedFailures(result, spec) {
  const log = result.logPath;
  const base = { suite: result.name, log };
  if (result.spawnError || result.sinkError || result.signal) {
    return { tests: [], unattributed: result.spawnError ?? result.sinkError ?? `killed by ${result.signal}` };
  }
  if (spec?.nodeEvents || spec?.nodeEventsEnv) {
    const tests = failedNodeTests(readEvents(result.eventsPath));
    const unnamed = tests.find(test => !test.file);
    if (!tests.length || unnamed || !Number.isInteger(result.tests)) {
      return { tests: tests.map(test => ({ ...base, ...test, where: relative(test.file), first: 'failed' })), unattributed: Number.isInteger(result.tests) ? 'failure without a rerunnable test name' : 'the test runner did not finish its report' };
    }
    return { tests: tests.map(test => ({ ...base, ...test, where: relative(test.file), first: 'failed', kind: 'node', spec })), unattributed: null };
  }
  if (spec?.requireGoReport) {
    const summary = goSummaryFromLog(log);
    if (!summary) return { tests: [], unattributed: 'no Go matrix summary' };
    if (summary.interrupted) return { tests: [], unattributed: `interrupted by ${summary.interrupted}` };
    const tests = (summary.failedCases ?? []).map(item => ({ ...base, name: item.name, where: item.package, first: item.first, cause: item.cause, kind: 'go', go: item, agentFixture: summary.agentFixture, spec }));
    const reasons = summary.unattributedFailures ?? [];
    return { tests, unattributed: reasons.length ? reasons.join('; ') : tests.length ? null : (summary.error ?? 'Go matrix failed without a named test') };
  }
  if (spec?.rerunWhole) {
    return { tests: [{ ...base, name: result.name, where: relative(spec.args?.[0]), first: 'failed', cause: result.failureDetail, kind: 'command', spec }], unattributed: null };
  }
  return { tests: [], unattributed: result.reportError ?? `exit ${result.code}` };
}

function routeFor(test, tempDir, logDir, index) {
  if (test.kind === 'node') return nodeRerunRoute(test.spec, test, path.join(logDir, `rerun-${index}.events.jsonl`));
  if (test.kind === 'go') {
    return {
      command: test.go.binary, cwd: test.go.cwd,
      args: ['-test.v', `-test.run=${anchoredTestPattern(test.name)}`, '-test.count=1', '-test.parallel=2'],
      env: { GOMAXPROCS: '1', ...(test.agentFixture ? { WORKASS_TEST_AGENT_BINARY: test.agentFixture } : {}) },
    };
  }
  return { command: test.spec.command, args: test.spec.args ?? [], cwd: test.spec.cwd, env: {} };
}

function runAlone(route, { logPath, tempDir, timeoutMs, abortSignal, spawnCommand }) {
  return new Promise(resolve => {
    let output = '';
    let timedOut = false;
    const log = fs.createWriteStream(logPath);
    const env = { ...process.env, TMPDIR: tempDir, TMP: tempDir, TEMP: tempDir, ...route.env };
    delete env.NODE_TEST_CONTEXT; // left by an outer `node --test`, see runSuiteMatrix
    let child;
    try {
      child = spawnCommand(route.command, route.args, { cwd: route.cwd, env, stdio: ['ignore', 'pipe', 'pipe'], detached: process.platform !== 'win32' });
    } catch (error) {
      log.end(() => resolve({ code: 127, signal: null, output: error.message, timedOut: false }));
      return;
    }
    const signalGroup = signal => {
      try { if (child.pid && process.platform !== 'win32') process.kill(-child.pid, signal); else child.kill(signal); } catch {}
    };
    let killTimer;
    const stop = () => { signalGroup('SIGTERM'); killTimer = setTimeout(() => signalGroup('SIGKILL'), 2000); killTimer.unref?.(); };
    const deadline = setTimeout(() => { timedOut = true; stop(); }, timeoutMs);
    abortSignal?.addEventListener('abort', stop, { once: true });
    const append = chunk => { output += chunk.toString(); log.write(chunk); };
    child.stdout.on('data', append);
    child.stderr.on('data', append);
    child.on('error', error => { output += `\n${error.message}`; });
    child.on('close', (code, signal) => {
      clearTimeout(deadline);
      clearTimeout(killTimer);
      abortSignal?.removeEventListener('abort', stop);
      log.end(() => resolve({ code: code ?? 127, signal, output, timedOut }));
    });
  });
}

function judgeRerun(test, route, run) {
  if (run.timedOut) return { passed: false, cause: `rerun timed out after ${Math.round(RERUN_TIMEOUT_MS / 1000)}s` };
  if (run.signal) return { passed: false, cause: `killed by ${run.signal}` };
  if (test.kind === 'node') {
    const events = readEvents(route.eventsPath);
    const failure = events.find(event => event.outcome === 'fail' && !event.todo);
    if (failure) return { passed: false, cause: failedNodeTests(events)[0]?.cause ?? failure.cause };
    const proven = test.wholeFile
      ? events.some(event => event.outcome === 'pass' && !event.skip && !isFileEvent(event))
      : events.some(event => event.outcome === 'pass' && !event.skip && event.nesting === 0 && event.name === test.name);
    if (run.code !== 0) return { passed: false, cause: `exit ${run.code}` };
    return proven ? { passed: true } : { passed: false, cause: 'the rerun did not report this test as passed' };
  }
  if (test.kind === 'go') {
    const inspected = inspectCaseOutput(test.name, run.code, run.output);
    if (run.code === 0 && !inspected.coverageError && inspected.rootPasses === 1) return { passed: true };
    return { passed: false, cause: inspected.rootSkips === 1 ? 'skipped on rerun, so not proven' : goFailureCause(test.name, run.code, run.signal, run.output) };
  }
  return run.code === 0 ? { passed: true } : { passed: false, cause: run.output.trim().split('\n').at(-1) || `exit ${run.code}` };
}

// Adds failedTests/flakes to the report and, when asked, reruns. A suite counts
// as passing only if every failure in it was named and every named test passed
// its rerun by itself.
export async function settleFailures(report, commands, { rerun = false, tempRoot, abortSignal, spawnCommand = spawn, timeoutMs = RERUN_TIMEOUT_MS } = {}) {
  const specs = new Map(commands.map(spec => [spec.name, spec]));
  const failedSuites = report.results.filter(suiteFailed).map(result => ({ result, ...namedFailures(result, specs.get(result.name)) }));
  report.rerun = rerun;
  report.reruns = [];
  report.flakes = [];
  report.failedTests = [];
  report.rerunSeconds = 0;
  // A suite with an unnamed failure fails whatever its named tests do next.
  const candidates = failedSuites.filter(item => !item.unattributed).flatMap(item => item.tests);
  const tooMany = rerun && candidates.length > MAX_RERUNS;
  if (tooMany) report.rerunSkipped = { reason: 'too_many_failures', count: candidates.length, limit: MAX_RERUNS };
  const started = performance.now();
  const logDir = report.logDir;
  const rerunRoot = tempRoot ?? logDir;
  let index = 0;
  for (const test of rerun && !tooMany ? candidates : []) {
    if (abortSignal?.aborted) break;
    index += 1;
    const tempDir = fs.mkdtempSync(path.join(rerunRoot, 'rerun-'));
    const route = routeFor(test, tempDir, logDir, index);
    const logPath = path.join(logDir, `rerun-${index}.log`);
    const testStarted = performance.now();
    let run;
    try {
      run = await runAlone(route, { logPath, tempDir, timeoutMs, abortSignal, spawnCommand });
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
    const verdict = judgeRerun(test, route, run);
    const seconds = (performance.now() - testStarted) / 1000;
    test.rerun = { ...verdict, seconds, log: logPath };
    report.reruns.push({ suite: test.suite, name: test.name, where: test.where, status: verdict.passed ? 'passed' : 'failed', seconds, log: logPath });
    if (verdict.passed) report.flakes.push({ suite: test.suite, name: test.name, where: test.where, first: test.first, cause: test.cause, seconds, log: logPath });
  }
  report.rerunSeconds = (performance.now() - started) / 1000;
  for (const { result, tests, unattributed } of failedSuites) {
    result.unattributed = unattributed;
    result.flaky = !unattributed && tests.length > 0 && tests.every(test => test.rerun?.passed);
    for (const test of tests) {
      if (test.rerun?.passed) continue;
      report.failedTests.push({ suite: test.suite, name: test.name, where: test.where, first: test.first, cause: test.rerun?.cause ?? test.cause, rerun: test.rerun ? 'failed_again' : !rerun ? 'off' : tooMany ? 'skipped_too_many' : 'not_attempted', log: test.rerun?.log ?? test.log });
    }
  }
  if (rerun && report.flakes.length && !abortSignal?.aborted && !report.interrupted) {
    report.correctness = report.results.every(result => !suiteFailed(result) || result.flaky);
  }
  return report;
}

export function testFailedLine(failure) {
  return ['WORKASS_TEST_FAILED', field('name', failure.name), field('suite', failure.suite), field('where', failure.where ?? '-'), `rerun=${failure.rerun}`, field('cause', oneLineCause(failure.cause)), field('log', failure.log)].join(' ');
}

export function testFlakeLine(flake) {
  return ['WORKASS_TEST_FLAKE', field('name', flake.name), field('suite', flake.suite), field('where', flake.where ?? '-'), `first=${flake.first}`, `rerun_seconds=${flake.seconds.toFixed(3)}`, field('cause', oneLineCause(flake.cause)), field('log', flake.log)].join(' ');
}
