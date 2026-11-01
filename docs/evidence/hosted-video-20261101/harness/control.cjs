// Is the file itself playable, and which response header decides it? Plain Electron with none of
// Workass: a bare node server with real Range support, optionally with the hosted CSP.
// electron control.cjs FILE OUT.json
const { app, BrowserWindow } = require('electron');
const http = require('node:http');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
app.setActivationPolicy?.('accessory');
app.setPath('userData', fs.mkdtempSync(path.join(os.tmpdir(), 'vrig-control-')));
const [file, out] = process.argv.slice(-2);
const size = fs.statSync(file).size;
process.on('uncaughtException', (e) => console.log('UNCAUGHT', e.stack));
app.on('window-all-closed', () => {});
const HOSTED = "sandbox allow-scripts allow-forms allow-downloads; connect-src https:; frame-ancestors 'self'";
const CASES = {
  'range, no csp': { range: true },
  'range, hosted csp (the sandbox)': { range: true, csp: HOSTED },
  'range, hosted csp without sandbox': { range: true, csp: "connect-src https:; frame-ancestors 'self'" },
  'range, media-only csp': { range: true, csp: "default-src 'none'; media-src 'self'; frame-ancestors 'self'" },
  'no range, no csp': { range: false },
};
const serve = (opts, log) => http.createServer((req, res) => {
  const headers = { 'Content-Type': 'video/mp4', 'X-Content-Type-Options': 'nosniff', ...(opts.csp ? { 'Content-Security-Policy': opts.csp } : {}), ...(opts.range ? { 'Accept-Ranges': 'bytes' } : {}) };
  const m = opts.range && /^bytes=(\d+)-(\d*)$/.exec(req.headers.range || '');
  log.push(req.headers.range || 'full');
  if (m) {
    const start = Number(m[1]); const end = m[2] ? Number(m[2]) : size - 1;
    res.writeHead(206, { ...headers, 'Content-Range': `bytes ${start}-${end}/${size}`, 'Content-Length': end - start + 1 });
    const stream = fs.createReadStream(file, { start, end }); stream.on('error', () => {}); res.on('close', () => stream.destroy()); res.on('error', () => {}); stream.pipe(res);
  } else { res.writeHead(200, { ...headers, 'Content-Length': size }); const stream = fs.createReadStream(file); stream.on('error', () => {}); res.on('close', () => stream.destroy()); res.on('error', () => {}); stream.pipe(res); }
});
const STATE = `(()=>{const v=document.querySelector('video');const r=v.getBoundingClientRect();return {origin:self.origin,crossorigin:v.getAttribute('crossorigin'),readyState:v.readyState,networkState:v.networkState,w:v.videoWidth,h:v.videoHeight,duration:v.duration,t:v.currentTime,paused:v.paused,rect:[Math.round(r.width),Math.round(r.height)]}})()`;
app.whenReady().then(async () => {
  const results = { file, size };
  for (const [name, opts] of Object.entries(CASES)) {
    const log = [];
    const server = serve(opts, log);
    await new Promise((r) => server.listen(0, '127.0.0.1', r));
    const win = new BrowserWindow({ show: false, focusable: false, width: 1280, height: 720, webPreferences: { backgroundThrottling: false } });
    win.webContents.setAudioMuted(true); // owner rule: playback tests run muted
    const failures = [];
    win.webContents.on('console-message', (ev) => { if (ev.level === 'error') failures.push(String(ev.message).slice(0, 200)); });
    await win.loadURL(`http://127.0.0.1:${server.address().port}/clip`).catch(() => {});
    await new Promise((r) => setTimeout(r, 2500));
    const first = await win.webContents.executeJavaScript(STATE);
    await win.webContents.executeJavaScript(`Promise.race([document.querySelector('video').play(),new Promise(r=>setTimeout(r,1500))]).catch(()=>{})`);
    await new Promise((r) => setTimeout(r, 2000));
    const second = await win.webContents.executeJavaScript(STATE);
    results[name] = { audioMuted: win.webContents.isAudioMuted(), first, later: { t: second.t, readyState: second.readyState }, plays: second.t > 0.2, ranges: log, errors: failures };
    console.log(name, JSON.stringify(results[name]));
    win.destroy(); server.closeAllConnections(); server.close();
  }
  fs.writeFileSync(out, JSON.stringify(results, null, 2));
  app.exit(0);
});
