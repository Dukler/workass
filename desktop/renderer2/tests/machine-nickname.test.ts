import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { createServer } from 'vite';
import { MACHINE_NICKNAME_MAX, normalizeMachineNickname } from '../src/machine-nickname.ts';

test('machine nicknames trim, clear, and reject bounded invalid input', () => {
  assert.deepEqual(normalizeMachineNickname('  Taller  '), { nickname: 'Taller' });
  assert.deepEqual(normalizeMachineNickname('   '), { nickname: '' });
  assert.match(normalizeMachineNickname('x'.repeat(MACHINE_NICKNAME_MAX + 1)).error ?? '', /hasta 64/);
  assert.match(normalizeMachineNickname('line\nbreak').error ?? '', /control/);
});

test('saving a nickname updates the local machine projection from the daemon receipt', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const Store = (await server.ssrLoadModule('/src/store/store.ts')).Store as new () => {
    state: { machines: Array<{ name: string; nickname: string; reportedName: string }> };
    setMachineNickname(machineId: string, nickname: string): Promise<{ ok: boolean; error?: string }>;
  };
  const previousWindow = (globalThis as any).window;
  const calls: unknown[][] = [];
  (globalThis as any).window = {
    api: {
      machinesNickname: async (...args: unknown[]) => {
        calls.push(args);
        return {
          ok: true,
          self: { machineId: 'm-self', name: 'This Mac' },
          machines: [{
            machineId: 'm-builder', name: 'builder-hostname', nickname: 'Taller',
            endpoints: [{ kind: 'lan', address: '192.168.1.50:80' }], secure: false,
          }],
        };
      },
    },
  };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });

  const target = new Store();
  assert.deepEqual(await target.setMachineNickname('m-builder', '  Taller  '), { ok: true });
  assert.deepEqual(calls, [['m-builder', 'Taller']]);
  assert.equal(target.state.machines[0]?.name, 'Taller');
  assert.equal(target.state.machines[0]?.nickname, 'Taller');
  assert.equal(target.state.machines[0]?.reportedName, 'builder-hostname');
});
