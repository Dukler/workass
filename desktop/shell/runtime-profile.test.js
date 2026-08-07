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

test('packaged Windows production is LAN-reachable and announces on the default port', () => {
  const resourcesPath = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-profile-windows-'));
  fs.copyFileSync(
    path.join(repoRoot, 'config', 'environments', 'windows-prod.env'),
    path.join(resourcesPath, 'workass-profile.env'),
  );
  const profile = resolveRuntimeProfile({
    env: {
      HOME: '/Users/test',
      USERPROFILE: 'C:/Users/test',
      LOCALAPPDATA: 'C:/Users/test/AppData/Local',
      WORKASS_PROFILE: 'prod',
    },
    isPackaged: true,
    resourcesPath,
  });
  assert.equal(profile.daemonPort, 80);
  assert.equal(profile.daemonBind, 'lan');
  assert.equal(profile.daemonURL, 'https://127.0.0.1:80');
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

test('packaged update channels are explicit and limited to local or GitHub', () => {
  assert.equal(parseProfile('WORKASS_UPDATE_CHANNEL=local').WORKASS_UPDATE_CHANNEL, 'local');
  const resourcesPath = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-profile-packaged-'));
  fs.writeFileSync(path.join(resourcesPath, 'workass-profile.env'), [
    'WORKASS_PROFILE=prod',
    'WORKASS_APP_NAME=Workass',
    'WORKASS_BUNDLE_ID=com.workass.app',
    'WORKASS_DAEMON_PORT=8788',
    'WORKASS_DAEMON_BIND=localhost',
    'WORKASS_VIEW_PORT=8798',
    'WORKASS_LAUNCHD_LABEL=com.workass.daemon',
    'WORKASS_DATA_ROOT="${HOME}/Library/Application Support/Workass"',
    'WORKASS_LOG_ROOT="${HOME}/Library/Logs/Workass/prod"',
    'WORKASS_UPDATE_CHANNEL=local',
  ].join('\n'));
  const local = resolveRuntimeProfile({ env: { HOME: '/Users/test', WORKASS_PROFILE: 'prod' }, isPackaged: true, resourcesPath });
  assert.equal(local.updateChannel, 'local');
  fs.appendFileSync(path.join(resourcesPath, 'workass-profile.env'), '\nWORKASS_UPDATE_CHANNEL=anything\n');
  assert.throws(() => resolveRuntimeProfile({ env: { HOME: '/Users/test', WORKASS_PROFILE: 'prod' }, isPackaged: true, resourcesPath }), /github or local/);
});
