'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { BrowserControlServer } = require('./browser-control-server');

test('authenticated provider-neutral control file routes RPC without leaking the token', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-control-'));
  const controlFile = path.join(dir, 'control.json');
  const calls = [];
  const manager = {
    async browserControl(method, params) {
      calls.push({ method, params });
      return { method, params, visible: true };
    },
  };
  const server = new BrowserControlServer({ manager, controlFile, token: 'test-only-token', isController: () => true });
  t.after(async () => { await server.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const state = await server.start();
  assert.equal(state.ready, true);
  const descriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  assert.equal(descriptor.token, 'test-only-token');
  assert.equal(fs.statSync(controlFile).mode & 0o777, 0o600);

  const denied = await fetch(descriptor.url, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: 1, method: 'browser.list' }),
  });
  assert.equal(denied.status, 403);
  const response = await fetch(descriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer test-only-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: 2, method: 'browser.open', params: { url: 'http://localhost:5173' } }),
  }).then((reply) => reply.json());
  assert.deepEqual(response, { id: 2, result: { method: 'browser.open', params: { url: 'http://localhost:5173' }, visible: true } });
  assert.deepEqual(calls, [{ method: 'browser.open', params: { url: 'http://localhost:5173' } }]);
  assert.doesNotMatch(JSON.stringify(state), /test-only-token/);
});
