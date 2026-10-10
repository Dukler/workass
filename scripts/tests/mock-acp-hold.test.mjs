import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

test('mock prompt and immediate cancel preserve wire order in one input write', async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), 'workass-mock-cancel-'));
  const child = spawn(process.execPath, [fileURLToPath(new URL('../../desktop/acp/mock-server.mjs', import.meta.url))], {
    cwd: temporary, env: { ...process.env, WORKASS_MOCK_ACP_DELAY_MS: '0' }, stdio: ['pipe', 'pipe', 'pipe'],
  });
  const exited = once(child, 'exit');
  child.stderr.resume();
  const lines = createInterface({ input: child.stdout });
  const pending = new Map();
  let sequence = 0;
  lines.on('line', line => {
    const frame = JSON.parse(line);
    const waiter = pending.get(frame.id);
    if (!waiter) return;
    pending.delete(frame.id);
    clearTimeout(waiter.timer);
    if (frame.error) waiter.reject(new Error(frame.error.message)); else waiter.resolve(frame.result);
  });
  const request = (method, params, cancel = false) => new Promise((resolve, reject) => {
    const id = ++sequence;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} did not settle`)); }, 2000);
    pending.set(id, { resolve, reject, timer });
    let wire = `${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`;
    if (cancel) wire += `${JSON.stringify({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: params.sessionId } })}\n`;
    child.stdin.write(wire);
  });
  try {
    await request('initialize', { protocolVersion: 1, clientCapabilities: {} });
    const session = await request('session/new', { cwd: temporary });
    const result = await request('session/prompt', { sessionId: session.sessionId, prompt: [{ type: 'text', text: '[mock:hold-until-steer] cancel immediately' }] }, true);
    assert.equal(result.stopReason, 'cancelled');
  } finally {
    for (const waiter of pending.values()) clearTimeout(waiter.timer);
    child.kill();
    await exited;
    lines.close();
    await fs.rm(temporary, { recursive: true, force: true });
  }
});

test('mock held prompt observes a release created before its directory watcher attaches', async () => {
  const root = fileURLToPath(new URL('../..', import.meta.url));
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), 'workass-mock-hold-'));
  const release = path.join(temporary, 'release');
  const preload = path.join(temporary, 'watch-boundary.mjs');
  // Force the actual race between the fixture's initial exists check and
  // fs.watch registration. No timing delay or copied hold implementation.
  await fs.writeFile(preload, `import fs from 'node:fs';
const original = fs.watch;
fs.watch = function(...args) {
  fs.writeFileSync(process.env.WORKASS_MOCK_ACP_HOLD_FILE, 'release');
  // A file created before registration need not publish a later notification.
  // Drop that notification deterministically instead of relying on OS timing.
  args[args.length - 1] = () => {};
  return original.apply(this, args);
};
`);
  const child = spawn(process.execPath, ['--import', preload, path.join(root, 'desktop/acp/mock-server.mjs')], {
    cwd: temporary,
    env: { ...process.env, WORKASS_MOCK_ACP_DELAY_MS: '0', WORKASS_MOCK_ACP_HOLD_FILE: release },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  const exited = once(child, 'exit');
  child.stderr.resume();
  const lines = createInterface({ input: child.stdout });
  const pending = new Map();
  let sequence = 0;
  lines.on('line', line => {
    const frame = JSON.parse(line);
    if (!pending.has(frame.id)) return;
    const waiter = pending.get(frame.id);
    pending.delete(frame.id);
    clearTimeout(waiter.timer);
    if (frame.error) waiter.reject(new Error(frame.error.message)); else waiter.resolve(frame.result);
  });
  const request = (method, params) => new Promise((resolve, reject) => {
    const id = ++sequence;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`${method} did not reach its release boundary`)); }, 2000);
    pending.set(id, { resolve, reject, timer });
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  });
  try {
    await request('initialize', { protocolVersion: 1, clientCapabilities: {} });
    const session = await request('session/new', { cwd: temporary });
    const result = await request('session/prompt', { sessionId: session.sessionId, prompt: [{ type: 'text', text: '[mock:hold-until-steer] held direction' }] });
    assert.equal(result.stopReason, 'end_turn');
    assert.equal(await fs.readFile(release, 'utf8'), 'release');
  } finally {
    for (const waiter of pending.values()) clearTimeout(waiter.timer);
    child.kill();
    await exited;
    lines.close();
    await fs.rm(temporary, { recursive: true, force: true });
  }
});
