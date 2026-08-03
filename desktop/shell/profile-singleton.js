'use strict';

function focusPrimaryWindow(getWindows) {
  const windows = typeof getWindows === 'function' ? getWindows() : [];
  const win = Array.isArray(windows)
    ? windows.find((candidate) => candidate && !candidate.isDestroyed?.())
    : null;
  if (!win) return false;
  if (win.isMinimized?.()) win.restore?.();
  win.show?.();
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
  app.on('second-instance', () => { focusPrimaryWindow(getWindows); });
  return true;
}

module.exports = { acquireProfileSingleton, focusPrimaryWindow };
