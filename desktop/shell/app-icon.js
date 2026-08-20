'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { Worker } = require('node:worker_threads');

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

const WINDOWS_SHORTCUT_DISCOVERY_SCRIPT = String.raw`
$ErrorActionPreference = 'Stop'
$requestedRoots = @()
if (-not [string]::IsNullOrWhiteSpace($env:WORKASS_SHORTCUT_ROOTS)) {
  $requestedRoots = @(ConvertFrom-Json -InputObject $env:WORKASS_SHORTCUT_ROOTS)
}
$roots = if ($requestedRoots.Count -gt 0) { $requestedRoots } else {
  @(
    [Environment]::GetFolderPath('Desktop'),
    [Environment]::GetFolderPath('CommonDesktopDirectory'),
    [Environment]::GetFolderPath('Programs'),
    [Environment]::GetFolderPath('CommonPrograms')
  )
}
$shell = New-Object -ComObject WScript.Shell
$items = [System.Collections.Generic.List[object]]::new()
foreach ($root in @($roots | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } | Select-Object -Unique)) {
  if (-not (Test-Path -LiteralPath $root -PathType Container)) { continue }
  foreach ($file in @(Get-ChildItem -LiteralPath $root -Filter '*.lnk' -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 4096)) {
    try {
      $shortcut = $shell.CreateShortcut($file.FullName)
      $items.Add([pscustomobject]@{
        path = $file.FullName
        targetPath = [string]$shortcut.TargetPath
        iconLocation = [string]$shortcut.IconLocation
      })
    } catch {}
    if ($items.Count -ge 4096) { break }
  }
  if ($items.Count -ge 4096) { break }
}
ConvertTo-Json -InputObject @($items) -Compress
`;

function resolveWindowsShortcutTargets({ roots = null, env = process.env, run = spawnSync } = {}) {
  const systemRoot = String(env.SystemRoot || env.SYSTEMROOT || env.WINDIR || '');
  const powershell = systemRoot
    ? path.win32.join(systemRoot, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe')
    : 'powershell.exe';
  const result = run(powershell, ['-NoLogo', '-NoProfile', '-NonInteractive', '-Command', WINDOWS_SHORTCUT_DISCOVERY_SCRIPT], {
    windowsHide: true,
    encoding: 'utf8',
    maxBuffer: 4 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'ignore'],
    env: {
      ...env,
      ...(Array.isArray(roots) ? { WORKASS_SHORTCUT_ROOTS: JSON.stringify(roots) } : {}),
    },
  });
  if (result.error || result.status !== 0) return { applied: false, reason: 'shortcut-discovery-failed', shortcuts: [] };
  let parsed;
  try { parsed = JSON.parse(String(result.stdout || '[]')); }
  catch { return { applied: false, reason: 'shortcut-discovery-invalid', shortcuts: [] }; }
  const items = (Array.isArray(parsed) ? parsed : parsed ? [parsed] : []).slice(0, 4096).map((item) => ({
    path: String(item?.path || ''),
    targetPath: String(item?.targetPath || ''),
    iconLocation: String(item?.iconLocation || ''),
  })).filter((item) => path.win32.isAbsolute(item.path) && path.win32.isAbsolute(item.targetPath));
  return { applied: true, shortcuts: items };
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

const WINDOWS_SHORTCUT_ICON_SCRIPT = [
  "$ErrorActionPreference = 'Stop'",
  '$request = ConvertFrom-Json -InputObject $env:WORKASS_SHORTCUT_ICON_REQUEST',
  "if ($null -eq $request -or [string]::IsNullOrWhiteSpace([string]$request.iconPath)) { throw 'shortcut icon request is incomplete' }",
  '$iconPath = [string]$request.iconPath',
  '$shell = New-Object -ComObject WScript.Shell',
  '$results = [System.Collections.Generic.List[object]]::new()',
  'foreach ($shortcutPath in @($request.shortcutPaths)) {',
  '  try {',
  '    $shortcut = $shell.CreateShortcut($shortcutPath)',
  "    $shortcut.IconLocation = $iconPath + ',0'",
  '    $shortcut.Save()',
  '    $results.Add([pscustomobject]@{ path = [string]$shortcutPath; applied = $true })',
  '  } catch {',
  '    $results.Add([pscustomobject]@{ path = [string]$shortcutPath; applied = $false })',
  '  }',
  '}',
  'ConvertTo-Json -InputObject @($results) -Compress',
].join('; ');

function writeWindowsShortcutIcons({ shortcutPaths, iconPath, env = process.env, run = spawnSync } = {}) {
  if (!Array.isArray(shortcutPaths) || shortcutPaths.length === 0) return { applied: true, shortcutCount: 0 };
  try {
    if (!iconPath || !fs.statSync(iconPath).isFile()) return { applied: false, reason: 'icon-missing' };
  } catch {
    return { applied: false, reason: 'icon-missing' };
  }
  const systemRoot = String(env.SystemRoot || env.SYSTEMROOT || env.WINDIR || '');
  const powershell = systemRoot
    ? path.join(systemRoot, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe')
    : '';
  if (!powershell || !fs.existsSync(powershell)) return { applied: false, reason: 'shortcut-writer-missing' };

  const succeeded = [];
  const failed = [];
  // Keep every native command line bounded even if a user has copied the
  // shortcut into many folders. Per-link receipts let a protected shared
  // shortcut fail without blocking a writable desktop shortcut.
  for (let index = 0; index < shortcutPaths.length; index += 64) {
    const batch = shortcutPaths.slice(index, index + 64);
    const result = run(powershell, [
      '-NoLogo', '-NoProfile', '-NonInteractive', '-Command', WINDOWS_SHORTCUT_ICON_SCRIPT,
    ], {
      windowsHide: true,
      encoding: 'utf8',
      maxBuffer: 1024 * 1024,
      stdio: ['ignore', 'pipe', 'ignore'],
      env: {
        ...env,
        WORKASS_SHORTCUT_ICON_REQUEST: JSON.stringify({ iconPath, shortcutPaths: batch }),
      },
    });
    if (result.error || result.status !== 0) {
      failed.push(...batch);
      continue;
    }
    let receipts;
    try {
      const parsed = JSON.parse(String(result.stdout || '[]'));
      receipts = Array.isArray(parsed) ? parsed : parsed ? [parsed] : [];
    } catch {
      failed.push(...batch);
      continue;
    }
    const byPath = new Map(receipts.map((receipt) => [path.win32.normalize(String(receipt?.path || '')).toLowerCase(), receipt?.applied === true]));
    for (const shortcut of batch) {
      if (byPath.get(path.win32.normalize(shortcut).toLowerCase()) === true) succeeded.push(shortcut);
      else failed.push(shortcut);
    }
  }
  return {
    applied: succeeded.length > 0,
    shortcutCount: succeeded.length,
    shortcutPaths: succeeded,
    failedShortcutPaths: failed,
    ...(failed.length > 0 ? { reason: 'shortcut-write-partial' } : {}),
  };
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
  cacheToolPath = '',
  run = spawnSync,
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
  const discovery = resolveShortcutTargets({ roots, env, run });
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
    ? writeShortcutIcons({ shortcutPaths: pendingShortcuts, iconPath, env, run })
    : { applied: true, shortcutCount: 0, shortcutPaths: [] };
  const readback = readShortcutTargets({ roots, env, run });
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

  const systemRoot = String(env.SystemRoot || env.SYSTEMROOT || env.WINDIR || '');
  const cacheTool = cacheToolPath || (systemRoot ? path.win32.join(systemRoot, 'System32', 'ie4uinit.exe') : '');
  if (!cacheTool || !fs.existsSync(cacheTool)) {
    return { applied: false, reason: 'icon-cache-notifier-missing', shortcutCount: 0 };
  }
  const cacheResult = run(cacheTool, ['-show'], { windowsHide: true, stdio: 'ignore' });
  if (cacheResult.error || cacheResult.status !== 0) {
    return { applied: false, reason: 'icon-cache-refresh-failed', shortcutCount: 0 };
  }
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

const WINDOWS_SHORTCUT_ICON_WORKER = `
'use strict';
const { parentPort, workerData } = require('node:worker_threads');
try {
  const { refreshWindowsShortcutIcons } = require(workerData.modulePath);
  const receipt = refreshWindowsShortcutIcons(workerData.options);
  parentPort.postMessage({ ok: true, receipt });
} catch (error) {
  parentPort.postMessage({ ok: false, error: String(error && error.message || error) });
}
`;

// Shortcut discovery may traverse redirected desktops and invoke PowerShell.
// Keep that entirely off Electron's main thread so the first healthy window —
// including an update-recovery relaunch — never freezes while Explorer's icon
// cache is refreshed.
function refreshWindowsShortcutIconsAsync(options = {}, { WorkerClass = Worker } = {}) {
  const workerOptions = {
    platform: options.platform,
    isPackaged: options.isPackaged,
    executablePath: options.executablePath,
    resourcesPath: options.resourcesPath,
    dataRoot: options.dataRoot,
    appVersion: options.appVersion,
    roots: options.roots,
    markerFile: options.markerFile,
    iconCacheDir: options.iconCacheDir,
    cacheToolPath: options.cacheToolPath,
  };
  return new Promise((resolve) => {
    let settled = false;
    const finish = (receipt) => {
      if (settled) return;
      settled = true;
      resolve(receipt);
    };
    let worker;
    try {
      worker = new WorkerClass(WINDOWS_SHORTCUT_ICON_WORKER, {
        eval: true,
        workerData: { modulePath: __filename, options: workerOptions },
      });
    } catch (error) {
      finish({ applied: false, reason: 'icon-refresh-worker-failed', error: String(error?.message || error) });
      return;
    }
    worker.once('message', (message) => {
      if (message?.ok) finish(message.receipt);
      else finish({ applied: false, reason: 'icon-refresh-worker-failed', error: String(message?.error || 'unknown worker failure') });
    });
    worker.once('error', (error) => {
      finish({ applied: false, reason: 'icon-refresh-worker-failed', error: String(error?.message || error) });
    });
    worker.once('exit', (code) => {
      if (!settled) finish({ applied: false, reason: 'icon-refresh-worker-exited', exitCode: code });
    });
    worker.unref?.();
  });
}

module.exports = {
  applyMacDockIcon,
  refreshWindowsShortcutIcons,
  refreshWindowsShortcutIconsAsync,
  resolveWindowsShortcutTargets,
  resolveAppIconPath,
  resolveWindowFrameOptions,
  resolveWindowIconPath,
  writeWindowsShortcutIcons,
};
