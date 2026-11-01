// The media policy replaces the sandbox, so markup named like media must not run.
// Plain Electron, muted, with the exact headers a hosted .mp4 and .webm get.
const { app, BrowserWindow } = require('electron');
const http = require('node:http');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
app.setActivationPolicy?.('accessory');
app.setPath('userData', fs.mkdtempSync(path.join(os.tmpdir(), 'vrig-disguised-')));
app.on('window-all-closed', () => {});
const out = process.argv[process.argv.length - 1];
const server = http.createServer((req, res) => {
  res.writeHead(200, { 'Content-Type': req.url.endsWith('webm') ? 'video/webm' : 'video/mp4', 'X-Content-Type-Options': 'nosniff',
    'Content-Security-Policy': "default-src 'none'; media-src 'self'; frame-ancestors 'self'" });
  res.end('<!doctype html><title>clean</title><script>document.title="SCRIPT RAN";fetch("/ran")</script><h1>html</h1>');
});
app.whenReady().then(async () => {
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const results = {};
  for (const name of ['evil.mp4', 'evil.webm']) {
    const win = new BrowserWindow({ show: false, focusable: false, webPreferences: { backgroundThrottling: false } });
    win.webContents.setAudioMuted(true);
    await win.loadURL(`http://127.0.0.1:${server.address().port}/${name}`).catch(() => {});
    await new Promise((r) => setTimeout(r, 1500));
    results[name] = await win.webContents.executeJavaScript(`({contentType:document.contentType,title:document.title,scripts:document.scripts.length,h1:document.querySelectorAll('h1').length,video:!!document.querySelector('video')})`);
    win.destroy();
  }
  fs.writeFileSync(out, JSON.stringify(results, null, 2));
  console.log(JSON.stringify(results));
  server.closeAllConnections(); server.close();
  app.exit(0);
});
