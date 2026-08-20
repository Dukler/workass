'use strict';

// Provider-neutral control plane for Workass's visible browser. ACP agents use
// it through the Workass MCP adapter; provider-specific adapters (such as
// Codex IAB) sit beside it and call the same BrowserManager.
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');

const MAX_BODY_BYTES = 2 * 1024 * 1024;
const MAX_MUTATION_RECEIPTS = 256;
const RECEIPT_VERSION = 1;
const MUTATING_METHODS = new Set([
  'browser.open', 'browser.navigate', 'browser.back', 'browser.forward',
  'browser.reload', 'browser.click', 'browser.type',
  'browser.scroll', 'browser.key', 'browser.batch',
]);

function mutationReceiptKey(operationId) {
  return crypto.createHash('sha256')
    .update(`workass-browser-receipt-v1\0${operationId}`)
    .digest('hex');
}

function validRequestDigest(value) {
  return /^[0-9a-f]{64}$/u.test(String(value || '').trim().toLowerCase());
}

function storedReceiptReply(request, stored) {
  const base = {
    id: request.id ?? null,
    operationId: String(request.operationId || '').trim(),
    requestDigest: String(request.requestDigest || '').trim().toLowerCase(),
    receipt: true,
  };
  if (stored.outcome === 'failed') return { ...base, error: 'browser mutation was rejected' };
  return {
    ...base,
    result: { operationId: base.operationId, status: 'completed', receipt: true },
  };
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
  constructor({ manager, controlFile, receiptDir, host = '127.0.0.1', port = 0, token, isController } = {}) {
    if (!manager || typeof manager.browserControl !== 'function') {
      throw new Error('browser control manager missing');
    }
    if (!controlFile) throw new Error('browser control file missing');
    this.manager = manager;
    this.controlFile = path.resolve(controlFile);
    this.receiptDir = path.resolve(receiptDir || path.join(path.dirname(this.controlFile), 'browser-receipts'));
    this.host = host;
    this.port = Number(port) || 0;
    this.token = token || crypto.randomBytes(32).toString('hex');
    this.instanceId = crypto.randomBytes(16).toString('hex');
    this.isController = typeof isController === 'function' ? isController : () => false;
    // This is an executor-side receipt cache, not actor state. It contains
    // only the immutable operation identity and the returned receipt; browser
    // arguments are kept on the in-flight call stack and never persisted here.
    this.mutationReceipts = new Map();
    // Restart-safe receipts contain only a hash of the caller operation id, its
    // already-hashed request identity, the fixed method, and terminal outcome.
    // Page payloads, selectors, typed text, URLs, raw errors and results never
    // enter this directory.
    this.persistentReceipts = new Map();
    this.server = null;
    this.url = '';
  }

  receiptPath(operationKey) {
    return path.join(this.receiptDir, `${operationKey}.json`);
  }

  decodeStoredReceipt(raw, expectedKey = '') {
    const record = raw && typeof raw === 'object' ? raw : null;
    if (!record || record.version !== RECEIPT_VERSION ||
        !/^[0-9a-f]{64}$/u.test(String(record.operationKey || '')) ||
        (expectedKey && record.operationKey !== expectedKey) ||
        !validRequestDigest(record.requestDigest) ||
        !MUTATING_METHODS.has(record.method) ||
        (record.outcome !== 'completed' && record.outcome !== 'failed')) {
      return null;
    }
    return {
      operationKey: record.operationKey,
      requestDigest: String(record.requestDigest).toLowerCase(),
      method: record.method,
      outcome: record.outcome,
    };
  }

  loadPersistentReceipts() {
    fs.mkdirSync(this.receiptDir, { recursive: true, mode: 0o700 });
    const candidates = [];
    for (const name of fs.readdirSync(this.receiptDir)) {
      const file = path.join(this.receiptDir, name);
      if (/^[0-9a-f]{64}\.json\.\d+\.[0-9a-f]{12}\.tmp$/u.test(name)) {
        try { fs.unlinkSync(file); } catch { /* stale incomplete receipt is never authority */ }
        continue;
      }
      if (!/^[0-9a-f]{64}\.json$/u.test(name)) continue;
      try {
        const stat = fs.statSync(file);
        const operationKey = name.slice(0, -5);
        const stored = stat.isFile() && stat.size > 0 && stat.size <= 4096
          ? this.decodeStoredReceipt(JSON.parse(fs.readFileSync(file, 'utf8')), operationKey)
          : null;
        if (stored) candidates.push({ file, stored, mtimeMs: stat.mtimeMs });
        else fs.unlinkSync(file);
      } catch { /* an incomplete receipt never becomes authority */ }
    }
    candidates.sort((a, b) => a.mtimeMs - b.mtimeMs || a.stored.operationKey.localeCompare(b.stored.operationKey));
    for (const candidate of candidates.slice(0, Math.max(0, candidates.length - MAX_MUTATION_RECEIPTS))) {
      try { fs.unlinkSync(candidate.file); } catch { /* bounded cleanup is best effort */ }
    }
    for (const candidate of candidates.slice(-MAX_MUTATION_RECEIPTS)) {
      this.persistentReceipts.set(candidate.stored.operationKey, candidate.stored);
    }
  }

  readPersistentReceipt(operationKey) {
    const file = this.receiptPath(operationKey);
    try {
      const stat = fs.statSync(file);
      if (!stat.isFile() || stat.size <= 0 || stat.size > 4096) return null;
      const stored = this.decodeStoredReceipt(JSON.parse(fs.readFileSync(file, 'utf8')), operationKey);
      if (!stored) return null;
      this.persistentReceipts.set(operationKey, stored);
      this.prunePersistentReceipts();
      return stored;
    } catch {
      return null;
    }
  }

  prunePersistentReceipts() {
    while (this.persistentReceipts.size > MAX_MUTATION_RECEIPTS) {
      const operationKey = this.persistentReceipts.keys().next().value;
      if (!operationKey) break;
      this.persistentReceipts.delete(operationKey);
      try { fs.unlinkSync(this.receiptPath(operationKey)); } catch { /* already absent */ }
    }
  }

  persistTerminalReceipt(operationId, method, requestDigest, outcome) {
    const operationKey = mutationReceiptKey(operationId);
    const stored = { operationKey, requestDigest: requestDigest.toLowerCase(), method, outcome };
    const existing = this.persistentReceipts.get(operationKey);
    if (existing) {
      if (existing.method !== method || existing.requestDigest !== stored.requestDigest || existing.outcome !== outcome) {
        throw new Error('browser mutation receipt identity changed');
      }
      return existing;
    }
    fs.mkdirSync(this.receiptDir, { recursive: true, mode: 0o700 });
    const target = this.receiptPath(operationKey);
    const tmp = `${target}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`;
    const body = JSON.stringify({ version: RECEIPT_VERSION, ...stored });
    let fd;
    try {
      fd = fs.openSync(tmp, 'wx', 0o600);
      fs.writeFileSync(fd, body, 'utf8');
      fs.fsyncSync(fd);
      fs.closeSync(fd);
      fd = undefined;
      fs.renameSync(tmp, target);
    } catch (error) {
      if (fd !== undefined) {
        try { fs.closeSync(fd); } catch { /* best effort */ }
      }
      try { fs.unlinkSync(tmp); } catch { /* best effort */ }
      throw error;
    }
    this.persistentReceipts.set(operationKey, stored);
    this.prunePersistentReceipts();
    return stored;
  }

  persistentReceipt(request) {
    const operationId = String(request.operationId || '').trim();
    const operationKey = mutationReceiptKey(operationId);
    // A replacing shell can start while the prior instance is still settling
    // an accepted request. Reload the exact hashed receipt on a cache miss so
    // that late terminal commit remains visible without replaying page input.
    const stored = this.persistentReceipts.get(operationKey) || this.readPersistentReceipt(operationKey);
    if (!stored) return null;
    const method = String(request.method || '').trim();
    const requestDigest = String(request.requestDigest || '').trim().toLowerCase();
    if ((method !== 'browser.receipt' && stored.method !== method) || stored.requestDigest !== requestDigest) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: true,
        error: 'browser mutation operation was reused with a different request',
      };
    }
    return storedReceiptReply(request, stored);
  }

  async dispatchMutation(request) {
    const operationId = String(request.operationId || '').trim();
    const requestDigest = String(request.requestDigest || '').trim().toLowerCase();
    const method = String(request.method || '').trim();
    if (!operationId || operationId.length > 256 || !validRequestDigest(requestDigest)) {
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
    const persisted = this.persistentReceipt(request);
    if (persisted) return persisted;

    this.pruneMutationReceipts();
    if (this.mutationReceipts.size >= MAX_MUTATION_RECEIPTS && !this.hasSettledMutationReceipt()) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: true,
        error: 'browser mutation receipt capacity is busy',
      };
    }

    const promise = Promise.resolve().then(async () => {
      let response;
      if (!this.isController()) {
        response = {
          id: request.id ?? null, operationId, requestDigest, receipt: true,
          error: 'Workass browser control requires the active controller',
        };
      } else {
        try {
          const result = await this.manager.browserControl(method, request.params || {});
          response = { id: request.id ?? null, operationId, requestDigest, receipt: true, result };
        } catch (err) {
          response = {
            id: request.id ?? null, operationId, requestDigest, receipt: true,
            error: String(err && err.message || err),
          };
        }
      }
      try {
        this.persistTerminalReceipt(operationId, method, requestDigest, response.error ? 'failed' : 'completed');
      } catch {
        return {
          id: request.id ?? null, operationId, requestDigest, receipt: false,
          error: 'browser mutation terminal receipt could not be persisted',
        };
      }
      return response;
    });
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
    const requestDigest = String(request.requestDigest || '').trim().toLowerCase();
    if (!operationId || operationId.length > 256 || !validRequestDigest(requestDigest)) {
      return {
        id: request.id ?? null, operationId, requestDigest, receipt: false,
        error: 'browser receipt requires bounded operation identity and request digest',
      };
    }
    const existing = this.mutationReceipts.get(operationId);
    if (!existing) {
      return this.persistentReceipt(request) || { id: request.id ?? null, operationId, requestDigest, receipt: false };
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
    this.loadPersistentReceipts();
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
        if (request.instanceId !== this.instanceId) {
          throw new Error('browser control descriptor ownership changed');
        }
        if (request.method === 'browser.controlStatus') {
          writeJSON(res, 200, {
            id: request.id ?? null,
            result: { ready: true, controller: this.isController(), instanceId: this.instanceId },
          });
        } else if (request.method === 'browser.receipt') {
          if (!String(request.operationId || '').trim() || !String(request.requestDigest || '').trim()) {
            throw new Error('browser receipt requires operationId and requestDigest');
          }
          writeJSON(res, 200, await this.readMutationReceipt(request));
        } else if (MUTATING_METHODS.has(request.method)) {
          if (!String(request.operationId || '').trim() || !String(request.requestDigest || '').trim()) {
            throw new Error('browser mutation requires operationId and requestDigest');
          }
          writeJSON(res, 200, await this.dispatchMutation(request));
        } else {
          const result = await this.manager.browserControl(request.method, request.params || {});
          writeJSON(res, 200, { id: request.id ?? null, result });
        }
      } catch (err) {
        writeJSON(res, 200, { id: request?.id ?? null, error: String(err && err.message || err) });
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
    fs.writeFileSync(tmp, JSON.stringify({
      version: 2, url: this.url, token: this.token, pid: process.pid, instanceId: this.instanceId,
    }), { mode: 0o600 });
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
      if (current && current.pid === process.pid && current.instanceId === this.instanceId) fs.unlinkSync(this.controlFile);
    } catch { /* absent or replaced by a newer shell */ }
  }
}

module.exports = { BrowserControlServer, MAX_MUTATION_RECEIPTS, mutationReceiptKey };
