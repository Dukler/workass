// Adding a machine by a VPN address it was already known by on the LAN: the
// daemon appends the endpoint to the existing row, and the address typed may
// carry no port. The store must still ask that machine for access.
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { createServer } from 'vite';

const FINGERPRINT = 'ab'.repeat(32);

test('a machine added by its VPN address is asked for access even when the LAN address is listed first', async (t) => {
	const server = await createServer({
		root: fileURLToPath(new URL('..', import.meta.url)),
		server: { middlewareMode: true, ws: false },
		appType: 'custom',
		logLevel: 'silent',
	});
	type Subject = {
		clearToastTimers(): void;
		state: { machines: Array<{ machineId: string; requested: boolean; address: string }> };
		addMachine(address: string): Promise<{ ok: boolean; error?: string }>;
	};
	let singleton: { clearToastTimers(): void } | undefined;
	let target: Subject | undefined;
	t.after(async () => { target?.clearToastTimers(); singleton?.clearToastTimers(); await server.close(); });
	const module = await server.ssrLoadModule('/src/store/store.ts') as { store: { clearToastTimers(): void }; Store: new () => Subject };
	singleton = module.store;

	const row = {
		machineId: 'm-builder', name: 'builder', secure: true, certFingerprint: FINGERPRINT, status: 'ok', addedBy: 'beacon',
		endpoints: [{ kind: 'lan', address: '192.168.1.71:8788' }, { kind: 'lan', address: '100.64.0.7:8788' }],
	};
	const book = { ok: true, self: { machineId: 'm-self', name: 'This Mac' }, machines: [row] };
	const dialled: string[] = [];
	const previousWindow = (globalThis as any).window;
	const previousWebSocket = (globalThis as any).WebSocket;
	// Nothing leaves this process: the socket only records where it was aimed.
	(globalThis as any).WebSocket = class {
		onopen = null; onclose = null; onerror = null; onmessage = null;
		constructor(url: string) { dialled.push(url); }
		send() {}
		close() {}
	};
	(globalThis as any).window = {
		api: {
			machinesList: async () => book,
			machinesAdd: async (address: string) => {
				assert.equal(address, '100.64.0.7');
				return { ...book, machine: row };
			},
		},
	};
	t.after(() => {
		// The socket never opens, so its route deadline would keep the process alive.
		(target as unknown as { machines?: { closeAll(): void } } | undefined)?.machines?.closeAll();
		if (previousWindow === undefined) delete (globalThis as any).window;
		else (globalThis as any).window = previousWindow;
		(globalThis as any).WebSocket = previousWebSocket;
	});

	const subject = new module.Store();
	target = subject;
	assert.deepEqual(await subject.addMachine('100.64.0.7'), { ok: true });
	assert.equal(subject.state.machines.length, 1);
	assert.equal(subject.state.machines[0].requested, true, 'the machine the daemon just added is asked for access');
	assert.equal(dialled.length, 1);
	assert.match(dialled[0], /^wss:\/\/192\.168\.1\.71:8788\//);
});
