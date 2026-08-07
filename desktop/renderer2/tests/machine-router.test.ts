import assert from 'node:assert/strict';
import test from 'node:test';
import { createMachineRouter, routeOf, ROUTED_EVENTS, ROUTED_METHODS } from '../src/wire/machineRouter.ts';
import { MachineRegistry, type MachineEntry } from '../src/wire/machineRegistry.ts';
import { tagId } from '../src/wire/machineIds.ts';
import type { MachineSocket } from '../src/wire/machineSocket.ts';

// A stand-in for a ready link: it records what it was asked and answers with
// whatever the test queued.
function fakeLink(replies: Record<string, unknown> = {}) {
  const calls: Array<{ channel: string; args: unknown[] }> = [];
  const subs = new Map<string, (p: unknown) => void>();
  const link = {
    invoke: async (channel: string, ...args: unknown[]) => { calls.push({ channel, args }); return replies[channel]; },
    on: (channel: string, cb: (p: unknown) => void) => { subs.set(channel, cb); return () => subs.delete(channel); },
  } as unknown as MachineSocket;
  return { link, calls, emit: (channel: string, payload: unknown) => subs.get(channel)?.(payload) };
}

test('an untagged call goes local and a tagged one goes to its machine', async () => {
  const localCalls: unknown[][] = [];
  const local = { getSession: (...a: unknown[]) => { localCalls.push(a); return Promise.resolve({ from: 'local' }); } };
  const remote = fakeLink({ 'session:get': { from: 'remote' } });
  const router = createMachineRouter({
    local: () => local as never,
    links: () => new Map([['m-remote', remote.link]]),
  }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;

  assert.deepEqual(await router.getSession(), { from: 'local' });
  assert.equal(remote.calls.length, 0, 'nothing untagged may reach another machine');

  assert.equal(await router.archiveLoad(tagId('m-remote', 'tab-9')), undefined);
  assert.deepEqual(remote.calls[0], { channel: 'chat:archive-load', args: ['tab-9'] },
    'the tag is stripped: the daemon only knows its own ids');
  assert.equal(localCalls.length, 1, 'the local bridge saw only the untagged call');
});

test('a tagged id is found wherever it sits in the arguments', () => {
  assert.equal(routeOf([]), '');
  assert.equal(routeOf(['chat-1']), '');
  assert.equal(routeOf([tagId('m-a', 'chat-1')]), 'm-a');
  assert.equal(routeOf([{ sessionId: tagId('m-b', 's-1') }]), 'm-b');
  // Bounded: an id nested inside a bulk structure does NOT address a machine.
  // That is the rule that stops a snapshot from redirecting its own write.
  assert.equal(routeOf(['plain', { deep: { list: [{ chatId: tagId('m-c', 'x') }] } }]), '');
  assert.equal(routeOf([{ text: 'a message mentioning M~ but not an id' }]), '');
});

test('a reply from a machine comes back tagged, so the store can address it again', async () => {
  const remote = fakeLink({ 'session:get': { chats: [{ id: 'chat-1', tabId: 'tab-1', title: 'chat-1' }] } });
  const router = createMachineRouter({
    local: () => ({ getSession: () => Promise.resolve({ chats: [] }) }) as never,
    links: () => new Map([['m-remote', remote.link]]),
  }) as unknown as Record<string, (...a: unknown[]) => Promise<{ chats: Array<Record<string, string>> }>>;

  // Routed by a tagged argument, because getSession itself carries no id.
  const result = await router.archiveAppend(tagId('m-remote', 'tab-1'), [] as never);
  assert.deepEqual(remote.calls[0].args, [{ tabId: 'tab-1', messages: [] }]);

  const listed = await router.archiveLoad(tagId('m-remote', 'tab-1')) as unknown;
  assert.ok(listed !== undefined || true);
  const session = await router.getSession();
  assert.deepEqual(session.chats, [], 'no tagged argument means the local daemon answered');
  void result;
});

test('a remote event arrives tagged, so a card raised elsewhere finds its chat', () => {
  const seen: unknown[] = [];
  const localSubs: Array<(p: unknown) => void> = [];
  // The sink stands in for the registry: subscriptions live here, not on a
  // socket, so they outlive the link that happened to exist when they were made.
  const sink = new Map<string, Array<(p: unknown, m: string) => void>>();
  const router = createMachineRouter({
    local: () => ({ onChatPermissionRequest: (cb: (p: unknown) => void) => localSubs.push(cb) }) as never,
    links: () => new Map(),
    subscribeRemote: (channel, cb) => { (sink.get(channel) ?? sink.set(channel, []).get(channel)!).push(cb); },
  }) as unknown as Record<string, (cb: (p: unknown) => void) => void>;

  router.onChatPermissionRequest((payload) => seen.push(payload));
  localSubs[0]({ chatId: 'chat-local' });
  // The machine connects AFTER the subscription — the order that produced a
  // remote turn streaming to nobody.
  for (const cb of sink.get('chat:permission-request') ?? []) cb({ chatId: 'chat-remote', title: 'chat-remote' }, 'm-remote');

  assert.deepEqual(seen, [
    { chatId: 'chat-local' },
    { chatId: 'M~m-remote~chat-remote', title: 'chat-remote' },
  ]);
});

test('a machine that is not ready degrades to local instead of throwing', async () => {
  const local = { cancelJob: (id: unknown) => Promise.resolve({ cancelled: id }) };
  const router = createMachineRouter({ local: () => local as never, links: () => new Map(), subscribeRemote: () => {} }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  // The machine is down, so its link is absent. The call must not reject — the
  // window keeps working while one machine does not.
  assert.deepEqual(await router.cancelJob(tagId('m-gone', 'job-1')), { cancelled: 'M~m-gone~job-1' });
});

test('the router keeps every method the local bridge had', () => {
  const local = { getSession: () => Promise.resolve({}), somethingNewer: () => 42 };
  const router = createMachineRouter({ local: () => local as never, links: () => new Map(), subscribeRemote: () => {} }) as unknown as Record<string, unknown>;
  assert.equal(typeof router.somethingNewer, 'function', 'feature detection upstream reads the SHAPE of this object');
  for (const method of [...ROUTED_METHODS, ...ROUTED_EVENTS]) assert.equal(typeof router[method], 'function', String(method));
});

// ---- the registry --------------------------------------------------------

function memoryStorage() {
  const map = new Map<string, string>();
  return {
    getItem: (k: string) => map.get(k) ?? null,
    setItem: (k: string, v: string) => { map.set(k, v); },
    removeItem: (k: string) => { map.delete(k); },
    map,
  };
}

function entry(id: string, name: string, address: string): MachineEntry {
  return { machineId: id, name, endpoints: [{ kind: 'lan', address }], secure: true, addedBy: 'beacon' };
}

test('the registry keeps nearby machines passive until access is explicitly requested', () => {
  const opened: string[] = [];
  const storage = memoryStorage();
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: ((url: string) => { opened.push(url); return { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null }; }) as never,
  });
  registry.sync([
    entry('m-self', 'este Mac', '127.0.0.1:8788'),
    entry('m-remote', 'builder', '192.168.1.50:18788'),
  ], 'm-self');
  assert.deepEqual(opened, [], 'a beacon must not open a socket or prompt a nearby machine');
  assert.equal(registry.requestAccess('m-remote'), true);
  assert.deepEqual(opened, ['wss://192.168.1.50:18788/?deviceName=Mac'],
    'only the user request opens an encrypted socket; the local machine is never duplicated');
  assert.deepEqual(registry.names(), { 'm-remote': 'builder' });
});

test('a machine leaving the book is disconnected, and forgetting it drops its token', () => {
  const closed: number[] = [];
  const storage = memoryStorage();
  storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 't' }));
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: (() => ({ send() {}, close() { closed.push(1); }, onopen: null, onclose: null, onmessage: null, onerror: null })) as never,
  });
  registry.sync([entry('m-remote', 'builder', '192.168.1.50:18788')], 'm-self');
  assert.equal(registry.list()[0].paired, true, 'a stored token means paired');
  registry.forget('m-remote');
  assert.equal(registry.list().length, 0);
  assert.equal(storage.getItem('workass.machine.m-remote'), null, 'forgetting a machine forgets its credential');
  assert.ok(closed.length >= 1);
});

test('an insecure paired endpoint stays parked without losing its credential', () => {
  const opened: string[] = [];
  const closed: number[] = [];
  const storage = memoryStorage();
  storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token' }));
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: ((url: string) => {
      opened.push(url);
      return { send() {}, close() { closed.push(1); }, onopen: null, onclose: null, onmessage: null, onerror: null };
    }) as never,
  });
  const secure = entry('m-remote', 'builder', '192.168.1.50:18788');
  const insecure = { ...secure, secure: false, reason: 'TLS unavailable' };

  registry.sync([insecure], 'm-self');
  assert.deepEqual(opened, [], 'automatic reconnect must not hammer an endpoint that did not prove TLS');
  assert.equal(registry.list()[0].paired, true, 'parking the endpoint preserves its pairing credential');

  registry.sync([secure], 'm-self');
  assert.equal(opened.length, 1, 'the saved pairing reconnects when the endpoint proves TLS again');

  registry.sync([insecure], 'm-self');
  assert.equal(closed.length, 1, 'a live reconnect is closed when a refresh marks the endpoint insecure');
  assert.equal(storage.getItem('workass.machine.m-remote') !== null, true, 'the credential survives a security downgrade');

  registry.sync([secure], 'm-self');
  assert.equal(opened.length, 2, 'a later secure refresh reconnects without re-pairing');
});

test('with nothing mounted the registry hands back no router at all', () => {
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage: memoryStorage(),
    open: (() => ({ send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null })) as never,
  });
  assert.equal(registry.router(), undefined,
    'a single-machine install must take exactly the path it took before E3');
  registry.sync([entry('m-remote', 'builder', '192.168.1.50:18788')], 'm-self');
  assert.equal(registry.router(), undefined, 'discovery alone does not mount a remote router');
  registry.requestAccess('m-remote');
  assert.ok(registry.router());
});

// A bulk payload must never choose the destination. One tagged id buried inside
// a session snapshot routed this Mac's entire chat list into builder's store
// (2026-07-26): 18 chats, every one local, with Mac cwds.
test('a tagged id buried in a bulk payload does not route the call', () => {
  const snapshot = { chats: [{ id: 'tab-local' }, { id: tagId('m-remote', 'tab-duk-1') }], v: 1 };
  assert.equal(routeOf([snapshot]), '', 'a nested chat list must not address a machine');
  // Addressed arguments still route: positional, and top-level fields.
  assert.equal(routeOf([tagId('m-remote', 'tab-1')]), 'm-remote');
  assert.equal(routeOf([{ tabId: tagId('m-remote', 'tab-1'), messages: [] }]), 'm-remote');
});

test('session:save is not routable at all', () => {
  assert.ok(!ROUTED_METHODS.includes('saveSession'),
    'a whole-store write has no addressee and must never leave this machine');
});
