'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { dialogOpened, noteActivity, pushEvent, runAction } = require('./browser-actions');

// A manager double: the page's activity count is whatever `count()` says,
// every outline read is counted, and the CDP session accepts Network.enable.
function fixture({ count = () => 3, frames = false, lines = ['- button "Go" [e1]'] } = {}) {
  const entry = {
    frames: new Map([['root', { id: 'root', parentFrameId: null }]]),
    mainDocumentReady: true, eventSeq: 0, activitySeq: 0,
  };
  if (frames) entry.frames.set('child', { id: 'child', parentFrameId: 'root', detached: false });
  const calls = { outlines: [], network: [] };
  let outlineLines = lines;
  const manager = {
    documentKey: () => 'doc-1',
    liveContents: () => ({ getURL: () => 'http://127.0.0.1/page', debugger: { isAttached: () => true, sendCommand: async (name) => { calls.network.push(name); } } }),
    evaluateMainWorld: async () => ({ n: count(), quietFor: 1000 }),
    takeOutline: async (_entry, options = {}) => {
      calls.outlines.push(options.activity ? 'with-activity' : 'fresh');
      const outline = { id: `o${calls.outlines.length}`, documentKey: 'doc-1', takenAt: Date.now(), lines: outlineLines.map((text, i) => ({ depth: 0, text, key: `k${i}:${text}` })),
        activityN: options.activity ? options.activity.n : count(), activitySeq: entry.activitySeq };
      entry.lastOutline = outline;
      return outline;
    },
  };
  return { entry, manager, calls, setLines: (next) => { outlineLines = next; } };
}

test('an action that changes nothing on a page without frames reads no outline', async () => {
  const { entry, manager, calls } = fixture();
  await manager.takeOutline(entry, {});
  calls.outlines.length = 0;
  const report = await runAction(manager, entry, async () => ({ ok: true }), { method: 'browser.click' });
  assert.equal(report.changes.none, true);
  assert.deepEqual(calls.outlines, [], 'neither a before nor an after outline');
  assert.deepEqual(calls.network, ['Network.enable', 'Network.disable'], 'requests are watched only while the action settles');
});

test('a change is outlined after the action and reported as a diff against the clean last outline', async () => {
  let n = 3;
  const { entry, manager, calls, setLines } = fixture({ count: () => n });
  await manager.takeOutline(entry, {});
  calls.outlines.length = 0;
  const report = await runAction(manager, entry, async () => { n = 4; setLines(['- button "Go" [e1]', '- text: Done']); return { ok: true }; }, { method: 'browser.click' });
  assert.deepEqual(calls.outlines, ['with-activity'], 'only the outline after: the last one was still clean');
  assert.equal(report.changes.added, 1);
  assert.match(report.changes.text, /Done/);
});

test('the last outline is not reused once the page has changed since', async () => {
  let n = 3;
  const { entry, manager, calls } = fixture({ count: () => n });
  await manager.takeOutline(entry, {});
  n = 7; // the page changed on its own after the agent read it
  calls.outlines.length = 0;
  await runAction(manager, entry, async () => ({ ok: true }), { method: 'browser.click' });
  assert.equal(calls.outlines[0], 'with-activity', 'a fresh outline before the action');
});

test('a request the action starts holds the report until it finishes', async () => {
  const { entry, manager, calls } = fixture();
  await manager.takeOutline(entry, {});
  calls.outlines.length = 0;
  const started = Date.now();
  const report = await runAction(manager, entry, async () => {
    noteActivity(entry, 'Network.requestWillBeSent', { requestId: 'r1', type: 'Fetch' }, null);
    setTimeout(() => noteActivity(entry, 'Network.loadingFinished', { requestId: 'r1' }, null), 200);
    return { ok: true };
  }, { method: 'browser.click' });
  assert.ok(Date.now() - started >= 200, 'waited for the request');
  assert.equal(calls.outlines.length, 1, 'outlined after: something happened');
  assert.ok(report.changes);
});

test('long-lived connections and child-frame requests do not hold an action', async () => {
  const { entry, manager } = fixture();
  await manager.takeOutline(entry, {});
  const started = Date.now();
  await runAction(manager, entry, async () => {
    noteActivity(entry, 'Network.requestWillBeSent', { requestId: 'ws', type: 'WebSocket' }, null);
    noteActivity(entry, 'Network.requestWillBeSent', { requestId: 'es', type: 'EventSource' }, null);
    noteActivity(entry, 'Network.requestWillBeSent', { requestId: 'child', type: 'Fetch' }, 'child-session');
    return { ok: true };
  }, { method: 'browser.click' });
  assert.ok(Date.now() - started < 1000, 'not held to the settle limit');
});

test('a page with frames, a hover and a navigation are always outlined after', async () => {
  for (const [options, method] of [[{ frames: true }, 'browser.click'], [{}, 'browser.hover']]) {
    const { entry, manager, calls } = fixture(options);
    await manager.takeOutline(entry, {});
    calls.outlines.length = 0;
    await runAction(manager, entry, async () => ({ ok: true }), { method });
    assert.equal(calls.outlines.length, 1, `${method} ${options.frames ? 'with frames' : ''}`);
  }
  const { entry, manager, calls } = fixture();
  calls.outlines.length = 0;
  const report = await runAction(manager, entry, async () => ({ ok: true }), { before: null, method: 'browser.navigate' });
  assert.deepEqual(calls.outlines, ['with-activity'], 'no outline before a navigation, one after');
  assert.equal(report.changes.navigated, true);
});

test('an unchanged page keeps its newest outline; a change, a load, or age on a page with frames makes a fresh one', async () => {
  const { BrowserManager } = require('./browser-manager');
  let n = 3;
  const entry = { lastOutline: { id: 'o1', documentKey: 'd', activityN: 3, activitySeq: 0, takenAt: Date.now() }, frames: new Map(), activitySeq: 0 };
  const self = { documentKey: () => 'd', evaluateMainWorld: async () => ({ n }) };
  const reuse = () => BrowserManager.prototype.reusableOutline.call(self, entry);
  assert.equal((await reuse())?.id, 'o1');
  n = 4;
  assert.equal(await reuse(), null, 'the page changed');
  n = 3; entry.activitySeq = 1;
  assert.equal(await reuse(), null, 'a frame or load event since');
  entry.activitySeq = 0; entry.loading = true;
  assert.equal(await reuse(), null, 'still loading');
  entry.loading = false;
  entry.frames.set('child', { id: 'child', parentFrameId: 'root' });
  entry.lastOutline.takenAt = Date.now() - 2500;
  assert.equal(await reuse(), null, 'frames are trusted for less long');
});

test('an action that opens a dialog reports it at once, even while the paused page holds commands', async () => {
  const { entry, manager } = fixture();
  await manager.takeOutline(entry, {});
  manager.liveContents = () => ({ getURL: () => 'http://127.0.0.1/page',
    debugger: { isAttached: () => true, sendCommand: (name) => (name === 'Network.disable' ? new Promise(() => {}) : Promise.resolve()) } });
  const started = Date.now();
  const report = await runAction(manager, entry, () => { dialogOpened(entry, { type: 'alert', message: 'Saved' }); return new Promise(() => {}); }, { method: 'browser.click' });
  assert.equal(report.dialog.type, 'alert');
  assert.ok(Date.now() - started < 500, 'not held by the paused page');
});

test('a tab the action opens is in its report even when it takes a moment to come up', async () => {
  const { entry, manager } = fixture();
  await manager.takeOutline(entry, {});
  const report = await runAction(manager, entry, async () => {
    entry.pendingTabs = 1;
    setTimeout(() => { pushEvent(entry, { type: 'tab_opened', tab_id: 9 }); entry.pendingTabs = 0; }, 150);
    return { ok: true };
  }, { method: 'browser.click' });
  assert.ok(report.events.some((event) => event.type === 'tab_opened'));
});
