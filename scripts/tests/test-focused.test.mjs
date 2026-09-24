import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import test from 'node:test';

const runner = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', 'test-focused.mjs');

test('focused checks share one budget and report success', () => {
  const result = spawnSync(process.execPath, [runner, '--', process.execPath, '-e', 'process.exit(0)', ':::', process.execPath, '-e', 'process.exit(0)'], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /WORKASS_FOCUSED_CHECK status=passed .* commands=2/);
});

test('focused checks stop a slow process group', () => {
  const result = spawnSync(process.execPath, [runner, '--timeout-ms', '150', '--', process.execPath, '-e', 'setTimeout(() => {}, 10_000)'], { encoding: 'utf8' });
  assert.equal(result.status, 124, result.stderr);
  assert.match(result.stdout, /WORKASS_FOCUSED_CHECK status=timeout/);
});
