'use strict';

// The webview host: pages are guests of <webview> elements the app renderer
// mounts. These fakes model what BrowserManager relies on: a guest attached
// (and hardened) by the app window, registered by host key, replaced when the
// app renderer reloads.
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const { BrowserManager } = require('./browser-manager');
const { hardenWebviewAttach, resolveBrowserHost } = require('./browser-webview-policy');
const { pipContentSize, pipInputToPage } = require('./browser-pip');

class FakeDebugger extends EventEmitter {
  constructor(owner) { super(); this.owner = owner; this.attached = false; this.calls = []; }
  isAttached() { return this.attached; }
  attach() { this.attached = true; }
  async sendCommand(method, params, sessionId) {
    this.calls.push({ method, params, sessionId });
    if (method === 'Target.getTargetInfo') return { targetInfo: { targetId: `target-${this.owner.id}` } };
    if (method === 'Page.getFrameTree') return { frameTree: { frame: { id: `frame-${this.owner.id}`, loaderId: 'l', url: this.owner.url }, childFrames: [] } };
    if (method === 'Emulation.setDeviceMetricsOverride') { this.owner.metrics = { ...this.owner.metrics, width: params.width, height: params.height, deviceScaleFactor: params.deviceScaleFactor }; return {}; }
    if (method === 'Page.createIsolatedWorld') return { executionContextId: 77 };
    if (method === 'Runtime.evaluate' && params && params.contextId === 77) return { result: { value: await this.owner.executeJavaScript(params.expression) } };
    return {};
  }
}

let nextId = 40;
class FakeGuest extends EventEmitter {
  constructor(host, session) {
    super();
    this.id = ++nextId;
    this.hostWebContents = host;
    this.session = session;
    this.url = 'about:blank';
    this.zoom = 1.25; // a guest starts at the app's zoom
    this.destroyed = false;
    this.debugger = new FakeDebugger(this);
    this.navigationHistory = { canGoBack: () => false, canGoForward: () => false };
    this.metrics = { width: 1440, height: 900, deviceScaleFactor: 1, scrollX: 0, scrollY: 0, documentWidth: 1440, documentHeight: 900 };
    this.loads = [];
  }
  getType() { return 'webview'; }
  isDestroyed() { return this.destroyed; }
  getURL() { return this.url; }
  isLoadingMainFrame() { return false; }
  getZoomFactor() { return this.zoom; }
  setZoomFactor(value) { this.zoom = value; }
  setUserAgent(value) { this.userAgent = value; }
  setWindowOpenHandler(handler) { this.windowOpenHandler = handler; }
  async loadURL(url) {
    this.loads.push(url);
    this.emit('did-start-navigation', {}, url, false, true);
    this.url = url;
    this.emit('did-navigate', {}, url);
    this.emit('dom-ready');
    this.emit('did-finish-load');
    this.emit('did-stop-loading');
  }
  async executeJavaScript(script) {
    if (script.includes('location.href, readyState: document.readyState')) return { url: this.url, readyState: 'complete' };
    if (script.includes('({ width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio')) return { ...this.metrics };
    return { ok: true };
  }
  close() { this.destroy(); }
  destroy() { if (this.destroyed) return; this.destroyed = true; this.emit('destroyed'); }
}

function hostedFixture() {
  const profile = {
    isPersistent: () => true,
    setUserAgent() {}, setPermissionCheckHandler() {}, setPermissionRequestHandler() {},
  };
  const win = { getContentSize: () => [1440, 900], webContents: { getZoomFactor: () => 1.25 } };
  const guests = new Map();
  const published = [];
  const manager = new BrowserManager({
    win, WebContentsView: class {}, session: { fromPartition: () => profile },
    chromeVersion: '150.0.0.0', platform: 'darwin', host: 'webview',
    webContentsFromId: (id) => guests.get(id) || null,
    onHosts: (hosts) => published.push(hosts),
  });
  manager.guestAttachTimeoutMs = 2000;
  const mount = (hostKey, { attached = true } = {}) => {
    const guest = new FakeGuest(win.webContents, profile);
    guests.set(guest.id, guest);
    if (attached) manager.noteAttachedGuest(guest);
    return { guest, reply: manager.registerGuest(hostKey, guest.id) };
  };
  return { manager, win, profile, guests, published, mount };
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test('a hosted page is mounted by the renderer, registered by its host key, and keeps one tab id', async () => {
  const { manager, published, mount } = hostedFixture();
  const entry = manager.get('chat-1');
  assert.equal(entry.view.webContents, null);
  const hosts = published.at(-1);
  assert.equal(hosts.length, 1);
  assert.equal(hosts[0].chatId, 'chat-1');
  assert.match(hosts[0].hostKey, /^wkb-/);

  assert.equal(manager.registerGuest('wkb-unknown', 1).ok, false);
  const stray = mount(hosts[0].hostKey, { attached: false });
  assert.equal(stray.reply.ok, false, 'a guest this window did not attach (and harden) is refused');

  const { guest, reply } = mount(hosts[0].hostKey);
  assert.equal(reply.ok, true);
  assert.equal(await entry.ready, true);
  assert.equal(entry.view.webContents, guest);
  assert.equal(manager.tabIdOf(entry), guest.id);
  assert.equal(guest.zoom, 1, 'the page lays out at 1:1 whatever the app zoom is');
  assert.equal(guest.debugger.attached, true);
  const metrics = guest.debugger.calls.find((call) => call.method === 'Emulation.setDeviceMetricsOverride');
  assert.deepEqual([metrics.params.width, metrics.params.height, metrics.params.scale], [1440, 900, 1]);
  assert.equal(manager.registerGuest(hosts[0].hostKey, guest.id).ok, true, 'registering the same guest again is harmless');
  manager.destroy();
});

test('when the app renderer reloads, the tab waits for its next guest and comes back at its URL', async () => {
  const { manager, published, mount } = hostedFixture();
  const entry = manager.get('chat-2');
  const hostKey = published.at(-1)[0].hostKey;
  const first = mount(hostKey).guest;
  await entry.ready;
  await first.loadURL('https://example.test/signed-in');
  entry.url = first.url;
  const tabId = manager.tabIdOf(entry);

  first.destroy();
  assert.equal(entry.view.webContents, null);
  assert.equal(entry.restoreURL, 'https://example.test/signed-in');
  assert.equal(published.at(-1)[0].hostKey, hostKey, 'the same host key is mounted again');

  const second = mount(hostKey).guest;
  assert.equal(await entry.ready, true);
  for (let i = 0; i < 5; i += 1) await settle();
  assert.equal(entry.view.webContents, second);
  assert.equal(manager.tabIdOf(entry), tabId, 'same tab id for the agent');
  assert.ok(second.loads.includes('https://example.test/signed-in'), 'the page is loaded back at its URL');
  assert.equal(manager.consumeArtifactNavigation(second, 'https://example.test/signed-in'), false,
    'a restore loads the page\'s own last URL without authorizing it the way a navigate command does');
  assert.equal(manager.browserTabs('chat-2')[0].id, tabId);
  // A late event from the replaced guest no longer touches the tab.
  first.emit('page-title-updated', {}, 'stale title');
  assert.notEqual(entry.title, 'stale title');
  manager.destroy();
});

test('a renderer reload while a page is still being set up never hangs the presentation queue', async () => {
  const { manager, published, guests, profile, win, mount } = hostedFixture();
  manager.guestAttachTimeoutMs = 5000;
  const entry = manager.get('chat-setup');
  const hostKey = published.at(-1).find((host) => host.chatId === 'chat-setup').hostKey;
  // The first guest's setup never finishes (its first script never answers).
  const first = new FakeGuest(win.webContents, profile);
  first.executeJavaScript = () => new Promise(() => {});
  guests.set(first.id, first);
  manager.noteAttachedGuest(first);
  assert.equal(manager.registerGuest(hostKey, first.id).ok, true);
  const shown = manager.activate({ chatId: 'chat-setup', bounds: { x: 0, y: 0, width: 400, height: 250 } });
  await settle();
  first.destroy(); // the app renderer reloads mid-setup
  mount(hostKey); // and mounts the page again
  const within = (promise, ms) => Promise.race([promise.then(() => 'settled', () => 'settled'), new Promise((resolve) => setTimeout(() => resolve('hung'), ms))]);
  assert.equal(await within(shown, 1500), 'settled', 'the activation that waited on the lost setup ends');
  const other = manager.get('chat-other');
  mount(published.at(-1).find((host) => host.chatId === 'chat-other').hostKey);
  await other.ready;
  assert.equal(await within(manager.activate({ chatId: 'chat-other', bounds: { x: 0, y: 0, width: 400, height: 250 } }), 1500), 'settled',
    'and every later presentation call still runs');
  // Even a readiness that never settles cannot hold the queue past its bound.
  manager.guestAttachTimeoutMs = 10;
  assert.equal(await manager.readyForPresentation({ ready: new Promise(() => {}) }), false);
  manager.destroy();
});

test('resetting a hosted page a person is looking at keeps the pane showing a page', async () => {
  const { manager, published, mount } = hostedFixture();
  const entry = manager.get('chat-reset');
  mount(published.at(-1).find((host) => host.chatId === 'chat-reset').hostKey);
  await entry.ready;
  await manager.activate({ chatId: 'chat-reset', bounds: { x: 10, y: 20, width: 344, height: 215 }, radius: 12 });
  const closing = manager.browserControl('browser.close', { chatId: 'chat-reset' });
  await settle();
  const fresh = manager.entries.get('chat-reset');
  mount(published.at(-1).find((host) => host.chatId === 'chat-reset').hostKey); // the renderer mounts the new page
  const result = await closing;
  assert.equal(result.status, 'reset');
  assert.equal(fresh.visible, true, 'shown where the old page was');
  assert.equal(manager.activeId, 'chat-reset');
  manager.destroy();
});

test('hosted presentation never moves a native view; the renderer places the page', async () => {
  const { manager, published, mount } = hostedFixture();
  const entry = manager.get('chat-3');
  mount(published.at(-1)[0].hostKey);
  await entry.ready;
  const state = await manager.activate({ chatId: 'chat-3', bounds: { x: 10, y: 20, width: 344, height: 215 }, radius: 12 });
  assert.equal(state.visible, true);
  assert.equal(state.host, 'webview');
  assert.equal(entry.presentationScale, 1);
  assert.equal(manager.place('chat-3', { x: 50, y: 60, width: 344, height: 215 }), true);
  assert.equal(await manager.hide('chat-3'), true);
  assert.equal(entry.visible, false);
  assert.equal(manager.close('chat-3'), true);
  assert.deepEqual(published.at(-1), [], 'a closed page leaves the renderer');
  manager.destroy();
});

test('a page that does not come up is reported, and a later mount still brings it up', async () => {
  const { manager, published, mount } = hostedFixture();
  manager.guestAttachTimeoutMs = 20;
  const entry = manager.get('chat-4');
  assert.equal(await entry.ready, false);
  assert.match(entry.error, /did not come up/);
  mount(published.at(-1)[0].hostKey);
  assert.equal(await entry.ready, true);
  manager.destroy();
});

test('will-attach-webview allows only a blank page in the browser profile, without preload or Node', () => {
  const prefs = { preload: '/evil.js', nodeIntegration: true, contextIsolation: false, sandbox: false, webSecurity: false, enableBlinkFeatures: 'X' };
  const params = { partition: 'persist:workass-browser', src: 'about:blank', preload: 'file:///evil.js' };
  assert.deepEqual(hardenWebviewAttach(prefs, params, { partition: 'persist:workass-browser' }), { allow: true });
  assert.equal(prefs.preload, undefined);
  assert.equal(params.preload, undefined);
  assert.equal(prefs.nodeIntegration, false);
  assert.equal(prefs.contextIsolation, true);
  assert.equal(prefs.sandbox, true);
  assert.equal(prefs.webSecurity, true);
  assert.equal(prefs.webviewTag, false);
  assert.equal(prefs.enableBlinkFeatures, undefined);
  assert.equal(hardenWebviewAttach({}, { partition: 'persist:other', src: 'about:blank' }, { partition: 'persist:workass-browser' }).allow, false);
  assert.equal(hardenWebviewAttach({}, { partition: 'persist:workass-browser', src: 'https://example.com' }, { partition: 'persist:workass-browser' }).allow, false);
});

test('the browser host is a native view by default on every platform, and the webview host stays selectable', () => {
  const files = new Map();
  const fs = { readFileSync: (file) => { if (!files.has(file)) throw new Error('ENOENT'); return files.get(file); } };
  // A <webview> guest shares the app window's focus: an agent's input there reaches the person's composer.
  assert.deepEqual(resolveBrowserHost({ env: {}, platform: 'darwin', dataRoot: '/d', fs }), { host: 'view', source: 'default' });
  assert.equal(resolveBrowserHost({ env: {}, platform: 'win32', dataRoot: '/d', fs }).host, 'view');
  assert.deepEqual(resolveBrowserHost({ env: { WORKASS_BROWSER_HOST: 'webview' }, platform: 'win32', dataRoot: '/d', fs }), { host: 'webview', source: 'env' });
  assert.equal(resolveBrowserHost({ env: {}, platform: 'linux', dataRoot: '/d', fs }).host, 'view');
  assert.deepEqual(resolveBrowserHost({ env: { WORKASS_BROWSER_HOST: 'view' }, platform: 'darwin', dataRoot: '/d', fs }), { host: 'view', source: 'env' });
  files.set('/d/shell-settings.json', JSON.stringify({ browserHost: 'view' }));
  assert.deepEqual(resolveBrowserHost({ env: {}, platform: 'darwin', dataRoot: '/d', fs }), { host: 'view', source: 'settings' });
  assert.deepEqual(resolveBrowserHost({ env: {}, platform: 'win32', dataRoot: '/d', fs }), { host: 'view', source: 'settings' });
  files.set('/d/shell-settings.json', JSON.stringify({ browserHost: 'webview' }));
  assert.deepEqual(resolveBrowserHost({ env: {}, platform: 'win32', dataRoot: '/d', fs }), { host: 'webview', source: 'settings' });
  files.set('/d/shell-settings.json', '{not json');
  assert.equal(resolveBrowserHost({ env: {}, platform: 'darwin', dataRoot: '/d', fs }).host, 'view');
});

test('a wait that joins a pending wait for the page gets its own full time', async () => {
  const { manager, published, mount } = hostedFixture();
  manager.guestAttachTimeoutMs = 150;
  const entry = manager.get('chat-join');
  const hostKey = published.at(-1).find((host) => host.chatId === 'chat-join').hostKey;
  const outcomes = [];
  void manager.awaitGuest(entry).then((value) => outcomes.push(['first', value]));
  await new Promise((resolve) => setTimeout(resolve, 100));
  void manager.awaitGuest(entry).then((value) => outcomes.push(['joined', value]));
  // Past the first wait's own deadline, not past the joined one's.
  await new Promise((resolve) => setTimeout(resolve, 90));
  assert.deepEqual(outcomes, [], 'nobody is told "did not come up" while a later wait still has time');
  mount(hostKey);
  await settle();
  assert.deepEqual(outcomes.map(([, value]) => value), [true, true]);
  manager.destroy();
});

test('a press-and-move in the pop-out that starts an HTML drag goes through Chromium\'s drag events, never an OS drag', async () => {
  const { manager, published, mount } = hostedFixture();
  const entry = manager.get('chat-pipdrag');
  const { guest } = mount(published.at(-1).find((host) => host.chatId === 'chat-pipdrag').hostKey);
  await entry.ready;
  let intercepting = false;
  const base = guest.debugger.sendCommand.bind(guest.debugger);
  guest.debugger.sendCommand = async (method, params, sessionId) => {
    if (method === 'Input.setInterceptDrags') intercepting = params.enabled;
    const result = await base(method, params, sessionId);
    // A link under the pointer: Chromium starts its drag on the first move.
    if (method === 'Input.dispatchMouseEvent' && params.type === 'mouseMoved' && params.buttons === 1 && intercepting) {
      guest.debugger.emit('message', {}, 'Input.dragIntercepted', { data: { items: [{ mimeType: 'text/uri-list', data: 'https://a.example/' }], dragOperationsMask: 1 } });
    }
    return result;
  };
  const send = (type, extra = {}) => manager.pipInput(entry, 'Input.dispatchMouseEvent', { type, x: 10, y: 10, button: 'left', buttons: type === 'mouseReleased' ? 0 : 1, modifiers: 0, ...extra });
  const steps = () => guest.debugger.calls.filter((call) => call.method.startsWith('Input.')).map((call) => (
    call.method === 'Input.dispatchMouseEvent' ? `mouse:${call.params.type}`
      : call.method === 'Input.dispatchDragEvent' ? `drag:${call.params.type}`
        : call.method === 'Input.setInterceptDrags' ? `intercept:${call.params.enabled}` : call.method));
  await send('mousePressed', { clickCount: 1 });
  await send('mouseMoved', { x: 20 });
  await send('mouseMoved', { x: 40 });
  await send('mouseReleased', { x: 60, clickCount: 1 });
  assert.deepEqual(steps(), ['intercept:true', 'mouse:mousePressed', 'mouse:mouseMoved', 'drag:dragEnter', 'drag:dragOver', 'drag:drop', 'mouse:mouseReleased', 'intercept:false']);
  const drop = guest.debugger.calls.find((call) => call.method === 'Input.dispatchDragEvent' && call.params.type === 'drop');
  assert.equal(drop.params.x, 60, 'dropped where the person let go');
  // A plain click: no drag, and interception is off again after it.
  guest.debugger.calls.length = 0;
  intercepting = false;
  await send('mousePressed', { clickCount: 1 });
  await send('mouseReleased', { clickCount: 1 });
  assert.deepEqual(steps(), ['intercept:true', 'mouse:mousePressed', 'mouse:mouseReleased', 'intercept:false']);
  assert.equal(guest.debugger.listenerCount('message') <= 1, true, 'no listener left behind');
  manager.destroy();
});

test('the separate window sends a person\'s pointer and keyboard to the page as on main', () => {
  const viewport = { width: 1440, height: 900 };
  assert.deepEqual(pipContentSize(viewport), [420, 263]);
  const mouse = (input) => pipInputToPage(input, viewport);
  assert.deepEqual(mouse({ type: 'mousePressed', button: 0, buttons: 1, clickCount: 1, x: 0.5, y: 0.25 }),
    { method: 'Input.dispatchMouseEvent', params: { type: 'mousePressed', x: 720, y: 225, button: 'left', buttons: 1, clickCount: 1, modifiers: 0 } });
  assert.deepEqual(mouse({ type: 'mouseWheel', x: 0, y: 1, deltaX: 0, deltaY: 99999 }),
    { method: 'Input.dispatchMouseEvent', params: { type: 'mouseWheel', x: 0, y: 900, deltaX: 0, deltaY: 2000, modifiers: 0 } });
  // hover, and a drag or a text selection (moves with the button held)
  assert.deepEqual(mouse({ type: 'mouseMoved', buttons: 0, x: 0.1, y: 0.1 }),
    { method: 'Input.dispatchMouseEvent', params: { type: 'mouseMoved', x: 144, y: 90, button: 'none', buttons: 0, modifiers: 0 } });
  assert.deepEqual(mouse({ type: 'mouseMoved', buttons: 1, x: 0.2, y: 0.1 }).params, { type: 'mouseMoved', x: 288, y: 90, button: 'left', buttons: 1, modifiers: 0 });
  // a double click
  assert.equal(mouse({ type: 'mousePressed', button: 0, buttons: 1, clickCount: 2, x: 0.5, y: 0.5 }).params.clickCount, 2);
  // typing, a shortcut, a composed (IME) text
  assert.deepEqual(mouse({ type: 'keyDown', key: 'a', code: 'KeyA', keyCode: 65, text: 'a' }),
    { method: 'Input.dispatchKeyEvent', params: { type: 'keyDown', key: 'a', code: 'KeyA', windowsVirtualKeyCode: 65, modifiers: 0, text: 'a', unmodifiedText: 'a' } });
  assert.deepEqual(mouse({ type: 'keyDown', key: 'v', code: 'KeyV', keyCode: 86, meta: true }).params.commands, ['paste']);
  assert.equal(mouse({ type: 'keyUp', key: 'Enter', code: 'Enter', keyCode: 13 }).params.type, 'keyUp');
  assert.deepEqual(mouse({ type: 'insertText', text: '日本' }), { method: 'Input.insertText', params: { text: '日本' } });
  // bounded
  assert.equal(mouse({ type: 'mousePressed', x: 1.2, y: 0.5 }), null);
  assert.equal(mouse({ type: 'insertText', text: 'x'.repeat(5000) }), null);
  assert.equal(mouse({ type: 'dispatchSomethingElse', x: 0.5, y: 0.5 }), null);
  // The window's own page script parses.
  const { pipPageURL } = require('./browser-pip');
  const html = decodeURIComponent(pipPageURL().replace(/^data:text\/html;charset=utf-8,/u, ''));
  const script = html.match(/<script>([\s\S]*)<\/script>/u)[1];
  assert.doesNotThrow(() => new Function(script));
  for (const listener of ["'mousemove'", "'keydown'", "'keyup'", "'compositionend'"]) assert.ok(script.includes(listener), listener);
});

test('after an app renderer reload the separate window follows the new page instead of freezing', async () => {
  const { openGuestPip } = require('./browser-pip');
  class FakeWin extends EventEmitter {
    constructor() {
      super();
      this.destroyed = false;
      this.webContents = Object.assign(new EventEmitter(), {
        ipc: new EventEmitter(), sent: [], setWindowOpenHandler() {}, send(channel, payload) { this.sent.push([channel, payload]); },
      });
    }
    async loadURL() { setTimeout(() => this.webContents.emit('did-finish-load'), 0); }
    setAlwaysOnTop() {} setAspectRatio() {} show() {} isDestroyed() { return this.destroyed; } destroy() { this.destroyed = true; this.emit('closed'); }
    getContentSize() { return [420, 263]; }
  }
  const page = (name) => ({ name, isDestroyed: () => false, setBackgroundThrottling() {}, capturePage: async () => ({ isEmpty: () => true }) });
  const subscribed = [];
  let current = page('first');
  const pip = openGuestPip({
    BrowserWindow: FakeWin, guest: current, title: 't', viewport: { width: 1440, height: 900 }, preload: '',
    sendInput: () => {}, onClosed: () => {}, embedder: null,
    subscribe: () => { subscribed.push(current.name); return () => subscribed.push(`end:${current.name}`); },
  });
  await settle(); await settle();
  assert.deepEqual(subscribed, ['first']);
  current = page('second'); // the reloaded renderer mounted a new guest
  pip.follow(current);
  assert.deepEqual(subscribed, ['first', 'end:second', 'second'], 'the window is subscribed again, to the new page');
  pip.close();
});

test('a page an agent drives copies frames only while the app window is hidden or minimized', () => {
  const state = { visible: false, minimized: false, subscribed: 0, ended: 0 };
  const self = {
    win: { isDestroyed: () => false, isVisible: () => state.visible, isMinimized: () => state.minimized },
    addFrameSubscriber: (_entry, _callback, options) => { assert.equal(options.full, false); state.subscribed += 1; return () => { state.ended += 1; }; },
    hostWindowShowing: BrowserManager.prototype.hostWindowShowing,
    isHosted: () => true,
  };
  const sync = (entry) => BrowserManager.prototype.syncPaintSource.call(self, entry);
  const entry = { painting: true };
  sync(entry);
  assert.equal(state.subscribed, 1, 'hidden app: frames are kept coming');
  state.visible = true;
  sync(entry);
  assert.equal(state.ended, 1, 'app on screen: the page paints by itself');
  state.minimized = true;
  sync(entry);
  assert.equal(state.subscribed, 2, 'minimized counts as hidden');
  entry.painting = false;
  sync(entry);
  assert.equal(state.ended, 2, 'no agent driving: no frames copied');
});
