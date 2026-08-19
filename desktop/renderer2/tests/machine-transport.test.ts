import assert from 'node:assert/strict';
import test from 'node:test';
import {
  MachineSocket, PRE_READY_CHANNELS, WorkassInvokeError,
  type MachineSocketLike, type MachineCredentials,
} from '../src/wire/machineSocket.ts';
import {
  isTagged, localId, machineOf, splitId, tagId, tagPayload, untagPayload,
} from '../src/wire/machineIds.ts';
import { machineWhere, remoteMachineBadge, shortMachineId, unifiedOrder } from '../src/machine-label.ts';
import { fleetDeviceToken, fleetProof } from '../src/wire/fleet.ts';

// ---- a socket the test drives -------------------------------------------

class FakeSocket implements MachineSocketLike {
  sent: string[] = [];
  closed = false;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onmessage: ((data: string) => void) | null = null;
  onerror: ((err: unknown) => void) | null = null;
  readonly url: string;
  constructor(url: string) { this.url = url; }
  send(data: string) { this.sent.push(data); }
  close() { this.closed = true; this.onclose?.(); }
  frames() { return this.sent.map((raw) => JSON.parse(raw) as { id: number; channel: string; args: unknown[] }); }
  deliver(frame: unknown) { this.onmessage?.(JSON.stringify(frame)); }
  reply(id: number, result: unknown, error: string | null = null) { this.deliver({ t: 'reply', id, result, error }); }
  event(channel: string, payload: unknown) { this.deliver({ t: 'event', channel, payload }); }
  access(payload: Record<string, unknown>) { this.event('lan:access-state', payload); }
}

interface Harness {
  link: MachineSocket;
  sockets: FakeSocket[];
  saved: MachineCredentials[];
  opens: { generation: number; instanceId: string; restarted: boolean }[];
  states: string[];
  timers: { fn: () => void; ms: number }[];
  runTimers(): void;
  last(): FakeSocket;
}

function harness(extra: Partial<Parameters<typeof MachineSocket.prototype.constructor>[0]> = {}): Harness {
  const sockets: FakeSocket[] = [];
  const saved: MachineCredentials[] = [];
  const opens: Harness['opens'] = [];
  const states: string[] = [];
  const timers: { fn: () => void; ms: number }[] = [];
  const link = new MachineSocket({
    machineId: 'm-remote', url: 'ws://192.168.1.50:18788', deviceName: 'iPhone',
    open: (url) => { const s = new FakeSocket(url); sockets.push(s); return s; },
    saveCredentials: (_id, next) => saved.push(next),
    onOpen: (info) => opens.push(info),
    onState: (state) => states.push(state),
    setTimer: (fn, ms) => { timers.push({ fn, ms }); return timers.length - 1; },
    clearTimer: (handle) => { const i = handle as number; if (timers[i]) timers[i] = { fn: () => {}, ms: 0 }; },
    ...(extra as object),
  });
  return {
    link, sockets, saved, opens, states, timers,
    runTimers() { const due = timers.splice(0, timers.length); for (const t of due) t.fn(); },
    last() { return sockets[sockets.length - 1]; },
  };
}

// ---- the two-phase gate --------------------------------------------------

test('an invoke waits for approval, except the calls that produce one', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();

  const queued = h.link.invoke('session:get');
  const allowed = h.link.invoke('lan:pairing-info');
  assert.deepEqual(h.last().frames().map((f) => f.channel), ['lan:pairing-info'],
    'only a pre-ready channel may go out before approval');

  h.last().access({ state: 'approved', deviceId: 'd1', name: 'iPhone', instanceId: 'i-1' });
  assert.deepEqual(h.last().frames().map((f) => f.channel), ['lan:pairing-info', 'session:get'],
    'approval flushes the queue');

  const frames = h.last().frames();
  h.last().reply(frames[1].id, { ok: true });
  h.last().reply(frames[0].id, { deprecated: true });
  assert.deepEqual(await queued, { ok: true });
  assert.deepEqual(await allowed, { deprecated: true });
  assert.equal(h.link.linkState, 'ready');
});

test('the pre-ready set is exactly the three channels the daemon allows', () => {
  assert.deepEqual([...PRE_READY_CHANNELS].sort(), ['fleet:challenge', 'fleet:enroll', 'lan:pairing-info']);
});

// ---- generation and restart ---------------------------------------------

test('a reply from a superseded socket is dropped, and its invoke is rejected', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });
  const first = h.last();
  const inflight = h.link.invoke('app:meta');
  const id = first.frames()[0].id;

  h.link.connect();                       // supersedes
  await assert.rejects(inflight, (err: unknown) => {
    assert.ok(err instanceof WorkassInvokeError);
    assert.equal((err as WorkassInvokeError).code, 'socket-replaced');
    return true;
  });
  first.reply(id, { late: true });        // must resolve nothing
  assert.equal(h.link.generation, 2);
});

test('the daemon restarting is a different event from reconnecting', () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });   // same daemon
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-2' });   // it restarted

  assert.deepEqual(h.opens.map((o) => o.generation), [1, 2, 3]);
  assert.deepEqual(h.opens.map((o) => o.restarted), [false, false, true],
    'only a changed instanceId is a restart; the first connect has nothing to compare to');
});

test('a closed socket rejects what was in flight and schedules one reconnect', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });
  const inflight = h.link.invoke('session:get');
  h.last().close();
  await assert.rejects(inflight, (err: unknown) => (err as WorkassInvokeError).code === 'socket-closed');
  assert.equal(h.timers.filter((t) => t.ms === 1500).length, 1);
  h.runTimers();
  assert.equal(h.sockets.length, 2, 'it reconnects');
});

test('close() does not reconnect, because the caller meant it', () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.link.close();
  h.runTimers();
  assert.equal(h.sockets.length, 1);
});

// ---- errors --------------------------------------------------------------

test('a channel error is an ordinary Error so an old daemon cannot flap us offline', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });
  const call = h.link.invoke('machines:list');
  h.last().reply(h.last().frames()[0].id, null, 'unknown channel: machines:list');
  await assert.rejects(call, (err: unknown) => {
    assert.ok(err instanceof Error);
    assert.ok(!(err instanceof WorkassInvokeError), 'a missing additive channel is NOT a transport failure');
    assert.match((err as Error).message, /unknown channel/);
    return true;
  });
});

// ---- enrolment -----------------------------------------------------------

test('being parked with a fleet key enrols without a human, and never sends the key or the token', async () => {
  const key = 'wf-byntydr27z7j3zsdpih3uulqhi';
  const h = harness({ fleetKey: key });
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'waiting', requestId: 'lan-1', instanceId: 'i-1' });

  await Promise.resolve();
  const challenge = h.last().frames().find((f) => f.channel === 'fleet:challenge');
  assert.ok(challenge, 'it asks for a challenge instead of waiting for a person');
  h.last().reply(challenge.id, { enabled: true, machineId: 'm-remote', serverNonce: 'sn-9', keyIds: ['k1'] });
  await Promise.resolve(); await Promise.resolve();

  const enrol = h.last().frames().find((f) => f.channel === 'fleet:enroll');
  assert.ok(enrol, 'it answers the challenge');
  const args = enrol.args[0] as { proof: string; clientNonce: string; name: string };
  assert.equal(args.proof, fleetProof(key, 'sn-9', args.clientNonce, 'm-remote'));

  const wire = h.last().sent.join('\n');
  const derived = fleetDeviceToken(key, 'sn-9', args.clientNonce, 'm-remote');
  assert.ok(!wire.includes(key), 'the fleet key never goes on the wire');
  assert.ok(!wire.includes(derived), 'neither does the token the client derived');

  h.last().reply(enrol.id, { ok: true, deviceId: 'd9', keyId: 'k1' });
  h.last().access({ state: 'approved', deviceId: 'd9', name: 'iPhone', deviceToken: null, instanceId: 'i-1' });
  await Promise.resolve(); await Promise.resolve();
  assert.ok(h.saved.some((c) => c.deviceToken === derived), 'the derived token is what gets persisted');
});

test('a machine with no fleet key leaves us parked rather than looping', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'waiting', instanceId: 'i-1' });
  await Promise.resolve();
  assert.equal(h.last().frames().length, 0, 'without a key there is nothing to try');
  assert.equal(h.link.linkState, 'open');
});

test('a rejected token is dropped so the next connect asks again', () => {
  const cleared: string[] = [];
  const h = harness({ credentials: { deviceToken: 'stale' }, clearCredentials: (id: string) => cleared.push(id) });
  h.link.connect();
  assert.match(h.last().url, /deviceToken=stale/);
  h.last().onopen?.();
  h.last().access({ state: 'rejected', reason: 'invalid-token', instanceId: 'i-1' });
  assert.deepEqual(cleared, ['m-remote']);
  h.last().close();
  h.runTimers();
  assert.equal(h.sockets.length, 1, 'a rejected request never retries until the user asks again');
  h.link.connect();
  assert.ok(!h.last().url.includes('deviceToken'), 'the stale token is gone from the next connect');
});

// ---- event replay --------------------------------------------------------

test('a late subscriber gets the last payload on a microtask', async () => {
  const h = harness();
  h.link.connect();
  h.last().onopen?.();
  h.last().access({ state: 'approved', instanceId: 'i-1' });
  h.last().event('chat:catalog', { models: 3 });
  const seen: unknown[] = [];
  const off = h.link.on('chat:catalog', (p) => seen.push(p));
  assert.deepEqual(seen, [], 'not synchronously');
  await Promise.resolve();
  assert.deepEqual(seen, [{ models: 3 }]);
  off();
  h.last().event('chat:catalog', { models: 4 });
  assert.equal(seen.length, 1, 'unsubscribe holds');
});

// ---- id namespacing ------------------------------------------------------

test('a remote id is tagged at the boundary and stripped on the way out', () => {
  const tagged = tagId('m-remote', 'chat-7');
  assert.equal(tagged, 'M~m-remote~chat-7');
  assert.deepEqual(splitId(tagged), { machineId: 'm-remote', id: 'chat-7' });
  assert.equal(machineOf(tagged), 'm-remote');
  assert.equal(localId(tagged), 'chat-7');
  assert.equal(tagId('m-remote', tagged), tagged, 'tagging twice is not a second tag');
});

test('a local id is left byte-identical, so nothing persisted stops matching', () => {
  for (const id of ['chat-7', 'conv-ms14xej6-miiiri', 'tab-1785030258642-58', '']) {
    assert.equal(tagId('', id), id);
    assert.equal(localId(id), id);
    assert.equal(machineOf(id), '');
    assert.equal(isTagged(id), false);
  }
});

test('only id-shaped keys are rewritten, and a round trip is lossless', () => {
  const payload = {
    chatId: 'chat-1', title: 'chat-1', modelId: 'opus[1m]', cwd: '/Users/dev/x',
    nested: { jobId: 'job-2', items: [{ id: 'a' }, { id: 'b' }] },
  };
  const tagged = tagPayload('m-remote', payload) as typeof payload;
  assert.equal(tagged.chatId, 'M~m-remote~chat-1');
  assert.equal(tagged.nested.jobId, 'M~m-remote~job-2');
  assert.equal(tagged.nested.items[0].id, 'M~m-remote~a');
  assert.equal(tagged.title, 'chat-1', 'a title that looks like an id is not one');
  assert.equal(tagged.modelId, 'opus[1m]');
  assert.equal(tagged.cwd, '/Users/dev/x');
  assert.deepEqual(untagPayload(tagged), payload);
});

// ---- the label -----------------------------------------------------------

test('local writes nothing and remote reads like a path', () => {
  const names = { 'm-remote': 'builder', 'm-here': 'este Mac' };
  assert.deepEqual(machineWhere('workass', '', names, 'm-here'), { machine: '', project: 'workass', full: 'workass' });
  assert.deepEqual(machineWhere('workass', 'm-here', names, 'm-here'), { machine: '', project: 'workass', full: 'workass' });
  assert.deepEqual(machineWhere('workass', 'm-remote', names, 'm-here'),
    { machine: 'builder', project: 'workass', full: 'builder/workass' });
});

test('a machine we have not named yet still reads as a machine', () => {
  const where = machineWhere('workass', 'm-cd95232395aa46ca42941bc803869022', {}, 'm-here');
  assert.equal(where.machine, '869022');
  assert.equal(shortMachineId('m-abc'), 'abc');
  assert.equal(shortMachineId(''), '?');
});

test('project picker badge marks only remote machines with a readable initial', () => {
  const names = { 'm-remote': 'builder', 'm-here': 'este Mac', 'm-accent': 'Árbol' };
  assert.equal(remoteMachineBadge('', names, 'm-here'), null);
  assert.equal(remoteMachineBadge('m-here', names, 'm-here'), null);
  assert.deepEqual(remoteMachineBadge('m-remote', names, 'm-here'), {
    machine: 'builder', initial: 'B', title: 'Proyecto remoto en builder',
  });
  assert.equal(remoteMachineBadge('m-accent', names, 'm-here')?.initial, 'Á');
  assert.equal(remoteMachineBadge('m-abcdef', {}, 'm-here')?.initial, 'A');
});

test('recency stays global across machines', () => {
  const rows = [
    { id: 'a', touched: 10, machineId: '' },
    { id: 'b', touched: 99, machineId: 'm-remote' },
    { id: 'c', touched: 50, machineId: '' },
  ];
  assert.deepEqual(unifiedOrder(rows).map((r) => r.id), ['b', 'c', 'a'],
    'a running turn elsewhere outranks a sleeping one here');
  const tie = [{ id: 'z', touched: 5, machineId: 'm-b' }, { id: 'y', touched: 5, machineId: 'm-a' }];
  assert.deepEqual(unifiedOrder(tie).map((r) => r.id), ['y', 'z'], 'ties are stable, not render-order');
});
