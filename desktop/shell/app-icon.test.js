'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  applyMacDockIcon,
  refreshWindowsShortcutIcons,
  resolveAppIconPath,
  resolveWindowFrameOptions,
  resolveWindowIconPath,
  shortcutTargetsExecutable,
} = require('./app-icon');

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

test('Windows resolves the packaged ICO used by the native window and taskbar', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-test-'));
  const iconPath = path.join(root, 'Workass.ico');
  fs.writeFileSync(iconPath, 'ico');
  assert.equal(resolveWindowIconPath({ platform: 'win32', isPackaged: true, resourcesPath: root, repoRoot: '/unused' }), iconPath);
  assert.equal(resolveWindowIconPath({ platform: 'darwin', isPackaged: true, resourcesPath: root, repoRoot: '/unused' }), null);

  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(main, /setAppUserModelId\(RUNTIME\.bundleId\)/);
  assert.match(main, /resolveWindowIconPath\(\{[\s\S]{0,500}icon:\s*windowIcon/);
  assert.match(main, /refreshWindowsShortcutIcons\(\{[\s\S]{0,300}appVersion:\s*APP_VERSION/);
});

test('Windows refreshes only shortcuts targeting this exact installed executable once per version', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-refresh-'));
  const desktop = path.join(root, 'Desktop');
  const dataRoot = 'C:\\Users\\test\\AppData\\Local\\Workass';
  const executablePath = 'C:\\Users\\test\\Apps\\Workass\\Workass.exe';
  const matching = path.join(desktop, 'Workass.lnk');
  const unrelated = path.join(desktop, 'Other.lnk');
  fs.mkdirSync(desktop, { recursive: true });
  fs.writeFileSync(matching, Buffer.concat([Buffer.from('fixture'), Buffer.from(executablePath, 'utf16le')]));
  fs.writeFileSync(unrelated, Buffer.concat([Buffer.from('fixture'), Buffer.from('C:\\Other\\Other.exe', 'utf16le')]));
  const old = new Date('2020-01-01T00:00:00Z');
  const refreshed = new Date('2026-08-13T17:00:00Z');
  fs.utimesSync(matching, old, old);
  fs.utimesSync(unrelated, old, old);
  const runs = [];
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, dataRoot, appVersion: '1.2.3',
    roots: [desktop], markerFile: path.join(root, 'marker.json'), env: { SystemRoot: root }, now: () => refreshed,
    run: (...args) => { runs.push(args); return { status: 0 }; },
  });
  assert.deepEqual(receipt, { applied: true, shortcutCount: 1, cacheRefresh: false });
  assert.equal(fs.statSync(matching).mtime.toISOString(), refreshed.toISOString());
  assert.equal(fs.statSync(unrelated).mtime.toISOString(), old.toISOString());
  assert.equal(shortcutTargetsExecutable(matching, executablePath), true);
  assert.equal(shortcutTargetsExecutable(unrelated, executablePath), false);
  assert.deepEqual(runs, []);

  const repeated = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, dataRoot, appVersion: '1.2.3',
    roots: [desktop], markerFile: path.join(root, 'marker.json'), env: { SystemRoot: root },
    now: () => new Date('2027-01-01T00:00:00Z'),
  });
  assert.deepEqual(repeated, { applied: false, reason: 'current', shortcutCount: 1 });
  assert.equal(fs.statSync(matching).mtime.toISOString(), refreshed.toISOString());
});

test('Windows invokes the built-in icon notifier after a packaged release changes', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-notifier-'));
  const systemRoot = path.join(root, 'Windows');
  const cacheTool = path.join(systemRoot, 'System32', 'ie4uinit.exe');
  fs.mkdirSync(path.dirname(cacheTool), { recursive: true });
  fs.writeFileSync(cacheTool, 'fixture');
  const calls = [];
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true,
    executablePath: 'C:\\Apps\\Workass\\Workass.exe',
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass',
    appVersion: '2.0.0', roots: [], markerFile: path.join(root, 'marker.json'), cacheToolPath: cacheTool,
    env: { SystemRoot: systemRoot },
    run: (...args) => { calls.push(args); return { status: 0 }; },
  });
  assert.equal(receipt.cacheRefresh, true);
  assert.deepEqual(calls, [[cacheTool, ['-show'], { windowsHide: true, stdio: 'ignore' }]]);
});

test('Windows uses native caption buttons while macOS keeps hidden-inset traffic lights', () => {
  assert.deepEqual(resolveWindowFrameOptions({ platform: 'win32' }), {
    frame: true,
    autoHideMenuBar: true,
  });
  assert.deepEqual(resolveWindowFrameOptions({ platform: 'darwin' }), {
    titleBarStyle: 'hiddenInset',
    trafficLightPosition: { x: 14, y: 14 },
  });
  assert.deepEqual(resolveWindowFrameOptions({ platform: 'linux' }), { frame: false });

  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(main, /\.\.\.resolveWindowFrameOptions\(\{\s*platform:\s*process\.platform\s*}\)/);
});
