'use strict';

const assert = require('node:assert/strict');
const http = require('node:http');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const { createConnectedArtifactBridge, parseArtifactURL, allowedHeaders, safeResponseHeaders, shouldInjectArtifactHeader } = require('./connected-artifacts');

function fixture(viewURL = 'http://127.0.0.1:32123') {
  const sent = [];
  const wc = new EventEmitter(); wc.id = 1; wc.mainFrame = {};
  wc.send = (_channel, payload) => sent.push(payload);
  const win = { webContents: wc };
  const bridge = createConnectedArtifactBridge({ win, viewServer: { url: viewURL }, getOwnedWebContents: () => [] });
  return { bridge, sent, wc };
}

test('artifact URL parsing rejects traversal and retains query', () => {
  assert.deepEqual(parseArtifactURL('/workass/connected-artifacts/m1/a1/css/main.css?v=2'), {
    machineId: 'm1', artifactId: 'a1', path: '/workass/artifacts/a1/css/main.css?v=2',
    routeFamily: 'legacy',
  });
  assert.deepEqual(parseArtifactURL('/workass/artifacts/@m1/a1/css/main.css?v=2#x'), {
    machineId: 'm1', artifactId: 'a1', path: '/workass/artifacts/a1/css/main.css?v=2',
    routeFamily: 'canonical',
  });
  assert.equal(parseArtifactURL('/workass/artifacts/m1/a1/file'), null);
  assert.equal(parseArtifactURL('/workass/artifacts/@./a1/file'), null);
  assert.equal(parseArtifactURL('/workass/artifacts/@../a1/file'), null);
  assert.equal(parseArtifactURL('/workass/artifacts/@m1/../secret'), null);
  assert.equal(parseArtifactURL('/workass/connected-artifacts/m1/a1/../secret'), null);
  assert.equal(parseArtifactURL('/workass/connected-artifacts/m1/a1/%2e%2e/secret'), null);
  assert.equal(parseArtifactURL('/workass/artifacts/@m1/a1/%2fsecret'), null);
  assert.equal(parseArtifactURL('/workass/artifacts/@m1/a1/..%2fsecret'), null);
});

test('request and response headers are narrowly allowlisted', () => {
  assert.deepEqual(allowedHeaders({ Range: 'bytes=0-2', Cookie: 'secret', 'If-None-Match': 'x' }), { range: 'bytes=0-2', 'if-none-match': 'x' });
  assert.equal(safeResponseHeaders({ 'Content-Type': 'text/plain', 'Set-Cookie': 'x', Location: 'https://remote/' })['set-cookie'], undefined);
  assert.equal(safeResponseHeaders({ Location: 'https://remote/' }).location, undefined);
});

test('header authorization follows owned frame and parent URLs', () => {
  const origin = 'http://127.0.0.1:8799/';
  const target = `${origin}workass/connected-artifacts/m/a/x.png`;
  const owned = () => true;
  assert.equal(shouldInjectArtifactHeader({ webContents: {}, frame: { url: origin } }, { origin, targetURL: target, owned }), true);
  assert.equal(shouldInjectArtifactHeader({ webContents: {}, frame: { url: 'https://evil.test/' } }, { origin, targetURL: target, owned }), false);
  assert.equal(shouldInjectArtifactHeader({ webContents: {}, frame: { url: target, parent: { url: origin } } }, { origin, targetURL: target, owned }), true);
  assert.equal(shouldInjectArtifactHeader({ webContents: {}, frame: { url: target, parent: { url: 'https://evil.test/' } } }, { origin, targetURL: target, owned }), false);
});

test('wrong capability and wrong host are denied', async () => {
  const { bridge } = fixture();
  const request = (headers) => new Promise((resolve) => {
    const req = new EventEmitter(); req.method = 'GET'; req.url = '/workass/connected-artifacts/m/a/x'; req.headers = headers; req.socket = { remoteAddress: '127.0.0.1' };
    const res = { writeHead: (status) => { res.status = status; }, end: () => resolve(res), once() {}, removeListener() {}, headersSent: false };
    void bridge.handle(req, res);
  });
  assert.equal((await request({ host: '127.0.0.1:32123', 'x-workass-artifact-access': 'wrong' })).status, 403);
  assert.equal((await request({ host: 'evil.test', 'x-workass-artifact-access': bridge.capability })).status, 403);
  bridge.close();
});

test('reply owner and frame are exact', () => {
  const { bridge, sent, wc } = fixture();
  const requestId = 'late-owner';
  const pending = bridge.reply({ sender: {}, senderFrame: wc.mainFrame }, { requestId });
  assert.equal(pending, false);
  assert.equal(sent.length, 0);
  bridge.close();
});

test('timeout rejects and late replies are discarded', async () => {
  const sent = []; const wc = new EventEmitter(); wc.mainFrame = {}; wc.send = (_channel, payload) => sent.push(payload);
  const bridge = createConnectedArtifactBridge({ win: { webContents: wc }, viewServer: { url: 'http://127.0.0.1:32123' }, timeoutMs: 10 });
  const req = new EventEmitter(); req.method = 'GET'; req.url = '/workass/connected-artifacts/m/a/x'; req.headers = { host: '127.0.0.1:32123', 'x-workass-artifact-access': bridge.capability }; req.socket = { remoteAddress: '127.0.0.1' };
  const res = new EventEmitter(); res.headersSent = false; res.writableEnded = false; res.writeHead = (s) => { res.status = s; res.headersSent = true; }; res.end = () => { res.writableEnded = true; };
  await bridge.handle(req, res);
  assert.equal(res.status, 503);
  assert.equal(bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: sent[0].requestId, ok: true, status: 200 }), false);
  bridge.close();
});

test('oversized or malformed chunks fail safely', async () => {
  const { bridge, sent, wc } = fixture();
  const req = new EventEmitter(); req.method = 'GET'; req.url = '/workass/connected-artifacts/m/a/x'; req.headers = { host: '127.0.0.1:32123', 'x-workass-artifact-access': bridge.capability }; req.socket = { remoteAddress: '127.0.0.1' };
  const res = new EventEmitter(); res.headersSent = false; res.writableEnded = false; res.writeHead = (s) => { res.status = s; res.headersSent = true; }; res.end = () => { res.writableEnded = true; }; res.destroy = () => { res.destroyed = true; };
  const running = bridge.handle(req, res); await new Promise((r) => setTimeout(r, 20));
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: sent[0].requestId, ok: true, status: 200, headers: {}, transferId: 't' });
  await new Promise((r) => setTimeout(r, 20));
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: sent[1].requestId, ok: true, bodyBase64: Buffer.alloc(128 * 1024 + 1).toString('base64'), eof: false });
  await new Promise((r) => setTimeout(r, 20));
  const close = sent.find((item) => item.op === 'close'); assert.ok(close);
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: close.requestId, ok: true });
  await running; assert.equal(res.destroyed, true); bridge.close();
});

test('HEAD never requests a body and closes its transfer', async () => {
  const { bridge, sent, wc } = fixture();
  const req = new EventEmitter(); req.method = 'HEAD'; req.url = '/workass/connected-artifacts/m/a/x'; req.headers = { host: '127.0.0.1:32123', 'x-workass-artifact-access': bridge.capability }; req.socket = { remoteAddress: '127.0.0.1' };
  const res = new EventEmitter(); res.headersSent = false; res.writableEnded = false; res.writeHead = (s) => { res.status = s; res.headersSent = true; }; res.end = () => { res.writableEnded = true; }; res.destroy = () => { res.destroyed = true; };
  const running = bridge.handle(req, res); await new Promise((r) => setTimeout(r, 20));
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: sent[0].requestId, ok: true, status: 200, headers: {}, transferId: 't' });
  await new Promise((r) => setTimeout(r, 20));
  const close = sent.find((x) => x.op === 'close'); assert.ok(close);
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: close.requestId, ok: true });
  await running; assert.equal(res.status, 200); assert.equal(sent.some((x) => x.op === 'read'), false); bridge.close();
});

test('redirects preserve canonical or legacy family, owner, query, and fragment', async () => {
  async function check(route, family, expectedPath) {
    const { bridge, sent, wc } = fixture();
    const req = new EventEmitter(); req.method = 'GET'; req.url = route;
    req.headers = { host: '127.0.0.1:32123', 'x-workass-artifact-access': bridge.capability };
    req.socket = { remoteAddress: '127.0.0.1' };
    const res = new EventEmitter(); res.headersSent = false; res.writableEnded = false;
    res.writeHead = (status, headers) => { res.status = status; res.headers = headers; res.headersSent = true; };
    res.write = () => true; res.end = () => { res.writableEnded = true; };
    const running = bridge.handle(req, res);
    await new Promise((resolve) => setTimeout(resolve, 10));
    const open = sent.find((item) => item.op === 'open'); assert.ok(open);
    assert.equal(open.machineId, 'm1'); assert.equal(open.path, expectedPath);
    bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, {
      requestId: open.requestId, ok: true, status: 302, transferId: 'redirect',
      headers: { Location: '/workass/artifacts/a1/index.html?next=1#fragment' },
    });
    await new Promise((resolve) => setTimeout(resolve, 10));
    const read = sent.find((item) => item.op === 'read'); assert.ok(read);
    bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: read.requestId, ok: true, bodyBase64: '', eof: true });
    await new Promise((resolve) => setTimeout(resolve, 10));
    const close = sent.find((item) => item.op === 'close'); assert.ok(close);
    bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: close.requestId, ok: true });
    await running;
    assert.equal(res.status, 302);
    assert.equal(res.headers.location, family === 'canonical'
      ? '/workass/artifacts/@m1/a1/index.html?next=1#fragment'
      : '/workass/connected-artifacts/m1/a1/index.html?next=1#fragment');
    bridge.close();
  }
  await check('/workass/artifacts/@m1/a1/index.html', 'canonical', '/workass/artifacts/a1/index.html');
  await check('/workass/connected-artifacts/m1/a1/index.html', 'legacy', '/workass/artifacts/a1/index.html');
});

test('aborting one request does not cancel another transfer', async () => {
  const { bridge, sent, wc } = fixture();
  const make = () => {
    const req = new EventEmitter(); req.method = 'GET'; req.url = '/workass/connected-artifacts/m/a/x'; req.headers = { host: '127.0.0.1:32123', 'x-workass-artifact-access': bridge.capability }; req.socket = { remoteAddress: '127.0.0.1' };
    const res = new EventEmitter(); res.headersSent = false; res.writableEnded = false; res.writeHead = () => { res.headersSent = true; }; res.write = () => true; res.end = () => { res.writableEnded = true; };
    return { req, res };
  };
  const first = make(); const second = make();
  const one = bridge.handle(first.req, first.res); const two = bridge.handle(second.req, second.res);
  await new Promise((r) => setTimeout(r, 20));
  const opens = sent.filter((x) => x.op === 'open'); assert.equal(opens.length, 2);
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: opens[0].requestId, ok: true, status: 200, transferId: 'one' });
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: opens[1].requestId, ok: true, status: 200, transferId: 'two' });
  first.req.emit('aborted');
  assert.equal(sent.filter((x) => x.requestId === opens[1].requestId && !x.op).length, 0);
  await new Promise((r) => setTimeout(r, 10));
  bridge.close(); await Promise.allSettled([one, two]);
});

test('owned renderer can stream bounded artifact chunks', async () => {
  let bridge; const sent = []; const wc = new EventEmitter(); wc.id = 1; wc.mainFrame = {}; wc.send = (_channel, payload) => sent.push(payload);
  const win = { webContents: wc };
  const server = http.createServer((req, res) => { void bridge.handle(req, res); });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  bridge = createConnectedArtifactBridge({ win, viewServer: { url: `http://127.0.0.1:${port}` }, getOwnedWebContents: () => [] });
  const response = new Promise((resolve) => { http.get({ host: '127.0.0.1', port, path: '/workass/connected-artifacts/m/a/file.txt', headers: { 'X-Workass-Artifact-Access': bridge.capability } }, resolve); });
  await new Promise((resolve) => setTimeout(resolve, 25));
  const open = sent.find((item) => item.op === 'open'); assert.ok(open);
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: open.requestId, ok: true, status: 200, headers: { 'Content-Type': 'text/plain' }, transferId: 't1' });
  await new Promise((resolve) => setTimeout(resolve, 25));
  const read = sent.find((item) => item.op === 'read'); assert.ok(read);
  bridge.reply({ sender: wc, senderFrame: wc.mainFrame }, { requestId: read.requestId, ok: true, bodyBase64: Buffer.from('ok').toString('base64'), eof: true });
  const res = await response; let body = ''; res.setEncoding('utf8'); res.on('data', (x) => { body += x; }); await new Promise((resolve) => res.on('end', resolve));
  assert.equal(body, 'ok');
  await new Promise((resolve) => setTimeout(resolve, 5));
  server.close(); bridge.close();
});
