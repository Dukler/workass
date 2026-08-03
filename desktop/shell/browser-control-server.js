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
    this.server = null;
    this.url = '';
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
        const result = await this.manager.browserControl(request.method, request.params || {});
        writeJSON(res, 200, { id: request.id ?? null, result });
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

module.exports = { BrowserControlServer, defaultControlFile };
