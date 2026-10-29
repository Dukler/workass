'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { browserEvaluate, dialogOpened, noteActivity, pushEvent, runAction, takeDownload } = require('./browser-actions');

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
    evaluateAgentWorld: async () => ({ n: count(), quietFor: 1000 }),
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

test('a change the activity counter cannot see (a property write, a shadow root) is still reported', async () => {
  const { entry, manager, setLines } = fixture();
  await manager.takeOutline(entry, {});
  // The count never moves: a script set a field's value, or a shadow root changed.
  const report = await runAction(manager, entry, async () => { setLines(['- button "Go" [e1]', '- textbox "Name" [e2] value="Ada"']); return { ok: true }; }, { method: 'browser.click' });
  assert.notEqual(report.changes.none, true, 'never a false "none"');
  assert.equal(report.changes.added, 1);
  assert.match(report.changes.text, /Ada/);
});

test('an action that changes nothing is reported as none from a fresh read', async () => {
  const { entry, manager, calls } = fixture();
  await manager.takeOutline(entry, {});
  calls.outlines.length = 0;
  const report = await runAction(manager, entry, async () => ({ ok: true }), { method: 'browser.click' });
  assert.equal(report.changes.none, true);
  assert.equal(calls.outlines.length, 1, 'the page is read after every action');
  assert.deepEqual(calls.network, ['Network.enable', 'Page.setInterceptFileChooserDialog', 'Network.disable', 'Page.setInterceptFileChooserDialog'],
    'requests are watched, and a file chooser intercepted, only while the action runs');
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

test('the outline before an action is a fresh read unless the last one is only a moment old and nothing moved', async () => {
  let n = 3;
  const { entry, manager, calls } = fixture({ count: () => n });
  await manager.takeOutline(entry, {});
  entry.lastOutline.takenAt = Date.now() - 5000;
  calls.outlines.length = 0;
  await runAction(manager, entry, async () => ({ ok: true }), { method: 'browser.click' });
  assert.equal(calls.outlines.length, 2, 'a five-second-old outline is read again before the action');
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

test('a file chooser the page opens during an action is reported, never shown', async () => {
  const { entry, manager } = fixture();
  await manager.takeOutline(entry, {});
  const report = await runAction(manager, entry, async () => {
    noteActivity(entry, 'Page.fileChooserOpened', { mode: 'selectSingle', backendNodeId: 7 }, null);
    return { ok: true };
  }, { method: 'browser.click' });
  const opened = report.events.find((event) => event.type === 'file_chooser_opened');
  assert.ok(opened, 'reported');
  assert.match(opened.note, /workass_browser_upload/);
  assert.equal(entry.fileChooserWatched, false, 'interception ends with the action');
  noteActivity(entry, 'Page.fileChooserOpened', { mode: 'selectSingle' }, null);
  assert.equal(entry.events.filter((event) => event.type === 'file_chooser_opened').length, 1, 'a person\'s own chooser between actions is not taken over');
});

// The shell's input path, put back to rest whatever happens.
function inputDouble({ hang = () => false } = {}) {
  const { BrowserManager } = require('./browser-manager');
  const sent = [];
  const debug = {
    isAttached: () => true,
    sendCommand: (method, params = {}) => { sent.push([method, params.type ?? params.enabled ?? null]); return hang(method, params) ? new Promise(() => {}) : Promise.resolve({}); },
  };
  const entry = { operationChain: Promise.resolve() };
  const self = {
    hostMode: 'webview', inputCommandTimeoutMs: 40, operationDeadlineMs: 80, events: [], isHosted: () => true,
    lifecycle(_entry, stage) { self.events.push(stage); },
    liveContents: () => ({ debugger: debug }),
    assertCurrentEntry() {},
    holdPainting(target) { target.holds = (target.holds || 0) + 1; },
    releasePainting(target) { target.holds -= 1; },
  };
  for (const name of ['boundedInput', 'withInputBound', 'keepAppFocus', 'restoreAppFocus', 'appFocusScript', 'releaseStuckInput', 'withPainting', 'serialize', 'browserClick']) self[name] = BrowserManager.prototype[name];
  self.executeCDP = (_target, method, params) => self.boundedInput(entry, debug, method, params, null);
  return { self, entry, sent };
}

test('an input command the page never takes fails in bounded time, and the press it left down is released', async () => {
  const { self, entry, sent } = inputDouble({ hang: (method, params) => method === 'Input.dispatchMouseEvent' && params.type === 'mouseMoved' });
  await self.executeCDP({}, 'Input.dispatchMouseEvent', { type: 'mousePressed', x: 5, y: 6, button: 'left' });
  assert.deepEqual(entry.pointerDown, { sessionId: null, x: 5, y: 6, button: 'left' });
  const started = Date.now();
  await assert.rejects(self.executeCDP({}, 'Input.dispatchMouseEvent', { type: 'mouseMoved', x: 9, y: 9 }), /did not take dispatchMouseEvent/);
  assert.ok(Date.now() - started < 1000);
  await self.releaseStuckInput(entry);
  assert.deepEqual(sent.slice(-2), [['Input.dispatchMouseEvent', 'mouseReleased'], ['Input.cancelDragging', null]]);
  assert.equal(entry.pointerDown, null);
});

test('an operation past its deadline lets go of focus emulation, painting and the pointer', async () => {
  const { self, entry, sent } = inputDouble();
  entry.pointerDown = { sessionId: null, x: 1, y: 2, button: 'left' };
  await assert.rejects(self.serialize(entry, () => new Promise(() => {})), /did not finish/);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(entry.holds, 0, 'the paint and focus hold is released at the deadline');
  assert.ok(sent.some(([method, type]) => method === 'Input.dispatchMouseEvent' && type === 'mouseReleased'));
  assert.ok(sent.some(([method]) => method === 'Input.cancelDragging'));
});

test('a click on a select list is refused instead of opening a native menu', async () => {
  const { self, sent } = inputDouble();
  self.pointerTarget = async () => ({ inputTarget: {}, inputPoint: { x: 3, y: 4 }, point: { x: 3, y: 4, nativeMenu: true }, reportedTarget: { tag: 'select' } });
  const result = await self.browserClick({}, { selector: '#pick' });
  assert.equal(result.status, 'native_menu');
  assert.match(result.reason, /workass_browser_select/);
  assert.deepEqual(sent, [], 'no input was sent');
});

test('a drag that fails halfway turns interception off and puts the pointer back to rest', async () => {
  const { browserDrag } = require('./browser-actions');
  const { self, entry, sent } = inputDouble({ hang: (method, params) => method === 'Input.dispatchMouseEvent' && params.type === 'mouseMoved' && params.buttons === 1 });
  const manager = {
    pointerTarget: async () => ({ inputTarget: {}, inputPoint: { x: 10, y: 10 }, reportedTarget: { tag: 'div' } }),
    liveContents: () => ({ debugger: { on() {}, removeListener() {} } }),
    executeCDP: self.executeCDP,
    releaseStuckInput: (target) => self.releaseStuckInput(target),
  };
  await assert.rejects(browserDrag(manager, entry, { fromSelector: '#a', deltaX: 50, deltaY: 0 }), /did not take/);
  assert.deepEqual(sent.find(([method, value]) => method === 'Input.setInterceptDrags' && value === false), ['Input.setInterceptDrags', false]);
  assert.ok(sent.some(([method, type]) => method === 'Input.dispatchMouseEvent' && type === 'mouseReleased'), 'the press is released');
  assert.ok(sent.some(([method]) => method === 'Input.cancelDragging'), 'the drag is cancelled');
  assert.equal(entry.pointerDown, null);
});

test('commands on the top frame honour their time limit too', async () => {
  const { BrowserManager } = require('./browser-manager');
  const self = Object.create(BrowserManager.prototype);
  const entry = { targetSessions: new Map(), view: { webContents: { debugger: { sendCommand: () => new Promise(() => {}) } } } };
  const started = Date.now();
  await assert.rejects(self.ownedSessionCommand(entry, 'Runtime.evaluate', {}, null, 60), /did not answer/);
  assert.ok(Date.now() - started < 1000, 'bounded like a frame\'s command');
});

test('evaluate aimed at a frame without its own script context is refused, never run in the parent', async () => {
  const calls = [];
  const manager = {
    observationTarget: async () => ({ frameId: 'child' }),
    frameForSession: () => ({ id: 'root', parentFrameId: null }),
    ownedSessionCommand: async (...args) => { calls.push(args); return { result: { value: 1 } }; },
  };
  const entry = { frames: new Map([['child', { id: 'child', parentFrameId: 'root', sessionId: null, contextId: null }]]), interactionGeneration: 0 };
  const result = await browserEvaluate(manager, entry, { expression: 'document.title', selector: '#inside' });
  assert.equal(result.status, 'unavailable');
  assert.deepEqual(calls, [], 'nothing ran');
});

test('an agent-driven page cannot fill the disk with downloads', () => {
  const fs = require('node:fs');
  const os = require('node:os');
  const path = require('node:path');
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wk-dl-'));
  const entry = { chatId: 'dl-chat', visible: false, agentActiveAt: Date.now() };
  const manager = { downloadsRoot: root };
  const item = (name, total = 10) => {
    const events = {};
    return { name, cancelled: false, getFilename: () => name, setSavePath(file) { this.path = file; }, getURL: () => 'https://files.example/' + name,
      getTotalBytes: () => total, getReceivedBytes: () => total, on(event, fn) { events[event] = fn; }, once(event, fn) { events[event] = fn; },
      cancel() { this.cancelled = true; }, finish() { fs.writeFileSync(this.path, 'x'.repeat(total)); events.done({}, 'completed'); } };
  };
  let allowed = 0;
  for (let index = 0; index < 15; index += 1) {
    const download = item(`f${index}.txt`);
    takeDownload(manager, entry, download);
    if (!download.cancelled) { allowed += 1; download.finish(); }
  }
  assert.ok(allowed <= 10, `a burst is capped (${allowed} allowed)`);
  assert.ok(entry.events.some((event) => event.type === 'download_blocked'), 'and reported');
  const huge = item('huge.bin', 3 * 1024 * 1024 * 1024);
  entry.downloadTimes = [];
  takeDownload(manager, entry, huge);
  assert.equal(huge.cancelled, true, 'the folder total is capped');
  fs.rmSync(root, { recursive: true, force: true });
});

test('a navigation near its operation deadline reports the page as loading instead of failing', async () => {
  const { entry, manager } = fixture();
  entry.mainDocumentReady = false;
  entry.loading = true;
  entry.mainCommitPending = true;
  entry.operationDeadlineAt = Date.now() + 400;
  const started = Date.now();
  const report = await runAction(manager, entry, async () => ({ id: 1, loading: true, loadingNote: 'The page is still loading.' }), { before: null, method: 'browser.navigate' });
  assert.ok(Date.now() - started < 1500, 'returns before the deadline');
  assert.equal(report.loadingNote, 'The page is still loading.');
  assert.equal(report.changes.loading, true);
});
