'use strict';

const crypto = require('node:crypto');

const ACCESS_HEADER = 'x-workass-artifact-access';
const MAX_CONCURRENT = 16;
const MAX_CHUNK = 128 * 1024;
const REQUEST_TIMEOUT_MS = 15_000;
const SAFE_REQUEST_HEADERS = new Set(['range', 'if-range', 'if-none-match', 'if-modified-since']);

function randomId() { return crypto.randomBytes(18).toString('hex'); }

function localAddress(address) {
  const value = String(address || '').replace(/^::ffff:/i, '');
  return value === '127.0.0.1' || value === '::1' || value === 'localhost';
}

function validPart(value) {
  return /^[A-Za-z0-9._~-]{1,200}$/.test(String(value || '')) && value !== '.' && value !== '..';
}

function parseArtifactURL(url) {
  const rawURL = String(url || '');
  const rawPath = rawURL.split('?')[0];
  if (/%(?:2e|2f|5c)/iu.test(rawPath) || rawPath.split('/').some((part) => part === '..' || part.includes('\\'))) return null;
  const parsed = new URL(rawURL, 'http://127.0.0.1');
  const prefix = '/workass/connected-artifacts/';
  if (!parsed.pathname.startsWith(prefix)) return null;
  const rest = parsed.pathname.slice(prefix.length).split('/');
  if (rest.length < 2 || !validPart(rest[0]) || !validPart(rest[1])) return null;
  const tail = rest.slice(2);
  if (tail.some((part, index) => (index !== tail.length - 1 && !part) || part === '.' || part === '..' || part.includes('\\'))) return null;
  return {
    machineId: rest[0], artifactId: rest[1],
    path: `/workass/artifacts/${rest[1]}${tail.length ? `/${tail.join('/')}` : ''}${parsed.search}`,
  };
}

function allowedHeaders(input) {
  const output = {};
  for (const [name, value] of Object.entries(input && typeof input === 'object' ? input : {})) {
    const key = String(name).toLowerCase();
    if (!SAFE_REQUEST_HEADERS.has(key) || value == null || Array.isArray(value)) continue;
    const rendered = String(value);
    if (rendered.length > 4096 || /[\r\n\0]/u.test(rendered)) continue;
    output[key] = rendered;
  }
  return output;
}

function safeResponseHeaders(input) {
  const names = new Set(['content-type', 'content-length', 'content-range', 'content-disposition',
    'accept-ranges', 'etag', 'last-modified', 'cache-control', 'content-security-policy',
    'x-content-type-options', 'x-workass-withheld', 'referrer-policy', 'location']);
  const output = {};
  for (const [name, value] of Object.entries(input && typeof input === 'object' ? input : {})) {
    const key = String(name).toLowerCase();
    if (!names.has(key) || value == null || Array.isArray(value)) continue;
    const rendered = String(value);
    if (rendered.length > 8192 || /[\r\n\0]/u.test(rendered)) continue;
    if (key === 'location') continue;
    output[key] = rendered;
  }
  return output;
}

function sameOrigin(a, origin) {
  try { const left = new URL(a); const right = new URL(origin); return left.protocol === right.protocol && left.host === right.host; } catch { return false; }
}

// Electron 43.1 does not expose details.initiator. Frame URLs are the stable
// authorization oracle: an owned frame must currently be on the shell origin
// or an artifact route served by this bridge, and its parent must be shell or
// artifact content. This prevents an external page mounted in an owned view
// from borrowing the bridge capability for fetches or subresources.
function shouldInjectArtifactHeader(details, { origin, targetURL, owned, authorizedNavigation } = {}) {
  if (!details || typeof owned !== 'function' || !owned(details.webContents)) return false;
  let targetPath;
  try { targetPath = new URL(targetURL).pathname; } catch { return false; }
  if (!sameOrigin(targetURL, origin) || !targetPath.startsWith('/workass/connected-artifacts/')) return false;
  if (details.resourceType === 'mainFrame' && authorizedNavigation?.(details.webContents, targetURL)) return true;
  let frame = details.frame;
  if (!frame) return false;
  // Electron reports an empty URL for a newly created iframe before its first
  // document commits. Only its trusted parent may authorize that initial load.
  if (details.resourceType === 'subFrame' && (!frame.url || frame.url === 'about:blank')) frame = frame.parent;
  if (!frame) return false;
  // Opaque sandbox origins still have a frame URL. Check every ancestor so an
  // externally embedded artifact cannot grant its parent access to this bridge.
  for (let current = frame; current; current = current.parent) {
    if (!sameOrigin(current.url, origin)) return false;
  }
  return true;
}

function createConnectedArtifactBridge({ win, viewServer, getOwnedWebContents, timeoutMs = REQUEST_TIMEOUT_MS } = {}) {
  if (!win || !win.webContents) throw new Error('artifact bridge requires window');
  const capability = crypto.randomBytes(32).toString('base64url');
  const expectedHost = (() => { try { return new URL(viewServer?.url || '').host; } catch { return ''; } })();
  const pending = new Map();
  let active = 0;
  let closed = false;

  const owned = (contents) => {
    if (contents === win.webContents) return true;
    try { return typeof getOwnedWebContents === 'function' && getOwnedWebContents().includes(contents); } catch { return false; }
  };
  const sendCancel = (requestId) => { try { win.webContents.send('workass-artifact:cancel', { requestId }); } catch {} };
  const cancelPending = (id) => {
    const item = pending.get(id);
    if (!item) return;
    pending.delete(id);
    clearTimeout(item.timer);
    item.tracker?.delete(id);
    sendCancel(id);
    item.reject(new Error('artifact request cancelled'));
  };
  const requestRenderer = (payload, tracker) => new Promise((resolve, reject) => {
    if (closed) { reject(new Error('artifact bridge closed')); return; }
    const timer = setTimeout(() => cancelPending(payload.requestId), timeoutMs);
    pending.set(payload.requestId, { resolve, reject, timer, tracker });
    tracker?.add(payload.requestId);
    try { win.webContents.send('workass-artifact:request', payload); }
    catch { cancelPending(payload.requestId); }
  });

  async function handle(req, res) {
    if (closed || !localAddress(req.socket?.remoteAddress) || !expectedHost || req.headers.host !== expectedHost) {
      res.writeHead(403); res.end('forbidden'); return;
    }
    const supplied = Buffer.from(String(req.headers[ACCESS_HEADER] || ''));
    const expected = Buffer.from(capability);
    if (supplied.length !== expected.length || !crypto.timingSafeEqual(supplied, expected)) {
      res.writeHead(403); res.end('forbidden'); return;
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') { res.writeHead(405, { Allow: 'GET, HEAD' }); res.end(); return; }
    if (active >= MAX_CONCURRENT) { res.writeHead(429); res.end(); return; }
    let target;
    try { target = parseArtifactURL(req.url); } catch {}
    if (!target) { res.writeHead(404); res.end('artifact unavailable'); return; }
    active += 1;
    const requestIds = new Set();
    let transferId = '';
    let aborted = false;
    const abort = () => {
      aborted = true;
      for (const id of [...requestIds]) cancelPending(id);
    };
    req.once('aborted', abort);
    res.once('close', abort);
    const rpc = (op, payload = {}) => requestRenderer({
      op, requestId: randomId(), machineId: target.machineId, artifactId: target.artifactId, ...payload,
    }, requestIds);
    try {
      const opened = await rpc('open', { path: target.path, method: req.method, headers: allowedHeaders(req.headers) });
      if (opened?.ok === false && opened.errorCode === 'unsupported') {
        res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
        res.end('The connected Workass instance needs an update to share artifacts.');
        return;
      }
      if (typeof opened?.transferId === 'string') transferId = opened.transferId;
      if (aborted || opened?.ok !== true || !transferId || transferId.length > 200
        || !Number.isInteger(opened.status) || opened.status < 200 || opened.status > 599
        || !opened.headers || typeof opened.headers !== 'object' || Array.isArray(opened.headers)) throw new Error('invalid artifact response');
      const headers = safeResponseHeaders(opened.headers);
      // The registry's canonical directory redirect must stay inside the same
      // machine bridge, never jump back to the legacy local artifact endpoint.
      const location = opened.headers.Location ?? opened.headers.location;
      if (typeof location === 'string') {
        const redirected = new URL(location, `http://artifact.invalid${target.path}`);
        if (redirected.origin !== 'http://artifact.invalid'
          || !redirected.pathname.startsWith(`/workass/artifacts/${target.artifactId}/`)) throw new Error('invalid artifact redirect');
        const suffix = redirected.pathname.slice('/workass/artifacts/'.length);
        const route = `/workass/connected-artifacts/${target.machineId}/${suffix}${redirected.search}`;
        if (!parseArtifactURL(route)) throw new Error('invalid artifact redirect');
        headers.location = route;
      }
      res.writeHead(opened.status, headers);
      if (req.method === 'HEAD' || opened.status === 204 || opened.status === 304) { res.end(); return; }
      while (!aborted) {
        const chunk = await rpc('read', { transferId });
        if (aborted || chunk?.ok !== true || typeof chunk.bodyBase64 !== 'string'
          || chunk.bodyBase64.length > 174764 || typeof chunk.eof !== 'boolean'
          || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u.test(chunk.bodyBase64)) throw new Error('invalid artifact chunk');
        const body = Buffer.from(chunk.bodyBase64, 'base64');
        if (body.length > MAX_CHUNK || (!body.length && !chunk.eof)) throw new Error('invalid artifact chunk');
        if (body.length && !res.write(body)) {
          await new Promise((resolve, reject) => {
            const cleanup = () => { clearTimeout(timer); res.removeListener('drain', drained); res.removeListener('close', failed); };
            const drained = () => { cleanup(); resolve(); };
            const failed = () => { cleanup(); reject(new Error('artifact response closed')); };
            const timer = setTimeout(failed, timeoutMs);
            res.once('drain', drained); res.once('close', failed);
            if (res.destroyed) failed();
          });
        }
        if (chunk.eof) { res.end(); break; }
      }
    } catch {
      if (!res.destroyed && !res.writableEnded) {
        if (res.headersSent) res.destroy();
        else { res.writeHead(503, { 'Content-Type': 'text/plain; charset=utf-8' }); res.end('artifact unavailable'); }
      }
    } finally {
      req.removeListener('aborted', abort);
      res.removeListener('close', abort);
      for (const id of [...requestIds]) cancelPending(id);
      if (transferId) { try { await rpc('close', { transferId }); } catch {} }
      active -= 1;
    }
  }

  const reply = (event, payload) => {
    if (!owned(event && event.sender) || !event || event.sender !== win.webContents
      || event.senderFrame !== win.webContents.mainFrame
      || !payload || typeof payload.requestId !== 'string') return false;
    const item = pending.get(payload.requestId); if (!item) return false;
    pending.delete(payload.requestId); item.tracker?.delete(payload.requestId); clearTimeout(item.timer); item.resolve(payload); return true;
  };
  const close = () => { closed = true; for (const [id, item] of pending) { clearTimeout(item.timer); item.reject(new Error('artifact bridge closed')); sendCancel(id); } pending.clear(); };
  return { capability, accessHeader: ACCESS_HEADER, handle, reply, close, owned, parseArtifactURL, allowedHeaders, safeResponseHeaders };
}

module.exports = { createConnectedArtifactBridge, parseArtifactURL, allowedHeaders, safeResponseHeaders, shouldInjectArtifactHeader, localAddress, MAX_CHUNK, MAX_CONCURRENT };
