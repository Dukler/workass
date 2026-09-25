'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

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

// Shortcut discovery and icon repair use Electron's native IShellLink bindings
// (shell.readShortcutLink/writeShortcutLink) in-process. Workass never spawns a
// hidden PowerShell COM sweep over every .lnk file, nor the ie4uinit.exe
// icon-cache process.
const MAX_WINDOWS_SHORTCUTS = 4096;

function windowsShortcutRoots({ env = process.env, desktopPath = '' } = {}) {
  const home = String(env.USERPROFILE || '');
  const appData = String(env.APPDATA || (home ? path.win32.join(home, 'AppData', 'Roaming') : ''));
  const publicRoot = String(env.PUBLIC || '');
  const programData = String(env.ProgramData || env.PROGRAMDATA || env.ALLUSERSPROFILE || '');
  return [
    desktopPath || (home ? path.win32.join(home, 'Desktop') : ''),
    publicRoot ? path.win32.join(publicRoot, 'Desktop') : '',
    appData ? path.win32.join(appData, 'Microsoft', 'Windows', 'Start Menu', 'Programs') : '',
    programData ? path.win32.join(programData, 'Microsoft', 'Windows', 'Start Menu', 'Programs') : '',
  ].filter(Boolean);
}

function listShortcutFiles(roots, limit = MAX_WINDOWS_SHORTCUTS) {
  const files = [];
  const seenRoots = new Set();
  for (const root of roots) {
    const key = path.win32.normalize(String(root || '')).toLowerCase();
    if (!root || seenRoots.has(key)) continue;
    seenRoots.add(key);
    const pending = [root];
    while (pending.length > 0 && files.length < limit) {
      const directory = pending.pop();
      let entries;
      try { entries = fs.readdirSync(directory, { withFileTypes: true }); } catch { continue; }
      for (const entry of entries) {
        const full = path.join(directory, entry.name);
        // Symlinks and junctions are neither regular files nor directories.
        if (entry.isDirectory()) pending.push(full);
        else if (entry.isFile() && /\.lnk$/i.test(entry.name)) files.push(full);
        if (files.length >= limit) break;
      }
    }
    if (files.length >= limit) break;
  }
  return files;
}

function resolveWindowsShortcutTargets({ roots = null, env = process.env, shell = null, desktopPath = '', enumerateFiles = listShortcutFiles } = {}) {
  if (!shell || typeof shell.readShortcutLink !== 'function') {
    return { applied: false, reason: 'shortcut-reader-missing', shortcuts: [] };
  }
  const searchRoots = Array.isArray(roots) ? roots : windowsShortcutRoots({ env, desktopPath });
  const shortcuts = [];
  for (const file of enumerateFiles(searchRoots)) {
    let details;
    try { details = shell.readShortcutLink(file); } catch { continue; }
    const targetPath = String(details?.target || '');
    const icon = String(details?.icon || '');
    const iconIndex = Number.isInteger(details?.iconIndex) ? details.iconIndex : 0;
    if (!path.win32.isAbsolute(file) || !path.win32.isAbsolute(targetPath)) continue;
    shortcuts.push({ path: file, targetPath, iconLocation: icon ? `${icon},${iconIndex}` : '' });
  }
  return { applied: true, shortcuts };
}

function writeWindowsShortcutIcons({ shortcutPaths, iconPath, shell = null } = {}) {
  if (!Array.isArray(shortcutPaths) || shortcutPaths.length === 0) return { applied: true, shortcutCount: 0 };
  try {
    if (!iconPath || !fs.statSync(iconPath).isFile()) return { applied: false, reason: 'icon-missing' };
  } catch {
    return { applied: false, reason: 'icon-missing' };
  }
  if (!shell || typeof shell.writeShortcutLink !== 'function') return { applied: false, reason: 'shortcut-writer-missing' };
  const succeeded = [];
  const failed = [];
  for (const shortcut of shortcutPaths) {
    let written = false;
    try { written = shell.writeShortcutLink(shortcut, 'update', { icon: iconPath, iconIndex: 0 }) === true; } catch { written = false; }
    (written ? succeeded : failed).push(shortcut);
  }
  return {
    applied: succeeded.length > 0,
    shortcutCount: succeeded.length,
    shortcutPaths: succeeded,
    failedShortcutPaths: failed,
    ...(failed.length > 0 ? { reason: 'shortcut-write-partial' } : {}),
  };
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

function shortcutUsesIcon(shortcut, iconPath) {
  const match = String(shortcut?.iconLocation || '').trim().match(/^(.*),\s*(-?\d+)$/);
  return Boolean(match) && match[2] === '0' &&
    path.win32.normalize(match[1].trim()).toLowerCase() === path.win32.normalize(iconPath).toLowerCase();
}

function materializeWindowsShortcutIcon(sourceIcon, iconDigest, iconCacheDir) {
  const iconPath = path.join(iconCacheDir, `Workass-${iconDigest.slice(0, 24)}.ico`);
  fs.mkdirSync(iconCacheDir, { recursive: true, mode: 0o700 });
  let current = '';
  try { current = crypto.createHash('sha256').update(fs.readFileSync(iconPath)).digest('hex'); } catch { /* copy below */ }
  if (current !== iconDigest) {
    const incoming = `${iconPath}.incoming-${process.pid}`;
    try {
      fs.copyFileSync(sourceIcon, incoming);
      const copied = crypto.createHash('sha256').update(fs.readFileSync(incoming)).digest('hex');
      if (copied !== iconDigest) throw new Error('shortcut icon copy checksum mismatch');
      fs.renameSync(incoming, iconPath);
    } finally {
      try { fs.rmSync(incoming, { force: true }); } catch { /* rename consumed it */ }
    }
  }
  return iconPath;
}

function pruneWindowsShortcutIcons(iconCacheDir, currentIcon, shortcuts) {
  let entries = [];
  try { entries = fs.readdirSync(iconCacheDir, { withFileTypes: true }); } catch { return; }
  const referenced = new Set();
  for (const shortcut of shortcuts || []) {
    const match = String(shortcut?.iconLocation || '').trim().match(/^(.*),\s*(-?\d+)$/);
    if (match) referenced.add(path.win32.normalize(match[1].trim()).toLowerCase());
  }
  for (const entry of entries) {
    if (!entry.isFile() || !/^Workass-[a-f0-9]{24}\.ico$/i.test(entry.name)) continue;
    const candidate = path.join(iconCacheDir, entry.name);
    if (candidate === currentIcon || referenced.has(path.win32.normalize(candidate).toLowerCase())) continue;
    try { fs.rmSync(candidate, { force: true }); } catch { /* retry next launch */ }
  }
}

function refreshWindowsShortcutIcons({
  platform = process.platform,
  isPackaged = false,
  executablePath = process.execPath,
  resourcesPath = '',
  dataRoot = '',
  appVersion = '',
  env = process.env,
  roots = null,
  markerFile = '',
  iconCacheDir = '',
  desktopPath = '',
  shell = null,
  resolveShortcutTargets = resolveWindowsShortcutTargets,
  readShortcutTargets = resolveShortcutTargets,
  writeShortcutIcons = writeWindowsShortcutIcons,
  now = () => new Date(),
} = {}) {
  if (platform !== 'win32' || !isPackaged) return { applied: false, reason: 'unsupported-runtime' };
  if (!path.win32.isAbsolute(executablePath) || !path.win32.isAbsolute(dataRoot) || !String(appVersion).trim()) {
    return { applied: false, reason: 'invalid-runtime' };
  }
  const marker = markerFile || path.join(dataRoot, 'run', 'windows-icon-refresh.json');
  const cacheDirectory = iconCacheDir || path.join(path.dirname(marker), 'shortcut-icons');
  const sourceIconPath = resolveWindowIconPath({
    platform,
    isPackaged,
    resourcesPath,
    repoRoot: '',
  });
  if (!sourceIconPath) return { applied: false, reason: 'icon-missing', shortcutCount: 0 };

  let iconDigest;
  try { iconDigest = crypto.createHash('sha256').update(fs.readFileSync(sourceIconPath)).digest('hex'); }
  catch { return { applied: false, reason: 'icon-missing', shortcutCount: 0 }; }
  let iconPath;
  try {
    iconPath = materializeWindowsShortcutIcon(
      sourceIconPath,
      iconDigest,
      cacheDirectory,
    );
  } catch {
    return { applied: false, reason: 'icon-cache-copy-failed', shortcutCount: 0 };
  }

  const timestamp = now();
  const discovery = resolveShortcutTargets({ roots, env, shell, desktopPath });
  if (!discovery?.applied) return { applied: false, reason: discovery?.reason || 'shortcut-discovery-failed', shortcutCount: 0 };
  const wanted = path.win32.normalize(executablePath).toLowerCase();
  const matched = new Map();
  for (const shortcut of discovery.shortcuts) {
    if (path.win32.normalize(shortcut.targetPath).toLowerCase() !== wanted) continue;
    const key = path.win32.normalize(shortcut.path).toLowerCase();
    if (!matched.has(key)) matched.set(key, shortcut);
  }
  const shortcutSet = [...matched.keys()].sort((left, right) => left.localeCompare(right));
  const shortcuts = shortcutSet.map((key) => matched.get(key).path);
  if (shortcuts.length === 0) return { applied: false, reason: 'no-shortcuts', shortcutCount: 0 };
  try {
    const previous = JSON.parse(fs.readFileSync(marker, 'utf8'));
    if (previous?.schemaVersion === 2 && previous.appVersion === appVersion &&
        String(previous.executablePath || '').toLowerCase() === executablePath.toLowerCase() &&
        previous.iconDigest === iconDigest && JSON.stringify(previous.shortcutSet) === JSON.stringify(shortcutSet) &&
        shortcutSet.every((key) => shortcutUsesIcon(matched.get(key), iconPath))) {
      return { applied: false, reason: 'current', shortcutCount: shortcuts.length };
    }
  } catch { /* no successful refresh for this exact icon and shortcut set */ }
  const pendingKeys = shortcutSet.filter((key) => !shortcutUsesIcon(matched.get(key), iconPath));
  const pendingShortcuts = pendingKeys.map((key) => matched.get(key).path);
  const shortcutWrite = pendingShortcuts.length > 0
    ? writeShortcutIcons({ shortcutPaths: pendingShortcuts, iconPath, shell })
    : { applied: true, shortcutCount: 0, shortcutPaths: [] };
  const readback = pendingShortcuts.length > 0
    ? readShortcutTargets({ roots, env, shell, desktopPath })
    : discovery;
  if (!readback?.applied) {
    return { applied: false, reason: readback?.reason || 'shortcut-readback-failed', shortcutCount: 0 };
  }
  const verified = new Set();
  for (const shortcut of readback.shortcuts) {
    const key = path.win32.normalize(shortcut.path).toLowerCase();
    if (!matched.has(key) || path.win32.normalize(shortcut.targetPath).toLowerCase() !== wanted) continue;
    if (!shortcutUsesIcon(shortcut, iconPath)) continue;
    verified.add(key);
  }
  const changedKeys = pendingKeys.filter((key) => verified.has(key));
  if (pendingKeys.length > 0 && changedKeys.length === 0) {
    return { applied: false, reason: shortcutWrite?.reason || 'shortcut-readback-mismatch', shortcutCount: 0 };
  }
  for (const key of changedKeys) {
    const shortcut = matched.get(key).path;
    try {
      const stat = fs.statSync(shortcut);
      fs.utimesSync(shortcut, stat.atime, timestamp);
    } catch { /* one inaccessible shared shortcut cannot block app startup */ }
  }
  const complete = verified.size === shortcutSet.length && shortcutSet.every((shortcut) => verified.has(shortcut));
  const shortcutCount = complete ? shortcuts.length : changedKeys.length;

  // Digest-named ICO files and the updated .lnk mtime make Explorer extract
  // the changed icon without an external cache-refresh process.
  const cacheRefresh = true;
  pruneWindowsShortcutIcons(cacheDirectory, iconPath, readback.shortcuts);
  if (!complete) {
    return {
      applied: false,
      reason: 'shortcut-write-partial',
      shortcutCount,
      failedShortcutCount: shortcutSet.length - verified.size,
      cacheRefresh,
    };
  }
  atomicJSON(marker, {
    schemaVersion: 2,
    appVersion,
    executablePath,
    iconPath,
    sourceIconPath,
    iconDigest,
    shortcutSet,
    shortcutCount,
    cacheRefresh,
    refreshedAt: timestamp.toISOString(),
  });
  return { applied: true, shortcutCount, cacheRefresh };
}

// Runs after the first window is created, on a later event-loop turn. Native
// shell-link I/O is bounded and remains on Electron's main thread.
function refreshWindowsShortcutIconsAsync(options = {}, { schedule = (task) => setTimeout(task, 0), refresh = refreshWindowsShortcutIcons } = {}) {
  return new Promise((resolve) => {
    const run = () => {
      try { resolve(refresh({ ...options })); }
      catch (error) { resolve({ applied: false, reason: 'icon-refresh-failed', error: String(error?.message || error) }); }
    };
    try {
      const handle = schedule(run);
      handle?.unref?.();
    } catch (error) { resolve({ applied: false, reason: 'icon-refresh-schedule-failed', error: String(error?.message || error) }); }
  });
}

module.exports = {
  applyMacDockIcon,
  refreshWindowsShortcutIcons,
  refreshWindowsShortcutIconsAsync,
  resolveWindowsShortcutTargets,
  windowsShortcutRoots,
  resolveAppIconPath,
  resolveWindowFrameOptions,
  resolveWindowIconPath,
  writeWindowsShortcutIcons,
};
