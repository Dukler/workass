'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { BrowserControlServer, MAX_MUTATION_RECEIPTS } = require('./browser-control-server');

const digest = (value) => crypto.createHash('sha256').update(String(value)).digest('hex');

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
  assert.equal(descriptor.version, 2);
  assert.equal(descriptor.token, 'test-only-token');
  assert.equal(fs.statSync(controlFile).mode & 0o777, 0o600);

  const denied = await fetch(descriptor.url, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: 1, method: 'browser.list' }),
  });
  assert.equal(denied.status, 403);
  const response = await fetch(descriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer test-only-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: 2, method: 'browser.list', instanceId: descriptor.instanceId, params: {} }),
  }).then((reply) => reply.json());
  assert.deepEqual(response, { id: 2, result: { method: 'browser.list', params: {}, visible: true } });
  assert.deepEqual(calls, [{ method: 'browser.list', params: {} }]);
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
  const post = (body) => {
    const active = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
    return fetch(active.url, {
      method: 'POST',
      headers: { Authorization: 'Bearer journal-token', 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...body, instanceId: active.instanceId }),
    }).then((reply) => reply.json());
  };

  const mutation = {
    method: 'browser.click', operationId: 'agent-mcp:once', requestDigest: digest('a'),
    params: { tabId: 7, selector: '#save', secret: 'must-not-be-journaled' },
  };
  const first = post({ ...mutation, id: 1 });
  const second = post({ ...mutation, id: 2 });
  const [firstReply, secondReply] = await Promise.all([first, second]);
  assert.equal(calls, 1);
  assert.equal(firstReply.receipt, true);
  assert.equal(secondReply.receipt, true);
  assert.deepEqual(firstReply.result, secondReply.result);

  const readback = await post({ id: 3, method: 'browser.receipt', operationId: 'agent-mcp:once', requestDigest: digest('a') });
  assert.equal(readback.receipt, true);
  assert.deepEqual(readback.result, firstReply.result);
  assert.equal(calls, 1);

  const conflict = await post({ ...mutation, id: 4, requestDigest: digest('b') });
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
  const post = (body) => {
    const active = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
    return fetch(active.url, {
      method: 'POST',
      headers: { Authorization: 'Bearer cache-token', 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...body, instanceId: active.instanceId }),
    }).then((reply) => reply.json());
  };
  const mutation = (operationId) => ({
    id: operationId, method: 'browser.click', operationId, requestDigest: digest(operationId),
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

  const oldest = await post({ id: 'oldest', method: 'browser.receipt', operationId: 'settled-0', requestDigest: digest('settled-0') });
  const newest = await post({ id: 'newest', method: 'browser.receipt', operationId: `settled-${MAX_MUTATION_RECEIPTS}`, requestDigest: digest(`settled-${MAX_MUTATION_RECEIPTS}`) });
  assert.equal(oldest.receipt, false);
  assert.equal(newest.receipt, true);
});

test('controller rejection and terminal outcome survive shell replacement without persisting browser payloads', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-restart-receipt-'));
  const controlFile = path.join(dir, 'control.json');
  const receiptDir = path.join(dir, 'receipts');
  let calls = 0;
  const manager = { async browserControl() { calls += 1; return { page: 'must-not-persist' }; } };
  const operationId = 'agent-mcp:controller-rejection';
  const requestDigest = digest('controller-rejection');
  const params = { selector: '#private', text: 'must-not-persist', url: 'https://private.invalid/' };
  let first = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'first-token', isController: () => false,
  });
  await first.start();
  const firstDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const rejected = await fetch(firstDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer first-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 1, method: 'browser.click', instanceId: firstDescriptor.instanceId,
      operationId, requestDigest, params,
    }),
  }).then((reply) => reply.json());
  assert.equal(rejected.receipt, true);
  assert.match(rejected.error, /active controller/);
  assert.equal(calls, 0);
  await first.close();

  const persisted = fs.readdirSync(receiptDir).filter((name) => name.endsWith('.json'));
  assert.equal(persisted.length, 1);
  const storedText = fs.readFileSync(path.join(receiptDir, persisted[0]), 'utf8');
  for (const forbidden of [operationId, params.selector, params.text, params.url, 'page']) {
    assert.equal(storedText.includes(forbidden), false, `receipt persisted forbidden text: ${forbidden}`);
  }

  const second = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'second-token', isController: () => true,
  });
  first = null;
  t.after(async () => { await second.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  await second.start();
  const secondDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const readback = await fetch(secondDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer second-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 2, method: 'browser.receipt', instanceId: secondDescriptor.instanceId,
      operationId, requestDigest,
    }),
  }).then((reply) => reply.json());
  assert.equal(readback.receipt, true);
  assert.equal(readback.error, 'browser mutation was rejected');
  assert.equal(calls, 0);
});

test('completed mutation receipt survives shell replacement and cannot re-execute', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-completed-receipt-'));
  const controlFile = path.join(dir, 'control.json');
  const receiptDir = path.join(dir, 'receipts');
  let calls = 0;
  const manager = {
    async browserControl() {
      calls += 1;
      return { clicked: true, pageText: 'must-not-persist' };
    },
  };
  const operationId = 'agent-mcp:completed-before-restart';
  const requestDigest = digest('completed-before-restart');
  const params = { selector: '#private', text: 'must-not-persist', url: 'https://private.invalid/' };
  const first = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'first-token', isController: () => true,
  });
  await first.start();
  const firstDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const completed = await fetch(firstDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer first-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 1, method: 'browser.click', instanceId: firstDescriptor.instanceId,
      operationId, requestDigest, params,
    }),
  }).then((reply) => reply.json());
  assert.equal(completed.receipt, true);
  assert.equal(completed.result.clicked, true);
  assert.equal(calls, 1);
  await first.close();

  const persisted = fs.readdirSync(receiptDir).filter((name) => name.endsWith('.json'));
  assert.equal(persisted.length, 1);
  const storedText = fs.readFileSync(path.join(receiptDir, persisted[0]), 'utf8');
  for (const forbidden of [operationId, params.selector, params.text, params.url, 'pageText', 'clicked']) {
    assert.equal(storedText.includes(forbidden), false, `receipt persisted forbidden text: ${forbidden}`);
  }

  const second = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'second-token', isController: () => true,
  });
  t.after(async () => { await second.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  await second.start();
  const secondDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const readback = await fetch(secondDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer second-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 2, method: 'browser.receipt', instanceId: secondDescriptor.instanceId,
      operationId, requestDigest,
    }),
  }).then((reply) => reply.json());
  assert.deepEqual(readback.result, { operationId, status: 'completed', receipt: true });
  assert.equal(readback.receipt, true);
  assert.equal(calls, 1);

  const retriedMutation = await fetch(secondDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer second-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 3, method: 'browser.click', instanceId: secondDescriptor.instanceId,
      operationId, requestDigest, params,
    }),
  }).then((reply) => reply.json());
  assert.equal(retriedMutation.receipt, true);
  assert.equal(retriedMutation.result.status, 'completed');
  assert.equal(calls, 1);
});

test('replacement shell observes a receipt committed after its startup', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-browser-late-receipt-'));
  const controlFile = path.join(dir, 'control.json');
  const receiptDir = path.join(dir, 'receipts');
  let calls = 0;
  let release;
  let markStarted;
  const blocked = new Promise((resolve) => { release = resolve; });
  const started = new Promise((resolve) => { markStarted = resolve; });
  const manager = {
    async browserControl() {
      calls += 1;
      markStarted();
      await blocked;
      return { clicked: true };
    },
  };
  const operationId = 'agent-mcp:late-shell-receipt';
  const requestDigest = digest('late-shell-receipt');
  const first = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'first-token', isController: () => true,
  });
  await first.start();
  const firstDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const pending = fetch(firstDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer first-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 1, method: 'browser.click', instanceId: firstDescriptor.instanceId,
      operationId, requestDigest, params: { selector: '#save' },
    }),
  }).then((reply) => reply.json());
  await started;

  const second = new BrowserControlServer({
    manager, controlFile, receiptDir, token: 'second-token', isController: () => true,
  });
  t.after(async () => {
    release();
    await first.close();
    await second.close();
    fs.rmSync(dir, { recursive: true, force: true });
  });
  await second.start();
  release();
  assert.equal((await pending).receipt, true);

  const secondDescriptor = JSON.parse(fs.readFileSync(controlFile, 'utf8'));
  const readback = await fetch(secondDescriptor.url, {
    method: 'POST',
    headers: { Authorization: 'Bearer second-token', 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: 2, method: 'browser.receipt', instanceId: secondDescriptor.instanceId,
      operationId, requestDigest,
    }),
  }).then((reply) => reply.json());
  assert.equal(readback.receipt, true);
  assert.equal(readback.result.status, 'completed');
  assert.equal(calls, 1);
});
