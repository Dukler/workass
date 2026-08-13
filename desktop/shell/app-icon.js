'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function resolveAppIconPath({ isPackaged, resourcesPath, repoRoot }) {
  const candidates = isPackaged
    ? [
        path.join(resourcesPath, 'Workass.png'),
        path.join(resourcesPath, 'Workass.icns'),
      ]
    : [path.join(repoRoot, 'desktop', 'assets', 'workass-macos.png')];
  return candidates.find((candidate) => fs.existsSync(candidate)) || null;
}

function resolveWindowIconPath({ platform = process.platform, isPackaged, resourcesPath, repoRoot }) {
  if (platform !== 'win32') return null;
  const candidates = isPackaged
    ? [path.join(resourcesPath, 'Workass.ico')]
    : [path.join(repoRoot, 'desktop', 'assets', 'icon.ico')];
  return candidates.find((candidate) => fs.existsSync(candidate)) || null;
}

function resolveWindowFrameOptions({ platform = process.platform } = {}) {
  if (platform === 'darwin') {
    return { titleBarStyle: 'hiddenInset', trafficLightPosition: { x: 14, y: 14 } };
  }
  if (platform === 'win32') {
    // Windows owns its caption buttons. Keeping the standard frame avoids a
    // renderer/preload/IPC dependency for minimize, maximize, and close.
    return { frame: true, autoHideMenuBar: true };
  }
  return { frame: false };
}

function applyMacDockIcon({ app, nativeImage, isPackaged, resourcesPath, repoRoot, platform = process.platform }) {
  if (platform !== 'darwin' || !app || !app.dock || typeof app.dock.setIcon !== 'function') {
    return { applied: false, reason: 'unsupported-platform' };
  }
  const iconPath = resolveAppIconPath({ isPackaged, resourcesPath, repoRoot });
  if (!iconPath) return { applied: false, reason: 'icon-missing' };
  const icon = nativeImage.createFromPath(iconPath);
  if (!icon || icon.isEmpty()) return { applied: false, reason: 'icon-empty', iconPath };
  app.dock.setIcon(icon);
  return { applied: true, iconPath };
}

function defaultWindowsShortcutRoots(env) {
  const roots = [
    env.USERPROFILE && path.join(env.USERPROFILE, 'Desktop'),
    env.USERPROFILE && path.join(env.USERPROFILE, 'OneDrive', 'Desktop'),
    env.PUBLIC && path.join(env.PUBLIC, 'Desktop'),
    env.APPDATA && path.join(env.APPDATA, 'Microsoft', 'Windows', 'Start Menu', 'Programs'),
    (env.ProgramData || env.PROGRAMDATA) && path.join(env.ProgramData || env.PROGRAMDATA, 'Microsoft', 'Windows', 'Start Menu', 'Programs'),
  ].filter(Boolean);
  return [...new Set(roots.map((root) => path.resolve(root)))];
}

function shortcutFiles(roots, maximum = 4096) {
  const output = [];
  const pending = roots.map((root) => ({ root, depth: 0 }));
  let inspected = 0;
  while (pending.length > 0 && inspected < maximum) {
    const current = pending.shift();
    let entries;
    try { entries = fs.readdirSync(current.root, { withFileTypes: true }); }
    catch { continue; }
    for (const entry of entries) {
      if (inspected >= maximum) break;
      inspected += 1;
      const candidate = path.join(current.root, entry.name);
      if (entry.isDirectory() && current.depth < 8) pending.push({ root: candidate, depth: current.depth + 1 });
      else if (entry.isFile() && path.extname(entry.name).toLowerCase() === '.lnk') output.push(candidate);
    }
  }
  return output;
}

function shortcutTargetsExecutable(file, executablePath) {
  let bytes;
  try {
    const stat = fs.statSync(file);
    if (!stat.isFile() || stat.size < 1 || stat.size > 1024 * 1024) return false;
    bytes = fs.readFileSync(file);
  } catch { return false; }
  const wanted = path.win32.normalize(executablePath).toLowerCase();
  return [bytes, bytes.subarray(1)].some((candidate) => candidate.toString('utf16le').toLowerCase().includes(wanted)) ||
    bytes.toString('latin1').replaceAll('/', '\\').toLowerCase().includes(wanted);
}

function atomicJSON(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const incoming = `${file}.incoming-${process.pid}`;
  try {
    fs.writeFileSync(incoming, `${JSON.stringify(value)}\n`, { mode: 0o600 });
    fs.renameSync(incoming, file);
  } finally {
    try { fs.rmSync(incoming, { force: true }); } catch { /* rename already consumed it */ }
  }
}

function refreshWindowsShortcutIcons({
  platform = process.platform,
  isPackaged = false,
  executablePath = process.execPath,
  dataRoot = '',
  appVersion = '',
  env = process.env,
  roots = null,
  markerFile = '',
  cacheToolPath = '',
  run = spawnSync,
  now = () => new Date(),
} = {}) {
  if (platform !== 'win32' || !isPackaged) return { applied: false, reason: 'unsupported-runtime' };
  if (!path.win32.isAbsolute(executablePath) || !path.win32.isAbsolute(dataRoot) || !String(appVersion).trim()) {
    return { applied: false, reason: 'invalid-runtime' };
  }
  const marker = markerFile || path.join(dataRoot, 'run', 'windows-icon-refresh.json');
  try {
    const previous = JSON.parse(fs.readFileSync(marker, 'utf8'));
    if (previous?.schemaVersion === 1 && previous.appVersion === appVersion &&
        String(previous.executablePath || '').toLowerCase() === executablePath.toLowerCase()) {
      return { applied: false, reason: 'current', shortcutCount: Number(previous.shortcutCount || 0) };
    }
  } catch { /* first launch of this executable version */ }

  const timestamp = now();
  let shortcutCount = 0;
  for (const shortcut of shortcutFiles(roots || defaultWindowsShortcutRoots(env))) {
    if (!shortcutTargetsExecutable(shortcut, executablePath)) continue;
    try {
      const stat = fs.statSync(shortcut);
      fs.utimesSync(shortcut, stat.atime, timestamp);
      shortcutCount += 1;
    } catch { /* one inaccessible shared shortcut cannot block app startup */ }
  }

  const systemRoot = String(env.SystemRoot || env.SYSTEMROOT || env.WINDIR || '');
  const cacheTool = cacheToolPath || (systemRoot ? path.win32.join(systemRoot, 'System32', 'ie4uinit.exe') : '');
  let cacheRefresh = false;
  if (cacheTool && fs.existsSync(cacheTool)) {
    const result = run(cacheTool, ['-show'], { windowsHide: true, stdio: 'ignore' });
    cacheRefresh = !result.error && result.status === 0;
  }
  atomicJSON(marker, {
    schemaVersion: 1,
    appVersion,
    executablePath,
    shortcutCount,
    cacheRefresh,
    refreshedAt: timestamp.toISOString(),
  });
  return { applied: true, shortcutCount, cacheRefresh };
}

module.exports = {
  applyMacDockIcon,
  refreshWindowsShortcutIcons,
  resolveAppIconPath,
  resolveWindowFrameOptions,
  resolveWindowIconPath,
  shortcutTargetsExecutable,
};
