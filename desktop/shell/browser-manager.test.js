'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const { BrowserManager, cleanUserAgent, normalizeBrowserURL, parseBrowserKey, resolveBrowserURL, safeBounds } = require('./browser-manager');

class FakeDebugger extends EventEmitter {
  constructor() { super(); this.attached = false; this.commands = []; this.commandCalls = []; }
  isAttached() { return this.attached; }
  attach(version) { this.owner.startupSequence.push('debugger.attach'); this.attached = true; this.version = version; }
  async sendCommand(method, params, sessionId) {
    this.owner.startupSequence.push(`cdp:${method}`);
    this.commands.push(method);
    this.commandCalls.push({ method, params, sessionId });
    this.lastCommand = { method, params, sessionId };
    if (method === 'Target.getTargetInfo') return { targetInfo: { targetId: `target-${this.owner.id}` } };
    if (method === 'Page.getFrameTree') {
      if (sessionId && this.owner.childFrameTree) return this.owner.childFrameTree;
      return this.owner.frameTree || { frameTree: { frame: { id: `frame-${this.owner.id}`, loaderId: 'loader-root', url: this.owner.url, securityOrigin: 'http://fixture.invalid' }, childFrames: [] } };
    }
    if (method === 'Accessibility.getFullAXTree') return { nodes: [] };
    if (method === 'Page.enable' || method === 'Runtime.enable' || method === 'DOM.enable' || method === 'Accessibility.enable' || method === 'Target.setAutoAttach' || method === 'Target.detachFromTarget') return {};
    if (method === 'Emulation.setDeviceMetricsOverride') {
      this.owner.metrics = { ...this.owner.metrics, width: params.width, height: params.height, deviceScaleFactor: params.deviceScaleFactor };
      return {};
    }
    if (method === 'Page.getLayoutMetrics') return { cssContentSize: { width: this.owner.metrics.width, height: Math.max(this.owner.metrics.height, this.owner.fullPageHeight) } };
    if (method === 'Page.captureScreenshot') {
      const host = this.owner.view?.parentWindow;
      this.owner.captureSurfaceAtScreenshot = {
        hostContentSize: host?.getContentSize?.() || null,
        viewBounds: this.owner.view?.getBounds?.() || null,
      };
      let width = this.owner.metrics.width; let height = this.owner.metrics.height;
      if (params.clip) { width = Math.ceil(params.clip.width); height = Math.ceil(params.clip.height); }
      else if (params.captureBeyondViewport) height = Math.max(height, 1800);
      const png = Buffer.alloc(25);
      Buffer.from('89504e470d0a1a0a', 'hex').copy(png);
      png.writeUInt32BE(width, 16); png.writeUInt32BE(height, 20);
      png[24] = this.owner.view?.attached ? 1 : 0;
      return { data: png.toString('base64') };
    }
    return {};
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
    this.startupSequence = [];
    this.session = profile;
    this.debugger = new FakeDebugger();
    this.debugger.owner = this;
    this.navigationHistory = new FakeHistory();
    this.url = 'about:blank';
    this.metrics = { width: 1440, height: 900, deviceScaleFactor: 1, scrollX: 0, scrollY: 0, documentWidth: 1440, documentHeight: 900 };
    this.fullPageHeight = FakeWebContents.fullPageHeight || 1800;
  }
  setUserAgent(value) { this.userAgent = value; }
  getURL() { return this.url; }
  async loadURL(url) { this.startupSequence.push(`load:${url}`); this.emit('did-start-navigation', {}, url, false, true); this.url = url; this.emit('did-navigate', {}, url); this.emit('did-finish-load'); this.emit('did-stop-loading'); }
  reload() { this.reloaded = true; }
  stop() { this.stopped = true; }
  close() { this.closed = true; }
  setWindowOpenHandler(handler) { this.windowOpenHandler = handler; }
  async executeJavaScript(script) {
    this.lastScript = script;
    this.startupSequence.push('executeJavaScript');
    if (script.includes('location.href, readyState: document.readyState')) return { url: this.url, readyState: 'complete' };
    if (script.includes('({ width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio')) return { ...this.metrics };
    if (script.includes('({ x: scrollX, y: scrollY, width: innerWidth, height: innerHeight })')) return { x: this.metrics.scrollX, y: this.metrics.scrollY, width: this.metrics.width, height: this.metrics.height };
    if (script.includes('querySelectorAll(') && script.endsWith('.length')) return 1;
    if (script.includes('const composedContains') || script.includes('const el = (() => {')) return { status: 'ready', x: 10, y: 20, tag: 'button', name: 'Save' };
    if (script.includes('const target=deep(document,x,y)')) return { status: 'ready', tag: 'button', name: 'Coordinate target' };
    if (script.includes('const condition=')) return { done: true, invalid: false, state: { readyState: 'complete', loading: false, url: this.url, width: this.metrics.width, height: this.metrics.height, scroll: { x: this.metrics.scrollX, y: this.metrics.scrollY } } };
    if (script.includes('const snapshotId =') && script.includes('const maxNodes =')) {
      const snapshotId = JSON.parse(script.match(/const snapshotId = (.+);/)?.[1] || '"snap"');
      return { snapshotId, url: this.url, title: 'Fixture', text: '', textLength: 0, textTruncated: false, semantic: [], nodeCount: 0, semanticTruncated: false, refs: [], frames: [], editors: [], interactive: [], scroll: { ...this.metrics } };
    }
    return { ok: true };
  }
  async insertText(text) { this.insertedTexts = [...(this.insertedTexts || []), text]; }
  // Electron resolves capturePage() with an EMPTY image for a view that is not
  // attached to the window, rather than rejecting. The fake has to model that
  // or it cannot guard the bug.
  async capturePage() {
    if (this.view && this.view.attached) return { isEmpty: () => false, toPNG: () => Buffer.from('fake-png') };
    return { isEmpty: () => true, toPNG: () => Buffer.alloc(0) };
  }
  sendInputEvent(event) { this.inputEvents = [...(this.inputEvents || []), event]; }
  setZoomFactor(value) { this.zoomFactor = value; }
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
  getBounds() { return this.bounds; }
  setBackgroundColor(color) { this.backgroundColor = color; }
}

class FakeBrowserWindow extends EventEmitter {
  constructor(options) {
    super();
    this.options = options;
    this.destroyed = false;
    this.bounds = { width: options.width, height: options.height };
    this.contentView = {
      children: [],
      addChildView: (view) => { view.attached = true; view.parentWindow = this; this.contentView.children.push(view); },
      removeChildView: (view) => { view.attached = false; if (view.parentWindow === this) view.parentWindow = null; this.contentView.children = this.contentView.children.filter((item) => item !== view); },
    };
  }
  async loadURL(url) { this.url = url; }
  setBounds(bounds) { this.bounds = bounds; }
  getContentSize() { return [this.bounds.width, this.bounds.height]; }
  setContentSize(width, height) { this.bounds = { width, height }; }
  isDestroyed() { return this.destroyed; }
  isVisible() { return false; }
  isMinimized() { return false; }
  destroy() { this.destroyed = true; this.emit('closed'); }
}

function fixture(overrides = {}) {
  const profile = {
    isPersistent: () => true,
    setUserAgent(value) { this.userAgent = value; },
    setPermissionCheckHandler(handler) { this.check = handler; },
    setPermissionRequestHandler(handler) { this.request = handler; },
  };
  FakeView.profile = profile;
  const win = { getContentSize: () => [1200, 800] };
  const nativeDpr = Number(overrides.nativeDpr) || 1;
  const shellZoom = Number(overrides.shellZoom) || 1;
  FakeWebContents.fullPageHeight = Number(overrides.fullPageHeight) || 1800;
  win.webContents = {
    getZoomFactor: () => shellZoom,
    executeJavaScript: async () => nativeDpr * shellZoom,
  };
  const contentView = {
    children: [],
    addChildView(view) { view.attached = true; view.parentWindow = win; this.children.push(view); },
    removeChildView(view) { view.attached = false; if (view.parentWindow === win) view.parentWindow = null; this.children = this.children.filter((item) => item !== view); },
  };
  win.contentView = contentView;
  const sessions = [];
  const session = { fromPartition(partition, options) { sessions.push({ partition, options }); return profile; } };
  const states = [];
  const manager = new BrowserManager({
    win, WebContentsView: FakeView, BrowserWindow: overrides.BrowserWindow || FakeBrowserWindow, nativeImage: overrides.nativeImage,
    session, chromeVersion: '140.0.7339.1', platform: 'darwin',
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
  const startup = entry.view.webContents.startupSequence;
  assert.ok(startup.indexOf('load:about:blank') < startup.indexOf('debugger.attach'));
  assert.ok(startup.indexOf('debugger.attach') < startup.indexOf('cdp:Emulation.setDeviceMetricsOverride'));
  assert.ok(startup.indexOf('load:about:blank') < startup.indexOf('cdp:Emulation.setDeviceMetricsOverride'));
  assert.equal(entry.captureHostAttached, false, 'activation reparents the initialized page out of its hidden host');
  const startupMetrics = entry.view.webContents.debugger.commands;
  const runtimeEnableIndex = startupMetrics.indexOf('Runtime.enable');
  assert.equal(startupMetrics[0], 'Target.getTargetInfo');
  assert.ok(['Page.enable', 'Runtime.enable', 'DOM.enable', 'Accessibility.enable', 'Page.getFrameTree', 'Target.setAutoAttach'].every((method) => startupMetrics.includes(method)));
  assert.ok(startupMetrics.indexOf('Target.setAutoAttach') < startupMetrics.indexOf('Emulation.setDeviceMetricsOverride'));
  assert.deepEqual(first.viewport, { width: 1440, height: 900, deviceScaleFactor: 1 });
  assert.equal(first.effectiveViewport.width, 1440);
  assert.equal(first.effectiveViewport.height, 900);
  assert.equal(profile.check(), false);
  assert.equal(win.contentView.children.length, 1);

  await manager.activate({ chatId: 'chat-b', bounds: { x: 910, y: 90, width: 500, height: 900 } });
  assert.equal(win.contentView.children.length, 1);
  assert.equal(manager.activeId, 'chat-b');
  assert.deepEqual(manager.entries.get('chat-b').bounds, { x: 910, y: 90, width: 290, height: 710 });
  assert.equal(await manager.hide('chat-b'), true);
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

test('closing a view during hidden-host initialization cannot recreate or publish it as usable', async () => {
  let releaseHostLoad;
  let markHostLoadStarted;
  const hostLoadGate = new Promise((resolve) => { releaseHostLoad = resolve; });
  const hostLoadStarted = new Promise((resolve) => { markHostLoadStarted = resolve; });
  const hosts = [];
  class DelayedCaptureHost extends FakeBrowserWindow {
    constructor(options) { super(options); hosts.push(this); }
    async loadURL(url) {
      if (this.options.show === false) {
        markHostLoadStarted();
        await hostLoadGate;
      }
      return super.loadURL(url);
    }
  }
  const { manager, states } = fixture({ BrowserWindow: DelayedCaptureHost });
  const entry = manager.openConversation({ chatId: 'closing-during-init', visible: false });
  await hostLoadStarted;
  const host = entry.captureHost;
  assert.ok(host && !host.destroyed);
  manager.close('closing-during-init');
  assert.equal(host.destroyed, true, 'close destroys the private host that is still initializing');
  releaseHostLoad();
  assert.equal(await entry.ready, false);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(entry.captureHost, null, 'late initialization completion must not reinstall the closed host');
  assert.equal(manager.entries.has('closing-during-init'), false);
  assert.equal(hosts.length, 1, 'late initialization must not create another capture host');
  assert.equal(states.some((state) => state.chatId === 'closing-during-init' && state.cdpAttached), false);
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
    if (script.includes('workass-browser-focused-frame-state')) return { hasFocus: true, activeFrame: false };
    if (script.includes('workass-browser-key-text-length')) return { found: true, length: 5 };
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

test('focused-frame key targeting follows the active iframe owner chain', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  await manager.browserControl('browser.open', { chatId: 'focused-frame-chain', visible: false });
  const entry = manager.entries.get('focused-frame-chain');
  const root = manager.frameForSession(entry, null);
  const activeChild = {
    id: 'frame-focused-child', parentFrameId: root.id, targetId: root.targetId,
    sessionId: null, contextId: 201, generation: 1, detached: false,
  };
  const unrelatedChild = {
    id: 'frame-unrelated-child', parentFrameId: root.id, targetId: root.targetId,
    sessionId: null, contextId: 202, generation: 2, detached: false,
  };
  entry.frames.set(activeChild.id, activeChild);
  entry.frames.set(unrelatedChild.id, unrelatedChild);
  const evaluated = [];
  manager.evaluateFrame = async (_entry, frameId, script) => {
    evaluated.push([frameId, script]);
    if (script.includes('workass-browser-focused-frame-state')) {
      return frameId === root.id ? { hasFocus: true, activeFrame: true } : { hasFocus: false, activeFrame: false };
    }
    if (script === 'document.hasFocus()') return frameId === activeChild.id;
    throw new Error('unexpected focus probe');
  };
  const debug = entry.view.webContents.debugger;
  const originalSendCommand = debug.sendCommand.bind(debug);
  debug.sendCommand = async (method, params, sessionId) => {
    if (method === 'DOM.getFrameOwner') return { backendNodeId: params.frameId === activeChild.id ? 301 : 302 };
    if (method === 'DOM.resolveNode') return { object: { objectId: params.backendNodeId === 301 ? 'owner-active' : 'owner-unrelated' } };
    if (method === 'Runtime.callFunctionOn') return { result: { value: params.objectId === 'owner-active' } };
    if (method === 'Runtime.releaseObject') return {};
    return originalSendCommand(method, params, sessionId);
  };

  const focused = await manager.focusedFrame(entry);
  assert.equal(focused.id, activeChild.id);
  assert.ok(evaluated.some(([frameId, script]) => frameId === activeChild.id && script === 'document.hasFocus()'));
  assert.equal(evaluated.some(([frameId]) => frameId === unrelatedChild.id), false,
    'a stale activeElement in an unrelated owned child is never treated as focused');
  manager.destroy();
});

test('browser.key verifies native deletion in the resolved frame after CDP consumes the selection', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'native-frame-key-verify', visible: false });
  const entry = manager.entries.get('native-frame-key-verify');
  const child = { id: 'frame-key-target', parentFrameId: 'root', targetId: 'child', generation: 1, detached: false };
  const textReads = [];
  manager.focusedFrame = async () => child;
  manager.focusedTextLength = async (_entry, frameId) => {
    textReads.push(frameId);
    return { found: true, length: textReads.length === 1 ? 12 : 0 };
  };
  manager.dispatchBrowserKey = async (_entry, parsed) => assert.equal(parsed.keyCode, 'Backspace');
  manager.evaluateFrame = async (_entry, frameId, script) => {
    assert.equal(frameId, child.id);
    assert.match(script, /workass-browser-key-barrier/);
    return undefined;
  };
  manager.deleteSelectedBrowserText = async (_entry, frameId) => {
    assert.equal(frameId, child.id);
    return { found: false, deletionVerified: false, strategy: 'keyboard-only' };
  };

  const result = await manager.browserControl('browser.key', { tabId: tab.id, key: 'Backspace' });
  assert.equal(result.deletionVerified, true);
  assert.equal(result.deletionStrategy, 'native-key-event');
  assert.deepEqual(textReads, [child.id, child.id]);
  manager.destroy();
});

test('browser.snapshot exposes Monaco roots and bounded model state to the agent', async () => {
  const { manager } = fixture();
  await manager.activate({ chatId: 'chat-snapshot-editor', bounds: { x: 0, y: 0, width: 500, height: 500 } });
  const wc = manager.entries.get('chat-snapshot-editor').view.webContents;
  wc.executeJavaScript = async (script) => {
    assert.doesNotThrow(() => new Function(`return ${script};`));
    assert.match(script, /\.monaco-editor/);
    assert.match(script, /const editorRoots/);
    const snapshotId = JSON.parse(script.match(/const snapshotId = (.+);/)?.[1] || '"snap"');
    return {
      snapshotId, url: 'https://example.test/editor', title: 'Editor', text: '', textLength: 0,
      editors: [{ selector: '#editor > div', kind: 'monaco', text: 'const value = 1;', valueLength: 16, truncated: false }],
      interactive: [], semantic: [], refs: [], frames: [], nodeCount: 0, semanticTruncated: false,
      scroll: { x: 0, y: 0, width: 1440, height: 900, deviceScaleFactor: 1 },
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
    if (script.endsWith('.length')) return 1;
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
    if (script.endsWith('.length')) return 1;
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
test('background screenshot keeps the desktop viewport and returns mapping metadata', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  manager.setAgentControlReady(true);
  const tab = await manager.browserControl('browser.open', { chatId: 'conv-shot', url: 'example.com' });
  assert.equal(tab.active, false);
  const shot = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  assert.deepEqual(shot.metadata.image, { width: 1440, height: 900 });
  assert.equal(shot.metadata.mode, 'viewport');
  assert.equal(shot.metadata.tab.id, tab.id);
  assert.equal(shot.metadata.viewport.width, 1440);
  assert.equal(shot.metadata.pixel_to_css.x, 1);
});

test('blank detached capture lazily reparents the same page to a non-focusing host and cleans it up', async () => {
  const nativeImage = {
    createFromBuffer(png) {
      const painted = png[24] === 1;
      return {
        isEmpty: () => false,
        toBitmap: () => {
          const bitmap = Buffer.alloc(64);
          if (painted) for (let offset = 0; offset < bitmap.length; offset += 4) {
            bitmap[offset] = offset;
            bitmap[offset + 1] = 255 - offset;
            bitmap[offset + 2] = offset ^ 0x5a;
            bitmap[offset + 3] = 255;
          }
          return bitmap;
        },
      };
    },
  };
  const { manager } = fixture({ requestOpen: () => {}, BrowserWindow: FakeBrowserWindow, nativeImage });
  const tab = await manager.browserControl('browser.open', { chatId: 'host-fallback', url: 'example.com', visible: false });
  const entry = manager.entries.get('host-fallback');
  const webContentsId = entry.view.webContents.id;
  // Model a host that was closed after startup: a later detached capture must
  // recreate/reparent the same owned WebContentsView instead of a clone.
  entry.captureHost.contentView.removeChildView(entry.view);
  entry.captureHostAttached = false;
  const originalSendCommand = entry.view.webContents.debugger.sendCommand;
  let captureRequests = 0;
  entry.view.webContents.debugger.sendCommand = function (method, ...args) {
    if (method === 'Page.captureScreenshot' && captureRequests++ === 0) return Promise.reject(new Error('injected first capture request failure'));
    return originalSendCommand.call(this, method, ...args);
  };
  let shot;
  try { shot = await manager.browserControl('browser.screenshot', { tabId: tab.id }); }
  finally { entry.view.webContents.debugger.sendCommand = originalSendCommand; }
  assert.equal(captureRequests, 2, 'the failed first request must trigger same-page hidden-host retry');
  assert.equal(shot.metadata.capture_surface, 'hidden_host');
  assert.equal(entry.view.webContents.id, webContentsId);
  assert.equal(entry.captureHost.options.show, false);
  assert.equal(entry.captureHost.options.focusable, false);
  assert.equal(entry.captureHost.options.webPreferences.partition, 'persist:workass-browser');
  assert.equal(entry.captureHostAttached, false, 'a temporary fallback restores the page to its originally detached state');
  assert.equal(entry.visible, false);
  assert.equal(entry.captureHost.contentView.children.length, 0);
  assert.equal(manager.close('host-fallback'), true);
  assert.equal(entry.captureHost, null);
});

test('viewport changes are per-tab, explicit, and independent from fitted panel presentation', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const first = await manager.browserControl('browser.open', { chatId: 'viewport-a', visible: false });
  const second = await manager.browserControl('browser.open', { chatId: 'viewport-b', visible: false });
  const firstEntry = manager.entries.get('viewport-a');
  const secondEntry = manager.entries.get('viewport-b');
  const navURL = firstEntry.view.webContents.getURL();

  const changed = await manager.browserControl('browser.setViewport', { tabId: first.id, width: 1280, height: 800 });
  assert.deepEqual(changed.viewport, { width: 1280, height: 800, deviceScaleFactor: 1 });
  assert.equal(changed.effectiveViewport.width, 1280);
  assert.equal(changed.viewportGeneration, 2);
  assert.equal(changed.visible, false);
  assert.deepEqual(secondEntry.viewport, { width: 1440, height: 900, deviceScaleFactor: 1 });
  const emulationCount = firstEntry.view.webContents.debugger.commands.filter((method) => method === 'Emulation.setDeviceMetricsOverride').length;
  await assert.rejects(manager.browserControl('browser.setViewport', { tabId: first.id, width: 319, height: 800 }), /width must be an integer/);
  assert.equal(firstEntry.view.webContents.debugger.commands.filter((method) => method === 'Emulation.setDeviceMetricsOverride').length, emulationCount);

  await manager.activate({ chatId: 'viewport-a', bounds: { x: 0, y: 0, width: 312, height: 600 } });
  assert.equal(firstEntry.presentationScale, 312 / 1280);
  assert.equal(firstEntry.view.bounds.width, 312);
  assert.equal(firstEntry.view.webContents.zoomFactor, undefined, 'presentation fitting must not set origin-wide page zoom');
  assert.deepEqual(firstEntry.effectiveViewport && { width: firstEntry.effectiveViewport.width, height: firstEntry.effectiveViewport.height, deviceScaleFactor: firstEntry.effectiveViewport.deviceScaleFactor }, { width: 1280, height: 800, deviceScaleFactor: 1 });
  assert.deepEqual(firstEntry.viewport, { width: 1280, height: 800, deviceScaleFactor: 1 });
  assert.equal(firstEntry.view.webContents.getURL(), navURL);
  const reset = await manager.browserControl('browser.resetViewport', { tabId: first.id });
  assert.deepEqual(reset.viewport, { width: 1440, height: 900, deviceScaleFactor: 1 });
  assert.equal(reset.viewportGeneration, 3);
});

test('full-page and clip screenshots return exact dimensions and preserve screenshot origins', async () => {
  const { manager } = fixture({ requestOpen: () => {}, nativeDpr: 2, fullPageHeight: 1801 });
  const tab = await manager.browserControl('browser.open', { chatId: 'capture-modes', visible: false });
  const entry = manager.entries.get('capture-modes');
  entry.view.webContents.metrics.scrollY = 35;
  const originalHostSize = entry.captureHost.getContentSize();
  const originalViewBounds = entry.view.getBounds();
  const full = await manager.browserControl('browser.screenshot', { tabId: tab.id, mode: 'full_page' });
  assert.deepEqual(full.metadata.image, { width: 1440, height: 1802 });
  assert.deepEqual(entry.view.webContents.captureSurfaceAtScreenshot.hostContentSize, [1440, 1802], 'the hidden host must align to the Retina native scale before the full-page request');
  assert.deepEqual(entry.view.webContents.captureSurfaceAtScreenshot.viewBounds, { x: 0, y: 0, width: 1440, height: 1802 });
  assert.deepEqual(entry.captureHost.getContentSize(), originalHostSize, 'the hidden host extent must be restored');
  assert.deepEqual(entry.view.getBounds(), originalViewBounds, 'the same native view bounds must be restored');
  assert.deepEqual(full.metadata.css_capture_rect, { x: 0, y: 0, width: 1440, height: 1801 });
  assert.equal(full.metadata.pixel_to_css.y, 1801 / 1802, 'pixel-to-CSS metadata must preserve the exact fractional raster mapping');
  assert.deepEqual(full.metadata.scroll_origin, { x: 0, y: 0 });
  assert.equal(entry.view.webContents.metrics.scrollY, 35);
  const originalSendCommand = entry.view.webContents.debugger.sendCommand;
  entry.view.webContents.debugger.sendCommand = function (method, ...args) {
    if (method === 'Page.captureScreenshot') return Promise.reject(new Error('injected full-page capture failure'));
    return originalSendCommand.call(this, method, ...args);
  };
  try {
    await assert.rejects(manager.browserControl('browser.screenshot', { tabId: tab.id, mode: 'full_page' }), /expanded hidden-host full-page capture failed: injected full-page capture failure/);
  } finally {
    entry.view.webContents.debugger.sendCommand = originalSendCommand;
  }
  assert.deepEqual(entry.captureHost.getContentSize(), originalHostSize, 'a thrown full-page capture must restore host extent');
  assert.deepEqual(entry.view.getBounds(), originalViewBounds, 'a thrown full-page capture must restore view bounds');
  assert.equal(entry.captureHostAttached, true);
  assert.equal(entry.view.webContents.metrics.scrollY, 35);
  const clip = await manager.browserControl('browser.screenshot', { tabId: tab.id, mode: 'clip', clip: { x: 25, y: 40, width: 300, height: 200 } });
  assert.deepEqual(clip.metadata.image, { width: 300, height: 200 });
  assert.deepEqual(clip.metadata.css_capture_rect, { x: 25, y: 40, width: 300, height: 200 });
  assert.equal(clip.metadata.pixel_to_css.x, 1);
  await assert.rejects(manager.browserControl('browser.screenshot', { tabId: tab.id, mode: 'viewport', clip: { x: 0, y: 0, width: 10, height: 10 } }), /only accepted with mode=clip/);
});

test('full-page host raster allocation is bounded after applying host DPR', async () => {
  const { manager } = fixture({ requestOpen: () => {}, nativeDpr: 2, fullPageHeight: 2160 });
  const tab = await manager.browserControl('browser.open', { chatId: 'retina-surface-bound', visible: false });
  const entry = manager.entries.get('retina-surface-bound');
  entry.viewport = { width: 3840, height: 2160, deviceScaleFactor: 1 };
  entry.view.webContents.metrics = {
    width: 3840, height: 2160, deviceScaleFactor: 1, scrollX: 0, scrollY: 0,
    documentWidth: 3840, documentHeight: 2160,
  };
  let captures = 0;
  const originalSendCommand = entry.view.webContents.debugger.sendCommand.bind(entry.view.webContents.debugger);
  entry.view.webContents.debugger.sendCommand = (method, params, sessionId) => {
    if (method === 'Page.captureScreenshot') captures += 1;
    return originalSendCommand(method, params, sessionId);
  };

  await assert.rejects(
    manager.browserControl('browser.screenshot', { tabId: tab.id, mode: 'full_page' }),
    /native surface raster exceeds the 16000000-pixel limit.*7680x4320 native pixels at host scale 2/,
  );
  assert.equal(captures, 0, 'the native host surface is rejected before Chromium allocates it');
});

test('a pane switch waits for an in-flight capture instead of being undone by capture restoration', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const first = await manager.activate({ chatId: 'capture-owner-a', bounds: { x: 0, y: 0, width: 312, height: 195 } });
  const entry = manager.entries.get('capture-owner-a');
  let releaseCapture;
  let markCaptureStarted;
  const captureStarted = new Promise((resolve) => { markCaptureStarted = resolve; });
  const originalSendCommand = entry.view.webContents.debugger.sendCommand.bind(entry.view.webContents.debugger);
  entry.view.webContents.debugger.sendCommand = (method, params, sessionId) => {
    if (method === 'Page.captureScreenshot') {
      markCaptureStarted();
      return new Promise((resolve, reject) => {
        releaseCapture = () => originalSendCommand(method, params, sessionId).then(resolve, reject);
      });
    }
    return originalSendCommand(method, params, sessionId);
  };

  const capture = manager.browserControl('browser.screenshot', { tabId: first.id });
  try {
    await captureStarted;
    let switchFinished = false;
    const switching = manager.activate({ chatId: 'capture-owner-b', bounds: { x: 0, y: 0, width: 312, height: 195 } })
      .then((value) => { switchFinished = true; return value; });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(manager.activeId, 'capture-owner-a');
    assert.equal(switchFinished, false, 'presentation changes wait while capture owns the shared native view');
    releaseCapture();
    await capture;
    const second = await switching;
    assert.equal(manager.activeId, 'capture-owner-b');
    assert.equal(manager.attachedView, manager.entries.get('capture-owner-b').view);
    assert.equal(second.visible, true);
  } finally {
    entry.view.webContents.debugger.sendCommand = originalSendCommand;
    releaseCapture?.();
  }
});

test('snapshot element refs dispatch trusted pointer input and fail stale after navigation', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'ref-click', visible: false });
  const entry = manager.entries.get('ref-click');
  const wc = entry.view.webContents;
  wc.executeJavaScript = async (script) => {
    if (script.includes('({ width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio')) return { ...wc.metrics };
    if (script.includes('({ x: scrollX, y: scrollY, width: innerWidth, height: innerHeight })')) return { x: wc.metrics.scrollX, y: wc.metrics.scrollY, width: wc.metrics.width, height: wc.metrics.height };
    if (script.includes('const snapshotId =') && script.includes('const maxNodes =')) {
      const snapshotId = JSON.parse(script.match(/const snapshotId = (.+);/)?.[1] || '"snap"');
      const node = { refIndex: 0, selector: '#save', role: 'button', name: 'Save', tag: 'button', bounds: { x: 10, y: 20, width: 80, height: 24 } };
      return { snapshotId, url: wc.getURL(), title: 'Fixture', text: 'Save', textLength: 4, textTruncated: false, semantic: [node], nodeCount: 1, semanticTruncated: false, refs: [{ path: [{ kind: 'child', index: 0 }], nodeKey: `${snapshotId}:0`, selector: '#save' }], frames: [], editors: [], interactive: [{ ...node }], scroll: { x: 0, y: 0, width: 1440, height: 900, deviceScaleFactor: 1 } };
    }
    if (script.includes('const composedContains')) return { status: 'ready', x: 50, y: 32, tag: 'button', name: 'Save' };
    return undefined;
  };
  const snapshot = await manager.browserControl('browser.snapshot', { tabId: tab.id });
  assert.equal(snapshot.semantic[0].element_ref, snapshot.interactive[0].element_ref);
  const clicked = await manager.browserControl('browser.click', { tabId: tab.id, elementRef: snapshot.semantic[0].element_ref, snapshotId: snapshot.snapshot_id });
  assert.equal(clicked.ok, true);
  assert.equal(clicked.trusted, true);
  assert.deepEqual(wc.debugger.commands.filter((method) => method === 'Input.dispatchMouseEvent'), ['Input.dispatchMouseEvent', 'Input.dispatchMouseEvent', 'Input.dispatchMouseEvent']);
  wc.emit('did-start-navigation', {}, 'https://example.test/next', false, true);
  const stale = await manager.browserControl('browser.click', { tabId: tab.id, elementRef: snapshot.semantic[0].element_ref, snapshotId: snapshot.snapshot_id });
  assert.equal(stale.status, 'stale');
});

test('DOM-only child-frame target geometry is mapped through its transformed owner exactly once', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'dom-frame-geometry', visible: false });
  const entry = manager.entries.get('dom-frame-geometry');
  const root = manager.frameForSession(entry, null);
  const child = {
    id: 'frame-dom-target', parentFrameId: root.id, loaderId: 'loader-child', targetId: root.targetId,
    sessionId: null, contextId: 303, generation: 4, access: 'same_target_context', detached: false,
  };
  entry.frames.set(child.id, child);
  manager.observationTarget = async () => ({ frameId: child.id, selector: '#transformed-action', path: [{ kind: 'child', index: 0 }] });
  const evaluated = [];
  manager.evaluateFrame = async (_entry, frameId, script) => {
    evaluated.push([frameId, script]);
    if (script === '({ width: innerWidth, height: innerHeight })') return { width: 100, height: 100 };
    assert.doesNotMatch(script, /const mapPoint =/, 'the child helper returns child-local geometry for manager mapping');
    return { status: 'ready', x: 25, y: 30, tag: 'button', name: 'Transformed action' };
  };
  manager.readEffectiveViewport = async () => ({ width: 400, height: 300, scrollX: 0, scrollY: 0 });
  const input = [];
  manager.executeCDP = async (target, method, params) => input.push({ target, method, params });
  const debug = entry.view.webContents.debugger;
  const originalSendCommand = debug.sendCommand.bind(debug);
  let contentQuadReads = 0;
  debug.sendCommand = async (method, params, sessionId) => {
    if (method === 'DOM.getFrameOwner') return { backendNodeId: 404 };
    if (method === 'DOM.getContentQuads') {
      contentQuadReads += 1;
      return { quads: [[10, 20, 210, 20, 210, 220, 10, 220]] };
    }
    if (method === 'DOM.resolveNode') return { object: { objectId: 'frame-owner' } };
    if (method === 'Runtime.callFunctionOn') return { result: { value: true } };
    if (method === 'Runtime.releaseObject') return {};
    return originalSendCommand(method, params, sessionId);
  };

  const clicked = await manager.browserControl('browser.click', { tabId: tab.id, selector: '#transformed-action' });
  assert.equal(clicked.ok, true);
  assert.deepEqual(clicked.point, { x: 60, y: 80 }, 'the 2x transformed iframe content quad maps local (25,30) to root (60,80)');
  assert.equal(contentQuadReads, 1, 'child-local geometry crosses the frame boundary exactly once');
  assert.deepEqual(input.map((call) => [call.method, call.params.x, call.params.y]), [
    ['Input.dispatchMouseEvent', 60, 80],
    ['Input.dispatchMouseEvent', 60, 80],
    ['Input.dispatchMouseEvent', 60, 80],
  ]);
  assert.ok(evaluated.some(([, script]) => !/const mapPoint =/.test(script)));
  manager.destroy();
});

test('an explicit missing frame stays stale and cannot fall back into the root document', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'explicit-frame-stale', visible: false });
  const entry = manager.entries.get('explicit-frame-stale');
  const wc = entry.view.webContents;
  const executed = [];
  wc.executeJavaScript = async (script) => { executed.push(script); return 'root-result'; };

  await assert.rejects(manager.evaluateFrame(entry, 'unknown-child-frame', 'globalThis.__shouldNotRun'), /stale or no longer owned/);
  assert.deepEqual(executed, []);
  entry.frames.clear();
  assert.equal(await manager.evaluateFrame(entry, null, 'root startup fallback'), 'root-result');
  assert.deepEqual(executed, ['root startup fallback'], 'the no-registry root startup path remains available');
  executed.length = 0;

  const rootId = `frame-${wc.id}`;
  entry.frames.set('frame-racing-child', {
    id: 'frame-racing-child', parentFrameId: rootId, targetId: entry.rootTargetId,
    sessionId: null, contextId: 404, generation: 1, detached: false,
  });
  manager.observationTarget = async () => {
    const target = { frameId: 'frame-racing-child', selector: '#disappearing' };
    await Promise.resolve();
    entry.frames.delete(target.frameId);
    return target;
  };
  const result = await manager.browserControl('browser.click', { tabId: tab.id, selector: '#disappearing' });
  assert.equal(result.status, 'stale');
  assert.deepEqual(executed, [], 'removal after target resolution cannot run the target script in the root realm');
  manager.destroy();
});

test('wait, diagnostics, and batch observation stay bounded and report actual results', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'observe-actions', visible: false });
  const entry = manager.entries.get('observe-actions');
  const waited = await manager.browserControl('browser.wait', { tabId: tab.id, condition: 'load', timeoutMs: 500 });
  assert.equal(waited.success, true);
  await assert.rejects(manager.browserControl('browser.wait', { tabId: tab.id, condition: 'load', timeoutMs: 15001 }), /timeout_ms/);
  entry.view.webContents.debugger.emit('message', {}, 'Runtime.consoleAPICalled', { type: 'warning', args: [{ type: 'string', value: 'token=private-value' }] });
  const diagnostics = await manager.browserControl('browser.diagnostics', { tabId: tab.id, limit: 100 });
  assert.equal(diagnostics.entries[0].kind, 'console.warning');
  assert.doesNotMatch(diagnostics.entries[0].message, /private-value/);

  const batch = await manager.browserControl('browser.batch', { tabId: tab.id, actions: [{ action: 'snapshot' }, { action: 'click', selector: '#save' }], observeAfter: true });
  assert.deepEqual(batch.completed_indexes, [0, 1]);
  assert.equal(batch.failed_index, null);
  assert.ok(batch.observe_after.snapshot_id);
  await assert.rejects(manager.browserControl('browser.batch', { tabId: tab.id, actions: [{ action: 'click', selector: '#save' }, { action: 'navigate', url: 'other.example' }] }), /unsupported browser batch action/);
  assert.equal(entry.view.webContents.getURL(), 'about:blank');
});

test('root diagnostics remain visible during navigation invalidation while stale child sessions are ignored', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'navigation-diagnostics', visible: false });
  const entry = manager.entries.get('navigation-diagnostics');
  manager.invalidateFrames(entry, null, 'document_replaced');
  entry.view.webContents.debugger.emit('message', {}, 'Runtime.consoleAPICalled', {
    type: 'warning', args: [{ type: 'string', value: 'root-navigation-warning' }],
  }, '');
  entry.view.webContents.debugger.emit('message', {}, 'Runtime.consoleAPICalled', {
    type: 'warning', args: [{ type: 'string', value: 'stale-child-warning' }],
  }, 'unowned-child-session');
  const diagnostics = await manager.browserControl('browser.diagnostics', { tabId: tab.id, limit: 100 });
  assert.ok(diagnostics.entries.some((item) => item.message.includes('root-navigation-warning')));
  assert.equal(diagnostics.entries.some((item) => item.message.includes('stale-child-warning')), false);
});

test('execution-context destruction is scoped to the exact target when context ids collide', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  await manager.browserControl('browser.open', { chatId: 'context-id-collision', visible: false });
  const entry = manager.entries.get('context-id-collision');
  const root = manager.frameForSession(entry, null);
  root.contextId = 73;
  entry.executionContexts.set(`${entry.rootTargetId}:${root.id}`, 73);
  const childA = {
    id: 'frame-context-a', parentFrameId: root.id, loaderId: 'loader-a', targetId: 'target-a',
    sessionId: 'session-a', contextId: 73, generation: 1, detached: false,
  };
  const childB = {
    id: 'frame-context-b', parentFrameId: root.id, loaderId: 'loader-b', targetId: 'target-b',
    sessionId: 'session-b', contextId: 73, generation: 2, detached: false,
  };
  entry.frames.set(childA.id, childA);
  entry.frames.set(childB.id, childB);
  entry.targetSessions.set('session-a', { targetId: 'target-a', frameId: childA.id });
  entry.targetSessions.set('session-b', { targetId: 'target-b', frameId: childB.id });
  entry.executionContexts.set('target-a:frame-context-a', 73);
  entry.executionContexts.set('target-b:frame-context-b', 73);
  const interrupted = [];
  for (const frameId of [root.id, childA.id, childB.id]) {
    entry.waits.add({ frameId, interrupt: (reason) => interrupted.push([frameId, reason]) });
  }
  entry.waits.add({ frameId: null, targetId: entry.rootTargetId, interrupt: (reason) => interrupted.push(['unknown-root', reason]) });

  await manager.handleCDPMessage(entry, 'Runtime.executionContextDestroyed', { executionContextId: 73 }, 'session-a');

  assert.equal(childA.contextId, null);
  assert.equal(childA.unavailableReason, 'execution_context_lost');
  assert.equal(childB.contextId, 73);
  assert.equal(root.contextId, 73);
  assert.equal(entry.executionContexts.has('target-a:frame-context-a'), false);
  assert.equal(entry.executionContexts.get('target-b:frame-context-b'), 73);
  assert.equal(entry.executionContexts.get(`${entry.rootTargetId}:${root.id}`), 73);
  assert.deepEqual(interrupted, [[childA.id, 'execution_context_lost']]);

  await manager.handleCDPMessage(entry, 'Runtime.executionContextDestroyed', { executionContextId: 73 }, null);
  assert.equal(root.contextId, null);
  assert.equal(childB.contextId, 73);
  assert.equal(entry.executionContexts.has(`${entry.rootTargetId}:${root.id}`), false);
  assert.deepEqual(interrupted, [
    [childA.id, 'execution_context_lost'],
    [root.id, 'execution_context_lost'],
    ['unknown-root', 'execution_context_lost'],
  ]);
  manager.destroy();
});

test('frame navigation and subtree removal retire descendant contexts and sessions once', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  await manager.browserControl('browser.open', { chatId: 'frame-subtree-retirement', visible: false });
  const entry = manager.entries.get('frame-subtree-retirement');
  const wc = entry.view.webContents;
  const root = manager.frameForSession(entry, null);
  const addOwnedSubtree = (suffix) => {
    const childId = `frame-child-${suffix}`;
    const grandchildId = `frame-grandchild-${suffix}`;
    const childSession = `session-child-${suffix}`;
    const grandchildSession = `session-grandchild-${suffix}`;
    const childTarget = `target-child-${suffix}`;
    const grandchildTarget = `target-grandchild-${suffix}`;
    entry.frames.set(childId, {
      id: childId, parentFrameId: root.id, loaderId: `loader-child-${suffix}`, targetId: childTarget,
      sessionId: childSession, contextId: 81, generation: 10, detached: false,
    });
    entry.frames.set(grandchildId, {
      id: grandchildId, parentFrameId: childId, loaderId: `loader-grandchild-${suffix}`, targetId: grandchildTarget,
      sessionId: grandchildSession, contextId: 82, generation: 11, detached: false,
    });
    entry.targetSessions.set(childSession, { targetId: childTarget, frameId: childId, parentSessionId: null });
    entry.targetSessions.set(grandchildSession, { targetId: grandchildTarget, frameId: grandchildId, parentSessionId: childSession });
    entry.executionContexts.set(`${childTarget}:${childId}`, 81);
    entry.executionContexts.set(`${grandchildTarget}:${grandchildId}`, 82);
    manager.childSessions.set(`${wc.id}:${childTarget}`, childSession);
    manager.childSessions.set(`${wc.id}:${grandchildTarget}`, grandchildSession);
    return { childId, grandchildId, childSession, grandchildSession, childTarget, grandchildTarget };
  };

  const navigated = addOwnedSubtree('nav');
  const oldDocumentGeneration = entry.documentGeneration;
  entry.mainNavigationPending = false;
  wc.frameTree = { frameTree: { frame: { id: root.id, loaderId: 'loader-root-after-nav', url: 'https://fixture.invalid/next' }, childFrames: [] } };
  await manager.handleCDPMessage(entry, 'Page.frameNavigated', {
    frame: { id: root.id, loaderId: 'loader-root-after-nav', url: 'https://fixture.invalid/next', securityOrigin: 'https://fixture.invalid' },
  }, null);
  assert.equal(entry.documentGeneration, oldDocumentGeneration + 1);
  assert.equal(entry.frames.get(root.id).loaderId, 'loader-root-after-nav');
  assert.equal(entry.frames.has(navigated.childId), false);
  assert.equal(entry.frames.has(navigated.grandchildId), false);
  assert.equal(entry.targetSessions.has(navigated.childSession), false);
  assert.equal(entry.targetSessions.has(navigated.grandchildSession), false);
  assert.equal(entry.executionContexts.has(`${navigated.childTarget}:${navigated.childId}`), false);
  assert.equal(entry.executionContexts.has(`${navigated.grandchildTarget}:${navigated.grandchildId}`), false);

  const removed = addOwnedSubtree('remove');
  wc.frameTree = { frameTree: {
    frame: { id: root.id, loaderId: 'loader-root-after-nav', url: 'https://fixture.invalid/next' },
    childFrames: [{ frame: {
      id: removed.childId, parentId: root.id, loaderId: 'loader-child-remove', url: 'https://fixture.invalid/child',
    }, childFrames: [{ frame: {
      id: removed.grandchildId, parentId: removed.childId, loaderId: 'loader-grandchild-remove', url: 'https://fixture.invalid/grandchild',
    }, childFrames: [] }] }],
  } };
  wc.debugger.commandCalls.length = 0;
  await manager.handleCDPMessage(entry, 'Page.frameSubtreeWillBeDetached', { rootFrameId: removed.childId }, null);
  await manager.handleCDPMessage(entry, 'Page.frameDetached', { frameId: removed.childId, reason: 'remove' }, null);
  await manager.handleCDPMessage(entry, 'Target.detachedFromTarget', { sessionId: removed.childSession }, null);
  await manager.handleCDPMessage(entry, 'Target.detachedFromTarget', { sessionId: removed.grandchildSession }, null);
  assert.equal(entry.frames.has(removed.childId), false);
  assert.equal(entry.frames.has(removed.grandchildId), false);
  assert.equal(entry.targetSessions.has(removed.childSession), false);
  assert.equal(entry.targetSessions.has(removed.grandchildSession), false);
  assert.equal(entry.executionContexts.has(`${removed.childTarget}:${removed.childId}`), false);
  assert.equal(entry.executionContexts.has(`${removed.grandchildTarget}:${removed.grandchildId}`), false);
  assert.equal(wc.debugger.commandCalls.filter((call) => call.method === 'Target.detachFromTarget').length, 0,
    'frame removal already retires the protocol-owned subtree; later detach events do not detach it again');
  manager.destroy();
});

test('nested scroll deltas are not confused with screenshot coordinates', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'scroll-delta-target', visible: false });
  const entry = manager.entries.get('scroll-delta-target');
  let observedTarget;
  manager.observationTarget = async (_entry, params) => {
    observedTarget = params;
    return { frameId: 'frame-scroll-delta-target', selector: '#nested-scroll' };
  };
  manager.evaluateFrame = async (_entry, _frameId, script) => {
    assert.match(script, /scrollBy\(1,2\)/);
    return { changed: true, nested: true, before: { elementY: 0 }, after: { elementY: 2 } };
  };
  const result = await manager.browserControl('browser.scroll', {
    tabId: tab.id, elementRef: 'opaque-scroll-ref', snapshotId: 'scroll-snapshot', x: 1, y: 2,
  });
  assert.deepEqual(observedTarget, { elementRef: 'opaque-scroll-ref', snapshotId: 'scroll-snapshot' });
  assert.equal(result.changed, true);
  assert.equal(result.nested, true);
});

test('batch prevalidates every key before mutation, stops on stale nested scroll, and retains actions after observe_after fails', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'batch-validation', visible: false });
  const entry = manager.entries.get('batch-validation');
  const inputCalls = () => entry.view.webContents.debugger.commandCalls.filter((call) => call.method === 'Input.dispatchKeyEvent').length;
  const beforeInvalid = inputCalls();
  await assert.rejects(manager.browserControl('browser.batch', {
    tabId: tab.id,
    actions: [{ action: 'key', key: 'A' }, { action: 'key', key: 'BogusModifier+A' }],
  }), /unsupported browser modifier/);
  assert.equal(inputCalls(), beforeInvalid, 'a malformed later key prevents the first key action');

  const stopped = await manager.browserControl('browser.batch', {
    tabId: tab.id,
    actions: [
      { action: 'scroll', elementRef: 'el_stale', snapshotId: 'snap_stale', y: 100 },
      { action: 'key', key: 'A' },
    ],
  });
  assert.deepEqual(stopped.completed_indexes, []);
  assert.equal(stopped.failed_index, 0);
  assert.deepEqual(stopped.unexecuted_indexes, [1]);
  assert.equal(inputCalls(), beforeInvalid, 'the action after a stale nested scroll is not run');

  let snapshots = 0;
  manager.browserSnapshot = async () => {
    snapshots += 1;
    if (snapshots === 2) throw new Error('token=never-retain-this');
    return { snapshot_id: 'retained-action-observation' };
  };
  const retained = await manager.browserControl('browser.batch', {
    tabId: tab.id, actions: [{ action: 'snapshot' }], observeAfter: true,
  });
  assert.deepEqual(retained.completed_indexes, [0]);
  assert.equal(retained.failed_index, null);
  assert.equal(retained.results[0].result.snapshot_id, 'retained-action-observation');
  assert.equal(retained.observe_after, null);
  assert.doesNotMatch(retained.observe_after_error, /never-retain-this/);
});

test('snapshot enforces the final UTF-8 budget after duplicated interactive, editor, frame, and text data', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'snapshot-budget', visible: false });
  const entry = manager.entries.get('snapshot-budget');
  entry.view.webContents.executeJavaScript = async (script) => {
    if (!script.includes('const snapshotId =')) return { ...entry.view.webContents.metrics };
    const snapshotId = JSON.parse(script.match(/const snapshotId = (.+);/)?.[1] || '"snap"');
    const refs = Array.from({ length: 500 }, (_value, index) => ({
      path: [{ kind: 'child', index }], nodeKey: `${snapshotId}:${index}`, selector: `#node-${index}`,
    }));
    const semantic = refs.map((_ref, refIndex) => ({
      refIndex, role: 'button', tag: 'button', selector: '#'.padEnd(80, 's'),
      ariaLabel: 'token=SNAPSHOT_SECRET_VALUE', name: 'SNAPSHOT_SECRET_VALUE'.padEnd(240, 'n'),
      text: 'Snapshot content '.repeat(16), actionable: true, disabled: false,
    }));
    const editors = Array.from({ length: 50 }, (_value, index) => ({
      selector: `#editor-${index}`.padEnd(100, 'e'), kind: 'monaco', text: 'editor data '.repeat(900), truncated: false,
    }));
    const frames = Array.from({ length: 80 }, (_value, index) => ({
      selector: `iframe:nth-of-type(${index + 1})`, name: 'Frame name '.repeat(30),
      accessible: false, access: 'unavailable', limitation: 'owned frame pending', bounds: null,
    }));
    return {
      snapshotId, url: 'https://fixture.invalid/', title: 'Budget fixture', text: 'body content '.repeat(2500),
      textLength: 32500, textLengthExact: true, textBytesReturned: 32500, textBytesScanned: 40000,
      textTruncatedBytes: 7500, textTruncated: true,
      semantic, interactive: semantic.map((node) => ({ ...node })), refs, frames, editors,
      nodeCount: 500, semanticTruncated: false, semanticTruncatedCount: 0,
      editorRootCount: 50, editorsTruncated: false,
      traversal: { visitedElements: 6000, maxElements: 10000, truncated: false, depthTruncated: false, frameListTruncated: false, errorCount: 0, errors: [], nodeCountExact: true },
      accessibility: { source: 'dom', axTreeAvailable: false, limitation: 'owned_cdp_ax_tree_not_resolved' },
      scroll: { x: 0, y: 0, width: 1440, height: 900, deviceScaleFactor: 1, documentWidth: 1440, documentHeight: 1800 },
    };
  };
  const snapshot = await manager.browserControl('browser.snapshot', { tabId: tab.id });
  assert.ok(Buffer.byteLength(JSON.stringify(snapshot), 'utf8') <= 64 * 1024);
  assert.equal(snapshot.truncation.total_bytes, Buffer.byteLength(JSON.stringify(snapshot), 'utf8'));
  assert.ok(snapshot.truncation.semantic_rows_removed + snapshot.truncation.interactive_rows_removed > 0 || snapshot.truncation.editor_rows_removed > 0);
  assert.ok(snapshot.text_bytes_scanned > snapshot.text_bytes_returned);
  assert.doesNotMatch(JSON.stringify(snapshot), /SNAPSHOT_SECRET_VALUE/);
  assert.equal(snapshot.semantic_returned, snapshot.semantic.length);
});

test('snapshot matches frame boundaries by owner identity and retains an inaccessible sibling', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  await manager.browserControl('browser.open', { chatId: 'mixed-frame-boundaries', visible: false });
  const entry = manager.entries.get('mixed-frame-boundaries');
  const wc = entry.view.webContents;
  const root = manager.frameForSession(entry, null);
  wc.frameTree = { frameTree: {
    frame: { id: root.id, loaderId: root.loaderId, url: wc.getURL(), securityOrigin: 'https://fixture.invalid' },
    childFrames: [{ frame: {
      id: 'frame-accessible-sibling', parentId: root.id, loaderId: 'loader-accessible-sibling',
      url: 'https://fixture.invalid/accessible',
    }, childFrames: [] }],
  } };
  await manager.refreshFrameTree(entry);
  let observedSnapshotId = '';
  manager.evaluateFrame = async (_entry, frameId, script) => {
    if (!script.includes('const snapshotId =') || !script.includes('const maxNodes =')) {
      throw new Error('synthetic sibling frame has no execution context');
    }
    observedSnapshotId = JSON.parse(script.match(/const snapshotId = (.+);/)?.[1] || '""');
    if (frameId !== root.id) throw new Error('synthetic sibling frame has no execution context');
    return {
      snapshotId: observedSnapshotId, url: wc.getURL(), title: 'Mixed frame boundary fixture', text: '',
      textLength: 0, textLengthExact: true, textBytesReturned: 0, textBytesScanned: 0, textTruncatedBytes: 0,
      semantic: [], nodeCount: 0, semanticTruncated: false, semanticTruncatedCount: 0,
      refs: [
        { path: [{ kind: 'child', index: 0 }], nodeKey: `${observedSnapshotId}:owner-accessible`, selector: 'iframe#accessible-frame' },
        { path: [{ kind: 'child', index: 1 }], nodeKey: `${observedSnapshotId}:owner-inaccessible`, selector: 'iframe#inaccessible-frame' },
      ],
      frames: [
        { refIndex: 0, name: 'Accessible sibling', selector: 'iframe#accessible-frame', bounds: { x: 10, y: 10, width: 100, height: 80 }, boundsAvailable: true, access: 'owned_frame_pending', limitation: 'owned_frame_target_unavailable' },
        { refIndex: 1, name: 'Inaccessible sibling', selector: 'iframe#inaccessible-frame', bounds: { x: 120, y: 10, width: 100, height: 80 }, boundsAvailable: true, access: 'inaccessible', limitation: 'cross_origin_target_unavailable' },
      ],
      editors: [], interactive: [], traversal: { visitedElements: 2, truncated: false, errors: [] },
      scroll: { x: 0, y: 0, width: 1440, height: 900, deviceScaleFactor: 1, documentWidth: 1440, documentHeight: 900 },
    };
  };
  const debug = wc.debugger;
  const originalSendCommand = debug.sendCommand.bind(debug);
  debug.sendCommand = async (method, params, sessionId) => {
    if (method === 'DOM.getFrameOwner' && params.frameId === 'frame-accessible-sibling') return { backendNodeId: 505 };
    if (method === 'DOM.resolveNode' && params.backendNodeId === 505) return { object: { objectId: 'accessible-frame-owner' } };
    if (method === 'Runtime.callFunctionOn' && params.objectId === 'accessible-frame-owner') {
      return { result: { value: [`${observedSnapshotId}:owner-accessible`] } };
    }
    return originalSendCommand(method, params, sessionId);
  };

  const snapshot = await manager.browserControl('browser.snapshot', { tabId: Number(wc.id) });
  assert.equal(snapshot.frames.length, 2);
  assert.ok(snapshot.frames.some((frame) => frame.selector === 'iframe#accessible-frame'));
  const inaccessible = snapshot.frames.find((frame) => frame.selector === 'iframe#inaccessible-frame');
  assert.ok(inaccessible, 'an accessible sibling does not suppress the unmatched DOM boundary');
  assert.equal(inaccessible.accessible, false);
  assert.equal(inaccessible.limitation, 'cross_origin_target_unavailable');
  manager.destroy();
});

test('screenshot coordinates become stale after scroll and host waits settle on close', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'stamp-and-wait', visible: false });
  const entry = manager.entries.get('stamp-and-wait');
  const shot = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  entry.view.webContents.metrics.scrollY = 35;
  const stale = await manager.browserControl('browser.click', {
    tabId: tab.id, screenshotId: shot.metadata.screenshot_id, x: 10, y: 10,
  });
  assert.equal(stale.status, 'stale');

  entry.view.webContents.executeJavaScript = () => new Promise(() => {});
  const waiting = manager.browserControl('browser.wait', { tabId: tab.id, condition: 'load', timeoutMs: 1000 });
  setTimeout(() => manager.close('stamp-and-wait'), 10);
  const result = await waiting;
  assert.equal(result.reason, 'tab_closed');
  assert.equal(entry.waits.size, 0);
});

test('viewport screenshot stamps use observed viewport coordinates and still reject navigation and clip extent changes', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  const tab = await manager.browserControl('browser.open', { chatId: 'viewport-stamp', visible: false });
  const entry = manager.entries.get('viewport-stamp');
  const viewport = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  entry.view.webContents.metrics.documentHeight += 22;
  const click = await manager.browserControl('browser.click', {
    tabId: tab.id, screenshotId: viewport.metadata.screenshot_id, x: 10, y: 10,
  });
  assert.equal(click.status, 'clicked', 'an unrelated document-extent change does not alter viewport-pixel coordinate mapping');

  const staleOnNavigation = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  entry.documentGeneration += 1;
  const navigationClick = await manager.browserControl('browser.click', {
    tabId: tab.id, screenshotId: staleOnNavigation.metadata.screenshot_id, x: 10, y: 10,
  });
  assert.equal(navigationClick.status, 'stale', 'root document navigation invalidates viewport screenshot coordinates');

  const clip = await manager.browserControl('browser.screenshot', {
    tabId: tab.id, mode: 'clip', clip: { x: 0, y: 0, width: 40, height: 40 },
  });
  entry.view.webContents.metrics.documentHeight += 1;
  const staleClip = await manager.browserControl('browser.click', {
    tabId: tab.id, screenshotId: clip.metadata.screenshot_id, x: 10, y: 10,
  });
  assert.equal(staleClip.status, 'stale', 'a document-space clip remains bound to its observed document extent');
  manager.destroy();
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
  assert.equal(snapshot.snapshot_id.length > 0, true);
  assert.deepEqual(snapshot.effective_viewport, { width: 1440, height: 900, device_scale_factor: 1 });
  const click = await manager.browserControl('browser.click', { tabId: tab.id, selector: '#save' });
  assert.equal(click.ok, true);
  assert.equal(click.trusted, true);
  const shot = await manager.browserControl('browser.screenshot', { tabId: tab.id });
  assert.equal(shot.mimeType, 'image/png');
  assert.deepEqual(shot.metadata.image, { width: 1440, height: 900 });
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
  assert.equal(snapshot.snapshot_id.length > 0, true);
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
    (await manager.browserControl('browser.snapshot', { chatId: 'conv-owned' })).url,
    'https://example.com',
  );

  const scoped = await manager.browserControl('browser.list', { chatId: 'conv-owned' });
  assert.equal(scoped.tabs.length, 1);
  assert.equal(scoped.tabs[0].id, owned.id);
  assert.equal(scoped.tabs[0].conversationId, 'conv-owned');
});

test('browser entries keep local and machine-tagged chats with the same raw id separate', async () => {
  const { manager } = fixture({ requestOpen: () => {} });
  manager.setAgentControlReady(true);

  const local = await manager.browserControl('browser.open', { chatId: 'same-chat', url: 'local.example' });
  const remote = await manager.browserControl('browser.open', { chatId: 'M~san-laptop~same-chat', url: 'remote.example' });

  assert.notEqual(local.id, remote.id);
  assert.equal(local.conversationId, 'same-chat');
  assert.equal(remote.conversationId, 'M~san-laptop~same-chat');
  assert.deepEqual(
    (await manager.browserControl('browser.list', { chatId: 'same-chat' })).tabs.map((tab) => tab.conversationId),
    ['same-chat'],
  );
  assert.deepEqual(
    (await manager.browserControl('browser.list', { chatId: 'M~san-laptop~same-chat' })).tabs.map((tab) => tab.conversationId),
    ['M~san-laptop~same-chat'],
  );
});

test('CDP adapter forwards root and child-target commands and events', async () => {
  const { manager } = fixture();
  const tab = await manager.activate({ chatId: 'chat-cdp', bounds: { x: 700, y: 60, width: 460, height: 680 } });
  const events = [];
  manager.onCDP((event) => events.push(event));
  const entry = manager.entries.get('chat-cdp');
  const rootFrameId = `frame-${entry.view.webContents.id}`;
  entry.view.webContents.frameTree = { frameTree: {
    frame: { id: rootFrameId, loaderId: 'loader-root', url: 'about:blank' },
    childFrames: [{ frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child-v2', url: 'http://fixture.invalid/frame' }, childFrames: [] }],
  } };
  entry.view.webContents.childFrameTree = { frameTree: { frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child-v3', url: 'http://fixture.invalid/frame' }, childFrames: [] } };
  await manager.refreshFrameTree(entry);
  entry.view.webContents.debugger.emit('message', {}, 'Page.frameAttached', { frameId: 'frame-child', parentFrameId: rootFrameId }, null);
  const provisionalGeneration = entry.frames.get('frame-child').generation;
  entry.view.webContents.debugger.emit('message', {}, 'Page.frameNavigated', {
    frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child-v3', url: 'http://fixture.invalid/frame', securityOrigin: 'http://fixture.invalid' },
  }, null);
  assert.ok(entry.frames.get('frame-child').generation > provisionalGeneration, 'authoritative frame navigation advances the owned identity generation');
  entry.view.webContents.frameTree.frameTree.childFrames[0].frame.loaderId = 'loader-child-v3';
  await assert.rejects(manager.attachTarget(entry.view.webContents.id, 'unowned-target'), /not an owned iframe/);
  await assert.rejects(manager.executeCDP({ tabId: entry.view.webContents.id }, 'Target.attachToTarget', { targetId: 'unowned-target' }), /restricted to the owned frame tree/);
  assert.equal(await manager.acceptAttachedTarget(entry, {
    sessionId: 'child-session-1', targetInfo: { targetId: 'child-target', type: 'iframe' },
  }, null), true);
  await manager.attachTarget(entry.view.webContents.id, 'child-target');
  await manager.executeCDP({ tabId: entry.view.webContents.id, targetId: 'child-target' }, 'Runtime.evaluate', { expression: '1+1' });
  assert.equal(entry.view.webContents.debugger.lastCommand.sessionId, 'child-session-1');
  events.length = 0;
  entry.view.webContents.debugger.emit('message', {}, 'Page.loadEventFired', { value: 1 }, 'child-session-1');
  assert.deepEqual(events[0], {
    tabId: entry.view.webContents.id, method: 'Page.loadEventFired', params: { value: 1 }, sessionId: 'child-session-1',
  });
  assert.equal(tab.cdpAttached, true);
});

test('browser navigation waits for an owned iframe target to finish binding', async () => {
  const { manager } = fixture();
  const tab = await manager.activate({ chatId: 'chat-frame-bind', bounds: { x: 0, y: 0, width: 400, height: 300 } });
  const entry = manager.entries.get('chat-frame-bind');
  const wc = entry.view.webContents;
  const rootFrameId = 'frame-' + wc.id;
  wc.frameTree = { frameTree: {
    frame: { id: rootFrameId, loaderId: 'loader-root', url: 'about:blank' },
    childFrames: [{ frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child', url: 'http://fixture.invalid/frame' }, childFrames: [] }],
  } };
  wc.childFrameTree = { frameTree: {
    frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child', url: 'http://fixture.invalid/frame' },
  } };
  await manager.refreshFrameTree(entry);
  let releaseAccessibility;
  const accessibilityGate = new Promise((resolve) => { releaseAccessibility = resolve; });
  const sendCommand = wc.debugger.sendCommand.bind(wc.debugger);
  wc.debugger.sendCommand = (method, params, sessionId) => {
    if (method === 'Accessibility.enable' && sessionId === 'child-session-pending') {
      return accessibilityGate.then(() => sendCommand(method, params, sessionId));
    }
    return sendCommand(method, params, sessionId);
  };
  wc.debugger.emit('message', {}, 'Target.attachedToTarget', {
    sessionId: 'child-session-pending',
    targetInfo: { targetId: 'child-target-pending', type: 'iframe' },
  }, null);
  assert.equal(entry.pendingTargetTasks.size, 1);
  const navigation = manager.browserControl('browser.navigate', {
    tabId: tab.id, url: 'https://fixture.invalid/after-frame',
  });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(wc.getURL(), 'about:blank', 'navigation must not race the child target setup commands');
  releaseAccessibility();
  await navigation;
  assert.equal(wc.getURL(), 'https://fixture.invalid/after-frame');
  assert.equal(entry.pendingTargetTasks.size, 0);
  assert.equal(entry.targetSessions.get('child-session-pending')?.targetId, 'child-target-pending');
  manager.destroy();
});

test('retiring an already detached frame session does not detach it twice', async () => {
  const { manager } = fixture();
  await manager.activate({ chatId: 'chat-frame-detach', bounds: { x: 0, y: 0, width: 400, height: 300 } });
  const entry = manager.entries.get('chat-frame-detach');
  const wc = entry.view.webContents;
  const rootFrameId = 'frame-' + wc.id;
  wc.frameTree = { frameTree: {
    frame: { id: rootFrameId, loaderId: 'loader-root', url: 'about:blank' },
    childFrames: [{ frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child', url: 'http://fixture.invalid/frame' }, childFrames: [] }],
  } };
  wc.childFrameTree = { frameTree: {
    frame: { id: 'frame-child', parentId: rootFrameId, loaderId: 'loader-child', url: 'http://fixture.invalid/frame' },
  } };
  await manager.refreshFrameTree(entry);
  assert.equal(await manager.acceptAttachedTarget(entry, {
    sessionId: 'child-session-detached', targetInfo: { targetId: 'child-target-detached', type: 'iframe' },
  }, null), true);
  wc.debugger.commandCalls.length = 0;
  await manager.handleCDPMessage(entry, 'Target.detachedFromTarget', { sessionId: 'child-session-detached' }, null);
  assert.equal(entry.targetSessions.has('child-session-detached'), false);
  assert.equal(wc.debugger.commandCalls.some((call) => call.method === 'Target.detachFromTarget'), false);

  assert.equal(await manager.acceptAttachedTarget(entry, {
    sessionId: 'child-session-explicit', targetInfo: { targetId: 'child-target-explicit', type: 'iframe' },
  }, null), true);
  wc.debugger.commandCalls.length = 0;
  await manager.detachTarget(wc.id, 'child-target-explicit');
  assert.equal(wc.debugger.commandCalls.filter((call) => call.method === 'Target.detachFromTarget').length, 1);
  manager.destroy();
});
