'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const {
  applyMacDockIcon,
  refreshWindowsShortcutIcons,
  refreshWindowsShortcutIconsAsync,
  resolveAppIconPath,
  resolveWindowsShortcutTargets,
  resolveWindowFrameOptions,
  resolveWindowIconPath,
  writeWindowsShortcutIcons,
} = require('./app-icon');

function cachedShortcutIcon(markerFile, sourceIcon) {
  const digest = crypto.createHash('sha256').update(fs.readFileSync(sourceIcon)).digest('hex');
  return path.join(path.dirname(markerFile), 'shortcut-icons', `Workass-${digest.slice(0, 24)}.ico`);
}

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
  assert.match(main, /refreshWindowsShortcutIconsAsync\(\{[\s\S]{0,300}resourcesPath:\s*process\.resourcesPath[\s\S]{0,300}appVersion:\s*APP_VERSION/);
});

test('Windows update recovery refreshes shortcuts off the Electron main thread', async () => {
  const calls = [];
  class FakeWorker extends EventEmitter {
    constructor(source, options) {
      super();
      calls.push({ source, options });
      queueMicrotask(() => this.emit('message', { ok: true, receipt: { applied: true, shortcutCount: 1, cacheRefresh: true } }));
    }
    unref() { this.unrefCalled = true; }
  }
  const receipt = await refreshWindowsShortcutIconsAsync({
    platform: 'win32', isPackaged: true,
    executablePath: 'C:\\Apps\\Workass\\Workass.exe',
    resourcesPath: 'C:\\Apps\\Workass\\resources',
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass',
    appVersion: '2.0.0',
  }, { WorkerClass: FakeWorker });
  assert.deepEqual(receipt, { applied: true, shortcutCount: 1, cacheRefresh: true });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].options.eval, true);
  assert.equal(calls[0].options.workerData.options.appVersion, '2.0.0');
  assert.equal(calls[0].options.workerData.options.executablePath, 'C:\\Apps\\Workass\\Workass.exe');
  assert.match(calls[0].source, /refreshWindowsShortcutIcons/);

  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.doesNotMatch(main, /deferred-update-relaunch/);
  assert.ok(main.indexOf('createWindow(viewURL') < main.indexOf('refreshWindowsShortcutIconsAsync({'));
});

test('Windows refreshes only shortcuts whose native TargetPath is the exact installed executable', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-refresh-'));
  const desktop = path.join(root, 'Desktop');
  const dataRoot = 'C:\\Users\\test\\AppData\\Local\\Workass';
  const executablePath = 'C:\\Users\\test\\Apps\\Workass\\Workass.exe';
  const matching = path.join(desktop, 'Workass.lnk');
  const unrelated = path.join(desktop, 'Other.lnk');
  const resourcesPath = path.join(root, 'resources');
  const iconPath = path.join(resourcesPath, 'Workass.ico');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  fs.mkdirSync(desktop, { recursive: true });
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(iconPath, 'ico');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(matching, 'shortcut');
  fs.writeFileSync(unrelated, 'shortcut');
  const old = new Date('2020-01-01T00:00:00Z');
  const refreshed = new Date('2026-08-13T17:00:00Z');
  const markerFile = path.join(root, 'marker.json');
  const cachedIcon = cachedShortcutIcon(markerFile, iconPath);
  fs.utimesSync(matching, old, old);
  fs.utimesSync(unrelated, old, old);
  const runs = [];
  const shortcutWrites = [];
  let matchingIcon = `${iconPath},0`;
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, dataRoot, appVersion: '1.2.3',
    resourcesPath, roots: [desktop], markerFile, cacheToolPath: cacheTool,
    env: { SystemRoot: root }, now: () => refreshed,
    run: (...args) => { runs.push(args); return { status: 0 }; },
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [
      { path: matching, targetPath: executablePath, iconLocation: matchingIcon },
      { path: unrelated, targetPath: 'C:\\Other\\Other.exe' },
    ] }),
    writeShortcutIcons: (input) => {
      shortcutWrites.push(input);
      matchingIcon = `${input.iconPath},0`;
      return { applied: true, shortcutCount: input.shortcutPaths.length, shortcutPaths: input.shortcutPaths };
    },
  });
  assert.deepEqual(receipt, { applied: true, shortcutCount: 1, cacheRefresh: true });
  assert.equal(fs.statSync(matching).mtime.toISOString(), refreshed.toISOString());
  assert.equal(fs.statSync(unrelated).mtime.toISOString(), old.toISOString());
  assert.deepEqual(shortcutWrites.map(({ shortcutPaths, iconPath: source }) => ({ shortcutPaths, iconPath: source })), [
    { shortcutPaths: [matching], iconPath: cachedIcon },
  ]);
  assert.deepEqual(runs, [[cacheTool, ['-show'], { windowsHide: true, stdio: 'ignore' }]]);

  const repeated = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, dataRoot, appVersion: '1.2.3',
    resourcesPath, roots: [desktop], markerFile, cacheToolPath: cacheTool,
    env: { SystemRoot: root },
    now: () => new Date('2027-01-01T00:00:00Z'),
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [
      { path: matching, targetPath: executablePath, iconLocation: matchingIcon },
      { path: unrelated, targetPath: 'C:\\Other\\Other.exe' },
    ] }),
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
  const resourcesPath = path.join(root, 'resources');
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'ico');
  const shortcut = path.join(root, 'Workass.lnk');
  fs.writeFileSync(shortcut, 'shortcut');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  const markerFile = path.join(root, 'marker.json');
  let currentIcon = `${path.join(resourcesPath, 'Workass.ico')},0`;
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true,
    executablePath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass',
    resourcesPath, appVersion: '2.0.0', roots: [], markerFile, cacheToolPath: cacheTool,
    env: { SystemRoot: systemRoot },
    run: (...args) => { calls.push(args); return { status: 0 }; },
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [
      { path: shortcut, targetPath: executablePath, iconLocation: currentIcon },
    ] }),
    writeShortcutIcons: ({ shortcutPaths, iconPath: targetIcon }) => {
      currentIcon = `${targetIcon},0`;
      return { applied: true, shortcutCount: shortcutPaths.length, shortcutPaths };
    },
  });
  assert.equal(receipt.cacheRefresh, true);
  assert.deepEqual(calls, [[cacheTool, ['-show'], { windowsHide: true, stdio: 'ignore' }]]);
});

test('Windows discovers known-folder shortcuts and reads TargetPath through built-in COM', () => {
  let invocation = null;
  const receipt = resolveWindowsShortcutTargets({
    env: { SystemRoot: 'C:\\Windows' },
    run: (command, args, options) => {
      invocation = { command, args, options };
      return {
        status: 0,
        stdout: JSON.stringify([
          { path: 'C:\\Users\\test\\Desktop\\Workass.lnk', targetPath: 'C:\\Apps\\Workass\\Workass.exe' },
        ]),
      };
    },
  });
  assert.deepEqual(receipt.shortcuts, [
    { path: 'C:\\Users\\test\\Desktop\\Workass.lnk', targetPath: 'C:\\Apps\\Workass\\Workass.exe', iconLocation: '' },
  ]);
  assert.equal(invocation.command, 'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe');
  assert.match(invocation.args.at(-1), /GetFolderPath\('Desktop'\)/);
  assert.match(invocation.args.at(-1), /CreateShortcut\(\$file\.FullName\)/);
  assert.match(invocation.args.at(-1), /TargetPath/);
  assert.match(invocation.args.at(-1), /IconLocation/);
});

test('Windows retries after zero shortcuts and refreshes when the matched shortcut set changes', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-set-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'marker.json');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  const firstShortcut = path.join(root, 'Desktop', 'Workass.lnk');
  const secondShortcut = path.join(root, 'Start Menu', 'Workass.lnk');
  fs.mkdirSync(path.dirname(firstShortcut), { recursive: true });
  fs.mkdirSync(path.dirname(secondShortcut), { recursive: true });
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'icon-v1');
  const iconPath = path.join(resourcesPath, 'Workass.ico');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(firstShortcut, 'shortcut');
  fs.writeFileSync(secondShortcut, 'shortcut');
  const base = {
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '3.0.0', markerFile, cacheToolPath: cacheTool,
    run: () => ({ status: 0 }),
  };
  let shortcuts = [];
  let writes = 0;
  const resolveShortcutTargets = () => ({ applied: true, shortcuts });
  const writeShortcutIcons = ({ shortcutPaths, iconPath: targetIcon }) => {
    writes += 1;
    for (const shortcut of shortcuts) {
      if (shortcutPaths.includes(shortcut.path)) shortcut.iconLocation = `${targetIcon},0`;
    }
    return { applied: true, shortcutCount: shortcutPaths.length, shortcutPaths };
  };

  assert.equal(refreshWindowsShortcutIcons({ ...base, resolveShortcutTargets, writeShortcutIcons }).reason, 'no-shortcuts');
  assert.equal(fs.existsSync(markerFile), false);

  shortcuts = [{ path: firstShortcut, targetPath: executablePath, iconLocation: `${iconPath},0` }];
  assert.equal(refreshWindowsShortcutIcons({ ...base, resolveShortcutTargets, writeShortcutIcons }).applied, true);
  assert.equal(writes, 1);
  assert.equal(refreshWindowsShortcutIcons({ ...base, resolveShortcutTargets, writeShortcutIcons }).reason, 'current');
  assert.equal(writes, 1);

  shortcuts = [
    { path: firstShortcut, targetPath: executablePath, iconLocation: `${iconPath},0` },
    { path: secondShortcut, targetPath: executablePath, iconLocation: `${iconPath},0` },
  ];
  assert.equal(refreshWindowsShortcutIcons({ ...base, resolveShortcutTargets, writeShortcutIcons }).shortcutCount, 2);
  assert.equal(writes, 2);
  assert.equal(JSON.parse(fs.readFileSync(markerFile, 'utf8')).shortcutSet.length, 2);
});

test('Windows writes no success marker after a partial matching-shortcut update', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-failed-write-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'marker.json');
  const shortcut = path.join(root, 'Workass.lnk');
  const secondShortcut = path.join(root, 'Workass Start.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'icon');
  fs.writeFileSync(shortcut, 'shortcut');
  fs.writeFileSync(secondShortcut, 'shortcut');
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '3.0.0', markerFile,
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [
      { path: shortcut, targetPath: executablePath },
      { path: secondShortcut, targetPath: executablePath },
    ] }),
    writeShortcutIcons: () => ({ applied: true, shortcutCount: 1 }),
  });
  assert.equal(receipt.applied, false);
  assert.equal(fs.existsSync(markerFile), false);
});

test('Windows retries when COM readback does not confirm every shortcut IconLocation', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-readback-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'marker.json');
  const shortcut = path.join(root, 'Workass.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'icon');
  fs.writeFileSync(shortcut, 'shortcut');
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '3.0.0', markerFile,
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [{ path: shortcut, targetPath: executablePath }] }),
    readShortcutTargets: () => ({ applied: true, shortcuts: [{
      path: shortcut, targetPath: executablePath, iconLocation: 'C:\\Other\\wrong.ico,0',
    }] }),
    writeShortcutIcons: () => ({ applied: true, shortcutCount: 1 }),
  });
  assert.equal(receipt.reason, 'shortcut-readback-mismatch');
  assert.equal(fs.existsSync(markerFile), false);
});

test('Windows retries shortcut repair until Explorer accepts the icon cache notification', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-cache-retry-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'marker.json');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  const shortcut = path.join(root, 'Workass.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  const iconPath = path.join(resourcesPath, 'Workass.ico');
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.writeFileSync(iconPath, 'icon');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(shortcut, 'shortcut');
  let notifierAttempts = 0;
  let shortcutWrites = 0;
  let currentIcon = `${iconPath},0`;
  const options = {
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '3.0.0', markerFile, cacheToolPath: cacheTool,
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [{
      path: shortcut, targetPath: executablePath, iconLocation: currentIcon,
    }] }),
    writeShortcutIcons: ({ shortcutPaths, iconPath: targetIcon }) => {
      shortcutWrites += 1;
      currentIcon = `${targetIcon},0`;
      return { applied: true, shortcutCount: 1, shortcutPaths };
    },
    run: () => { notifierAttempts += 1; return { status: notifierAttempts === 1 ? 1 : 0 }; },
  };
  assert.equal(refreshWindowsShortcutIcons(options).reason, 'icon-cache-refresh-failed');
  assert.equal(fs.existsSync(markerFile), false);
  assert.equal(refreshWindowsShortcutIcons(options).applied, true);
  assert.equal(refreshWindowsShortcutIcons(options).reason, 'current');
  assert.equal(shortcutWrites, 1);
  assert.equal(notifierAttempts, 2);
});

test('Windows binds matching shortcuts to the packaged ICO through the built-in shortcut writer', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-writer-'));
  const systemRoot = path.join(root, 'Windows');
  const powershell = path.join(systemRoot, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
  const iconPath = path.join(root, 'resources', 'Workass.ico');
  fs.mkdirSync(path.dirname(powershell), { recursive: true });
  fs.mkdirSync(path.dirname(iconPath), { recursive: true });
  fs.writeFileSync(powershell, 'fixture');
  fs.writeFileSync(iconPath, 'ico');
  const calls = [];
  const shortcutPaths = ['C:\\Users\\test\\Desktop\\Workass.lnk', 'C:\\Users\\test\\Start Menu\\Workass.lnk'];
  const receipt = writeWindowsShortcutIcons({
    shortcutPaths,
    iconPath,
    env: { SystemRoot: systemRoot },
    run: (...args) => {
      calls.push(args);
      const request = JSON.parse(args[2].env.WORKASS_SHORTCUT_ICON_REQUEST);
      return {
        status: 0,
        stdout: JSON.stringify(request.shortcutPaths.map((shortcutPath) => ({ path: shortcutPath, applied: true }))),
      };
    },
  });
  assert.deepEqual(receipt, {
    applied: true,
    shortcutCount: 2,
    shortcutPaths,
    failedShortcutPaths: [],
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], powershell);
  assert.deepEqual(JSON.parse(calls[0][2].env.WORKASS_SHORTCUT_ICON_REQUEST), { iconPath, shortcutPaths });
  assert.deepEqual(calls[0][2], {
    windowsHide: true,
    encoding: 'utf8',
    maxBuffer: 1024 * 1024,
    stdio: ['ignore', 'pipe', 'ignore'],
    env: { SystemRoot: systemRoot, WORKASS_SHORTCUT_ICON_REQUEST: JSON.stringify({ iconPath, shortcutPaths }) },
  });
});

test('Windows retries a shortcut recreated at the same path after a successful marker', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-reset-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'run', 'marker.json');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  const shortcut = path.join(root, 'Desktop', 'Workass.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.mkdirSync(path.dirname(shortcut), { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'release-icon');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(shortcut, 'shortcut');
  let currentIcon = `${path.join(resourcesPath, 'Workass.ico')},0`;
  let writes = 0;
  const options = {
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '4.0.0', markerFile, cacheToolPath: cacheTool,
    run: () => ({ status: 0 }),
    resolveShortcutTargets: () => ({ applied: true, shortcuts: [{
      path: shortcut, targetPath: executablePath, iconLocation: currentIcon,
    }] }),
    writeShortcutIcons: ({ shortcutPaths, iconPath }) => {
      writes += 1;
      currentIcon = `${iconPath},0`;
      return { applied: true, shortcutCount: shortcutPaths.length, shortcutPaths };
    },
  };
  assert.equal(refreshWindowsShortcutIcons(options).applied, true);
  assert.equal(refreshWindowsShortcutIcons(options).reason, 'current');
  currentIcon = 'C:\\Windows\\System32\\shell32.dll,0';
  assert.equal(refreshWindowsShortcutIcons(options).applied, true);
  assert.equal(writes, 2);
});

test('one protected shared shortcut does not block a writable user shortcut or its retry', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-partial-permission-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'run', 'marker.json');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  const desktop = path.join(root, 'Desktop', 'Workass.lnk');
  const shared = path.join(root, 'Public Desktop', 'Workass.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.mkdirSync(path.dirname(desktop), { recursive: true });
  fs.mkdirSync(path.dirname(shared), { recursive: true });
  fs.writeFileSync(path.join(resourcesPath, 'Workass.ico'), 'release-icon');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(desktop, 'shortcut');
  fs.writeFileSync(shared, 'shortcut');
  const shortcuts = [
    { path: desktop, targetPath: executablePath, iconLocation: 'C:\\wrong.ico,0' },
    { path: shared, targetPath: executablePath, iconLocation: 'C:\\wrong.ico,0' },
  ];
  const batches = [];
  let attempt = 0;
  let notifications = 0;
  const options = {
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '4.0.0', markerFile, cacheToolPath: cacheTool,
    resolveShortcutTargets: () => ({ applied: true, shortcuts }),
    run: () => { notifications += 1; return { status: 0 }; },
    writeShortcutIcons: ({ shortcutPaths, iconPath }) => {
      attempt += 1;
      batches.push([...shortcutPaths]);
      const succeeded = attempt === 1 ? [desktop] : shortcutPaths;
      for (const item of shortcuts) {
        if (succeeded.includes(item.path)) item.iconLocation = `${iconPath},0`;
      }
      return {
        applied: succeeded.length > 0,
        shortcutCount: succeeded.length,
        shortcutPaths: succeeded,
        failedShortcutPaths: shortcutPaths.filter((item) => !succeeded.includes(item)),
      };
    },
  };
  const partial = refreshWindowsShortcutIcons(options);
  assert.equal(partial.reason, 'shortcut-write-partial');
  assert.equal(partial.shortcutCount, 1);
  assert.equal(partial.cacheRefresh, true);
  assert.equal(fs.existsSync(markerFile), false);
  const complete = refreshWindowsShortcutIcons(options);
  assert.equal(complete.applied, true);
  assert.deepEqual(batches, [[desktop, shared], [shared]]);
  assert.equal(notifications, 2);
  assert.equal(fs.existsSync(markerFile), true);
});

test('Windows shortcuts use a digest-specific cached ICO and prune only unreferenced old icons', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-icon-versioned-'));
  const resourcesPath = path.join(root, 'resources');
  const markerFile = path.join(root, 'run', 'marker.json');
  const iconCacheDir = path.join(root, 'run', 'shortcut-icons');
  const cacheTool = path.join(root, 'ie4uinit.exe');
  const shortcut = path.join(root, 'Desktop', 'Workass.lnk');
  const retainedShortcut = path.join(root, 'Desktop', 'Other.lnk');
  const executablePath = 'C:\\Apps\\Workass\\Workass.exe';
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.mkdirSync(iconCacheDir, { recursive: true });
  fs.mkdirSync(path.dirname(shortcut), { recursive: true });
  const sourceIcon = path.join(resourcesPath, 'Workass.ico');
  fs.writeFileSync(sourceIcon, 'release-icon-v4');
  fs.writeFileSync(cacheTool, 'cache');
  fs.writeFileSync(shortcut, 'shortcut');
  fs.writeFileSync(retainedShortcut, 'shortcut');
  const unreferenced = path.join(iconCacheDir, `Workass-${'a'.repeat(24)}.ico`);
  const referenced = path.join(iconCacheDir, `Workass-${'b'.repeat(24)}.ico`);
  fs.writeFileSync(unreferenced, 'old-a');
  fs.writeFileSync(referenced, 'old-b');
  const shortcuts = [
    { path: shortcut, targetPath: executablePath, iconLocation: `${sourceIcon},0` },
    { path: retainedShortcut, targetPath: 'C:\\Other\\Other.exe', iconLocation: `${referenced},0` },
  ];
  let writtenIcon = '';
  const receipt = refreshWindowsShortcutIcons({
    platform: 'win32', isPackaged: true, executablePath, resourcesPath,
    dataRoot: 'C:\\Users\\test\\AppData\\Local\\Workass', appVersion: '4.0.0', markerFile, iconCacheDir, cacheToolPath: cacheTool,
    run: () => ({ status: 0 }),
    resolveShortcutTargets: () => ({ applied: true, shortcuts }),
    writeShortcutIcons: ({ shortcutPaths, iconPath }) => {
      writtenIcon = iconPath;
      shortcuts[0].iconLocation = `${iconPath},0`;
      return { applied: true, shortcutCount: shortcutPaths.length, shortcutPaths };
    },
  });
  assert.equal(receipt.applied, true);
  assert.notEqual(writtenIcon, sourceIcon);
  assert.match(path.basename(writtenIcon), /^Workass-[a-f0-9]{24}\.ico$/);
  assert.deepEqual(fs.readFileSync(writtenIcon), fs.readFileSync(sourceIcon));
  assert.equal(fs.existsSync(unreferenced), false);
  assert.equal(fs.existsSync(referenced), true);
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
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  assert.match(main, /\.\.\.resolveWindowFrameOptions\(\{\s*platform:\s*process\.platform\s*}\)/);
  assert.doesNotMatch(main, /workass-window:control/);
  assert.doesNotMatch(preload, /workass-window:control/);
});
