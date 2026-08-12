'use strict';

// Provider-neutral control plane for Workass's visible browser. ACP agents use
// it through the Workass MCP adapter; provider-specific adapters (such as
// Codex IAB) sit beside it and call the same BrowserManager.
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');

const MAX_BODY_BYTES = 2 * 1024 * 1024;
const MAX_MUTATION_RECEIPTS = 256;
const MUTATING_METHODS = new Set([
  'browser.open', 'browser.navigate', 'browser.back', 'browser.forward',
  'browser.reload', 'browser.click', 'browser.type',
  'browser.scroll', 'browser.key', 'browser.batch',
]);

function defaultControlFile() {
  return process.env.WORKASS_BROWSER_CONTROL_FILE
    || path.join(os.homedir(), '.workass', 'browser-control.json');
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => {
      body += chunk;
      if (Buffer.byteLength(body) > MAX_BODY_BYTES) {
        reject(new Error('browser control request too large'));
        req.destroy();
      }
    });
    req.on('end', () => resolve(body));
    req.on('error', reject);
  });
}

function writeJSON(res, status, payload) {
  const body = JSON.stringify(payload);
  res.writeHead(status, {
    'Cache-Control': 'no-store',
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

class BrowserControlServer {
  constructor({ manager, controlFile = defaultControlFile(), host = '127.0.0.1', port = 0, token, isController } = {}) {
    if (!manager || typeof manager.browserControl !== 'function') {
      throw new Error('browser control manager missing');
    }
    this.manager = manager;
    this.controlFile = path.resolve(controlFile);
    this.host = host;
    this.port = Number(port) || 0;
    this.token = token || crypto.randomBytes(32).toString('hex');
    this.isController = typeof isController === 'function' ? isController : () => false;
    // This is an executor-side receipt cache, not actor state. It contains
    // only the immutable operation identity and the returned receipt; browser
    // arguments are kept on the in-flight call stack and never persisted here.
    this.mutationReceipts = new Map();
    this.server = null;
    this.url = '';
  }

  async dispatchMutation(request) {
    const operationId = String(request.operationId || '').trim();
    const requestDigest = String(request.requestDigest || '').trim();
    const method = String(request.method || '').trim();
    if (!operationId || operationId.length > 256 || !requestDigest || requestDigest.length > 256) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: true,
        error: 'browser mutation requires bounded operation identity and request digest',
      };
    }
    const existing = this.mutationReceipts.get(operationId);
    if (existing) {
      if (existing.method !== method || existing.requestDigest !== requestDigest) {
        return {
          id: request.id ?? null, operationId, requestDigest, receipt: true,
          error: 'browser mutation operation was reused with a different request',
        };
      }
      const response = existing.promise ? await existing.promise : existing.response;
      return { ...response, id: request.id ?? null };
    }

    this.pruneMutationReceipts();
    if (this.mutationReceipts.size >= MAX_MUTATION_RECEIPTS && !this.hasSettledMutationReceipt()) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: true,
        error: 'browser mutation receipt capacity is busy',
      };
    }

    const promise = Promise.resolve()
      .then(() => this.manager.browserControl(method, request.params || {}))
      .then(
        (result) => ({ id: request.id ?? null, operationId, requestDigest, receipt: true, result }),
        (err) => ({
          id: request.id ?? null, operationId, requestDigest, receipt: true,
          error: String(err && err.message || err),
        }),
      );
    const record = { method, requestDigest, promise, response: null };
    this.mutationReceipts.set(operationId, record);
    this.pruneMutationReceipts();
    const response = await promise;
    record.response = response;
    record.promise = null;
    this.pruneMutationReceipts();
    return { ...response, id: request.id ?? null };
  }

  pruneMutationReceipts() {
    // Map insertion order gives deterministic oldest-first eviction. A live
    // dispatch is never evicted; if every record is in flight, the temporary
    // size may exceed the settled-record bound until one completes.
    while (this.mutationReceipts.size > MAX_MUTATION_RECEIPTS) {
      let evict;
      for (const [operationId, record] of this.mutationReceipts) {
        if (!record.promise) {
          evict = operationId;
          break;
        }
      }
      if (evict === undefined) break;
      this.mutationReceipts.delete(evict);
    }
  }

  hasSettledMutationReceipt() {
    for (const record of this.mutationReceipts.values()) {
      if (!record.promise) return true;
    }
    return false;
  }

  async readMutationReceipt(request) {
    const operationId = String(request.operationId || '').trim();
    const requestDigest = String(request.requestDigest || '').trim();
    if (!operationId || operationId.length > 256 || !requestDigest || requestDigest.length > 256) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: false,
        error: 'browser receipt requires bounded operation identity and request digest',
      };
    }
    const existing = this.mutationReceipts.get(operationId);
    if (!existing) {
      return { id: request.id ?? null, operationId, requestDigest, receipt: false };
    }
    if (existing.requestDigest !== requestDigest) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: true,
        error: 'browser mutation operation was reused with a different request',
      };
    }
    const response = existing.promise ? await existing.promise : existing.response;
    return { ...response, id: request.id ?? null };
  }

  async start() {
    if (this.server) return this.publicState();
    this.server = http.createServer(async (req, res) => {
      if (req.method !== 'POST' || req.url !== '/rpc') {
        writeJSON(res, 404, { error: 'not found' });
        return;
      }
      if (req.headers.authorization !== `Bearer ${this.token}`) {
        writeJSON(res, 403, { error: 'forbidden' });
        return;
      }
      let request;
      try {
        request = JSON.parse(await readBody(req));
        if (!request || typeof request.method !== 'string') throw new Error('invalid browser control request');
        if (MUTATING_METHODS.has(request.method) && !this.isController()) {
          throw new Error('Workass browser control requires the active controller');
        }
        if (request.method === 'browser.receipt') {
          if (!String(request.operationId || '').trim() || !String(request.requestDigest || '').trim()) {
            throw new Error('browser receipt requires operationId and requestDigest');
          }
          writeJSON(res, 200, await this.readMutationReceipt(request));
        } else if (MUTATING_METHODS.has(request.method) &&
          (String(request.operationId || '').trim() || String(request.requestDigest || '').trim())) {
          if (!String(request.operationId || '').trim() || !String(request.requestDigest || '').trim()) {
            throw new Error('browser mutation requires operationId and requestDigest');
          }
          writeJSON(res, 200, await this.dispatchMutation(request));
        } else {
          // Keep the direct shell surface byte-compatible for older local
          // callers. The MCP actor path always supplies the journal metadata.
          const result = await this.manager.browserControl(request.method, request.params || {});
          writeJSON(res, 200, { id: request.id ?? null, result });
        }
      } catch (err) {
        writeJSON(res, 200, { id: request && request.id || null, error: String(err && err.message || err) });
      }
    });
    await new Promise((resolve, reject) => {
      const fail = (err) => reject(err);
      this.server.once('error', fail);
      this.server.listen(this.port, this.host, () => {
        this.server.off('error', fail);
        resolve();
      });
    });
    const address = this.server.address();
    const actualPort = typeof address === 'object' && address ? address.port : this.port;
    this.url = `http://${this.host}:${actualPort}/rpc`;
    fs.mkdirSync(path.dirname(this.controlFile), { recursive: true, mode: 0o700 });
    const tmp = `${this.controlFile}.${process.pid}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify({ version: 1, url: this.url, token: this.token, pid: process.pid }), { mode: 0o600 });
    fs.renameSync(tmp, this.controlFile);
    return this.publicState();
  }

  publicState() {
    return { ready: !!this.server, controlFile: this.controlFile };
  }

  async close() {
    const server = this.server;
    this.server = null;
    if (server) await new Promise((resolve) => server.close(() => resolve()));
    try {
      const current = JSON.parse(fs.readFileSync(this.controlFile, 'utf8'));
      if (current && current.pid === process.pid) fs.unlinkSync(this.controlFile);
    } catch { /* absent or replaced by a newer shell */ }
  }
}

module.exports = { BrowserControlServer, MAX_MUTATION_RECEIPTS, defaultControlFile };
