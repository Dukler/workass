// A hidden Electron window on the REAL renderer the scratch daemon serves, with a
// small control port so a shell can drive it and photograph the rail. It never
// shows on screen or takes focus.
const { app, BrowserWindow } = require('electron');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const url = process.env.RAIL_URL || 'https://127.0.0.1:18990/';
const outDir = process.env.RAIL_SHOTS;
const port = Number(process.env.RAIL_PORT || 18992);
app.commandLine.appendSwitch('ignore-certificate-errors');
app.setActivationPolicy?.('accessory');
app.whenReady().then(async () => {
  app.dock?.hide?.();
  const win = new BrowserWindow({
    show: false, focusable: false, width: 1500, height: 950, paintWhenInitiallyHidden: true,
    webPreferences: { partition: 'persist:rail-proof', backgroundThrottling: false },
  });
  win.webContents.on('console-message', (_e, level, message) => { if (level >= 2) console.error('[renderer]', message); });
  await win.loadURL(url);
  http.createServer(async (req, res) => {
    const parsed = new URL(req.url, 'http://x');
    const body = await new Promise((resolve) => { let b = ''; req.on('data', (c) => { b += c; }); req.on('end', () => resolve(b)); });
    try {
      if (parsed.pathname === '/shot') {
        const image = await win.webContents.capturePage();
        const name = parsed.searchParams.get('name') || 'shot';
        const file = path.join(outDir, name + '.png');
        fs.writeFileSync(file, image.toPNG());
        res.end(JSON.stringify({ file }));
      } else if (parsed.pathname === '/eval') {
        const value = await win.webContents.executeJavaScript(body, true);
        res.end(JSON.stringify({ value }));
      } else if (parsed.pathname === '/key') {
        for (const type of ['keyDown', 'char', 'keyUp']) win.webContents.sendInputEvent({ type, keyCode: parsed.searchParams.get('key') || 'Return' });
        res.end('{}');
      } else if (parsed.pathname === '/reload') {
        await win.loadURL(url);
        res.end('{}');
      } else { res.statusCode = 404; res.end('{}'); }
    } catch (error) { res.statusCode = 500; res.end(JSON.stringify({ error: String(error) })); }
  }).listen(port, '127.0.0.1');
});
