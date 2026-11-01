// The REAL shell (desktop/shell/main.js) and the real renderer, in the pinned Electron, never
// taking the person's focus. Adds a probe port and a passive network log of every artifact
// request; everything else is the app. Run with RIG_SHELL_DIR to run another checkout's shell.
const Module = require('node:module');
const http = require('node:http');
const fs = require('node:fs');
const electron = require('electron');
const { app, BrowserWindow, webContents } = electron;
app.setActivationPolicy?.('accessory');
// The rig window never takes focus and is usually covered. A covered window makes its pages
// document.hidden and stops painting, which pauses a video-only clip and yields stale captures.
app.commandLine.appendSwitch('disable-backgrounding-occluded-windows');
let first = true;
let mainWindow = null;
const browserStates = [];
const browserCalls = [];
const RigWindow = new Proxy(BrowserWindow, {
  construct(Target, [options = {}]) {
    const isMain = first;
    first = false;
    const win = new Target(isMain ? { ...options, show: false } : options);
    if (isMain) { const send = win.webContents.send.bind(win.webContents); win.webContents.send = (channel, payload) => { if (channel === 'workass-browser:state' && payload) browserStates.push({ at: Date.now(), url: payload.url, loading: payload.loading, error: payload.error, viewport: payload.viewport, effective: payload.effectiveViewport && [payload.effectiveViewport.width, payload.effectiveViewport.height], gen: payload.viewportGeneration, bounds: payload.bounds }); return send(channel, payload); }; }
    if (isMain) { mainWindow = win; win.setBounds({ x: 60, y: 60, width: 1400, height: 900 }); win.showInactive(); }
    return win;
  },
});
const realLoad = Module._load;
Module._load = function load(request, ...rest) {
  if (request === 'electron') return new Proxy(electron, { get: (target, key) => (key === 'BrowserWindow' ? RigWindow : target[key]) });
  return realLoad.call(this, request, ...rest);
};
const net = [];
const consoleLog = [];
const interesting = (url) => /\/workass\/(connected-)?artifacts\//.test(url);
const record = (phase, d) => {
  if (!interesting(d.url)) return;
  const h = d.responseHeaders || {};
  const pick = (name) => { const k = Object.keys(h).find((x) => x.toLowerCase() === name); return k ? h[k][0] : undefined; };
  const rq = d.requestHeaders || {};
  net.push({ at: Date.now(), phase, type: d.resourceType, url: new URL(d.url).pathname, range: rq.Range || rq.range,
    capability: phase === 'send' ? Object.keys(rq).some((k) => k.toLowerCase() === 'x-workass-artifact-access') : undefined,
    frameUrl: d.frame ? d.frame.url : undefined, hasFrame: !!d.frame, wc: d.webContentsId,
    status: d.statusCode, contentType: pick('content-type'), acceptRanges: pick('accept-ranges'), contentRange: pick('content-range'), length: pick('content-length'),
    error: d.error });
};
app.on('session-created', (s) => {
  s.webRequest.onSendHeaders((d) => record('send', d));
  s.webRequest.onCompleted((d) => record('done', d));
  s.webRequest.onErrorOccurred((d) => record('error', d));
});
app.on('web-contents-created', (_e, wc) => {
  // Owner rule 2026-11-01: playback tests run muted. From creation, so autoplay never sounds.
  wc.setAudioMuted(true);
  wc.on('console-message', (ev) => { if (wc !== mainWindow?.webContents) consoleLog.push({ wc: wc.id, level: ev.level, message: String(ev.message).slice(0, 300) }); });
});
const realHandle = electron.ipcMain.handle.bind(electron.ipcMain);
electron.ipcMain.handle = (channel, fn) => realHandle(channel, async (event, ...args) => {
  const row = /^workass-browser:(activate|resize|command|deactivate)/.test(channel) ? { at: Date.now(), channel, args: JSON.stringify(args).slice(0, 220) } : null;
  if (row) browserCalls.push(row);
  try { const r = await fn(event, ...args); if (row) row.doneAt = Date.now(); return r; } catch (e) { if (row) { row.doneAt = Date.now(); row.error = String(e).slice(0, 160); } throw e; }
});
http.createServer(async (req, res) => {
  const parsed = new URL(req.url, 'http://x');
  const body = await new Promise((resolve) => { let b = ''; req.on('data', (c) => { b += c; }); req.on('end', () => resolve(b)); });
  res.setHeader('content-type', 'application/json');
  try {
    if (parsed.pathname === '/windows') return res.end(JSON.stringify({ pid: process.pid, windows: BrowserWindow.getAllWindows().map((w) => ({ visible: w.isVisible(), bounds: w.getBounds(), main: w === mainWindow })) }));
    if (parsed.pathname === '/contents') return res.end(JSON.stringify({ contents: webContents.getAllWebContents().map((w) => ({ id: w.id, type: w.getType(), url: w.getURL(), audioMuted: w.isAudioMuted(), audible: w.isCurrentlyAudible() })) }));
    if (parsed.pathname === '/net') return res.end(JSON.stringify({ net, console: consoleLog }));
    if (parsed.pathname === '/browser') return res.end(JSON.stringify({ states: browserStates, calls: browserCalls }));
    if (parsed.pathname === '/net-reset') { net.length = 0; consoleLog.length = 0; return res.end('{}'); }
    if (parsed.pathname === '/eval') return res.end(JSON.stringify({ value: await mainWindow.webContents.executeJavaScript(body, true) }));
    if (parsed.pathname === '/pageeval') return res.end(JSON.stringify({ value: await webContents.fromId(Number(parsed.searchParams.get('id'))).executeJavaScript(body, true) }));
    if (parsed.pathname === '/pagecapture') {
      const image = await webContents.fromId(Number(parsed.searchParams.get('id'))).capturePage();
      fs.writeFileSync(parsed.searchParams.get('file'), image.toPNG());
      return res.end(JSON.stringify(image.getSize()));
    }
    res.statusCode = 404; res.end('{}');
  } catch (error) { res.statusCode = 500; res.end(JSON.stringify({ error: String(error) })); }
}).listen(Number(process.env.RIG_PROBE_PORT), '127.0.0.1');
require(`${process.env.RIG_SHELL_DIR}/main.js`);
