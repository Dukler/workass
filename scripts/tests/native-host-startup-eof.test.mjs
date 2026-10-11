import test from 'node:test';
import assert from 'node:assert/strict';
import { PassThrough } from 'node:stream';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..');

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

// 2026-10-11 review: stdin closing while session/new was still inside
// createAgentSession left the late session subscribed, never disposed, and
// replying success after shutdown. Shutdown must join in-flight opens.
for (const kind of ['omp', 'pi']) {
  test(`${kind} host EOF during session startup disposes the late session`, async (t) => {
    const scratch = await mkdtemp(path.join(tmpdir(), `workass-${kind}-eof-`));
    t.after(() => rm(scratch, { recursive: true, force: true }));
    const gate = deferred();
    const entered = deferred();
    let disposed = 0;
    let subscriptions = 0;
    const native = {
      abort: async () => {}, dispose: () => { disposed++; },
      subscribe: () => { subscriptions++; return () => { subscriptions--; }; },
      model: { provider: 'fixture', id: 'model' }, thinkingLevel: 'off',
      modelRegistry: { getAvailable: () => [{ provider: 'fixture', id: 'model' }] },
      modelRuntime: { getAvailable: async () => [{ provider: 'fixture', id: 'model' }] },
      bindExtensions: async () => {}, getAvailableThinkingLevels: () => ['off'], settings: { get: () => false },
    };
    const sdk = {
      AgentRegistry: class {},
      SessionManager: { create: () => ({ getSessionId: () => `${kind}-scratch-session`, ensureOnDisk: async () => {}, flush: async () => {} }) },
      getAgentDir: () => scratch, DefaultResourceLoader: class { async reload() {} },
      createAgentSession: async () => { entered.resolve(); await gate.promise; return { session: native, setToolUIContext: () => {} }; },
    };
    const input = new PassThrough();
    const output = new PassThrough();
    const messages = [];
    output.on('data', (chunk) => {
      for (const line of String(chunk).split('\n').filter(Boolean)) messages.push(JSON.parse(line));
    });
    const mod = await import(pathToFileURL(path.join(root, 'scripts', `${kind}-native-host.mjs`)).href);
    const serve = (kind === 'omp' ? mod.serveOMP : mod.servePi)({ sdkModule: sdk, input, output });
    input.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'session/new', params: { cwd: scratch } }) + '\n');
    await entered.promise;
    input.end();
    setTimeout(() => gate.resolve(), 50);
    await serve;
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(subscriptions, 0, 'the late session is still subscribed after shutdown');
    assert.ok(disposed >= 1, 'the late session was never disposed');
    assert.equal(messages.find((message) => message.id === 1)?.result, undefined, 'a session opened after EOF replied success');
  });
}
