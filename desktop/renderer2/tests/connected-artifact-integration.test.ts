import assert from 'node:assert/strict';
import test from 'node:test';
import http from 'node:http';
import { createRequire } from 'node:module';
import { installWorkassArtifactsBridge } from '../src/connected-artifacts.ts';
const require = createRequire(import.meta.url);
const { createConnectedArtifactBridge } = require('../../shell/connected-artifacts.js');

for (const local of [false, true]) test(`HTTP bridge streams binary through exact ${local ? 'local' : 'remote'} machine RPC`, async (t) => {
  let onRequest: (payload: any) => void = () => {};
  let onCancel: (payload: any) => void = () => {};
  const mainFrame = {};
  const webContents = { mainFrame, send(channel: string, payload: any) {
    queueMicrotask(() => channel.endsWith(':request') ? onRequest(payload) : onCancel(payload));
  } };
  let bridge: any;
  const server = http.createServer((req, res) => void bridge.handle(req, res));
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address() as { port: number };
  const origin = `http://127.0.0.1:${address.port}`;
  bridge = createConnectedArtifactBridge({ win: { webContents }, viewServer: { url: origin } });
  const expected = Buffer.alloc(300000);
  for (let i = 0; i < expected.length; i++) expected[i] = i % 251;
  let offset = 0;
  const calls: string[] = [];
  const link = { async invoke(channel: string, payload: any) {
    calls.push(channel);
    if (channel === 'artifact:open') {
      assert.equal(payload.path, '/workass/artifacts/report/data.bin?version=1');
      return { transferId: 'one', status: 200, headers: { 'Content-Type': 'application/octet-stream', 'Content-Length': String(expected.length) } };
    }
    assert.equal(payload.transferId, 'one');
    if (channel === 'artifact:close') return {};
    const chunk = expected.subarray(offset, offset + 128 * 1024);
    offset += chunk.length;
    return { bodyBase64: chunk.toString('base64'), eof: offset === expected.length };
  } } as any;
  const localAPI = {
    artifactConnectionGeneration: () => 1,
    artifactOpen: (payload: any) => link.invoke('artifact:open', payload),
    artifactRead: (payload: any) => link.invoke('artifact:read', payload),
    artifactClose: (payload: any) => link.invoke('artifact:close', payload),
  };
  const dispose = installWorkassArtifactsBridge({
    local: () => localAPI,
    localMachineId: () => local ? 'remote' : 'other',
    linkFor: (id) => { assert.equal(local, false, 'local reads must not require a remote peer'); assert.equal(id, 'remote'); return link; },
    ownsLink: (id, current) => id === 'remote' && current === link,
  }, {
    supported: true,
    onRequest: (cb) => { onRequest = cb; return () => {}; },
    onCancel: (cb) => { onCancel = cb; return () => {}; },
    reply: async (payload) => bridge.reply({ sender: webContents, senderFrame: mainFrame }, payload),
  });
  t.after(() => { dispose(); bridge.close(); server.closeAllConnections(); server.close(); });
  const path = `${origin}/workass/artifacts/@remote/report/data.bin?version=1`;
  const denied = await fetch(path);
  assert.equal(denied.status, 403);
  assert.deepEqual(calls, []);
  const response = await fetch(path, { headers: { [bridge.accessHeader]: bridge.capability } });
  assert.equal(response.status, 200);
  assert.deepEqual(Buffer.from(await response.arrayBuffer()), expected);
  assert.deepEqual(calls, ['artifact:open', 'artifact:read', 'artifact:read', 'artifact:read']);
});
