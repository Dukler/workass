// The real shell (RIG_SHELL_DIR/main.js) in the pinned Electron, never taking focus and never
// reaching the speakers: Chromium's own output is off, every page is muted from creation, and a
// page's setAudioMuted is recorded instead of applied. So the rig reads what the shell asked for.
const Module = require('node:module');
const http = require('node:http');
const electron = require('electron');
const { app, BrowserWindow, webContents } = electron;
app.setActivationPolicy?.('accessory');
app.commandLine.appendSwitch('mute-audio');
app.commandLine.appendSwitch('disable-backgrounding-occluded-windows');
let first = true;
let mainWindow = null;
const RigWindow = new Proxy(BrowserWindow, {
  construct(Target, [options = {}]) {
    const isMain = first;
    first = false;
    const win = new Target(isMain ? { ...options, show: false } : options);
    if (isMain) { mainWindow = win; win.setBounds({ x: 60, y: 60, width: 1400, height: 900 }); win.showInactive(); }
    return win;
  },
});
const realLoad = Module._load;
Module._load = function load(request, ...rest) {
  if (request === 'electron') return new Proxy(electron, { get: (target, key) => (key === 'BrowserWindow' ? RigWindow : target[key]) });
  return realLoad.call(this, request, ...rest);
};
const asked = new Map();
app.on('web-contents-created', (_e, wc) => {
  wc.setAudioMuted(true);
  const calls = [];
  asked.set(wc.id, calls);
  wc.setAudioMuted = (value) => { calls.push(value); };
});
http.createServer(async (req, res) => {
  const parsed = new URL(req.url, 'http://x');
  const body = await new Promise((resolve) => { let b = ''; req.on('data', (c) => { b += c; }); req.on('end', () => resolve(b)); });
  res.setHeader('content-type', 'application/json');
  try {
    if (parsed.pathname === '/setheight') { const b = mainWindow.getBounds(); mainWindow.setBounds({ ...b, height: Number(parsed.searchParams.get('h')) }); return res.end('{}'); }
    if (parsed.pathname === '/views') return res.end(JSON.stringify({ views: mainWindow.contentView.children.map((v) => v.getBounds()) }));
    if (parsed.pathname === '/windows') return res.end(JSON.stringify({ pid: process.pid }));
    if (parsed.pathname === '/contents') return res.end(JSON.stringify({ contents: webContents.getAllWebContents().map((w) => ({ id: w.id, type: w.getType(), url: w.getURL(), reallyMuted: w.isAudioMuted(), audible: w.isCurrentlyAudible(), asked: asked.get(w.id) || [] })) }));
    if (parsed.pathname === '/eval') return res.end(JSON.stringify({ value: await mainWindow.webContents.executeJavaScript(body, true) }));
    if (parsed.pathname === '/pageeval') return res.end(JSON.stringify({ value: await webContents.fromId(Number(parsed.searchParams.get('id'))).executeJavaScript(body, true) }));
    res.statusCode = 404; res.end('{}');
  } catch (error) { res.statusCode = 500; res.end(JSON.stringify({ error: String(error) })); }
}).listen(Number(process.env.RIG_PROBE_PORT), '127.0.0.1');
require(`${process.env.RIG_SHELL_DIR}/main.js`);
