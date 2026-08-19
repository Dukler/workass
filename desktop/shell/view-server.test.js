'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const { createViewServer, injectBridge, safeStaticPath, tlsConnectOptions } = require('./view-server');

test('injects bridge and one-time controller migration', () => {
  const html = injectBridge('<!doctype html><head></head><body></body>');
  assert.match(html, /<script src="\/lan-bridge\.js"><\/script>/);
  assert.match(html, /workass\.shell\.controllerMigration\.v1/);
  assert.match(html, /lanTakeControl/);
  assert.match(html, /__workass-shell\/controller/);
  assert.match(html, /__workass-shell\/catalog/);
});

test('controller migration marker does not strand an approved replacement shell', async () => {
  const html = injectBridge('<!doctype html><head></head><body></body>');
  const scripts = [...html.matchAll(/<script(?: [^>]*)?>([\s\S]*?)<\/script>/g)];
  const migration = scripts.map((match) => match[1]).find((source) => source.includes('controllerMigration'));
  assert.ok(migration);

  let accessListener = null;
  let takeControlCalls = 0;
  const reports = [];
  const localStorage = {
    getItem: () => 'done',
    setItem: () => {},
  };
  const fetch = async (url, options) => {
    if (url === '/__workass-shell/controller') reports.push(JSON.parse(options.body));
    return { ok: true };
  };
  const window = {
    api: {
      onLanAccessState: (listener) => { accessListener = listener; },
      lanTakeControl: async () => { takeControlCalls += 1; return { controller: true }; },
    },
  };

  vm.runInNewContext(migration, { fetch, localStorage, window });
  assert.equal(typeof accessListener, 'function');
  await accessListener({ state: 'approved', controller: false, deviceId: 'replacement-shell' });

  assert.equal(takeControlCalls, 1);
  assert.deepEqual(reports, [{ controller: true }]);
});

test('controller migration does not steal control back for the same device', async () => {
  const html = injectBridge('<!doctype html><head></head><body></body>');
  const scripts = [...html.matchAll(/<script(?: [^>]*)?>([\s\S]*?)<\/script>/g)];
  const migration = scripts.map((match) => match[1]).find((source) => source.includes('controllerMigration'));
  assert.ok(migration);

  let accessListener = null;
  let takeControlCalls = 0;
  const reports = [];
  const localStorage = {
    getItem: () => 'current-shell',
    setItem: () => {},
  };
  const fetch = async (url, options) => {
    if (url === '/__workass-shell/controller') reports.push(JSON.parse(options.body));
    return { ok: true };
  };
  const window = {
    api: {
      onLanAccessState: (listener) => { accessListener = listener; },
      lanTakeControl: async () => { takeControlCalls += 1; return { controller: true }; },
    },
  };

  vm.runInNewContext(migration, { fetch, localStorage, window });
  await accessListener({ state: 'approved', controller: false, deviceId: 'current-shell' });

  assert.equal(takeControlCalls, 0);
  assert.deepEqual(reports, [{ controller: false }]);
});

test('an explicit rebuild recovery may reclaim the same device exactly once', async () => {
  const html = injectBridge('<!doctype html><head></head><body></body>', { recoverController: true });
  const scripts = [...html.matchAll(/<script(?: [^>]*)?>([\s\S]*?)<\/script>/g)];
  const migration = scripts.map((match) => match[1]).find((source) => source.includes('controllerMigration'));
  assert.ok(migration);

  let accessListener = null;
  let takeControlCalls = 0;
  const reports = [];
  const localStorage = {
    getItem: () => 'current-shell',
    setItem: () => {},
  };
  const fetch = async (url, options) => {
    if (url === '/__workass-shell/controller') reports.push(JSON.parse(options.body));
    return { ok: true };
  };
  const window = {
    api: {
      onLanAccessState: (listener) => { accessListener = listener; },
      lanTakeControl: async () => { takeControlCalls += 1; return { controller: true }; },
    },
  };

  vm.runInNewContext(migration, { fetch, localStorage, window });
  await accessListener({ state: 'approved', controller: false, deviceId: 'current-shell' });
  await accessListener({ state: 'approved', controller: false, deviceId: 'current-shell' });

  assert.equal(takeControlCalls, 1);
  assert.deepEqual(reports, [{ controller: true }, { controller: false }]);
});

test('rejects renderer path traversal', () => {
  const root = path.join(os.tmpdir(), 'renderer-root');
  assert.equal(safeStaticPath(root, '/../../outside'), '');
  assert.equal(safeStaticPath(root, '/assets/app.js'), path.join(root, 'assets', 'app.js'));
});

test('TLS proxy never sends an IP literal as SNI', () => {
  const ca = Buffer.from('test-ca');
  assert.deepEqual(tlsConnectOptions({ hostname: '127.0.0.1', ca }, 18788), {
    host: '127.0.0.1', port: 18788, ca,
  });
  assert.deepEqual(tlsConnectOptions({ hostname: 'workass.local', ca }, 18788), {
    host: 'workass.local', port: 18788, ca, servername: 'workass.local',
  });
});

test('serves local renderer, proxies HTTP, and tunnels WebSocket upgrade', async (t) => {
  const renderer = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-shell-renderer-'));
  fs.mkdirSync(path.join(renderer, 'assets'));
  fs.writeFileSync(path.join(renderer, 'index.html'), '<!doctype html><head></head><body>local-index</body>');
  fs.writeFileSync(path.join(renderer, 'assets', 'app.js'), 'window.localBundle=true;');
  t.after(() => fs.rmSync(renderer, { recursive: true, force: true }));

  const daemon = http.createServer((req, res) => {
    if (req.url === '/lan-bridge.js') {
      res.writeHead(200, { 'Content-Type': 'text/javascript' });
      res.end('window.daemonBridge=true;');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end('daemon-http');
  });
  daemon.on('upgrade', (_req, socket) => {
    socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nUPSTREAM');
  });
  await new Promise((resolve) => daemon.listen(0, '127.0.0.1', resolve));
  t.after(() => daemon.close());
  const daemonPort = daemon.address().port;

  const view = await createViewServer({ daemonURL: `http://127.0.0.1:${daemonPort}`, rendererDir: renderer, port: 0, runtimeVersion: '43.1.1' });
  t.after(() => view.close());
  const index = await fetch(view.url + '/').then((r) => r.text());
  assert.match(index, /local-index/);
  assert.match(index, /lan-bridge\.js/);
  assert.equal(await fetch(view.url + '/assets/app.js').then((r) => r.text()), 'window.localBundle=true;');
  assert.equal(await fetch(view.url + '/lan-bridge.js').then((r) => r.text()), 'window.daemonBridge=true;');
  assert.equal(await fetch(view.url + '/daemon-only').then((r) => r.text()), 'daemon-http');
  let status = await fetch(view.url + '/__workass-shell/status').then((r) => r.json());
  assert.equal(status.controller, null);
  assert.equal(status.browser, null);
  assert.equal(status.windowVisible, false);
  assert.equal(status.window, null);
  assert.equal(status.electronVersion, '43.1.1');
  assert.equal(status.daemonOrigin, `http://127.0.0.1:${daemonPort}`);
  assert.equal((await fetch(view.url + '/__workass-shell/controller', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ controller: true }),
  })).status, 204);
  status = await fetch(view.url + '/__workass-shell/status').then((r) => r.json());
  assert.equal(status.controller, true);
  assert.ok(status.reportedAt);
  assert.equal((await fetch(view.url + '/__workass-shell/catalog', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ groups: [
      { providerId: 'claude', status: 'ready', models: [{ modelId: 'haiku', name: 'Haiku 4.5' }] },
      { providerId: 'codex', status: 'ready', models: [{ modelId: 'gpt-test', name: 'GPT Test' }] },
      { providerId: 'missing', status: 'not-found', models: [{ modelId: 'ignored', name: 'Ignored' }] },
    ] }),
  })).status, 204);
  status = await fetch(view.url + '/__workass-shell/status').then((r) => r.json());
  assert.deepEqual(status.claude, { status: 'ready', models: [{ modelId: 'haiku', name: 'Haiku 4.5' }] });
  assert.equal(status.catalog.readyModelCount, 2);
  assert.ok(status.catalog.reportedAt);
  assert.deepEqual(status.catalog.groups.map((group) => group.providerId), ['claude', 'codex', 'missing']);
  view.reportBrowserState({
    chatId: 'tab-test', url: 'https://example.com/?token=must-not-leak',
    persistent: true, cdpAttached: true, agentControl: true, loading: false,
  });
  status = await fetch(view.url + '/__workass-shell/status').then((r) => r.json());
  assert.deepEqual(status.browser, {
    chatId: 'tab-test', persistent: true, cdpAttached: true, agentControl: true, loading: false,
    hasError: false, reportedAt: status.browser.reportedAt,
  });
  assert.doesNotMatch(JSON.stringify(status.browser), /example|token|must-not-leak/);
  view.reportWindowState({ visible: true, minimized: false, focused: true });
  status = await fetch(view.url + '/__workass-shell/status').then((r) => r.json());
  assert.equal(status.windowVisible, true);
  assert.deepEqual(status.window, {
    visible: true, minimized: false, focused: true, reportedAt: status.window.reportedAt,
  });

  const viewPort = Number(new URL(view.url).port);
  const upgraded = await new Promise((resolve, reject) => {
    const socket = net.connect({ host: '127.0.0.1', port: viewPort }, () => {
      socket.write('GET /?deviceName=test HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGVzdC1rZXk=\r\nSec-WebSocket-Version: 13\r\n\r\n');
    });
    let data = '';
    socket.on('data', (chunk) => {
      data += chunk.toString('utf8');
      if (data.includes('UPSTREAM')) { socket.destroy(); resolve(data); }
    });
    socket.on('error', reject);
    setTimeout(() => reject(new Error('upgrade timeout')), 2000);
  });
  assert.match(upgraded, /101 Switching Protocols/);
  assert.match(upgraded, /UPSTREAM/);
});

test('screenshot endpoint serves the registered capture and forwards ?click', async (t) => {
  const renderer = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-shell-shot-'));
  fs.writeFileSync(path.join(renderer, 'index.html'), '<!doctype html><head></head><body>i</body>');
  t.after(() => fs.rmSync(renderer, { recursive: true, force: true }));
  const view = await createViewServer({ daemonURL: 'http://127.0.0.1:9', rendererDir: renderer, port: 0 });
  t.after(() => view.close());

  // No window registered yet → 503, never a proxy attempt.
  assert.equal((await fetch(view.url + '/__workass-shell/screenshot')).status, 503);

  let received;
  view.setCapture(async (opts) => { received = opts; return Buffer.from('PNGBYTES'); });
  const res = await fetch(view.url + '/__workass-shell/screenshot?click=' + encodeURIComponent('.tico[title="x"]') + '&event=dblclick&value=hello');
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('content-type'), 'image/png');
  assert.equal(Buffer.from(await res.arrayBuffer()).toString(), 'PNGBYTES');
  assert.deepEqual(received, { click: '.tico[title="x"]', event: 'dblclick', target: null, value: 'hello' });

  await fetch(view.url + '/__workass-shell/screenshot'); // no click → null, event defaults
  assert.equal(received.click, null);
  assert.equal(received.event, 'click');
  assert.equal(received.value, null);
});

test('performance endpoint controls only the registered bounded renderer probe', async (t) => {
  const renderer = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-shell-perf-'));
  fs.writeFileSync(path.join(renderer, 'index.html'), '<!doctype html><head></head><body>i</body>');
  t.after(() => fs.rmSync(renderer, { recursive: true, force: true }));
  const view = await createViewServer({ daemonURL: 'http://127.0.0.1:9', rendererDir: renderer, port: 0 });
  t.after(() => view.close());

  assert.equal((await fetch(view.url + '/__workass-shell/perf')).status, 503);
  const actions = [];
  view.setPerf(async (action) => { actions.push(action); return { action, maxFrameGapMs: 17 }; });
  const started = await (await fetch(view.url + '/__workass-shell/perf?action=start')).json();
  const read = await (await fetch(view.url + '/__workass-shell/perf')).json();
  assert.deepEqual(started, { action: 'start', maxFrameGapMs: 17 });
  assert.deepEqual(read, { action: 'read', maxFrameGapMs: 17 });
  assert.deepEqual(actions, ['start', 'read']);
});

// A renderer promotion used to require relaunching Electron, and that relaunch
// is exactly what stranded the controller lease on 2026-07-26. Reloading the
// window in place is the cheaper, safer delivery — and it must carry the same
// marker-clearing step the renderer's own ⌘, "Recargar" performs, or it is
// merely an F5 and cannot repair "running but not the controller".
test('reload endpoint reloads the window in place and can arm controller recovery', async (t) => {
  const renderer = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-view-reload-'));
  fs.writeFileSync(path.join(renderer, 'index.html'), '<!doctype html><head></head><body>x</body>');
  t.after(() => fs.rmSync(renderer, { recursive: true, force: true }));

  const daemon = http.createServer((req, res) => { res.writeHead(200); res.end('daemon'); });
  await new Promise((resolve) => daemon.listen(0, '127.0.0.1', resolve));
  t.after(() => daemon.close());

  const view = await createViewServer({
    daemonURL: `http://127.0.0.1:${daemon.address().port}`, rendererDir: renderer, port: 0, runtimeVersion: '43.1.1',
  });
  t.after(() => view.close());

  // Before a window exists the endpoint refuses honestly rather than 404ing,
  // so a promotion script can tell "not ready yet" from "not supported".
  assert.equal((await fetch(view.url + '/__workass-shell/reload', { method: 'POST' })).status, 503);

  const calls = [];
  view.setReload(async (opts) => { calls.push(opts); return { reloaded: true, url: 'http://127.0.0.1:0/' }; });

  const plain = await fetch(view.url + '/__workass-shell/reload', { method: 'POST' });
  assert.equal(plain.status, 200);
  assert.equal((await plain.json()).reloaded, true);
  assert.deepEqual(calls[0], { recoverController: false });

  await fetch(view.url + '/__workass-shell/reload?recoverController=1', { method: 'POST' });
  assert.deepEqual(calls[1], { recoverController: true });

  // GET must not reload: a browser preloading or a stray probe must never be
  // able to reload the user's window out from under them.
  assert.equal((await fetch(view.url + '/__workass-shell/reload')).status, 405);
  assert.equal(calls.length, 2);
});

test('recovery endpoint invokes only the shell-owned daemon recovery transaction', async (t) => {
  const renderer = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-view-recovery-'));
  fs.writeFileSync(path.join(renderer, 'index.html'), '<!doctype html><head></head><body>x</body>');
  t.after(() => fs.rmSync(renderer, { recursive: true, force: true }));
  const view = await createViewServer({ daemonURL: 'http://127.0.0.1:9', rendererDir: renderer, port: 0 });
  t.after(() => view.close());
  assert.equal((await fetch(view.url + '/__workass-shell/recover', { method: 'POST' })).status, 503);
  const calls = [];
  view.setRecovery(async () => { calls.push('recover'); return { recovered: true }; });
  const reply = await fetch(view.url + '/__workass-shell/recover', { method: 'POST' });
  assert.equal(reply.status, 200);
  assert.deepEqual(await reply.json(), { recovered: true });
  assert.deepEqual(calls, ['recover']);
  assert.equal((await fetch(view.url + '/__workass-shell/recover')).status, 405);
});
