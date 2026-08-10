'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { applyMacDockIcon, resolveAppIconPath, resolveWindowIconPath } = require('./app-icon');

test('packaged icon resolution prefers the original PNG used for the Dock', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-test-'));
  const png = path.join(root, 'Workass.png');
  const icns = path.join(root, 'Workass.icns');
  fs.writeFileSync(png, 'png');
  fs.writeFileSync(icns, 'icns');
  assert.equal(resolveAppIconPath({ isPackaged: true, resourcesPath: root, repoRoot: '/unused' }), png);
});

test('macOS applies a non-empty native image to the running Dock item', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-test-'));
  const iconPath = path.join(root, 'desktop', 'assets', 'workass-macos.png');
  fs.mkdirSync(path.dirname(iconPath), { recursive: true });
  fs.writeFileSync(iconPath, 'png');
  const applied = [];
  const image = { isEmpty: () => false };
  const receipt = applyMacDockIcon({
    app: { dock: { setIcon: (value) => applied.push(value) } },
    nativeImage: { createFromPath: (value) => value === iconPath ? image : null },
    isPackaged: false,
    resourcesPath: '/unused',
    repoRoot: root,
    platform: 'darwin',
  });
  assert.deepEqual(applied, [image]);
  assert.deepEqual(receipt, { applied: true, iconPath });
});

test('non-macOS platforms do not attempt a Dock mutation', () => {
  let called = false;
  const receipt = applyMacDockIcon({
    app: { dock: { setIcon: () => { called = true; } } },
    nativeImage: { createFromPath: () => { called = true; } },
    isPackaged: false,
    resourcesPath: '',
    repoRoot: '',
    platform: 'win32',
  });
  assert.equal(called, false);
  assert.deepEqual(receipt, { applied: false, reason: 'unsupported-platform' });
});

test('Windows resolves the packaged ICO used by the frameless window and taskbar', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-test-'));
  const iconPath = path.join(root, 'Workass.ico');
  fs.writeFileSync(iconPath, 'ico');
  assert.equal(resolveWindowIconPath({ platform: 'win32', isPackaged: true, resourcesPath: root, repoRoot: '/unused' }), iconPath);
  assert.equal(resolveWindowIconPath({ platform: 'darwin', isPackaged: true, resourcesPath: root, repoRoot: '/unused' }), null);

  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(main, /setAppUserModelId\(RUNTIME\.bundleId\)/);
  assert.match(main, /resolveWindowIconPath\(\{[\s\S]{0,500}icon:\s*windowIcon/);
});
