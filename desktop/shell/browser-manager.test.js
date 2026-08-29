'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const { BrowserManager, cleanUserAgent, normalizeBrowserURL, parseBrowserKey, resolveBrowserURL, safeBounds } = require('./browser-manager');

class FakeDebugger extends EventEmitter {
  constructor() { super(); this.attached = false; this.commands = []; this.commandCalls = []; }
  isAttached() { return this.attached; }
  attach(version) { this.attached = true; this.version = version; }
  async sendCommand(method, params, sessionId) {
    this.commands.push(method);
    this.commandCalls.push({ method, params, sessionId });
    this.lastCommand = { method, params, sessionId };
    if (method === 'Target.attachToTarget') return { sessionId: 'child-session-1' };
    return { targetInfo: { targetId: 'target-1' } };
  }
}

class FakeHistory {
  constructor() { this.back = false; this.forward = false; }
  canGoBack() { return this.back; }
  canGoForward() { return this.forward; }
  goBack() { this.wentBack = true; }
  goForward() { this.wentForward = true; }
}

class FakeWebContents extends EventEmitter {
  constructor(profile) {
    super();
    this.id = ++FakeWebContents.seq;
    this.session = profile;
    this.debugger = new FakeDebugger();
    this.navigationHistory = new FakeHistory();
    this.url = 'about:blank';
  }
  setUserAgent(value) { this.userAgent = value; }
  getURL() { return this.url; }
  async loadURL(url) { this.url = url; this.emit('did-navigate', {}, url); this.emit('did-stop-loading'); }
  reload() { this.reloaded = true; }
  stop() { this.stopped = true; }
  close() { this.closed = true; }
  setWindowOpenHandler(handler) { this.windowOpenHandler = handler; }
  async executeJavaScript(script) { this.lastScript = script; return { ok: true }; }
  async insertText(text) { this.insertedTexts = [...(this.insertedTexts || []), text]; }
  // Electron resolves capturePage() with an EMPTY image for a view that is not
  // attached to the window, rather than rejecting. The fake has to model that
  // or it cannot guard the bug.
  async capturePage() {
    if (this.view && this.view.attached) return { isEmpty: () => false, toPNG: () => Buffer.from('fake-png') };
    return { isEmpty: () => true, toPNG: () => Buffer.alloc(0) };
  }
  sendInputEvent(event) { this.inputEvents = [...(this.inputEvents || []), event]; }
}
FakeWebContents.seq = 100;

class FakeView {
  constructor(options) {
    this.options = options;
    this.attached = false;
    this.webContents = new FakeWebContents(FakeView.profile);
    this.webContents.view = this;
  }
  setBounds(bounds) { this.bounds = bounds; }
  setBackgroundColor(color) { this.backgroundColor = color; }
}

function fixture(overrides = {}) {
  const profile = {
    isPersistent: () => true,
    setUserAgent(value) { this.userAgent = value; },
    setPermissionCheckHandler(handler) { this.check = handler; },
    setPermissionRequestHandler(handler) { this.request = handler; },
  };
  FakeView.profile = profile;
  const contentView = {
    children: [],
    addChildView(view) { view.attached = true; this.children.push(view); },
    removeChildView(view) { view.attached = false; this.children = this.children.filter((item) => item !== view); },
  };
  const win = { contentView, getContentSize: () => [1200, 800] };
  const sessions = [];
  const session = { fromPartition(partition, options) { sessions.push({ partition, options }); return profile; } };
  const states = [];
  const manager = new BrowserManager({
    win, WebContentsView: FakeView, session, chromeVersion: '140.0.7339.1', platform: 'darwin',
    onState: (state) => states.push(state),
    requestOpen: overrides.requestOpen,
  });
  return { manager, win, profile, sessions, states };
}

test('uses one persistent profile and CDP-attached isolated views', async () => {
  const { manager, win, profile, sessions } = fixture();
  const first = await manager.activate({ chatId: 'chat-a', bounds: { x: 900, y: 80, width: 280, height: 640 }, url: 'example.com' });
  assert.deepEqual(sessions, [{ partition: 'persist:workass-browser', options: { cache: true } }]);
  assert.equal(first.persistent, true);
  assert.equal(first.cdpAttached, true);
  assert.equal(first.url, 'https://example.com');
  const entry = manager.entries.get('chat-a');
  assert.equal(entry.view.options.webPreferences.nodeIntegration, false);
  assert.equal(entry.view.options.webPreferences.sandbox, true);
  assert.equal(entry.view.options.webPreferences.partition, 'persist:workass-browser');
  assert.equal(entry.view.webContents.debugger.version, '1.3');
  assert.deepEqual(entry.view.webContents.debugger.commands, ['Target.getTargetInfo']);
  assert.equal(profile.check(), false);
  assert.equal(win.contentView.children.length, 1);

  await manager.activate({ chatId: 'chat-b', bounds: { x: 910, y: 90, width: 500, height: 900 } });
  assert.equal(win.contentView.children.length, 1);
  assert.equal(manager.activeId, 'chat-b');
  assert.deepEqual(manager.entries.get('chat-b').bounds, { x: 910, y: 90, width: 290, height: 710 });
  assert.equal(manager.hide('chat-b'), true);
  assert.equal(win.contentView.children.length, 0);
});

test('health probe attaches CDP without displaying the browser rail', async () => {
  const { manager, win } = fixture();
  const state = await manager.probe();
  assert.equal(state.chatId, '__workass-health__');
  assert.equal(state.persistent, true);
  assert.equal(state.cdpAttached, true);
  assert.equal(win.contentView.children.length, 0);
  assert.equal(manager.attachedView, null);
});

test('navigation commands preserve exact browser identities and shared history', async () => {
  const { manager } = fixture();
  await manager.activate({ chatId: 'chat-a', bounds: { x: 0, y: 0, width: 300, height: 300 } });
  const wc = manager.entries.get('chat-a').view.webContents;
  wc.navigationHistory.back = true;
  wc.navigationHistory.forward = true;
  await manager.command('chat-a', 'navigate', 'workass browser');
  assert.match(wc.getURL(), /^https:\/\/www\.google\.com\/search\?q=/);
  await manager.command('chat-a', 'back');
  await manager.command('chat-a', 'forward');
  await manager.command('chat-a', 'reload');
  await manager.command('chat-a', 'stop');
  assert.equal(wc.navigationHistory.wentBack, true);
  assert.equal(wc.navigationHistory.wentForward, true);
  assert.equal(wc.reloaded, true);
  assert.equal(wc.stopped, true);
  assert.equal(manager.close('chat-a'), true);
  assert.equal(wc.closed, true);
});

test('destroy is idempotent after Electron has already destroyed the owning window', async () => {
  const { manager, win } = fixture();
  await manager.activate({ chatId: 'chat-shutdown', bounds: { x: 0, y: 0, width: 300, height: 300 } });
  const wc = manager.entries.get('chat-shutdown').view.webContents;

  // BrowserWindow emits `closed` only after its native contentView has gone
  // away. Rebuild teardown must still dispose the child WebContentsView without
  // throwing an uncaught "Object has been destroyed" dialog.
  win.contentView.removeChildView = () => { throw new Error('Object has been destroyed'); };

  assert.doesNotThrow(() => manager.destroy());
  assert.doesNotThrow(() => manager.destroy());
  assert.equal(manager.entries.size, 0);
  assert.equal(manager.attachedView, null);
  assert.equal(wc.closed, true);
});

test('URL, UA, and bounds normalization stay constrained', () => {
  assert.equal(normalizeBrowserURL('localhost:5173/app'), 'http://localhost:5173/app');
  assert.equal(normalizeBrowserURL('example.com'), 'https://example.com');
  assert.equal(normalizeBrowserURL('search words'), 'https://www.google.com/search?q=search%20words');
  assert.doesNotMatch(cleanUserAgent('140.1.2.3', 'darwin'), /Electron/);
  assert.match(cleanUserAgent('140.1.2.3', 'darwin'), /Chrome\/140\.0\.0\.0/);
  assert.deepEqual(safeBounds({ x: -5, y: 90, width: 9999, height: 9999 }, [1000, 700]), { x: 0, y: 90, width: 1000, height: 610 });
});

test('browser shortcuts use CDP in a background tab and separate modifier chords from the actual key', async () => {
  assert.deepEqual(parseBrowserKey('Meta+A', 'darwin'), { keyCode: 'A', modifiers: ['meta'] });
  assert.deepEqual(parseBrowserKey('Control+Shift+P', 'darwin'), { keyCode: 'P', modifiers: ['control', 'shift'] });
  assert.deepEqual(parseBrowserKey('CommandOrControl+A', 'darwin'), { keyCode: 'A', modifiers: ['meta'] });
  assert.deepEqual(parseBrowserKey('CommandOrControl+A', 'win32'), { keyCode: 'A', modifiers: ['control'] });
  assert.deepEqual(parseBrowserKey('Control+-', 'darwin'), { keyCode: '-', modifiers: ['control'] });
  assert.throws(() => parseBrowserKey('Hyper+A', 'darwin'), /unsupported browser modifier/);
  assert.throws(() => parseBrowserKey('Control+', 'darwin'), /invalid browser shortcut/);

  const { manager, win } = fixture({ requestOpen: () => {} });
  manager.setAgentControlReady(true);
  const tab = await manager.browserControl('browser.open', { chatId: 'chat-keys', url: 'example.com' });
  const wc = manager.browserEntries()[0].view.webContents;
  assert.equal(tab.active, false);
  assert.equal(win.contentView.children.length, 0);
  wc.executeJavaScript = async (script) => {
    assert.doesNotThrow(() => new Function(`return ${script};`));
    if (script.includes('workass-browser-select-all')) {
      return { found: true, selectionVerified: true, strategy: 'contenteditable-range' };
    }
    if (script.includes('workass-browser-key-barrier')) return undefined;
    if (script.includes('workass-browser-delete-selection')) {
      return { found: true, deletionVerified: true, strategy: 'contenteditable-command' };
    }
    throw new Error('unexpected browser shortcut script');
  };
  const result = await manager.browserControl('browser.key', { tabId: tab.id, key: 'Meta+A' });
  assert.deepEqual(result, {
    sent: true, key: 'A', modifiers: ['meta'], selectionVerified: true, selectionStrategy: 'contenteditable-range',
  });
  assert.equal(wc.inputEvents, undefined);
  assert.deepEqual(wc.debugger.commandCalls.filter((call) => call.method === 'Input.dispatchKeyEvent'), [
    {
      method: 'Input.dispatchKeyEvent',
      params: {
        type: 'rawKeyDown', modifiers: 4, key: 'a', code: 'KeyA', windowsVirtualKeyCode: 65, unmodifiedText: 'a',
      },
      sessionId: undefined,
    },
    {
      method: 'Input.dispatchKeyEvent',
      params: {
        type: 'keyUp', modifiers: 4, key: 'a', code: 'KeyA', windowsVirtualKeyCode: 65, unmodifiedText: 'a',
      },
      sessionId: undefined,
    },
  ]);

  const deletion = await manager.browserControl('browser.key', { tabId: tab.id, key: 'Backspace' });
  assert.deepEqual(deletion, {
    sent: true, key: 'Backspace', modifiers: [], deletionVerified: true, deletionStrategy: 'contenteditable-command',
  });
  assert.deepEqual(
    wc.debugger.commandCalls.filter((call) => call.method === 'Input.dispatchKeyEvent').slice(-2).map((call) => call.params),
    [
      { type: 'rawKeyDown', modifiers: 0, key: 'Backspace', code: 'Backspace', windowsVirtualKeyCode: 8 },
      { type: 'keyUp', modifiers: 0, key: 'Backspace', code: 'Backspace', windowsVirtualKeyCode: 8 },
    ],
  );
});

test('browser.snapshot exposes Monaco roots and bounded model state to the agent', async () => {
  const { manager } = fixture();
  await manager.activate({ chatId: 'chat-snapshot-editor', bounds: { x: 0, y: 0, width: 500, height: 500 } });
  const wc = manager.entries.get('chat-snapshot-editor').view.webContents;
  wc.executeJavaScript = async (script) => {
    assert.doesNotThrow(() => new Function(`return ${script};`));
    assert.match(script, /\.monaco-editor/);
    assert.match(script, /editors: editorRoots/);
    return {
      url: 'https://example.test/editor', title: 'Editor', text: '',
      editors: [{ selector: '#editor > div', kind: 'monaco', text: 'const value = 1;', valueLength: 16, truncated: false }],
      interactive: [],
    };
  };
  const snapshot = await manager.browserControl('browser.snapshot', {});
  assert.equal(snapshot.editors[0].kind, 'monaco');
  assert.equal(snapshot.editors[0].text, 'const value = 1;');
});

test('browser.type replaces a multiline Monaco-style editor in a background tab and reports exact readback', async () => {
  const { manager, win } = fixture({ requestOpen: () => {} });
  manager.setAgentControlReady(true);
  const tab = await manager.browserControl('browser.open', { chatId: 'chat-editor', url: 'example.com' });
  const wc = manager.browserEntries()[0].view.webContents;
  assert.equal(tab.active, false);
  assert.equal(win.contentView.children.length, 0);
  const scripts = [];
  wc.executeJavaScript = async (script) => {
    scripts.push(script);
    assert.doesNotThrow(() => new Function(`return ${script};`));
    if (script.includes('workass-browser-type-prepare')) {
      return {
        found: true, editable: true, strategy: 'native', focused: true, editor: 'monaco',
        selectionVerified: false, selectionStrategy: 'keyboard-fallback',
      };
    }
    if (script.includes('workass-browser-input-barrier')) return undefined;
    if (script.includes('workass-browser-type-verify')) {
      const clearing = script.includes('const expected = "";');
      return {
        found: true, focused: true, changed: true, inputAccepted: true,
        exact: true, visibleMatch: true, verification: 'model', valueLength: clearing ? 0 : 550,
      };
    }
    return undefined;
  };
  const replacement = Array.from({ length: 40 }, (_, index) => `line ${index + 1}: value`).join('\n');
  const result = await manager.browserControl('browser.type', { tabId: tab.id, selector: '.monaco-editor textarea', text: replacement });

  assert.equal(wc.insertedTexts, undefined);
  assert.equal(wc.inputEvents, undefined);
  assert.deepEqual(wc.debugger.commandCalls.filter((call) => call.method.startsWith('Input.')), [
    {
      method: 'Input.dispatchKeyEvent',
      params: {
        type: 'rawKeyDown', modifiers: 4, key: 'a', code: 'KeyA', windowsVirtualKeyCode: 65,
        unmodifiedText: 'a', commands: ['selectAll'],
      },
      sessionId: undefined,
    },
    {
      method: 'Input.dispatchKeyEvent',
      params: {
        type: 'keyUp', modifiers: 4, key: 'a', code: 'KeyA', windowsVirtualKeyCode: 65, unmodifiedText: 'a',
      },
      sessionId: undefined,
    },
    { method: 'Input.insertText', params: { text: replacement }, sessionId: undefined },
  ]);
  assert.equal(result.strategy, 'native');
  assert.equal(result.changed, true);
  assert.equal(result.replacementVerified, true);
  assert.equal(result.verification, 'model');
  assert.equal(scripts.some((script) => script.includes('workass-browser-type-verify')), true);

  const cleared = await manager.browserControl('browser.type', { tabId: tab.id, selector: '.monaco-editor', text: '' });
  assert.equal(cleared.replacementVerified, true);
  assert.equal(cleared.valueLength, 0);
  assert.equal(scripts.some((script) => script.includes('workass-browser-delete-barrier')), true);
  assert.equal(scripts.some((script) => script.includes('workass-browser-delete-selection')), true);
  assert.deepEqual(
    wc.debugger.commandCalls.filter((call) => call.method === 'Input.dispatchKeyEvent').slice(-4).map((call) => call.params.key),
    ['a', 'a', 'Backspace', 'Backspace'],
  );
});

test('browser.type rejects a false success when a form control does not retain the replacement', async () => {
  const { manager } = fixture();
  await manager.activate({ chatId: 'chat-controlled-input', bounds: { x: 0, y: 0, width: 400, height: 400 } });
  const wc = manager.entries.get('chat-controlled-input').view.webContents;
  wc.executeJavaScript = async (script) => {
    assert.doesNotThrow(() => new Function(`return ${script};`));
    if (script.includes('workass-browser-type-prepare')) {
      return {
        found: true, editable: true, strategy: 'value', focused: true,
        changed: false, replacementVerified: false, valueLength: 3,
      };
    }
    return undefined;
  };
  await assert.rejects(
    manager.browserControl('browser.type', { selector: '#controlled', text: 'replacement' }),
    /did not retain the replacement text/,
  );
});

// Reported 2026-07-26: file:///…/chat-list.html became https://file///… and
// failed as ERR_NAME_NOT_RESOLVED, which reads as a DNS fault rather than an
// unsupported scheme.
test('unsupported schemes and local paths are refused by name, never rewritten into a host', async () => {
  for (const local of [
    'file:///Users/dev/Workspace/workass-mobile/docs/mocks/chat-list.html',
    'file:/Users/dev/mock.html',
    '/Users/dev/mock.html',
    'C:\\Users\\dev\\mock.html',
  ]) {
    const resolved = resolveBrowserURL(local);
    assert.equal(resolved.url, undefined, `${local} must not resolve to a URL`);
    assert.match(resolved.error, /does not open local files/);
    assert.match(resolved.error, /workass_host_artifact/);   // says what to do instead
    assert.throws(() => normalizeBrowserURL(local), /workass_host_artifact/);
  }
  assert.match(resolveBrowserURL('ftp://example.com/pub').error, /http and https URLs only/);
  assert.match(resolveBrowserURL('javascript:alert(1)').error, /http and https URLs only/);
  // The ordinary cases still work: a scheme guard must not eat host:port.
  assert.equal(normalizeBrowserURL('localhost:5173/app'), 'http://localhost:5173/app');
  assert.equal(normalizeBrowserURL('example.com:8080/x'), 'https://example.com:8080/x');
  assert.equal(normalizeBrowserURL('about:blank'), 'about:blank');

  // An agent asking for one gets the message; a user opening the pane still
  // gets a browser, with the reason shown in the pane.
  const { manager } = fixture();
  manager.setAgentControlReady(true);
  await assert.rejects(
    manager.browserControl('browser.open', { chatId: 'conv-file', url: 'file:///Users/dev/mock.html' }),
    /workass_host_artifact/,
  );
  const state = await manager.activate({ chatId: 'tab-file', url: 'file:///Users/dev/mock.html' });
  assert.equal(state.url, 'about:blank');
  assert.match(state.error, /does not open local files/);
});

// Reported 2026-07-26: screenshotting a tab whose own navigate result said
// {"active": false} answered "browser screenshot returned no image".
test('a screenshot of a tab that is not visible says so instead of returning no image', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  manager.setAgentControlReady(true);
  const tab = await manager.browserControl('browser.open', { chatId: 'conv-shot', url: 'example.com' });
  assert.equal(tab.active, false);
  await assert.rejects(
    manager.browserControl('browser.screenshot', { tabId: tab.id }),
    (err) => {
      assert.match(err.message, /not the visible tab/);
      assert.match(err.message, new RegExp(`browser tab ${tab.id}\\b`));
      assert.match(err.message, /conv-shot/);          // which chat to open
      return true;
    },
  );
  // The identical call succeeds once that tab is the visible one.
  await manager.activate({ chatId: 'conv-shot', bounds: { x: 0, y: 0, width: 400, height: 400 } });
  const shot = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  assert.equal(Buffer.from(shot.base64, 'base64').toString(), 'fake-png');
});

test('provider-neutral browser control drives the same visible view', async () => {
  let manager;
  const opened = [];
  ({ manager } = fixture({ requestOpen: (conversationId) => {
    opened.push(conversationId);
    void manager.activate({ chatId: 'tab-visible', conversationId, bounds: { x: 700, y: 60, width: 460, height: 680 } });
  } }));
  manager.setAgentControlReady(true);
  const tab = await manager.browserControl('browser.open', { chatId: 'conversation-visible', url: 'localhost:5173' });
  assert.equal(opened.length >= 1, true);
  assert.equal(opened[0], 'conversation-visible');
  assert.equal(tab.chatId, 'tab-visible');
  assert.equal(tab.conversationId, 'conversation-visible');
  assert.equal(tab.url, 'http://localhost:5173');
  assert.equal(manager.publicState(manager.entries.get('tab-visible')).agentControl, true);
  const snapshot = await manager.browserControl('browser.snapshot', { chatId: 'conversation-visible' });
  assert.deepEqual(snapshot, { ok: true });
  const click = await manager.browserControl('browser.click', { tabId: tab.id, selector: '#save' });
  assert.deepEqual(click, { ok: true });
  const shot = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  assert.equal(shot.mimeType, 'image/png');
  assert.equal(Buffer.from(shot.base64, 'base64').toString(), 'fake-png');
});

test('browser.open runs in the background without stealing the visible view, then the view adopts it on open', async () => {
  const opened = [];
  // Simulate the user viewing a DIFFERENT chat: the renderer marks the owning
  // chat's pane but never activates a visible view for this conversation.
  const { manager, win } = fixture({ requestOpen: (conversationId) => { opened.push(conversationId); } });
  manager.setAgentControlReady(true);

  const tab = await manager.browserControl('browser.open', { chatId: 'conv-bg', url: 'example.com' });
  assert.deepEqual(opened, ['conv-bg']);                 // renderer asked to mark the OWNING chat's pane
  assert.equal(tab.conversationId, 'conv-bg');
  assert.equal(tab.active, false);                       // background: not the visible view
  assert.match(tab.url, /^https:\/\/example\.com/);
  assert.equal(win.contentView.children.length, 0);      // never attached — no screen-steal

  // The agent drives the background browser fine.
  const snapshot = await manager.browserControl('browser.snapshot', { chatId: 'conv-bg' });
  assert.deepEqual(snapshot, { ok: true });
  assert.equal(win.contentView.children.length, 0);

  // The user finally opens that chat: the renderer activates with the UI tab id
  // and ADOPTS the background entry rather than spawning a duplicate view.
  const before = manager.entries.size;
  await manager.activate({ chatId: 'tab-7', conversationId: 'conv-bg', bounds: { x: 700, y: 60, width: 400, height: 600 } });
  assert.equal(manager.entries.size, before);            // adopted, not duplicated
  assert.equal(manager.entries.has('tab-7'), true);
  assert.equal(manager.entries.has('conv-bg'), false);
  assert.equal(manager.activeId, 'tab-7');
  assert.equal(manager.entries.get('tab-7').conversationId, 'conv-bg');
  assert.equal(win.contentView.children.length, 1);      // now visible
});

test('agent browser control is owner-scoped and never falls back to another chat', async () => {
  const { manager } = fixture();
  manager.setAgentControlReady(true);

  await manager.activate({
    chatId: 'tab-visible',
    conversationId: 'conv-visible',
    bounds: { x: 700, y: 60, width: 460, height: 680 },
  });
  const visibleTabId = manager.entries.get('tab-visible').view.webContents.id;
  const owned = await manager.browserControl('browser.open', {
    chatId: 'conv-owned',
    url: 'example.com',
  });

  await assert.rejects(
    manager.browserControl('browser.snapshot', { chatId: 'conv-without-browser' }),
    /no Workass browser tab belongs to chat/i,
  );
  await assert.rejects(
    manager.browserControl('browser.snapshot', { chatId: 'conv-owned', tabId: visibleTabId }),
    /belongs to another chat/i,
  );
  assert.deepEqual(
    await manager.browserControl('browser.snapshot', { chatId: 'conv-owned' }),
    { ok: true },
  );

  const scoped = await manager.browserControl('browser.list', { chatId: 'conv-owned' });
  assert.equal(scoped.tabs.length, 1);
  assert.equal(scoped.tabs[0].id, owned.id);
  assert.equal(scoped.tabs[0].conversationId, 'conv-owned');
});

test('CDP adapter forwards root and child-target commands and events', async () => {
  const { manager } = fixture();
  const tab = await manager.activate({ chatId: 'chat-cdp', bounds: { x: 700, y: 60, width: 460, height: 680 } });
  const events = [];
  manager.onCDP((event) => events.push(event));
  const entry = manager.entries.get('chat-cdp');
  await manager.attachTarget(entry.view.webContents.id, 'child-target');
  await manager.executeCDP({ tabId: entry.view.webContents.id, targetId: 'child-target' }, 'Runtime.evaluate', { expression: '1+1' });
  assert.equal(entry.view.webContents.debugger.lastCommand.sessionId, 'child-session-1');
  entry.view.webContents.debugger.emit('message', {}, 'Page.loadEventFired', { value: 1 }, 'child-session-1');
  assert.deepEqual(events[0], {
    tabId: entry.view.webContents.id, method: 'Page.loadEventFired', params: { value: 1 }, sessionId: 'child-session-1',
  });
  assert.equal(tab.cdpAttached, true);
});
