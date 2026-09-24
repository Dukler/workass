import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { mkdtemp, mkdir, readFile, rm, writeFile, rename } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { runGoSuite } from '../test-go-suite.mjs';

function fakeSpawn(observed) {
  return (command, args) => {
    const child = new EventEmitter();
    child.stdout = new PassThrough(); child.stderr = new PassThrough();
    child.kill = () => { setImmediate(() => child.emit('close', null, 'SIGTERM')); return true; };
    const finish = (code, stdout = '') => {
      child.stdout.end(stdout); child.stderr.end();
      setImmediate(() => child.emit('close', code, null));
    };
    setImmediate(() => child.emit('spawn'));
    if (args[0] === 'list') { finish(0, 'workass/internal/acp\nworkass/cmd/workass\n'); return child; }
    if (args[0] === 'test' && args.includes('-c')) {
      const output = args[args.indexOf('-o') + 1];
      const staging = `${output}.${process.pid}.${Math.random()}.tmp`;
      observed.compiles.push(output);
      writeFile(staging, 'compiled fake binary').then(() => rename(staging, output)).then(() => finish(0));
      return child;
    }
    if (String(command).endsWith('.test')) {
      observed.executions.push(String(command));
      if (args[0] === '-test.list') { finish(0, 'TestCache\n'); return child; }
      finish(0, '=== RUN TestCache\n--- PASS: TestCache (0.01s)\n');
      return child;
    }
    finish(90);
    return child;
  };
}

test('heavy binaries compile on every invocation and each run uses a pinned per-run artifact', async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-cache-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const cacheDir = path.join(root, 'cache');
  const logs = path.join(root, 'logs');
  await mkdir(logs);
  const observed = { compiles: [], executions: [] };
  const spawn = fakeSpawn(observed);
  const results = await Promise.all([0, 1].map(() => runGoSuite({ cwd: root, logDir: logs, cacheDir, workers: 2, spawn, signalHandlers: false })));
  assert.ok(results.every(result => result.ok), results.map(result => result.error).join('; '));
  assert.equal(observed.compiles.length, 4, 'each heavy package is compiled every run');
  assert.equal(new Set(observed.compiles).size, 2, 'compiled artifact paths are stable per package');
  assert.equal(observed.executions.length, 8, 'each invocation lists and runs both packages');
  assert.equal(new Set(observed.executions).size, 4, 'each run executes its own pinned artifact path');
  for (const output of observed.compiles) assert.equal(await readFile(output, 'utf8'), 'compiled fake binary');
  assert.ok(observed.executions.every(binary => !observed.compiles.includes(binary)));
});
