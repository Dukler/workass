import assert from 'node:assert/strict';
import test from 'node:test';
import { createMachineRouter, machineScopeOf, RemoteMachineUnavailableError, routeOf } from '../src/wire/machineRouter.ts';
import { MachineRegistry, type MachineEntry } from '../src/wire/machineRegistry.ts';
import { tagId } from '../src/wire/machineIds.ts';
import type { MachineSocket, MachineSocketLike } from '../src/wire/machineSocket.ts';
import { fleetDeviceToken, fleetProof } from '../src/wire/fleet.ts';

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
  assert.equal(await router.archiveLoad(tagId('m-remote', 'tab-9'), { tail: 10 }), undefined);
  assert.deepEqual(remote.calls[1], { channel: 'chat:archive-load', args: ['tab-9', { tail: 10 }] },
    'a bounded history option reaches the owning daemon without changing the wire envelope');
  assert.equal(await router.archiveLoad(tagId('m-remote', 'tab-9'), {
    beforeMessageId: tagId('m-remote', 'message-41'), limit: 40,
  }), undefined);
  assert.deepEqual(remote.calls[2], {
    channel: 'chat:archive-load', args: ['tab-9', { beforeMessageId: 'message-41', limit: 40 }],
  }, 'a remote page boundary is stripped before it reaches the owning actor');
  assert.equal(localCalls.length, 1, 'the local bridge saw only the untagged call');
});

test('server folder creation uses the additive fs:create-dir payload', async () => {
  const calls: unknown[][] = [];
  const local = { createDir: (...args: unknown[]) => { calls.push(args); return Promise.resolve({ path: '/Users/me/New App' }); } };
  const router = createMachineRouter({ local: () => local as never, links: () => new Map(), subscribeRemote: () => {} }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  await router.createDir('/Users/me', 'New App');
  assert.deepEqual(calls, [['/Users/me', 'New App']]);
});

test('project artwork is read by the daemon that owns the tagged chat', async () => {
  const remote = fakeLink({
    'project:icon': { found: true, mimeType: 'image/png', base64: 'iVBORw==' },
  });
  const localCalls: unknown[][] = [];
  const router = createMachineRouter({
    local: () => ({ projectIcon: (...args: unknown[]) => { localCalls.push(args); return Promise.resolve({ found: false }); } }) as never,
    links: () => new Map([['m-builder', remote.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;

  const result = await router.projectIcon(tagId('m-builder', 'chat-7'), '/srv/workass');

  assert.deepEqual(localCalls, [], 'a remote path must never be read by the local daemon');
  assert.deepEqual(remote.calls, [{
    channel: 'project:icon',
    args: [{ chatId: 'chat-7', cwd: '/srv/workass' }],
  }]);
  assert.deepEqual(result, { found: true, mimeType: 'image/png', base64: 'iVBORw==' });
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
  const remote = fakeLink({
    'chat:archive-append': { ok: true, tabId: 'tab-1', chatId: 'chat-1' },
    'chat:archive-load': [{ id: 'message-1', chatId: 'chat-1', role: 'assistant', content: 'remote answer' }],
  });
  const router = createMachineRouter({
    local: () => ({ getSession: () => Promise.resolve({ chats: [] }) }) as never,
    links: () => new Map([['m-remote', remote.link]]),
  }) as unknown as Record<string, (...a: unknown[]) => Promise<{ chats: Array<Record<string, string>> }>>;

  // Routed by a tagged argument, because getSession itself carries no id.
  const result = await router.archiveAppend(tagId('m-remote', 'tab-1'), [] as never) as unknown as Record<string, unknown>;
  assert.deepEqual(remote.calls[0].args, [{ tabId: 'tab-1', messages: [] }]);
  assert.deepEqual(result, {
    ok: true,
    tabId: tagId('m-remote', 'tab-1'),
    chatId: tagId('m-remote', 'chat-1'),
  });

  const listed = await router.archiveLoad(tagId('m-remote', 'tab-1')) as unknown;
  assert.deepEqual(listed, [{
    id: tagId('m-remote', 'message-1'),
    chatId: tagId('m-remote', 'chat-1'),
    role: 'assistant',
    content: 'remote answer',
  }]);
  const session = await router.getSession();
  assert.deepEqual(session.chats, [], 'no tagged argument means the local daemon answered');
});

test('a remotely-tagged chat:create is created by that machine and its receipt stays tagged', async () => {
  const remote = fakeLink({
    'chat:create': { ok: true, tabId: 'tab-new', chatId: 'chat-new', operationId: 'create-1' },
  });
  const localCalls: unknown[] = [];
  const router = createMachineRouter({
    local: () => ({ chatCreate: (opts: unknown) => { localCalls.push(opts); return Promise.resolve({ ok: true }); } }) as never,
    links: () => new Map([['m-lagpc', remote.link]]),
    controlLinks: () => new Map([['m-lagpc', remote.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...a: unknown[]) => Promise<Record<string, unknown>>>;

  const result = await router.chatCreate({
    tabId: tagId('m-lagpc', 'tab-new'),
    chatId: tagId('m-lagpc', 'chat-new'),
    operationId: 'create-1',
  });

  assert.deepEqual(localCalls, [], 'a remote create must never reach the local daemon');
  assert.deepEqual(remote.calls, [{
    channel: 'chat:create',
    args: [{ tabId: 'tab-new', chatId: 'chat-new', operationId: 'create-1' }],
  }]);
  assert.equal(result.tabId, tagId('m-lagpc', 'tab-new'));
  assert.equal(result.chatId, tagId('m-lagpc', 'chat-new'));
});

test('plan usage routes by its tagged chat but sends only the provider payload', async () => {
  const remote = fakeLink({ 'app-chat:refresh-plan-usage': { ok: true, providerId: 'codex' } });
  const localCalls: unknown[][] = [];
  const router = createMachineRouter({
    local: () => ({ appChatRefreshPlanUsage: (...args: unknown[]) => { localCalls.push(args); return Promise.resolve({ ok: true, providerId: 'codex' }); } }) as never,
    links: () => new Map([['m-lagpc', remote.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;

  await router.appChatRefreshPlanUsage('codex', tagId('m-lagpc', 'tab-1'));

  assert.deepEqual(localCalls, []);
  assert.deepEqual(remote.calls, [{
    channel: 'app-chat:refresh-plan-usage',
    args: [{ providerId: 'codex' }],
  }]);
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

test('remote catalog keeps exact model and mode ids while carrying its machine owner', () => {
  const seen: unknown[] = [];
  const localSubs = new Map<string, (payload: unknown) => void>();
  const remoteSubs = new Map<string, (payload: unknown, machineId: string) => void>();
  const router = createMachineRouter({
    local: () => ({ onChatCatalog: (cb: (payload: unknown) => void) => { localSubs.set('chat:catalog', cb); } }) as never,
    links: () => new Map(),
    subscribeRemote: (channel, cb) => { remoteSubs.set(channel, cb); },
  }) as unknown as { onChatCatalog(cb: (payload: unknown) => void): void };

  router.onChatCatalog((payload) => seen.push(payload));
  const local = { groups: [{ providerId: 'same', models: [{ modelId: 'local-model' }], modes: [{ id: 'local-mode' }] }] };
  const remote = { groups: [{ providerId: 'same', models: [{ modelId: 'remote-model' }], modes: [{ id: 'remote-mode' }] }] };
  localSubs.get('chat:catalog')?.(local);
  remoteSubs.get('chat:catalog')?.(remote, 'm-lagpc');

  assert.equal(machineScopeOf(seen[0]), '');
  assert.equal(machineScopeOf(seen[1]), 'm-lagpc');
  assert.deepEqual(seen, [local, remote], 'catalog identifiers stay byte-identical to their owning daemon');
});

test('remote provider snapshots keep provider ids and carry their exact machine owner', () => {
  const seen: unknown[] = [];
  const localSubs = new Map<string, (payload: unknown) => void>();
  const remoteSubs = new Map<string, (payload: unknown, machineId: string) => void>();
  const router = createMachineRouter({
    local: () => ({ onProvidersList: (cb: (payload: unknown) => void) => { localSubs.set('providers:list', cb); } }) as never,
    links: () => new Map(),
    subscribeRemote: (channel, cb) => { remoteSubs.set(channel, cb); },
  }) as unknown as { onProvidersList(cb: (payload: unknown) => void): void };

  router.onProvidersList((payload) => seen.push(payload));
  const local = [{ id: 'codex', name: 'Codex', enabled: true, status: 'ready' }];
  const remote = [{ id: 'devin', name: 'Devin', enabled: true, status: 'ready' }];
  localSubs.get('providers:list')?.(local);
  remoteSubs.get('providers:list')?.(remote, 'm-san');

  assert.equal(machineScopeOf(seen[0]), '');
  assert.equal(machineScopeOf(seen[1]), 'm-san');
  assert.deepEqual(seen, [local, remote], 'provider ids stay byte-identical to their owning daemon');
});

test('remote provider toggle uses the exact machine control lane', async () => {
  const data = fakeLink();
  const control = fakeLink({
    'providers:toggle': [{ id: 'devin', name: 'Devin', enabled: false, disabledByUser: true, status: 'inactive' }],
  });
  const localCalls: unknown[][] = [];
  const router = createMachineRouter({
    local: () => ({ providersToggle: (...args: unknown[]) => { localCalls.push(args); return Promise.resolve([]); } }) as never,
    links: () => new Map([['m-san', data.link]]),
    controlLinks: () => new Map([['m-san', control.link]]),
    subscribeRemote: () => {},
  }) as unknown as { providersToggle(id: string, enabled: boolean): Promise<Array<Record<string, unknown>>> };

  const result = await router.providersToggle(tagId('m-san', 'devin'), false);
  assert.deepEqual(localCalls, []);
  assert.deepEqual(control.calls, [{ channel: 'providers:toggle', args: [{ id: 'devin', enabled: false }] }]);
  assert.equal(result[0]?.id, tagId('m-san', 'devin'));
});

test('unpartitioned machine-wide snapshots stay on their owning Workass window', () => {
  const machineWideSnapshots = [
    'onChatPlanUsage',
    'onProcChanged',
    'onProvidersUpdates',
    'onProvidersUpdateProgress',
    'onAppUpdate',
  ] as const;
  const seenUpdates: unknown[] = [];
  const seenProgress: unknown[] = [];
  const localSubs = new Map<string, (payload: unknown) => void>();
  const remoteChannels: string[] = [];
  const local = Object.fromEntries(machineWideSnapshots.map((method) => [
    method,
    (cb: (payload: unknown) => void) => { localSubs.set(method, cb); },
  ]));
  const router = createMachineRouter({
    local: () => local as never,
    links: () => new Map(),
    subscribeRemote: (channel) => { remoteChannels.push(channel); },
  }) as unknown as Record<string, (cb: (payload: unknown) => void) => void>;

  for (const method of machineWideSnapshots) {
    router[method](method === 'onProvidersUpdates'
      ? (payload) => seenUpdates.push(payload)
      : method === 'onProvidersUpdateProgress'
        ? (payload) => seenProgress.push(payload)
        : () => {});
  }
  localSubs.get('onProvidersUpdates')?.({ updates: [{ providerId: 'codex' }] });
  localSubs.get('onProvidersUpdateProgress')?.({ providerId: 'codex', status: 'running' });

  assert.deepEqual(seenUpdates, [{ updates: [{ providerId: 'codex' }] }]);
  assert.deepEqual(seenProgress, [{ providerId: 'codex', status: 'running' }]);
  assert.deepEqual(remoteChannels, [], 'unpartitioned machine-wide subscriptions must never attach to a remote sink');
});

test('a tagged call to an unavailable machine fails without touching the local daemon', async () => {
  const localCalls: unknown[] = [];
  const local = { cancelJob: (id: unknown) => { localCalls.push(id); return Promise.resolve({ cancelled: id }); } };
  const router = createMachineRouter({ local: () => local as never, links: () => new Map(), subscribeRemote: () => {} }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  await assert.rejects(
    router.cancelJob(tagId('m-gone', 'job-1')),
    (error: unknown) => error instanceof RemoteMachineUnavailableError && error.machineId === 'm-gone',
  );
  assert.deepEqual(localCalls, [], 'a remote operation must never fall through to this machine');

  assert.deepEqual(await router.cancelJob('job-local'), { cancelled: 'job-local' });
  assert.deepEqual(localCalls, ['job-local'], 'an untagged operation remains local');
});

test('an additive local bridge method remains available without router changes', () => {
  const local = { getSession: () => Promise.resolve({}), somethingNewer: () => 42 };
  const router = createMachineRouter({ local: () => local as never, links: () => new Map(), subscribeRemote: () => {} }) as unknown as Record<string, unknown>;
  assert.equal(typeof router.somethingNewer, 'function', 'feature detection upstream reads the SHAPE of this object');
});

test('a pending data read uses a different link from one control admission receipt', async () => {
  let releaseBulk!: (value: unknown) => void;
  const blockedBulk = new Promise((resolve) => { releaseBulk = resolve; });
  const data = fakeLink({ 'session:get': blockedBulk });
  const control = fakeLink({ 'job:start': { id: 'job-control' } });
  const urgent = fakeLink();
  const bulkRead = data.link.invoke('session:get');
  const router = createMachineRouter({
    local: () => ({}) as never,
    links: () => new Map([['m-lagpc', data.link]]),
    controlLinks: () => new Map([['m-lagpc', control.link]]),
    urgentLinks: () => new Map([['m-lagpc', urgent.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...args: unknown[]) => Promise<Record<string, unknown>>>;

  const receipt = await router.startJob({
    tabId: tagId('m-lagpc', 'tab-1'), chatId: tagId('m-lagpc', 'chat-1'),
    operationId: 'transport-one', userMessageId: 'user-one', assistantMessageId: 'assistant-one',
  });
  assert.equal(receipt.id, tagId('m-lagpc', 'job-control'));
  assert.deepEqual(control.calls.map((call) => call.channel), ['job:start']);
  assert.deepEqual(data.calls.map((call) => call.channel), ['session:get']);

  releaseBulk({ hydrated: true });
  assert.deepEqual(await bulkRead, { hydrated: true });
});

test('urgent Stop is admitted while the ordered control steer handler is still blocked', async () => {
  let releaseSteer!: (value: unknown) => void;
  const blockedSteer = new Promise((resolve) => { releaseSteer = resolve; });
  const data = fakeLink();
  const control = fakeLink({ 'app-chat:steer': blockedSteer });
  const urgent = fakeLink({ 'job:cancel': { cancelled: true } });
  const router = createMachineRouter({
    local: () => ({}) as never,
    links: () => new Map([['m-lagpc', data.link]]),
    controlLinks: () => new Map([['m-lagpc', control.link]]),
    urgentLinks: () => new Map([['m-lagpc', urgent.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;

  const steer = router.appChatSteer(tagId('m-lagpc', 'session-1'), 'redirect', [], 'user-1', 'assistant-1', {});
  await Promise.resolve();
  assert.deepEqual(await router.cancelJob(tagId('m-lagpc', 'job-1')), { cancelled: true });
  assert.deepEqual(control.calls.map((call) => call.channel), ['app-chat:steer']);
  assert.deepEqual(urgent.calls.map((call) => call.channel), ['job:cancel']);
  releaseSteer({ ok: true, strategy: 'receipt-live' });
  await steer;
});

test('send-critical remote methods never fall back to the bulk data socket', async () => {
  const data = fakeLink();
  const router = createMachineRouter({
    local: () => ({}) as never,
    links: () => new Map([['m-lagpc', data.link]]),
    controlLinks: () => new Map(),
    urgentLinks: () => new Map(),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;

  await assert.rejects(router.startJob({ tabId: tagId('m-lagpc', 'tab-1'), chatId: tagId('m-lagpc', 'chat-1') }), RemoteMachineUnavailableError);
  await assert.rejects(router.cancelJob(tagId('m-lagpc', 'job-1')), RemoteMachineUnavailableError);
  assert.deepEqual(data.calls, []);
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

const CERT_FINGERPRINT = 'ab'.repeat(32);

function entry(id: string, name: string, address: string): MachineEntry {
	return { machineId: id, name, endpoints: [{ kind: 'lan', address }], secure: true, certFingerprint: CERT_FINGERPRINT, addedBy: 'beacon' };
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
    { ...entry('m-remote', 'builder', '192.168.1.50:18788'), nickname: 'Taller' },
  ], 'm-self');
  assert.deepEqual(opened, [], 'a beacon must not open a socket or prompt a nearby machine');
  assert.equal(registry.requestAccess('m-remote'), true);
  assert.equal(opened.length, 1, 'only the user request opens an encrypted socket; the local machine is never duplicated');
  assert.match(opened[0], /^wss:\/\/192\.168\.1\.50:18788\/\?connectionGroup=cg-[^&]+&deviceName=Mac$/);
  assert.deepEqual(registry.names(), { 'm-remote': 'Taller' });
  assert.deepEqual(registry.list().map(({ name, reportedName, nickname }) => ({ name, reportedName, nickname })), [
    { name: 'Taller', reportedName: 'builder', nickname: 'Taller' },
  ]);
});

test('approval persists the discovered certificate pin before later token reconnects', () => {
	const storage = memoryStorage();
	let socket: MachineSocketLike | undefined;
	const trusted: Array<[string, string]> = [];
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		trustEndpoint: (address, fingerprint) => { trusted.push([address, fingerprint]); return true; },
		open: (() => (socket = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null })) as never,
	});
	registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
	assert.equal(registry.requestAccess('m-remote'), true);
	assert.deepEqual(trusted, [['192.168.1.71:80', CERT_FINGERPRINT]]);
	socket?.onopen?.();
	socket?.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceToken: 'paired-token', deviceId: 'device-1', name: 'Mac', instanceId: 'instance-1',
	} }));
	const saved = JSON.parse(storage.getItem('workass.machine.m-remote') || '{}');
	assert.equal(saved.deviceToken, 'paired-token');
	assert.equal(saved.certFingerprint, CERT_FINGERPRINT);
});

test('fleet approval queues the first Start and Stop until exact-group auxiliary handshakes', async () => {
	const fleetKey = 'wf-byntydr27z7j3zsdpih3uulqhi';
	const storage = memoryStorage();
	const opened: Array<{ url: string; socket: MachineSocketLike; sent: string[] }> = [];
	const remoteEvents: unknown[] = [];
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		open: ((url: string) => {
			const sent: string[] = [];
			const socket: MachineSocketLike = {
				send(data) { sent.push(data); }, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null,
			};
			opened.push({ url, socket, sent });
			return socket;
		}) as never,
	});
	registry.useFleetKey(fleetKey);
	registry.subscribeRemote('job:event', (payload) => { remoteEvents.push(payload); });
	registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
	assert.equal(registry.requestAccess('m-remote'), true);
	const primary = opened[0];
	primary.socket.onopen?.();
	primary.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'waiting', requestId: 'fleet-waiting', instanceId: 'instance-1',
	} }));
	await Promise.resolve();
	const challenge = primary.sent.map((raw) => JSON.parse(raw)).find((frame) => frame.channel === 'fleet:challenge');
	assert.ok(challenge);
	primary.socket.onmessage?.(JSON.stringify({ t: 'reply', id: challenge.id, result: {
		enabled: true, machineId: 'm-remote', serverNonce: 'server-nonce', keyIds: ['key-1'],
	}, error: null }));
	await Promise.resolve(); await Promise.resolve();
	const enrol = primary.sent.map((raw) => JSON.parse(raw)).find((frame) => frame.channel === 'fleet:enroll');
	assert.ok(enrol);
	const enrolment = enrol.args[0] as { clientNonce: string; proof: string };
	assert.equal(enrolment.proof, fleetProof(fleetKey, 'server-nonce', enrolment.clientNonce, 'm-remote'));
	const derivedToken = fleetDeviceToken(fleetKey, 'server-nonce', enrolment.clientNonce, 'm-remote');

	// The server's approved event is intentionally delivered before the enrol
	// reply. At this point the primary may hydrate, but no reusable token has
	// been persisted and no auxiliary network connection can exist yet.
	primary.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceId: 'device-fleet', name: 'Mac', instanceId: 'instance-1',
	} }));
	assert.equal(opened.length, 1);
	const router = registry.router() as unknown as {
		startJob(arg: Record<string, unknown>): Promise<Record<string, unknown>>;
		cancelJob(id: string): Promise<Record<string, unknown>>;
	};
	const start = router.startJob({
		tabId: tagId('m-remote', 'tab-first'), chatId: tagId('m-remote', 'chat-first'),
		operationId: 'first-start', userMessageId: 'first-user', assistantMessageId: 'first-assistant',
	});
	const stop = router.cancelJob(tagId('m-remote', 'job-first'));
	await Promise.resolve();
	assert.deepEqual(primary.sent.map((raw) => JSON.parse(raw).channel), ['fleet:challenge', 'fleet:enroll'],
		'first Start/Stop wait on owned auxiliary placeholders and never fall back to primary');

	primary.socket.onmessage?.(JSON.stringify({ t: 'reply', id: enrol.id, result: { ok: true, deviceId: 'device-fleet' }, error: null }));
	await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
	assert.equal(opened.length, 3, 'persisting the proven token opens exactly control and urgent siblings');
	const parsedURLs = opened.map(({ url }) => new URL(url));
	const groups = parsedURLs.map((url) => url.searchParams.get('connectionGroup'));
	assert.ok(groups[0]);
	assert.deepEqual(groups, [groups[0], groups[0], groups[0]], 'one primary mount owns one exact connection group');
	const control = opened.find(({ url }) => new URL(url).searchParams.get('purpose') === 'control');
	const urgent = opened.find(({ url }) => new URL(url).searchParams.get('purpose') === 'urgent');
	assert.ok(control && urgent);
	assert.equal(new URL(control.url).searchParams.get('deviceToken'), derivedToken);
	assert.equal(new URL(urgent.url).searchParams.get('deviceToken'), derivedToken);

	for (const sibling of [control, urgent]) {
		sibling.socket.onopen?.();
		sibling.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
			state: 'approved', deviceId: 'device-fleet', instanceId: 'instance-1',
		} }));
	}
	const startFrame = control.sent.map((raw) => JSON.parse(raw)).find((frame) => frame.channel === 'job:start');
	const stopFrame = urgent.sent.map((raw) => JSON.parse(raw)).find((frame) => frame.channel === 'job:cancel');
	assert.ok(startFrame && stopFrame, 'pre-ready invokes flush on their exact lanes after authentication');
	control.socket.onmessage?.(JSON.stringify({ t: 'reply', id: startFrame.id, result: {
		id: 'job-first', kind: 'app-chat', status: 'running', tabId: 'tab-first', chatId: 'chat-first',
	}, error: null }));
	urgent.socket.onmessage?.(JSON.stringify({ t: 'reply', id: stopFrame.id, result: { cancelled: true }, error: null }));
	assert.equal((await start).id, tagId('m-remote', 'job-first'));
	assert.deepEqual(await stop, { cancelled: true });

	control.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'job:event', payload: { id: 'must-be-ignored' } }));
	assert.deepEqual(remoteEvents, [], 'auxiliary sockets cannot dispatch projection events');
});

test('a rejected auxiliary clears credentials and unmounts the machine immediately', () => {
	const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
	const opened: Array<{ url: string; socket: MachineSocketLike }> = [];
	const unmounted: string[] = [];
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		onUnmount: (machineId) => { unmounted.push(machineId); },
		open: ((url: string) => {
			const socket: MachineSocketLike = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null };
			opened.push({ url, socket });
			return socket;
		}) as never,
	});
	registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
	const primary = opened[0];
	primary.socket.onopen?.();
	primary.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceId: 'device-1', instanceId: 'instance-1',
	} }));
	const control = opened.find(({ url }) => new URL(url).searchParams.get('purpose') === 'control');
	assert.ok(control);
	control.socket.onopen?.();
	control.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceId: 'device-1', instanceId: 'instance-1',
	} }));
	control.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'rejected', reason: 'revoked', instanceId: 'instance-1',
	} }));
	assert.deepEqual(unmounted, ['m-remote']);
	assert.equal(storage.getItem('workass.machine.m-remote'), null);
	assert.equal(registry.linkFor('m-remote'), undefined);
	assert.equal(registry.list()[0].link, 'rejected');
});

test('every primary reconnect rotates its group and replaces exactly one auxiliary pair', () => {
	const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
	const opened: Array<{ url: string; socket: MachineSocketLike; sent: string[]; closed: boolean }> = [];
	const timers: Array<{ fn: () => void; ms: number; cleared: boolean }> = [];
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		open: ((url: string) => {
			const record = { url, sent: [] as string[], closed: false, socket: undefined as unknown as MachineSocketLike };
			record.socket = {
				send(data) { record.sent.push(data); }, close() { record.closed = true; },
				onopen: null, onclose: null, onmessage: null, onerror: null,
			};
			opened.push(record);
			return record.socket;
		}) as never,
		setTimer: (fn, ms) => { timers.push({ fn, ms, cleared: false }); return timers.length - 1; },
		clearTimer: (handle) => { if (timers[handle as number]) timers[handle as number].cleared = true; },
	});
	registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
	const primary1 = opened[0];
	primary1.socket.onopen?.();
	primary1.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceId: 'device-1', instanceId: 'instance-1',
	} }));
	assert.equal(opened.length, 3);
	const group1 = new URL(primary1.url).searchParams.get('connectionGroup');
	assert.ok(group1);
	assert.deepEqual(opened.slice(0, 3).map(({ url }) => new URL(url).searchParams.get('connectionGroup')), [group1, group1, group1]);

	primary1.socket.onclose?.();
	assert.equal(opened.slice(1, 3).every((record) => record.closed), true, 'primary loss fences its exact auxiliary pair before reconnect');
	const reconnect = timers.find((timer) => timer.ms === 1_500 && !timer.cleared);
	assert.ok(reconnect);
	reconnect.fn();
	const primary2 = opened[3];
	assert.ok(primary2);
	const group2 = new URL(primary2.url).searchParams.get('connectionGroup');
	assert.ok(group2 && group2 !== group1, 'a new physical primary gets a new opaque group');
	primary2.socket.onopen?.();
	primary2.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
		state: 'approved', deviceId: 'device-1', instanceId: 'instance-1',
	} }));
	assert.equal(opened.length, 6, 'reconnect creates one replacement control and one replacement urgent socket');
	assert.deepEqual(opened.slice(3, 6).map(({ url }) => new URL(url).searchParams.get('connectionGroup')), [group2, group2, group2]);
	assert.deepEqual(opened.slice(3, 6).map(({ url }) => new URL(url).searchParams.get('purpose')), [null, 'control', 'urgent']);
});

test('boot-time event replay delivers a machine-scoped remote catalog', () => {
  const storage = memoryStorage();
  const sockets: MachineSocketLike[] = [];
  const seen: unknown[] = [];
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: (() => {
      const socket = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null } as MachineSocketLike;
      sockets.push(socket);
      return socket;
    }) as never,
  });
  registry.subscribeRemoteMethod('onChatCatalog', (payload) => { seen.push(payload); });
  registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
  assert.equal(registry.requestAccess('m-remote'), true);
  const primary = sockets[0];
  primary.onopen?.();
  primary.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'approved', deviceToken: 'paired-token', deviceId: 'device-1', instanceId: 'instance-1',
  } }));
  primary.onmessage?.(JSON.stringify({ t: 'event', channel: 'chat:catalog', payload: {
    groups: [{ providerId: 'shared', models: [{ modelId: 'remote-only' }], modes: [{ id: 'remote-mode' }] }],
  } }));

  assert.equal(seen.length, 1);
  assert.equal(machineScopeOf(seen[0]), 'm-remote');
  assert.deepEqual(seen[0], {
    groups: [{ providerId: 'shared', models: [{ modelId: 'remote-only' }], modes: [{ id: 'remote-mode' }] }],
  });
});

test('a changed certificate never receives a stored device token', () => {
	const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
	const opened: string[] = [];
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		open: ((url: string) => { opened.push(url); return { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null }; }) as never,
	});
	registry.sync([{ ...entry('m-remote', 'builder', '192.168.1.71:80'), certFingerprint: 'cd'.repeat(32) }], 'm-self');
	assert.deepEqual(opened, []);
	assert.equal(storage.getItem('workass.machine.m-remote'), null, 'identity change requires a fresh explicit approval');
	assert.equal(registry.list()[0].paired, false);
});

test('a machine leaving the book is disconnected, and forgetting it drops its token', () => {
  const closed: number[] = [];
  const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 't', certFingerprint: CERT_FINGERPRINT }));
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

test('forget unmounts an admitted machine and persists the explicit reconnect fence', () => {
  const storage = memoryStorage();
  const unmounted: string[] = [];
  let socket: MachineSocketLike | undefined;
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    onUnmount: (machineId) => { unmounted.push(machineId); },
    open: (() => (socket = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null })) as never,
    setTimer: () => 1,
    clearTimer: () => {},
  });
  registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
  assert.equal(registry.requestAccess('m-remote'), true);
  socket?.onopen?.();
  socket?.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'approved', deviceToken: 'paired-token', deviceId: 'device-1', instanceId: 'instance-1',
  } }));
  socket?.onclose?.();
  assert.deepEqual(unmounted, [], 'an ordinary offline transition keeps the mounted chat projection');

  registry.forget('m-remote');
  assert.deepEqual(unmounted, ['m-remote']);
  assert.equal(storage.getItem('workass.machine.m-remote'), null);
  assert.equal(storage.getItem('workass.machine.m-remote.auto-connect-disabled'), '1');
});

test('rejection unmounts exactly once, fences stale sockets, and requires an explicit reconnect', () => {
  const storage = memoryStorage();
  const opened: MachineSocketLike[] = [];
  const unmounted: string[] = [];
  const openedGenerations: number[] = [];
  const remoteEvents: unknown[] = [];
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    onUnmount: (machineId) => { unmounted.push(machineId); },
    onOpen: (_machineId, info) => { openedGenerations.push(info.generation); },
    open: (() => {
      const socket: MachineSocketLike = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null };
      opened.push(socket);
      return socket;
    }) as never,
  });
  registry.subscribeRemote('job:event', (payload) => { remoteEvents.push(payload); });
  registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
  assert.equal(registry.requestAccess('m-remote'), true);
  const first = opened[0];
  first.onopen?.();
  first.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'approved', deviceToken: 'paired-token', deviceId: 'device-1', instanceId: 'instance-1',
  } }));
  const admitted = registry.linkFor('m-remote');
  assert.ok(admitted);
  assert.equal(registry.ownsLink('m-remote', admitted), true);
  assert.deepEqual(openedGenerations, [1]);

  first.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'rejected', reason: 'denied', instanceId: 'instance-1',
  } }));
  assert.deepEqual(unmounted, ['m-remote']);
  assert.equal(registry.linkFor('m-remote'), undefined);
  assert.equal(registry.ownsLink('m-remote', admitted), false);
  assert.equal(storage.getItem('workass.machine.m-remote'), null);
  assert.equal(storage.getItem('workass.machine.m-remote.auto-connect-disabled'), '1');

  // A callback already queued by the rejected socket has no authority now.
  first.onmessage?.(JSON.stringify({ t: 'event', channel: 'job:event', payload: { id: 'late-job' } }));
  assert.deepEqual(remoteEvents, []);
  registry.forget('m-remote');
  assert.deepEqual(unmounted, ['m-remote'], 'forget after rejection is idempotent');

  // Discovery and a fleet key may repaint a nearby row, but cannot silently
  // remount it after a human rejection — even in a fresh renderer registry.
  const afterReload: MachineSocketLike[] = [];
  const reloaded = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: (() => {
      const socket: MachineSocketLike = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null };
      afterReload.push(socket);
      return socket;
    }) as never,
  });
  reloaded.useFleetKey('fleet-key-held-in-memory');
  reloaded.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
  assert.deepEqual(afterReload, [], 'fleet discovery cannot undo an explicit rejection');
  assert.equal(reloaded.requestAccess('m-remote'), true);
  assert.equal(afterReload.length, 1, 'only an explicit request clears the reconnect fence');
  assert.equal(storage.getItem('workass.machine.m-remote.auto-connect-disabled'), null);
});

test('rejection remains fenced for the current runtime when browser storage is unavailable', () => {
  const opened: MachineSocketLike[] = [];
  const unavailableStorage = {
    getItem: () => { throw new Error('storage unavailable'); },
    setItem: () => { throw new Error('storage unavailable'); },
    removeItem: () => { throw new Error('storage unavailable'); },
  };
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage: unavailableStorage,
    open: (() => {
      const socket: MachineSocketLike = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null };
      opened.push(socket);
      return socket;
    }) as never,
  });
  registry.useFleetKey('fleet-key-held-in-memory');
  const remote = entry('m-remote', 'builder', '192.168.1.71:80');
  registry.sync([remote], 'm-self');
  opened[0].onopen?.();
  opened[0].onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'approved', instanceId: 'instance-1',
  } }));
  opened[0].onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
    state: 'denied', reason: 'denied', instanceId: 'instance-1',
  } }));

  registry.sync([remote], 'm-self');
  assert.equal(opened.length, 1, 'a later book refresh cannot undo the in-memory rejection');
});

test('an insecure paired endpoint stays parked without losing its credential', () => {
  const opened: string[] = [];
  const closed: number[] = [];
  const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
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
  assert.equal(registry.list()[0].link, 'idle', 'detaching must not leave a stale ready/connecting transport state');
  assert.equal(registry.linkFor('m-remote'), undefined);
  assert.equal(storage.getItem('workass.machine.m-remote') !== null, true, 'the credential survives a security downgrade');

  registry.sync([secure], 'm-self');
  assert.equal(opened.length, 2, 'a later secure refresh reconnects without re-pairing');
});

test('an endpoint address change replaces the immutable socket exactly once', () => {
  const opened: Array<{ url: string; socket: MachineSocketLike }> = [];
  const storage = memoryStorage();
  storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
  const registry = new MachineRegistry({
    local: () => ({}) as never, deviceName: 'Mac', storage,
    open: ((url: string) => {
      const socket: MachineSocketLike = { send() {}, close() {}, onopen: null, onclose: null, onmessage: null, onerror: null };
      opened.push({ url, socket });
      return socket;
    }) as never,
  });
  registry.sync([entry('m-remote', 'builder', '192.168.1.71:80')], 'm-self');
  assert.equal(opened.length, 1);

  registry.sync([entry('m-remote', 'builder', '10.0.0.71:443')], 'm-self');
  assert.equal(opened.length, 2);
  assert.match(opened[1].url, /^wss:\/\/10\.0\.0\.71:443\//);
  assert.equal(registry.list()[0].paired, true, 'address rotation retains the machine credential');

  registry.sync([entry('m-remote', 'builder', '10.0.0.71:443')], 'm-self');
  assert.equal(opened.length, 2, 'an unchanged endpoint does not churn its socket');
});

test('an unreachable paired endpoint cancels retries and reconnects only after a reachable refresh', () => {
	const opened: MachineSocketLike[] = [];
	const timers: { fn: () => void; cleared: boolean }[] = [];
	const storage = memoryStorage();
	storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: CERT_FINGERPRINT }));
	const registry = new MachineRegistry({
		local: () => ({}) as never, deviceName: 'Mac', storage,
		open: (() => {
			const socket: MachineSocketLike = { send() {}, close() { socket.onclose?.(); }, onopen: null, onclose: null, onmessage: null, onerror: null };
			opened.push(socket);
			return socket;
		}) as never,
		setTimer: (fn) => { timers.push({ fn, cleared: false }); return timers.length - 1; },
		clearTimer: (handle) => { if (timers[handle as number]) timers[handle as number].cleared = true; },
	});
	const reachable = { ...entry('m-remote', 'builder', '192.168.1.71:80'), status: 'ok' };
	const unreachable = { ...reachable, status: 'unreachable', reason: 'did not answer' };

	registry.sync([reachable], 'm-self');
	assert.equal(opened.length, 1);
	opened[0].close();
	registry.sync([unreachable], 'm-self');
	for (const timer of timers) if (!timer.cleared) timer.fn();
	assert.equal(opened.length, 1, 'parking an unreachable machine cancels its already-scheduled reconnect');
	assert.equal(registry.list()[0].paired, true, 'parking preserves the device token');
	assert.equal(registry.list()[0].reachable, false);

	registry.sync([reachable], 'm-self');
	assert.equal(opened.length, 2, 'a later healthy probe reconnects exactly once');
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

test('session:save is not routable at all', async () => {
  const localCalls: unknown[] = [];
  const remote = fakeLink();
  const router = createMachineRouter({
    local: () => ({ saveSession: async (snapshot: unknown) => { localCalls.push(snapshot); return true; } }) as never,
    links: () => new Map([['m-remote', remote.link]]),
    subscribeRemote: () => {},
  });
  const snapshot = { activeId: tagId('m-remote', 'tab-1'), chats: [] };
  assert.equal(await router.saveSession?.(snapshot as never), true);
  assert.deepEqual(localCalls, [snapshot]);
  assert.deepEqual(remote.calls, [], 'a whole-store write has no remote addressee');
});
