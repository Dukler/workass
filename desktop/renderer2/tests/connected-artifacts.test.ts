import assert from 'node:assert/strict';
import test from 'node:test';
import { connectedArtifactURL, installWorkassArtifactsBridge } from '../src/connected-artifacts.ts';

test('connected artifact URLs stay on the local shell origin and preserve subpaths/query', () => {
  assert.equal(
    connectedArtifactURL('san-laptop', '/workass/artifacts/report/preview.png?download=1', 'http://127.0.0.1:8799', true),
    'http://127.0.0.1:8799/workass/artifacts/@san-laptop/report/preview.png?download=1',
  );
  assert.equal(connectedArtifactURL('', '/workass/artifacts/local/'), '/workass/artifacts/local/');
  assert.equal(connectedArtifactURL('san-laptop', 'https://192.0.2.10:8788/workass/artifacts/report/', 'http://127.0.0.1', true), 'http://127.0.0.1/workass/artifacts/@san-laptop/report/');
  assert.equal(connectedArtifactURL('san-laptop', '/workass/artifacts/../secret'), '');
  assert.equal(connectedArtifactURL('san-laptop', '/workass/artifacts/report/file%20name.png?x=1#view', 'http://127.0.0.1', true), 'http://127.0.0.1/workass/artifacts/@san-laptop/report/file%20name.png?x=1#view');
  assert.equal(connectedArtifactURL('san-laptop', '/workass/artifacts/report/', 'http://127.0.0.1', false), '');
  assert.equal(connectedArtifactURL('', 'https://example.com/file'), 'https://example.com/file');
});

test('artifact requests select the exact ready link and reject stale or unavailable links', async () => {
  let current: object | undefined;
  const calls: unknown[][] = [];
  const socket = { invoke: async (...args: unknown[]) => { calls.push(args); return { transferId: 't1', status: 200, headers: {} }; } } as any;
  current = socket;
  const registry = { linkFor: () => current as any, ownsLink: (_id: string, link: object) => link === current };
  let request: any; let cancel: any; const replies: any[] = [];
  const bridge = { supported: true, onRequest: (cb: any) => { request = cb; return () => {}; }, onCancel: (cb: any) => { cancel = cb; return () => {}; }, reply: async (p: any) => replies.push(p) };
  installWorkassArtifactsBridge(registry, bridge);
  await request({ op: 'open', requestId: 'r1', machineId: 'san-laptop', path: '/workass/artifacts/report/', method: 'GET' });
  assert.deepEqual(calls[0], ['artifact:open', { path: '/workass/artifacts/report/', method: 'GET', headers: {} }]);
  assert.equal(replies[0].ok, true);
  current = undefined;
  await request({ op: 'read', requestId: 'r2', machineId: 'san-laptop', transferId: 't1' });
  assert.match(replies[1].error, /unavailable|response invalid/);
  current = socket;
  const pending = request({ op: 'open', requestId: 'r3', machineId: 'san-laptop', path: '/workass/artifacts/report/' });
  current = {};
  await pending;
  assert.match(replies[2].error, /unavailable|response invalid/);
});

test('cancellation before open reply closes the late transfer on its original link', async () => {
  let resolveOpen!: (value: unknown) => void;
  const calls: unknown[][] = [];
  const link = { invoke: (channel: string, payload: unknown) => { calls.push([channel, payload]); return new Promise((resolve) => { resolveOpen = resolve; }); } } as any;
  const registry = { linkFor: () => link, ownsLink: () => true };
  let request: any; let cancel: any; const bridge = { supported: true, onRequest: (cb: any) => { request = cb; return () => {}; }, onCancel: (cb: any) => { cancel = cb; return () => {}; }, reply: async () => {} };
  installWorkassArtifactsBridge(registry, bridge);
  const pending = request({ op: 'open', requestId: 'cancel-open', machineId: 'A', path: '/workass/artifacts/x/' });
  cancel({ requestId: 'cancel-open' });
  resolveOpen({ transferId: 'late', status: 200, headers: {} });
  await pending;
  assert.deepEqual(calls.map(([channel]) => channel), ['artifact:open', 'artifact:close']);
  assert.deepEqual(calls[1]?.[1], { transferId: 'late' });
});

test('stale link is rejected before any remote invoke', async () => {
  let invoked = false;
  const link = { invoke: async () => { invoked = true; } } as any;
  const registry = { linkFor: () => link, ownsLink: () => false };
  let request: any; const replies: any[] = [];
  const bridge = { supported: true, onRequest: (cb: any) => { request = cb; return () => {}; }, onCancel: () => () => {}, reply: async (p: any) => replies.push(p) };
  installWorkassArtifactsBridge(registry, bridge);
  await request({ op: 'open', requestId: 'stale', machineId: 'A', path: '/workass/artifacts/x/' });
  assert.equal(invoked, false); assert.equal(replies[0].ok, false);
});

test('same transfer id on two machines remains isolated and disposal closes live transfers', async () => {
  const calls: Array<[string, unknown, string]> = [];
  const links = new Map<string, any>();
  for (const machine of ['A', 'B']) links.set(machine, { invoke: async (channel: string, payload: unknown) => { calls.push([channel, payload, machine]); return channel === 'artifact:open' ? { transferId: 'same', status: 200, headers: {} } : { bodyBase64: '', eof: true }; } });
  const registry = { linkFor: (id: string) => links.get(id), ownsLink: () => true };
  let request: any; const bridge = { supported: true, onRequest: (cb: any) => { request = cb; return () => {}; }, onCancel: () => () => {}, reply: async () => {} };
  const dispose = installWorkassArtifactsBridge(registry, bridge);
  await request({ op: 'open', requestId: 'a-open', machineId: 'A', path: '/workass/artifacts/x/' });
  await request({ op: 'open', requestId: 'b-open', machineId: 'B', path: '/workass/artifacts/x/' });
  await request({ op: 'read', requestId: 'b-read', machineId: 'B', transferId: 'same' });
  assert.equal(calls.at(-1)?.[2], 'B');
  dispose();
  assert.deepEqual(calls.slice(-2).map(([, payload, machine]) => [payload, machine]).sort((a, b) => String(a[1]).localeCompare(String(b[1]))), [[{ transferId: 'same' }, 'A'], [{ transferId: 'same' }, 'B']]);
});

test('canonical ownership survives copying and malformed links cannot be normalized into a different owner', () => {
  const origin = 'http://127.0.0.1:8799';
  for (const chatOwner of ['', 'other']) {
    for (const link of ['/workass/artifacts/@owner/report/?q=1#x', 'http://old:8788/workass/connected-artifacts/owner/report/?q=1#x']) {
      assert.equal(connectedArtifactURL(chatOwner, link, origin, true), `${origin}/workass/artifacts/@owner/report/?q=1#x`);
    }
  }
  for (const path of ['/workass/artifacts/@./report/', '/workass/artifacts/@owner/../report/', '/workass/artifacts/@owner/%2e%2e/report/', '/workass/connected-artifacts//report/', '/workass/connected-artifacts/owner/./report/', '/workass/artifacts/@owner/report/a//b']) {
    assert.equal(connectedArtifactURL('other', path, origin, true), '', path);
    assert.equal(connectedArtifactURL('other', `http://old:8788${path}`, origin, true), '', path);
  }
  assert.equal(connectedArtifactURL('', '/workass/artifacts/@owner/report/', origin, false, 'owner'), `${origin}/workass/artifacts/@owner/report/`);
  assert.equal(connectedArtifactURL('', '/workass/artifacts/@other/report/', origin, false, 'owner'), '');
});

test('local transfers pin identity, API and connection generation without requiring any peer', async () => {
  let identity = 'local'; let generation = 1;
  let finish: (value: any) => void = () => {};
  const calls: string[] = [];
  const original = {
    artifactConnectionGeneration: () => generation,
    artifactOpen: async () => { calls.push('open'); return { transferId: 'one', status: 200, headers: {} }; },
    artifactRead: async () => { calls.push('read'); return new Promise<any>((resolve) => { finish = resolve; }); },
    artifactClose: async () => { calls.push('close'); },
  };
  let localAPI = original;
  let request: any;
  const replies: any[] = [];
  const dispose = installWorkassArtifactsBridge({
    local: () => localAPI, localMachineId: () => identity,
    linkFor: () => { throw new Error('no peers'); }, ownsLink: () => false,
  }, {
    supported: true, onRequest: (cb) => { request = cb; return () => {}; },
    onCancel: () => () => {}, reply: async (value) => { replies.push(value); },
  });
  for (const change of [() => { generation++; }, () => { identity = 'changed'; }, () => { localAPI = { ...original }; }]) {
    identity = 'local'; localAPI = original;
    await request({ op: 'open', requestId: 'open', machineId: 'local', path: '/workass/artifacts/report/' });
    assert.equal(replies.at(-1).ok, true);
    const reading = request({ op: 'read', requestId: 'read', machineId: 'local', transferId: 'one' });
    change(); finish({ bodyBase64: 'b2s=', eof: true }); await reading;
    assert.equal(replies.at(-1).ok, false);
  }
  identity = 'local'; localAPI = original;
  await request({ op: 'open', requestId: 'open', machineId: 'local', path: '/workass/artifacts/report/' });
  const reads = calls.filter((call) => call === 'read').length;
  generation++;
  await request({ op: 'read', requestId: 'read', machineId: 'local', transferId: 'one' });
  assert.equal(replies.at(-1).ok, false);
  assert.equal(calls.filter((call) => call === 'read').length, reads);
  dispose();
  assert.equal(calls.includes('close'), false, 'stale identifiers must never be replayed against a replacement connection');
});
