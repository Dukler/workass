'use strict';

// Electron-only renderer host. It keeps the UI bytes outside the always-on Go
// daemon so rebuilding/restarting the shell cannot interrupt daemon-owned ACP
// turns. All application state and wire invokes still go to the daemon through
// the byte-transparent HTTP/WebSocket proxy below.
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const net = require('node:net');
const path = require('node:path');
const tls = require('node:tls');

const MIME = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.ico': 'image/x-icon',
  '.jpg': 'image/jpeg',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.webp': 'image/webp',
  '.woff2': 'font/woff2',
};

function controllerMigration(recoverController = false) {
  return `<script>(()=>{
  const key = 'workass.shell.controllerMigration.v1';
  let recoveryAvailable = ${recoverController === true ? 'true' : 'false'};
  const post = (path, body) => fetch(path, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
  }).catch(() => {});
  const report = (controller) => fetch('/__workass-shell/controller', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ controller: !!controller })
  }).catch(() => {});
  if (!window.api || typeof window.api.onLanAccessState !== 'function') return;
  window.api.onLanAccessState(async (state) => {
    if (!state || state.state !== 'approved') return;
    const deviceId = String(state.deviceId || '');
    let migratedFor = '';
    try { migratedFor = localStorage.getItem(key) || ''; } catch (_) {}
    if (state.controller) {
      recoveryAvailable = false;
      try { localStorage.setItem(key, deviceId || 'done'); } catch (_) {}
      report(true);
      return;
    }
    // A completed migration belongs to one concrete device. Do not let that
    // device steal control back later, but do recover after Electron's device
    // identity was replaced (including the legacy literal "done" marker).
    if (deviceId && migratedFor === deviceId && !recoveryAvailable) { report(false); return; }
    // An explicitly requested shell rebuild may consume one recovery attempt
    // when an older duplicate left the daemon lease on a dead device identity.
    // Normal launches inject false, and even a recovery launch cannot steal
    // control back again after its first approved connection.
    recoveryAvailable = false;
    if (typeof window.api.lanTakeControl !== 'function') return;
    try {
      const result = await window.api.lanTakeControl();
      if (result && result.controller) {
        localStorage.setItem(key, deviceId || 'done');
        report(true);
      } else report(false);
    } catch (_) {}
  });
  if (typeof window.api.onChatCatalog === 'function') {
    window.api.onChatCatalog((catalog) => {
      post('/__workass-shell/catalog', {
        groups: catalog && Array.isArray(catalog.groups) ? catalog.groups.map((group) => ({
          providerId: String(group && group.providerId || ''),
          status: String(group && group.status || ''),
          models: Array.isArray(group && group.models) ? group.models.map((model) => ({
            modelId: String(model && model.modelId || ''), name: String(model && model.name || '')
          })) : []
        })) : []
      });
    });
  }
})();</script>`;
}

function injectBridge(html, { recoverController = false } = {}) {
  const scripts = `  <script src="/lan-bridge.js"></script>\n  ${controllerMigration(recoverController)}\n`;
  return html.includes('</head>') ? html.replace('</head>', scripts + '</head>') : scripts + html;
}

function safeStaticPath(rendererDir, pathname) {
  let decoded;
  try { decoded = decodeURIComponent(pathname); } catch { return ''; }
  const rel = decoded === '/' || decoded === '' ? 'index.html' : decoded.replace(/^\/+/, '');
  const root = path.resolve(rendererDir);
  const file = path.resolve(root, rel);
  if (file !== root && !file.startsWith(root + path.sep)) return '';
  return file;
}

function proxyHTTP(req, res, daemon) {
  const transport = daemon.protocol === 'https:' ? https : http;
  const upstream = transport.request({
    protocol: daemon.protocol,
    hostname: daemon.hostname,
    port: daemon.port || (daemon.protocol === 'https:' ? 443 : 80),
    method: req.method,
    path: req.url,
    headers: { ...req.headers, host: daemon.host },
    ...(daemon.ca ? { ca: daemon.ca } : {}),
  }, (reply) => {
    res.writeHead(reply.statusCode || 502, reply.headers);
    reply.pipe(res);
  });
  upstream.on('error', (err) => {
    if (!res.headersSent) res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end(`daemon unavailable: ${err.message}`);
  });
  req.pipe(upstream);
}

function tlsConnectOptions(daemon, port) {
  const options = { host: daemon.hostname, port, ...(daemon.ca ? { ca: daemon.ca } : {}) };
  // SNI is a DNS-name extension. Node 25 rejects an IP literal here, while
  // Electron 43 still accepts it with a deprecation warning before the
  // connection fails later. Omitting SNI for an IP keeps normal certificate
  // verification: checkServerIdentity still validates the certificate's IP
  // subjectAltName against `host`.
  if (net.isIP(daemon.hostname) === 0) options.servername = daemon.hostname;
  return options;
}

function proxyUpgrade(req, socket, head, daemon) {
  const secure = daemon.protocol === 'https:';
  const port = Number(daemon.port || (secure ? 443 : 80));
  const upstream = secure
    ? tls.connect(tlsConnectOptions(daemon, port))
    : net.connect({ host: daemon.hostname, port });
  const fail = () => { try { socket.destroy(); } catch (_) {} };
  upstream.once('error', fail);
  socket.once('error', () => { try { upstream.destroy(); } catch (_) {} });
  socket.once('close', () => { try { upstream.destroy(); } catch (_) {} });
  upstream.once('close', () => { try { socket.destroy(); } catch (_) {} });
  upstream.once(secure ? 'secureConnect' : 'connect', () => {
    const headers = [];
    for (const [name, value] of Object.entries(req.headers)) {
      if (value == null) continue;
      const rendered = name.toLowerCase() === 'host' ? daemon.host : (Array.isArray(value) ? value.join(', ') : value);
      headers.push(`${name}: ${rendered}`);
    }
    const prefix = daemon.pathname === '/' ? '' : daemon.pathname.replace(/\/$/, '');
    upstream.write(`${req.method} ${prefix}${req.url} HTTP/${req.httpVersion}\r\n${headers.join('\r\n')}\r\n\r\n`);
    if (head && head.length) upstream.write(head);
    socket.pipe(upstream).pipe(socket);
  });
}

function createViewServer({ daemonURL, daemonCAPath = '', rendererDir, host = '127.0.0.1', port = 8799, runtimeVersion = null, appVersion = null, recoverController = false }) {
  const daemon = new URL(daemonURL);
  if (daemon.protocol !== 'http:' && daemon.protocol !== 'https:') {
    return Promise.reject(new Error(`unsupported daemon protocol: ${daemon.protocol}`));
  }
  if (daemon.protocol === 'https:' && daemonCAPath) {
    try { daemon.ca = fs.readFileSync(daemonCAPath); } catch (err) {
      return Promise.reject(new Error(`daemon certificate unavailable: ${err.message}`));
    }
  }
  const root = path.resolve(rendererDir);
  const indexPath = path.join(root, 'index.html');
  if (!fs.existsSync(indexPath)) {
    return Promise.reject(new Error(`renderer build missing: ${indexPath}`));
  }

  const shellStatus = { controller: null, reportedAt: null, catalog: null, claude: null, browser: null };
  // Dev screenshot hook (2026-07-12): the shell registers a capture fn (the real
  // main-window webContents.capturePage) so the build workflow / an agent can GET
  // a PNG of exactly what the user sees — no more iterating on the UI blind.
  let captureFn = null;
  let probeFn = null;
  let perfFn = null;
  let reloadFn = null;
	let recoveryFn = null;
  const server = http.createServer((req, res) => {
    const parsedUrl = new URL(req.url || '/', 'http://shell.local');
    const pathname = parsedUrl.pathname;
    if (pathname === '/__workass-shell/probe' && req.method === 'GET') {
      if (typeof probeFn !== 'function') { res.writeHead(503, { 'Content-Type': 'text/plain' }); res.end('renderer window not ready'); return; }
      const selector = parsedUrl.searchParams.get('selector') || '*';
      Promise.resolve().then(() => probeFn(selector)).then((data) => {
        const body = JSON.stringify(data);
        res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
        res.end(body);
      }).catch((err) => { if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'text/plain' }); res.end(`probe failed: ${err && err.message || err}`); });
      return;
    }
    if (pathname === '/__workass-shell/perf' && req.method === 'GET') {
      if (typeof perfFn !== 'function') { res.writeHead(503, { 'Content-Type': 'text/plain' }); res.end('renderer window not ready'); return; }
      const action = parsedUrl.searchParams.get('action') || 'read';
      Promise.resolve().then(() => perfFn(action)).then((data) => {
        const body = JSON.stringify(data);
        res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
        res.end(body);
      }).catch((err) => { if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'text/plain' }); res.end(`perf failed: ${err && err.message || err}`); });
      return;
    }
    if (pathname === '/__workass-shell/screenshot' && req.method === 'GET') {
      if (typeof captureFn !== 'function') {
        res.writeHead(503, { 'Content-Type': 'text/plain; charset=utf-8' });
        res.end('renderer window not ready');
        return;
      }
      // Optional `?click=<css selector>` drives the UI into a specific state
      // (e.g. toggle the rail) before capturing — so states can be reviewed, not
      // guessed. Constrained to a click; no arbitrary JS. Localhost dev only.
      const opts = {
        click: parsedUrl.searchParams.get('click') || null,
        event: parsedUrl.searchParams.get('event') || 'click',
        target: parsedUrl.searchParams.get('target') || null,
        value: (parsedUrl.searchParams.get('value') || '').slice(0, 4096) || null,
      };
      Promise.resolve().then(() => captureFn(opts)).then((png) => {
        if (!Buffer.isBuffer(png) || png.length === 0) throw new Error('empty capture');
        res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'image/png', 'Content-Length': png.length });
        res.end(png);
      }).catch((err) => {
        if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'text/plain; charset=utf-8' });
        res.end(`capture failed: ${err && err.message || err}`);
      });
      return;
    }
    // Reload the renderer IN PLACE. Delivering a new renderer used to mean
    // relaunching Electron, and a relaunch is what can lose controller authority
    // (2026-07-26: a promotion left the app up with an empty catalog and no way
    // back). A window reload re-runs the injected bridge, so it dials a fresh
    // socket and re-hydrates without the shell — or the daemon — going anywhere.
    if (pathname === '/__workass-shell/reload') {
      // POST only, and said out loud: without this the path falls through to the
      // daemon proxy, which answers 200 for a GET that reloaded nothing.
      if (req.method !== 'POST') { res.writeHead(405, { 'Allow': 'POST', 'Content-Type': 'text/plain' }); res.end('reload requires POST'); return; }
      if (typeof reloadFn !== 'function') { res.writeHead(503, { 'Content-Type': 'text/plain' }); res.end('renderer window not ready'); return; }
      // The renderer's own ⌘, "Recargar" clears this marker before reloading so
      // the migration script may re-take a stranded lease; the automation path
      // has to be able to ask for the same thing, or it is a weaker reload.
      const recoverController = parsedUrl.searchParams.get('recoverController') === '1';
      Promise.resolve().then(() => reloadFn({ recoverController })).then((data) => {
        const body = JSON.stringify(data || { reloaded: true });
        res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
        res.end(body);
      }).catch((err) => { if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'text/plain' }); res.end(`reload failed: ${err && err.message || err}`); });
      return;
    }
    if (pathname === '/__workass-shell/recover') {
      if (req.method !== 'POST') { res.writeHead(405, { 'Allow': 'POST', 'Content-Type': 'text/plain' }); res.end('recovery requires POST'); return; }
      if (typeof recoveryFn !== 'function') { res.writeHead(503, { 'Content-Type': 'text/plain' }); res.end('daemon recovery is not ready'); return; }
      Promise.resolve().then(() => recoveryFn()).then((data) => {
        const body = JSON.stringify(data || { recovered: true });
        res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
        res.end(body);
      }).catch((err) => { if (!res.headersSent) res.writeHead(500, { 'Content-Type': 'text/plain' }); res.end(`recovery failed: ${err && err.message || err}`); });
      return;
    }
    if (pathname === '/__workass-shell/status' && req.method === 'GET') {
      const body = JSON.stringify({
        controller: shellStatus.controller,
        reportedAt: shellStatus.reportedAt,
        catalog: shellStatus.catalog,
        claude: shellStatus.claude,
        browser: shellStatus.browser,
        electronVersion: runtimeVersion,
        appVersion,
        rendererDir: root,
        daemonOrigin: daemon.origin,
      });
      res.writeHead(200, { 'Cache-Control': 'no-store', 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
      res.end(body);
      return;
    }
    if (pathname === '/__workass-shell/catalog' && req.method === 'POST') {
      let body = '';
      req.setEncoding('utf8');
      req.on('data', (chunk) => { if (body.length < 8192) body += chunk; });
      req.on('end', () => {
        try {
          const payload = JSON.parse(body || '{}');
          const groups = Array.isArray(payload.groups) ? payload.groups.slice(0, 32).map((group) => ({
            providerId: String(group && group.providerId || '').slice(0, 80),
            status: String(group && group.status || '').slice(0, 40),
            models: Array.isArray(group && group.models) ? group.models.slice(0, 64).map((model) => ({
              modelId: String(model && model.modelId || '').slice(0, 160),
              name: String(model && model.name || '').slice(0, 160),
            })).filter((model) => model.modelId) : [],
          })).filter((group) => group.providerId) : [];
          const reportedAt = new Date().toISOString();
          shellStatus.catalog = {
            reportedAt,
            readyModelCount: groups.filter((group) => group.status === 'ready')
              .reduce((total, group) => total + group.models.length, 0),
            groups,
          };
          const claude = groups.find((group) => group.providerId === 'claude');
          shellStatus.claude = claude ? { status: claude.status, models: claude.models } : null;
          res.writeHead(204); res.end();
        } catch {
          res.writeHead(400); res.end();
        }
      });
      return;
    }
    if (pathname === '/__workass-shell/controller' && req.method === 'POST') {
      let body = '';
      req.setEncoding('utf8');
      req.on('data', (chunk) => { if (body.length < 1024) body += chunk; });
      req.on('end', () => {
        try {
          const payload = JSON.parse(body || '{}');
          shellStatus.controller = payload.controller === true;
          shellStatus.reportedAt = new Date().toISOString();
          res.writeHead(204); res.end();
        } catch {
          res.writeHead(400); res.end();
        }
      });
      return;
    }
    const file = safeStaticPath(root, pathname);
    if (file && fs.existsSync(file) && fs.statSync(file).isFile()) {
      try {
        if (file === indexPath) {
          const html = injectBridge(fs.readFileSync(file, 'utf8'), { recoverController });
          res.writeHead(200, {
            'Cache-Control': 'no-store',
            'Content-Type': MIME['.html'],
            'Content-Length': Buffer.byteLength(html),
          });
          res.end(html);
          return;
        }
        const data = fs.readFileSync(file);
        res.writeHead(200, {
          'Cache-Control': 'no-store',
          'Content-Type': MIME[path.extname(file).toLowerCase()] || 'application/octet-stream',
          'Content-Length': data.length,
        });
        res.end(data);
        return;
      } catch (err) {
        res.writeHead(500, { 'Content-Type': 'text/plain; charset=utf-8' });
        res.end(`renderer read failed: ${err.message}`);
        return;
      }
    }
    proxyHTTP(req, res, daemon);
  });
  const sockets = new Set();
  server.on('connection', (socket) => {
    sockets.add(socket);
    socket.once('close', () => sockets.delete(socket));
  });
  server.on('upgrade', (req, socket, head) => proxyUpgrade(req, socket, head, daemon));

  return new Promise((resolve, reject) => {
    const onError = (err) => reject(err);
    server.once('error', onError);
    server.listen(port, host, () => {
      server.off('error', onError);
      const addr = server.address();
      const actualPort = typeof addr === 'object' && addr ? addr.port : port;
      resolve({
        server,
        url: `http://${host}:${actualPort}`,
        reportBrowserState: (state) => {
          const raw = state && typeof state === 'object' ? state : {};
          shellStatus.browser = {
            chatId: String(raw.chatId || '').slice(0, 200),
            persistent: raw.persistent === true,
            cdpAttached: raw.cdpAttached === true,
            agentControl: raw.agentControl === true,
            loading: raw.loading === true,
            hasError: !!raw.error,
            reportedAt: new Date().toISOString(),
          };
        },
        isController: () => shellStatus.controller === true,
        // Registered by main.js once the window exists: () => Promise<Buffer PNG>.
        setCapture: (fn) => { captureFn = typeof fn === 'function' ? fn : null; },
        // (selector) => Promise<JSON> — live DOM rects/visibility, to debug layout.
        setProbe: (fn) => { probeFn = typeof fn === 'function' ? fn : null; },
        // ('start'|'read'|'stop') => Promise<JSON> — bounded main-renderer frame
        // and Long Task metrics for real-browser performance gates.
        setPerf: (fn) => { perfFn = typeof fn === 'function' ? fn : null; },
        // ({recoverController}) => Promise<JSON> — reload the window in place, so
        // a renderer promotion never needs the shell relaunch that can strand the
        // controller lease.
        setReload: (fn) => { reloadFn = typeof fn === 'function' ? fn : null; },
		setRecovery: (fn) => { recoveryFn = typeof fn === 'function' ? fn : null; },
        close: () => new Promise((done) => {
          for (const socket of sockets) socket.destroy();
          server.close(() => done());
        }),
      });
    });
  });
}

module.exports = { createViewServer, injectBridge, safeStaticPath, tlsConnectOptions };
