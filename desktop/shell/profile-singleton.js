'use strict';

function shouldShowWindowOnCreate({ platform = process.platform, updateRelaunch = false } = {}) {
  // A Windows GUI process started by the independent updater must surface a
  // real window immediately. Deferring its first show to ready-to-show left a
  // healthy daemon and shell process behind with no visible desktop on some
  // machines. macOS keeps the existing hidden-until-ready launch to avoid a
  // white flash while its signed bundle is settling after the atomic swap.
  return platform === 'win32' || updateRelaunch !== true;
}

function focusPrimaryWindow(getWindows, app = null) {
  const windows = typeof getWindows === 'function' ? getWindows() : [];
  const win = Array.isArray(windows)
    ? windows.find((candidate) => candidate && !candidate.isDestroyed?.())
    : null;
  if (!win) return false;
  if (win.isMinimized?.()) win.restore?.();
  win.show?.();
  app?.focus?.({ steal: true });
  win.focus?.();
  return true;
}

function acquireProfileSingleton({ app, profile, dataRoot, getWindows }) {
  if (!app || typeof app.requestSingleInstanceLock !== 'function') return false;
  const acquired = app.requestSingleInstanceLock({
    workassProfile: String(profile || ''),
    workassDataRoot: String(dataRoot || ''),
  });
  if (!acquired) return false;
  app.on('second-instance', () => { focusPrimaryWindow(getWindows, app); });
  return true;
}

module.exports = { acquireProfileSingleton, focusPrimaryWindow, shouldShowWindowOnCreate };
