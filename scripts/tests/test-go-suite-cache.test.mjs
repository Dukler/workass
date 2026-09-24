import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { mkdtemp, mkdir, readFile, rm, writeFile, rename } from 'node:fs/promises';
import { statSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { runGoSuite } from '../test-go-suite.mjs';

function fakeSpawn(observed, root) {
  return (command, args) => {
    const child = new EventEmitter();
    child.stdout = new PassThrough(); child.stderr = new PassThrough();
    child.kill = () => { setImmediate(() => child.emit('close', null, 'SIGTERM')); return true; };
    const finish = (code, stdout = '') => {
      child.stdout.end(stdout); child.stderr.end();
      setImmediate(() => child.emit('close', code, null));
    };
    setImmediate(() => child.emit('spawn'));
    if (args[0] === 'list') { finish(0, ['workass/internal/acp', 'workass/cmd/workass', 'workass/internal/cache'].map(pkg => `${pkg}\t${path.join(root, pkg.replace('workass/', ''))}\ttrue`).join('\n') + '\n'); return child; }
    if (args[0] === 'test' && args.includes('-c')) {
      const output = args[args.indexOf('-o') + 1];
      const staging = `${output}.${process.pid}.${Math.random()}.tmp`;
      observed.compiles.push(output);
      writeFile(staging, 'compiled fake binary').then(() => rename(staging, output)).then(() => { observed.compiledInodes.push(statSync(output).ino); finish(0); });
      return child;
    }
    if (String(command).endsWith('.test')) {
      observed.executions.push(String(command));
      if (args[0] !== '-test.list') {
        assert.ok(args.includes('-test.count=1'), 'every test binary run disables Go test result caching');
        observed.executionInodes.push(statSync(command).ino);
      }
      if (args[0] === '-test.list') { finish(0, 'TestCache\n'); return child; }
      finish(0, '=== RUN TestCache\n--- PASS: TestCache (0.01s)\n');
      return child;
    }
    finish(90);
    return child;
  };
}

test('Go binaries compile on every invocation and each run pins its compiled inode', async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'workass-go-suite-cache-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const cacheDir = path.join(root, 'cache');
  const logs = path.join(root, 'logs');
  await mkdir(logs);
  const observed = { compiles: [], executions: [], executionInodes: [], compiledInodes: [] };
  const spawn = fakeSpawn(observed, root);
  const results = [];
  for (let i = 0; i < 2; i++) results.push(await runGoSuite({ cwd: root, logDir: logs, cacheDir, workers: 2, spawn, signalHandlers: false }));
  assert.ok(results.every(result => result.ok), results.map(result => result.error).join('; '));
  assert.equal(observed.compiles.length, 6, 'all test packages are compiled every run');
  assert.equal(new Set(observed.compiles).size, 3, 'compiled artifact paths are stable per package');
  assert.equal(observed.executions.length, 12, 'each invocation lists and runs all three packages');
  assert.equal(new Set(observed.executions).size, 6, 'each run executes its own pinned artifact path');
  for (const output of observed.compiles) assert.equal(await readFile(output, 'utf8'), 'compiled fake binary');
  const compiledInodes = new Set(observed.compiledInodes);
  assert.ok(observed.executionInodes.every(inode => compiledInodes.has(inode)), 'run hard links pin a compiler-produced inode');
  assert.equal(observed.executionInodes.length, 6, 'each test binary executes once per invocation');
});
