'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { BrowserControlServer, MAX_MUTATION_RECEIPTS } = require('./browser-control-server');

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

test('journaled browser mutations execute once and support pure receipt readback', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-journal-'));
  const controlFile = path.join(dir, 'control.json');
  let calls = 0;
  const manager = {
    async browserControl(method, params) {
      calls += 1;
      await new Promise((resolve) => setTimeout(resolve, 10));
      return { method, params, execution: calls };
    },
  };
  const server = new BrowserControlServer({ manager, controlFile, token: 'journal-token', isController: () => true });
  t.after(async () => { await server.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const descriptor = await server.start();
  const post = (body) => fetch(JSON.parse(fs.readFileSync(controlFile, 'utf8')).url, {
    method: 'POST',
    headers: { Authorization: 'Bearer journal-token', 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }).then((reply) => reply.json());

  const mutation = {
    method: 'browser.click', operationId: 'agent-mcp:once', requestDigest: 'digest-a',
    params: { tabId: 7, selector: '#save', secret: 'must-not-be-journaled' },
  };
  const first = post({ ...mutation, id: 1 });
  const second = post({ ...mutation, id: 2 });
  const [firstReply, secondReply] = await Promise.all([first, second]);
  assert.equal(calls, 1);
  assert.equal(firstReply.receipt, true);
  assert.equal(secondReply.receipt, true);
  assert.deepEqual(firstReply.result, secondReply.result);

  const readback = await post({ id: 3, method: 'browser.receipt', operationId: 'agent-mcp:once', requestDigest: 'digest-a' });
  assert.equal(readback.receipt, true);
  assert.deepEqual(readback.result, firstReply.result);
  assert.equal(calls, 1);

  const conflict = await post({ ...mutation, id: 4, requestDigest: 'digest-b' });
  assert.match(conflict.error, /reused with a different request/);
  assert.equal(calls, 1);
  assert.equal(descriptor.ready, true);
});

test('mutation receipt cache evicts settled records deterministically but never in-flight records', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-cache-'));
  const controlFile = path.join(dir, 'control.json');
  let releaseInFlight;
  let markStarted;
  const started = new Promise((resolve) => { markStarted = resolve; });
  const inFlight = new Promise((resolve) => { releaseInFlight = resolve; });
  const manager = {
    async browserControl(method, params) {
      if (params.operationId === 'in-flight') {
        markStarted();
        await inFlight;
      }
      return { method, operationId: params.operationId };
    },
  };
  const server = new BrowserControlServer({ manager, controlFile, token: 'cache-token', isController: () => true });
  t.after(async () => { releaseInFlight(); await server.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  await server.start();
  const post = (body) => fetch(JSON.parse(fs.readFileSync(controlFile, 'utf8')).url, {
    method: 'POST',
    headers: { Authorization: 'Bearer cache-token', 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }).then((reply) => reply.json());
  const mutation = (operationId) => ({
    id: operationId, method: 'browser.click', operationId, requestDigest: `digest-${operationId}`,
    params: { operationId },
  });

  const live = post(mutation('in-flight'));
  await started;
  for (let index = 0; index <= MAX_MUTATION_RECEIPTS; index += 1) {
    await post(mutation(`settled-${index}`));
  }
  assert.equal(server.mutationReceipts.has('in-flight'), true);
  assert.equal(server.mutationReceipts.size <= MAX_MUTATION_RECEIPTS, true);
  releaseInFlight();
  await live;
  assert.equal(server.mutationReceipts.size <= MAX_MUTATION_RECEIPTS, true);

  const oldest = await post({ id: 'oldest', method: 'browser.receipt', operationId: 'settled-0', requestDigest: 'digest-settled-0' });
  const newest = await post({ id: 'newest', method: 'browser.receipt', operationId: `settled-${MAX_MUTATION_RECEIPTS}`, requestDigest: `digest-settled-${MAX_MUTATION_RECEIPTS}` });
  assert.equal(oldest.receipt, false);
  assert.equal(newest.receipt, true);
});
