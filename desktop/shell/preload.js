'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('workassBrowser', {
  supported: true,
  activate: (payload) => ipcRenderer.invoke('workass-browser:activate', payload),
  resize: (payload) => ipcRenderer.invoke('workass-browser:resize', payload),
  hide: (chatId) => ipcRenderer.invoke('workass-browser:hide', chatId),
  close: (chatId) => ipcRenderer.invoke('workass-browser:close', chatId),
  command: (chatId, command, value) => ipcRenderer.invoke('workass-browser:command', { chatId, command, value }),
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

contextBridge.exposeInMainWorld('workassClipboard', {
  supported: true,
  copyImageAt: (payload) => ipcRenderer.invoke('workass-clipboard:copy-image-at', payload),
  openImageExternal: (payload) => ipcRenderer.invoke('workass-image:open-external', payload),
});

contextBridge.exposeInMainWorld('workassWindow', {
  platform: process.platform,
  control: (action) => ipcRenderer.invoke('workass-window:control', action),
});

contextBridge.exposeInMainWorld('workassRecovery', {
  restartDaemon: () => ipcRenderer.invoke('workass-recovery:restart-daemon'),
});
