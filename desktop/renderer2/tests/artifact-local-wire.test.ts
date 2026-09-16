import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import vm from 'node:vm';

test('local artifact methods use the existing socket and never queue across reconnect', async () => {
  const source = fs.readFileSync(new URL('../../../internal/httpserve/lan_bridge.go', import.meta.url), 'utf8');
  const script = source.slice(source.indexOf('`') + 1, source.lastIndexOf('`'));
  const sockets: Socket[] = [];
  const timers = new Map<number, () => void>();
  let timerID = 0;
  class Socket {
    static OPEN = 1;
    readyState = 0;
    sent: any[] = [];
    onopen?: () => void;
    onclose?: () => void;
    onmessage?: (event: { data: string }) => void;
    constructor(_url: string) { sockets.push(this); }
    send(data: string) { this.sent.push(JSON.parse(data)); }
    close() { this.readyState = 3; this.onclose?.(); }
    approve() {
      this.readyState = 1; this.onopen?.();
      this.onmessage?.({ data: JSON.stringify({ t: 'event', channel: 'lan:access-state', payload: { state: 'approved' } }) });
    }
    reply(value: unknown) {
      const request = this.sent.at(-1);
      this.onmessage?.({ data: JSON.stringify({ t: 'reply', id: request.id, result: value }) });
    }
  }
  const window: any = { dispatchEvent() {} };
  vm.runInNewContext(script, {
    window, WebSocket: Socket, URLSearchParams, navigator: { platform: 'test' },
    location: { protocol: 'http:', host: 'localhost' },
    localStorage: { getItem: () => '', setItem() {}, removeItem() {} },
    CustomEvent: class {},
    setTimeout: (fn: () => void) => { timers.set(++timerID, fn); return timerID; },
    clearTimeout: (id: number) => timers.delete(id),
  });
  const api = window.api;
  assert.equal(api.artifactConnectionGeneration(), 0);
  for (const method of ['artifactOpen', 'artifactRead', 'artifactClose']) {
    await assert.rejects(api[method]({}), /socket-not-ready/);
  }
  assert.equal(sockets.length, 1);
  sockets[0].approve();
  const generation = api.artifactConnectionGeneration();
  assert.ok(generation > 0);
  assert.equal(sockets[0].sent.length, 0, 'disconnected calls must not queue');
  const opened = api.artifactOpen({ path: '/workass/artifacts/report/' });
  assert.equal(sockets[0].sent[0].channel, 'artifact:open');
  sockets[0].reply({ transferId: 'one' });
  assert.equal((await opened).transferId, 'one');
  const read = api.artifactRead({ transferId: 'one' });
  sockets[0].close();
  await assert.rejects(read, /socket-closed/);
  assert.equal(api.artifactConnectionGeneration(), 0);
  await assert.rejects(api.artifactRead({ transferId: 'one' }), /socket-not-ready/);
  const reconnect = [...timers.values()][0];
  timers.clear(); reconnect();
  assert.equal(sockets.length, 2);
  sockets[1].approve();
  assert.ok(api.artifactConnectionGeneration() > generation);
  assert.equal(sockets[1].sent.length, 0, 'a new socket must not replay an old transfer');
  assert.equal(window.api, api, 'API identity alone cannot detect a reconnect');
});
