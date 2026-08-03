'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { parseProfile, resolveRuntimeProfile } = require('./runtime-profile');

const repoRoot = path.resolve(__dirname, '..', '..');

test('production and development profiles isolate every mutable runtime root and port', () => {
  const env = { HOME: '/Users/test' };
  const prod = resolveRuntimeProfile({ env: { ...env, WORKASS_PROFILE: 'prod' }, repoRoot });
  const dev = resolveRuntimeProfile({ env: { ...env, WORKASS_PROFILE: 'dev' }, repoRoot });
  assert.equal(prod.daemonPort, 8788);
  assert.equal(prod.viewPort, 8798);
  assert.equal(prod.launchdLabel, 'com.workass.daemon');
  assert.equal(dev.daemonPort, 18788);
  assert.equal(dev.viewPort, 8799);
  assert.notEqual(prod.dataRoot, dev.dataRoot);
  assert.notEqual(prod.userDataDir, dev.userDataDir);
  assert.notEqual(prod.browserControlFile, dev.browserControlFile);
  assert.equal(prod.appName, 'Workass');
  assert.equal(dev.appName, 'Workass Dev');
});

test('test profile requires an isolated temporary root', () => {
  assert.throws(() => resolveRuntimeProfile({ env: { HOME: '/Users/test', WORKASS_PROFILE: 'test' }, repoRoot }), /WORKASS_TEST_ROOT/);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-profile-test-'));
  const profile = resolveRuntimeProfile({ env: { HOME: '/Users/test', WORKASS_PROFILE: 'test', WORKASS_TEST_ROOT: root }, repoRoot });
  assert.equal(profile.dataRoot, root);
  assert.equal(profile.daemonPort, 0);
  assert.equal(profile.viewPort, 0);
});

test('profile parser rejects executable and unknown assignments', () => {
  assert.throws(() => parseProfile('WORKASS_PROFILE=$(id)'), /unsafe/);
  assert.throws(() => parseProfile('WORKASS_TOKEN=nope'), /invalid profile assignment/);
});
