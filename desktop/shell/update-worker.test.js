'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { runTransaction, validateTransaction } = require('./update-worker');

function transactionFixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-worker-'));
  const parent = path.join(root, 'Applications');
  fs.mkdirSync(parent, { recursive: true });
  return {
    schemaVersion: 1,
    updateId: 'update-fixture-1234',
    platform: 'darwin',
    currentVersion: '1.0.0',
    targetVersion: '1.1.0',
    shellPID: 44,
    installTarget: path.join(parent, 'Workass.app'),
    incomingTarget: path.join(parent, '.Workass.app.incoming-update-fixture-1234'),
    backupTarget: path.join(parent, '.Workass.app.previous-update-fixture-1234'),
    receiptPath: path.join(root, 'state', 'receipt.json'),
    daemonHealthURL: 'https://127.0.0.1:8788/workass/health',
    shellStatusURL: 'http://127.0.0.1:8798/__workass-shell/status',
    designatedRequirement: 'identifier "com.workass.app" and certificate root = H"abc"',
  };
}

function operations(overrides = {}) {
  const calls = [];
  return {
    calls,
    waitUntil: async (predicate) => predicate(),
    shellExited: async () => true,
    stopDaemonService: async () => { calls.push('stop-service'); },
    daemonDown: async () => true,
    verifyIncoming: async () => { calls.push('verify'); },
    activate: async () => { calls.push('activate'); },
    launchInstalled: async () => { calls.push('launch'); },
    healthy: async (version) => { calls.push(`healthy:${version}`); return true; },
    stopLaunched: async () => { calls.push('stop-launched'); },
    rollback: async () => { calls.push('rollback'); },
    cleanup: async () => { calls.push('cleanup'); },
    cleanupFailed: async () => { calls.push('cleanup-failed'); },
    ...overrides,
  };
}

test('transaction validation requires one same-parent atomic swap', () => {
  const tx = transactionFixture();
  assert.equal(validateTransaction(tx), tx);
  assert.throws(() => validateTransaction({ ...tx, backupTarget: path.join(path.dirname(path.dirname(tx.backupTarget)), 'elsewhere', 'old') }), /one parent/);
  assert.throws(() => validateTransaction({ ...tx, designatedRequirement: '' }), /invalid macOS/);
});

test('healthy activation deletes the backup only after all runtime gates pass', async () => {
  const tx = transactionFixture();
  const ops = operations();
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.activated, true);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'activate', 'launch', 'healthy:1.1.0', 'cleanup',
  ]);
  assert.equal(JSON.parse(fs.readFileSync(tx.receiptPath, 'utf8')).phase, 'healthy');
});

test('failed new runtime is rolled back and the old version must pass the same gates', async () => {
  const tx = transactionFixture();
  const ops = operations({
    healthy: async (version) => {
      ops.calls.push(`healthy:${version}`);
      return version === tx.currentVersion;
    },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'activate', 'launch', 'healthy:1.1.0',
    'stop-launched', 'rollback', 'launch', 'healthy:1.0.0', 'cleanup-failed',
  ]);
});

test('daemon stop failure relaunches the untouched app instead of leaving Workass closed', async () => {
  const tx = transactionFixture();
  const ops = operations({
    daemonDown: async () => false,
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.activated, false);
  assert.equal(receipt.rolledBack, false);
  assert.deepEqual(ops.calls, ['stop-service', 'launch', 'healthy:1.0.0']);
});

test('signature failure relaunches the untouched previous release without a bogus rollback swap', async () => {
  const tx = transactionFixture();
  const ops = operations({
    verifyIncoming: async () => { ops.calls.push('verify'); throw new Error('bad signature'); },
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.activated, false);
  assert.equal(receipt.rolledBack, false);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'stop-launched', 'launch', 'healthy:1.0.0', 'cleanup-failed',
  ]);
});

test('forced post-swap health failure restores the previous bytes on disk', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'bad-new-release');
  const ops = operations({
    activate: async () => {
      fs.renameSync(tx.installTarget, tx.backupTarget);
      fs.renameSync(tx.incomingTarget, tx.installTarget);
    },
    rollback: async () => {
      fs.renameSync(tx.installTarget, tx.incomingTarget);
      fs.renameSync(tx.backupTarget, tx.installTarget);
    },
    cleanupFailed: async () => { fs.rmSync(tx.incomingTarget, { recursive: true, force: true }); },
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
  assert.equal(fs.existsSync(tx.backupTarget), false);
  assert.equal(fs.existsSync(tx.incomingTarget), false);
});
