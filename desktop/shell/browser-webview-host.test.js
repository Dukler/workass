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
  assert.equal(manager.browserTabs('chat-2')[0].id, tabId);
  // A late event from the replaced guest no longer touches the tab.
  first.emit('page-title-updated', {}, 'stale title');
  assert.notEqual(entry.title, 'stale title');
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

test('the browser host is webview by default on macOS, view on Windows, and either stays selectable', () => {
  const files = new Map();
  const fs = { readFileSync: (file) => { if (!files.has(file)) throw new Error('ENOENT'); return files.get(file); } };
  assert.equal(resolveBrowserHost({ env: {}, platform: 'darwin', dataRoot: '/d', fs }).host, 'webview');
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
  assert.equal(resolveBrowserHost({ env: {}, platform: 'darwin', dataRoot: '/d', fs }).host, 'webview');
});

test('the separate window maps clicks and scrolls onto the page and keeps its shape', () => {
  const viewport = { width: 1440, height: 900 };
  assert.deepEqual(pipContentSize(viewport), [420, 263]);
  assert.deepEqual(pipInputToPage({ type: 'mousePressed', button: 0, x: 0.5, y: 0.25 }, viewport),
    { type: 'mousePressed', x: 720, y: 225, button: 'left', clickCount: 1, buttons: 1 });
  assert.deepEqual(pipInputToPage({ type: 'mouseWheel', x: 0, y: 1, deltaX: 0, deltaY: 99999 }, viewport),
    { type: 'mouseWheel', x: 0, y: 900, deltaX: 0, deltaY: 2000 });
  assert.equal(pipInputToPage({ type: 'keyDown', x: 0.5, y: 0.5 }, viewport), null, 'only clicks and scrolls are sent');
  assert.equal(pipInputToPage({ type: 'mousePressed', x: 1.2, y: 0.5 }, viewport), null);
});

test('a page an agent drives copies frames only while the app window is hidden or minimized', () => {
  const state = { visible: false, minimized: false, subscribed: 0, ended: 0 };
  const self = {
    win: { isDestroyed: () => false, isVisible: () => state.visible, isMinimized: () => state.minimized },
    addFrameSubscriber: (_entry, _callback, options) => { assert.equal(options.full, false); state.subscribed += 1; return () => { state.ended += 1; }; },
    hostWindowShowing: BrowserManager.prototype.hostWindowShowing,
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
