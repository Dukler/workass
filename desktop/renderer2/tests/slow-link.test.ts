// On a slow link one large frame can take tens of seconds while every reply
// queued behind it waits. The bridge reports that its socket is busy (an
// announced frame in transit, or large frames still arriving); the health
// monitor and hydration steps must then wait instead of declaring the daemon
// lost. An older bridge reports nothing and every fixed timeout is unchanged.
import assert from 'node:assert/strict';
import { after, afterEach, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let ConnectionMonitor: any;
let wireBusy: () => boolean;
const stores: any[] = [];

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true, ws: false }, appType: 'custom' });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  ConnectionMonitor = (await vite.ssrLoadModule('/src/wire/connection.ts')).ConnectionMonitor;
  wireBusy = (await vite.ssrLoadModule('/src/wire/api.ts')).wireBusy;
});
after(async () => {
  for (const store of stores) store.clearToastTimers();
  restoreWindow();
  await vite.close();
});

const hadWindow = Object.prototype.hasOwnProperty.call(globalThis, 'window');
const previousWindow = (globalThis as any).window;
function restoreWindow() {
  if (hadWindow) (globalThis as any).window = previousWindow;
  else delete (globalThis as any).window;
}

async function flush() { for (let i = 0; i < 20; i++) await Promise.resolve(); }

describe('slow link', () => {
  afterEach(restoreWindow);

  test('wireBusy follows the bridge and is false for an older bridge', () => {
    (globalThis as any).window = {};
    assert.equal(wireBusy(), false);
    (globalThis as any).window = { __workassWireState: () => ({ open: true, ready: true, lastMessageAt: 1, pending: 2, busy: true }) };
    assert.equal(wireBusy(), true);
    (globalThis as any).window = { __workassWireState: () => ({ open: false, ready: false, lastMessageAt: 1, pending: 2, busy: true }) };
    assert.equal(wireBusy(), false, 'a closed socket is never busy');
  });

  test('a ping stuck behind a frame in transit does not disconnect; a silent socket does', async (t) => {
    t.mock.timers.enable({ apis: ['setTimeout'] });
    try {
      for (const busy of [true, false]) {
        const statuses: string[] = [];
        const monitor = new ConnectionMonitor(() => new Promise(() => {}), (s: string) => statuses.push(s), () => {}, () => busy);
        monitor.start();
        for (let i = 0; i < 30; i++) { t.mock.timers.tick(1000); await flush(); }
        monitor.stop();
        await flush();
        if (busy) assert.deepEqual(statuses, [], 'a busy slow link was declared disconnected');
        else assert.equal(statuses[0], 'disconnected', 'a dead socket was never noticed');
      }
    } finally {
      t.mock.timers.reset();
    }
  });

  test('a hydration step waits while the socket is busy and gives up once it is not', async (t) => {
    let busy = true;
    (globalThis as any).window = { api: {}, __workassWireState: () => ({ open: true, ready: true, lastMessageAt: 1, pending: 1, busy }) };
    const store = new StoreCtor();
    stores.push(store);
    t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
    try {
      let settled: unknown = 'pending';
      void store.guardedStep('slow read', () => new Promise(() => {}), 15000).then((value: unknown) => { settled = value; });
      for (let i = 0; i < 120; i++) { t.mock.timers.tick(1000); await flush(); }
      assert.equal(settled, 'pending', 'a step failed while its reply was still in transit');
      busy = false;
      for (let i = 0; i < 16; i++) { t.mock.timers.tick(1000); await flush(); }
      assert.deepEqual(settled, { ok: false });
    } finally {
      t.mock.timers.reset();
    }
  });
});

// Energy lane 2026-11-13: a hidden window pings every 30 s instead of 5 s, and
// probeNow (the store calls it when the window shows) pings at once.
test('a hidden window pings less often and pings at once when shown', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const flushAll = async () => { for (let i = 0; i < 5; i++) await Promise.resolve(); };
  let hidden = false;
  let pings = 0;
  const monitor = new ConnectionMonitor(() => { pings++; return Promise.resolve(); }, () => {}, () => {}, () => false, () => hidden);
  try {
    monitor.start();
    for (let i = 0; i < 20; i++) { t.mock.timers.tick(1000); await flushAll(); }
    assert.equal(pings, 4, 'visible: every 5 s');
    hidden = true;
    pings = 0;
    t.mock.timers.tick(5000); await flushAll();   // the sleep that began while visible
    pings = 0;
    for (let i = 0; i < 60; i++) { t.mock.timers.tick(1000); await flushAll(); }
    assert.equal(pings, 2, 'hidden: every 30 s');
    hidden = false;
    pings = 0;
    monitor.probeNow(); await flushAll();
    assert.equal(pings, 1, 'shown: at once');
    const status = monitor.connected();
    assert.equal(status, true);
  } finally {
    monitor.stop();
    t.mock.timers.reset();
  }
});
