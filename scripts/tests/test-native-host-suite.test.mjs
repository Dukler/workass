import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const helper = path.join(root, 'scripts/test-native-host-suite.mjs');

function runHelper(file) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [helper, file], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    child.stdout.setEncoding('utf8').on('data', chunk => { stdout += chunk; });
    child.stderr.setEncoding('utf8').on('data', chunk => { stderr += chunk; });
    child.once('error', reject);
    child.once('close', (code, signal) => resolve({ code, signal, stdout, stderr }));
  });
}

test('programmatic native-host runner overlaps root cases with a four-case cap', async t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-native-host-runner-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const fixture = path.join(dir, 'overlap.test.mjs');
  fs.writeFileSync(fixture, `
import test from 'node:test';
import assert from 'node:assert/strict';
let active = 0;
let maximum = 0;
let arrivals = 0;
let release;
const barrier = new Promise(resolve => { release = resolve; });
for (let index = 0; index < 8; index++) {
  test('root ' + index, async () => {
    active++;
    maximum = Math.max(maximum, active);
    arrivals++;
    if (arrivals === 4) release();
    try {
      await barrier;
      assert.equal(maximum, 4);
      assert.ok(maximum <= 4);
    } finally {
      active--;
    }
  });
}
`);
  const result = await runHelper(fixture);
  assert.equal(result.code, 0, `${result.stderr}\n${result.stdout}`);
  assert.match(result.stdout, /tests 8/);
  assert.match(result.stdout, /pass 8/);
});

test('programmatic native-host runner exits nonzero on a test failure', async t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-native-host-runner-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const fixture = path.join(dir, 'failure.test.mjs');
  fs.writeFileSync(fixture, `import test from 'node:test'; test('intentional failure', () => { throw new Error('fixture failure'); });\n`);
  const result = await runHelper(fixture);
  assert.equal(result.code, 1, `${result.stderr}\n${result.stdout}`);
  assert.match(result.stdout, /fail 1/);
});
