'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('workassBrowser', {
  supported: true,
  activate: (payload) => ipcRenderer.invoke('workass-browser:activate', payload),
  resize: (payload) => ipcRenderer.invoke('workass-browser:resize', payload),
  hide: (chatId) => ipcRenderer.invoke('workass-browser:hide', chatId),
  popOut: (chatId) => ipcRenderer.invoke('workass-browser:pop-out', chatId),
  popIn: (chatId) => ipcRenderer.invoke('workass-browser:pop-in', chatId),
  close: (chatId) => ipcRenderer.invoke('workass-browser:close', chatId),
  command: (chatId, command, value) => ipcRenderer.invoke('workass-browser:command', { chatId, command, value }),
  setViewport: (chatId, width, height) => ipcRenderer.invoke('workass-browser:set-viewport', { chatId, width, height }),
  resetViewport: (chatId) => ipcRenderer.invoke('workass-browser:reset-viewport', { chatId }),
  onOpenRequest: (callback) => {
    const listener = (_event, chatId) => callback(chatId || undefined);
    ipcRenderer.on('workass-browser:open-request', listener);
    return () => ipcRenderer.removeListener('workass-browser:open-request', listener);
  },
  onState: (callback) => {
    const listener = (_event, state) => callback(state);
    ipcRenderer.on('workass-browser:state', listener);
    return () => ipcRenderer.removeListener('workass-browser:state', listener);
  },
});

// The main process owns the loopback capability. The renderer only receives
// narrowly scoped artifact work and returns remote daemon chunks over the
// authenticated MachineSocket it already owns.
contextBridge.exposeInMainWorld('workassArtifacts', {
  supported: true,
  onRequest: (callback) => {
    const listener = (_event, request) => callback(request);
    ipcRenderer.on('workass-artifact:request', listener);
    return () => ipcRenderer.removeListener('workass-artifact:request', listener);
  },
  onCancel: (callback) => {
    const listener = (_event, request) => callback(request);
    ipcRenderer.on('workass-artifact:cancel', listener);
    return () => ipcRenderer.removeListener('workass-artifact:cancel', listener);
  },
  reply: (payload) => ipcRenderer.invoke('workass-artifact:reply', payload),
});

contextBridge.exposeInMainWorld('workassClipboard', {
  supported: true,
  copyText: (text) => ipcRenderer.invoke('workass-clipboard:copy-text', text),
  copyImageAt: (payload) => ipcRenderer.invoke('workass-clipboard:copy-image-at', payload),
  openImageExternal: (payload) => ipcRenderer.invoke('workass-image:open-external', payload),
});

// main.js installs render-frame-guard.js on this window: every navigation of a
// chat HTML page frame is cancelled. Pages run their scripts only when this
// says so; without it (older shells, plain browsers) they render static. It
// says so only when the main process confirms the guard is attached to this
// very window right now: a hardcoded `true` ran page scripts even when the
// guard had never been installed (review 2026-10-21).
function renderFrameGuardInstalled() {
  try {
    return ipcRenderer.sendSync('workass-render-frames:guard') === true;
  } catch {
    return false;
  }
}

contextBridge.exposeInMainWorld('workassRenderFrames', {
  supported: true,
  navigationGuard: renderFrameGuardInstalled(),
});

contextBridge.exposeInMainWorld('workassWindow', {
  platform: process.platform,
});

contextBridge.exposeInMainWorld('workassRecovery', {
  restartDaemon: () => ipcRenderer.invoke('workass-recovery:restart-daemon'),
});

contextBridge.exposeInMainWorld('workassMachines', {
	trustEndpoint: (payload) => ipcRenderer.sendSync('workass-machines:trust-endpoint', payload),
});

contextBridge.exposeInMainWorld('workassUpdater', {
  getState: () => ipcRenderer.invoke('workass-updater:get-state'),
  diagnostics: () => ipcRenderer.invoke('workass-updater:diagnostics'),
  check: () => ipcRenderer.invoke('workass-updater:check'),
  apply: () => ipcRenderer.invoke('workass-updater:apply'),
  applyAuthorized: (payload) => ipcRenderer.invoke('workass-updater:apply-authorized', payload),
  onState: (callback) => {
    const listener = (_event, state) => callback(state);
    ipcRenderer.on('workass-updater:state', listener);
    return () => ipcRenderer.removeListener('workass-updater:state', listener);
  },
});
