'use strict';

// Preload of the separate browser window (browser-pip.js): live frames in,
// clicks and scrolls out. Nothing else is exposed.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('workassPip', {
  onFrame: (callback) => {
    const listener = (_event, frame) => {
      if (frame && frame.data) callback({ data: frame.data });
    };
    ipcRenderer.on('workass-pip:frame', listener);
    return () => ipcRenderer.removeListener('workass-pip:frame', listener);
  },
  input: (event) => ipcRenderer.send('workass-pip:input', event),
});
