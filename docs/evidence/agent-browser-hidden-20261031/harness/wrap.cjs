// The REAL shell (desktop/shell/main.js) and the real renderer, in the pinned Electron,
// that never takes the person's focus: the app window comes up with showInactive, as
// the rule for GUI test lanes requires. Adds a probe port; everything else is the app.
const Module = require('node:module');
const http = require('node:http');
const electron = require('electron');
const { app, BrowserWindow, webContents } = electron;
app.setActivationPolicy?.('accessory');
let first = true;
let mainWindow = null;
// A Proxy, not a subclass: Electron's own getAllWindows only lists exact BrowserWindows.
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
const windows = () => BrowserWindow.getAllWindows().map((win) => ({
  id: win.id, title: win.getTitle(), visible: win.isVisible(), focused: win.isFocused(), minimized: win.isMinimized(),
  fullscreen: win.isFullScreen(), bounds: win.getBounds(), main: win === mainWindow,
}));
http.createServer(async (req, res) => {
  const parsed = new URL(req.url, 'http://x');
  const body = await new Promise((resolve) => { let b = ''; req.on('data', (c) => { b += c; }); req.on('end', () => resolve(b)); });
  res.setHeader('content-type', 'application/json');
  try {
    if (parsed.pathname === '/windows') return res.end(JSON.stringify({ windows: windows(), pid: process.pid }));
    if (parsed.pathname === '/eval') return res.end(JSON.stringify({ value: await mainWindow.webContents.executeJavaScript(body, true) }));
    if (parsed.pathname === '/crash') { webContents.fromId(Number(parsed.searchParams.get('id'))).forcefullyCrashRenderer(); return res.end('{}'); }
    if (parsed.pathname === '/pageeval') return res.end(JSON.stringify({ value: await webContents.fromId(Number(parsed.searchParams.get('id'))).executeJavaScript(body, true) }));
    if (parsed.pathname === '/shot') {
      const image = await mainWindow.webContents.capturePage();
      require('node:fs').writeFileSync(parsed.searchParams.get('file'), image.toPNG());
      return res.end('{}');
    }
    res.statusCode = 404; res.end('{}');
  } catch (error) { res.statusCode = 500; res.end(JSON.stringify({ error: String(error) })); }
}).listen(Number(process.env.RIG_PROBE_PORT), '127.0.0.1');
require(`${process.env.RIG_REPO}/desktop/shell/main.js`);
