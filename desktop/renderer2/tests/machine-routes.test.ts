// A machine row can name several ways to reach one daemon: its LAN address and a
// VPN one, say. The client tries them in order, moves on when one does not open
// or cannot be pinned, and leaves a failed one alone for a while so two dead
// addresses never become a tight loop (portable-networking §5).
import assert from 'node:assert/strict';
import test from 'node:test';
import { MachineRegistry, ROUTE_COOLDOWN_MS, ROUTE_OPEN_DEADLINE_MS, type MachineEntry } from '../src/wire/machineRegistry.ts';
import { RECONNECT_DELAY_MS, type MachineSocketLike } from '../src/wire/machineSocket.ts';

const FINGERPRINT = 'ab'.repeat(32);
const LAN = '192.168.1.71:8788';
const VPN = '100.64.0.7:8788';
const V6 = '[fd7a:115c:a1e0::7]:8788';
const NAMED = 'builder.tailnet.example:8788';

interface Opened { url: string; socket: MachineSocketLike; sent: string[]; closed: boolean }
interface Timer { fn: () => void; ms: number; cleared: boolean; fired: boolean }

function rig(options: { paired?: boolean; trust?: (address: string) => boolean } = {}) {
	const map = new Map<string, string>();
	const storage = {
		getItem: (k: string) => map.get(k) ?? null,
		setItem: (k: string, v: string) => { map.set(k, v); },
		removeItem: (k: string) => { map.delete(k); },
	};
	if (options.paired !== false) {
		storage.setItem('workass.machine.m-remote', JSON.stringify({ deviceToken: 'paired-token', certFingerprint: FINGERPRINT }));
	}
	const opened: Opened[] = [];
	const timers: Timer[] = [];
	const pins: string[] = [];
	let clock = 1_000_000;
	const registry = new MachineRegistry({
		local: () => ({}) as never,
		deviceName: 'Mac',
		storage,
		now: () => clock,
		trustEndpoint: (address, fingerprint, machineId) => {
			assert.equal(fingerprint, FINGERPRINT);
			pins.push(`trust ${address} ${machineId}`);
			return options.trust ? options.trust(address) : true;
		},
		releaseEndpoint: (address, machineId) => { pins.push(`release ${address} ${machineId}`); },
		open: ((url: string) => {
			const record: Opened = { url, sent: [], closed: false, socket: undefined as unknown as MachineSocketLike };
			record.socket = {
				send(data: string) { record.sent.push(data); },
				close() { record.closed = true; },
				onopen: null, onclose: null, onmessage: null, onerror: null,
			};
			opened.push(record);
			return record.socket;
		}) as never,
		setTimer: (fn, ms) => { timers.push({ fn, ms, cleared: false, fired: false }); return timers.length - 1; },
		clearTimer: (handle) => { const timer = timers[handle as number]; if (timer) timer.cleared = true; },
	});
	const primaries = () => opened.filter((record) => !/[?&]purpose=/.test(record.url));
	const hostOf = (url: string) => new URL(url.replace(/^wss:/, 'https:')).host;
	return {
		registry,
		opened,
		timers,
		pins,
		primaries,
		hosts: () => primaries().map((record) => hostOf(record.url)),
		last: () => primaries()[primaries().length - 1],
		armed: (ms: number) => timers.filter((timer) => timer.ms === ms && !timer.cleared && !timer.fired).length,
		fire(ms: number) {
			for (const timer of timers.slice()) {
				if (timer.ms !== ms || timer.cleared || timer.fired) continue;
				timer.fired = true;
				timer.fn();
			}
		},
		advance(ms: number) { clock += ms; },
		/** The socket never opened: refused, unreachable, or a certificate the pin refused. */
		fail(record: Opened) { record.socket.onclose?.(); },
		approve(record: Opened) {
			record.socket.onopen?.();
			record.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: {
				state: 'approved', deviceToken: 'paired-token', deviceId: 'device-1', name: 'Mac', instanceId: 'instance-1',
			} }));
		},
	};
}

function machine(...addresses: string[]): MachineEntry {
	return {
		machineId: 'm-remote', name: 'builder', secure: true, certFingerprint: FINGERPRINT, status: 'ok', addedBy: 'manual',
		endpoints: addresses.map((address, index) => ({ kind: index ? 'vpn' : 'lan', address })),
	};
}

test('a dead first endpoint falls back to a live second one, and the pin follows the route', () => {
	const r = rig();
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	assert.deepEqual(r.hosts(), [LAN]);

	r.fail(r.primaries()[0]);
	assert.deepEqual(r.hosts(), [LAN, VPN], 'a failed open moves straight on, without waiting out a reconnect on the dead one');
	assert.equal(r.armed(RECONNECT_DELAY_MS), 0, 'the abandoned socket schedules no reconnect of its own');

	r.approve(r.last());
	const [view] = r.registry.list();
	assert.equal(view.link, 'ready');
	assert.equal(view.address, VPN, 'the row names the address it is really using');
	assert.deepEqual(r.pins, [`trust ${LAN} m-remote`, `release ${LAN} m-remote`, `trust ${VPN} m-remote`]);
	const auxiliaries = r.opened.filter((record) => /[?&]purpose=/.test(record.url));
	assert.equal(auxiliaries.length, 2, 'control and urgent sockets open once the primary is ready');
	for (const record of auxiliaries) assert.equal(new URL(record.url.replace(/^wss:/, 'https:')).host, VPN, 'auxiliaries ride the route the primary proved');
});

test('an address whose pin is held by another machine is skipped for the next one', () => {
	const r = rig({ trust: (address) => address !== LAN });
	r.registry.sync([machine(LAN, V6)], 'm-self');
	assert.deepEqual(r.hosts(), [V6]);
	assert.match(r.last().url, /^wss:\/\/\[fd7a:115c:a1e0::7\]:8788\/\?/, 'an IPv6 address is dialled in brackets');
	r.approve(r.last());
	assert.equal(r.registry.list()[0].link, 'ready');

	const blocked = rig({ trust: () => false });
	blocked.registry.sync([machine(LAN, V6)], 'm-self');
	assert.deepEqual(blocked.hosts(), [], 'no address could be pinned, so nothing is dialled');
	assert.equal(blocked.registry.list()[0].reason, 'no se pudo fijar la identidad TLS');
});

test('a hostname endpoint is dialled as typed', () => {
	const r = rig();
	r.registry.sync([machine(NAMED)], 'm-self');
	assert.match(r.last().url, /^wss:\/\/builder\.tailnet\.example:8788\/\?/);
	assert.deepEqual(r.pins, [`trust ${NAMED} m-remote`]);
});

test('an open that hangs is abandoned for the next route, and a lone route is never cut short', () => {
	const r = rig();
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	assert.equal(r.armed(ROUTE_OPEN_DEADLINE_MS), 1);
	r.fire(ROUTE_OPEN_DEADLINE_MS);
	assert.deepEqual(r.hosts(), [LAN, VPN], 'a black-holed LAN address does not hold the VPN one back');
	assert.equal(r.primaries()[0].closed, true);
	r.approve(r.last());
	assert.equal(r.armed(ROUTE_OPEN_DEADLINE_MS), 0, 'an opened socket keeps no deadline');
	assert.equal(r.registry.list()[0].link, 'ready');

	const lone = rig();
	lone.registry.sync([machine(VPN)], 'm-self');
	assert.equal(lone.armed(ROUTE_OPEN_DEADLINE_MS), 0, 'a slow link with nowhere else to go keeps waiting');
});

test('a connected socket stays on its address through book reorders and reconnects', () => {
	const r = rig();
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	r.fail(r.primaries()[0]);
	r.approve(r.last());
	assert.equal(r.registry.list()[0].link, 'ready');

	r.registry.sync([machine(VPN, LAN)], 'm-self');
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	r.advance(ROUTE_COOLDOWN_MS * 2);
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	assert.deepEqual(r.hosts(), [LAN, VPN], 'a working route is never swapped for a preferred one');

	// It dropped after working: the socket's own reconnect retries the same address.
	r.last().socket.onclose?.();
	assert.equal(r.armed(RECONNECT_DELAY_MS), 1);
	r.fire(RECONNECT_DELAY_MS);
	assert.deepEqual(r.hosts(), [LAN, VPN, VPN]);
	r.approve(r.last());
	assert.equal(r.registry.list()[0].link, 'ready');

	r.registry.sync([machine(LAN)], 'm-self');
	assert.deepEqual(r.hosts(), [LAN, VPN, VPN, LAN], 'a route that left the book is replaced');
});

test('two dead routes cool down instead of looping', () => {
	const r = rig();
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	r.fail(r.primaries()[0]);
	assert.deepEqual(r.hosts(), [LAN, VPN]);

	r.fail(r.last());
	assert.deepEqual(r.hosts(), [LAN, VPN], 'the LAN address failed a moment ago, so nothing is dialled at once');
	assert.equal(r.armed(RECONNECT_DELAY_MS), 1, 'the ordinary reconnect delay paces the retries');
	r.fire(RECONNECT_DELAY_MS);
	assert.deepEqual(r.hosts(), [LAN, VPN, VPN]);
	r.fail(r.last());
	assert.deepEqual(r.hosts(), [LAN, VPN, VPN]);

	r.advance(ROUTE_COOLDOWN_MS);
	r.fire(RECONNECT_DELAY_MS);
	r.fail(r.last());
	assert.deepEqual(r.hosts(), [LAN, VPN, VPN, VPN, LAN], 'once its cooldown ends the first route is tried again');

	// A fresh connect after a detach prefers a route that is not cooling.
	const fresh = rig();
	fresh.registry.sync([machine(LAN, VPN)], 'm-self');
	fresh.fail(fresh.primaries()[0]);
	fresh.registry.sync([{ ...machine(LAN, VPN), status: 'unreachable' }], 'm-self');
	fresh.registry.sync([machine(LAN, VPN)], 'm-self');
	assert.deepEqual(fresh.hosts(), [LAN, VPN, VPN], 'the cooling LAN address is not the first choice');
});

test('an address that does not prove the machine during fleet enrolment moves on', async () => {
	const r = rig({ paired: false });
	r.registry.useFleetKey('wf-byntydr27z7j3zsdpih3uulqhi');
	r.registry.sync([machine(LAN, VPN)], 'm-self');
	assert.deepEqual(r.hosts(), [LAN]);
	const first = r.last();
	first.socket.onopen?.();
	first.socket.onmessage?.(JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: { state: 'waiting', instanceId: 'i-1' } }));
	await Promise.resolve();
	const challenge = first.sent.map((data) => JSON.parse(data)).find((frame) => frame.channel === 'fleet:challenge');
	assert.ok(challenge, 'a parked client with the key asks for a challenge');
	first.socket.onmessage?.(JSON.stringify({ t: 'reply', id: challenge.id, result: { machineId: 'm-somebody-else' } }));
	for (let i = 0; i < 5; i += 1) await Promise.resolve();
	assert.deepEqual(r.hosts(), [LAN, VPN], 'the next route is tried at once');
	assert.equal(first.closed, true);
});
