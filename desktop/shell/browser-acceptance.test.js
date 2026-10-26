'use strict';

// Acceptance tests for the review of the webview browser branch, written by
// the reviewer apart from the builder's own tests. Each "finding N" test
// fails on the reviewed tip (f6d29cb2) and passes once the finding is fixed;
// each "parity" test pins something main's browser does that the branch must
// keep on both hosts. A test marked todo is an open finding: it fails today
// and its todo comes off with the fix.
//
// Fakes: the shell's own fake page (browser-fake-page.test-helper.js, whose
// page and agent JavaScript worlds are separate as in Chromium) and, for the
// window-level paths, a small native-view host and a <webview> host below.
//
// Parity checklist (main's browser, kept on both hosts):
// - Popups keep their opener; a person's popups open as windows; a popup that
//   needs the person can be seen: tests below (the last is open). Chromium
//   really keeping window.opener through createWindow: manual-only (needs a
//   real Electron run).
// - Downloads: a hidden agent page saves into the chat's folder, a person's
//   download keeps the save dialog, bursts are capped: tests below.
// - JS dialogs pause the page tools and are answered through CDP: test below.
//   Whether Electron also shows its own message box: manual-only.
// - Pop-out typing, hover, drag and selection: native-view host moves the
//   live view into the window, manual-only (needs a GUI); webview host is
//   finding 12, tested when its commit lands.
// - Monaco and CodeMirror read and type: finding 6 tests and the snapshot
//   parity test.
// - Secret redaction in every output (json, compact, outline and its diffs,
//   read, evaluate, dialog messages): tests below.
// - Artifact navigation authorization: navigate authorizes once, page and
//   restore navigations never: tests below.
// - Tab identity across an app renderer reload: both hosts, tests below
//   (page state across it on the webview host is finding 13).
// - The existing tool contracts: cmd/workass/browser_acceptance_test.go.

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { BrowserManager } = require('./browser-manager');
const actions = require('./browser-actions');
const { fakeBrowser } = require('./browser-fake-page.test-helper');

const settle = async (rounds = 6) => { for (let i = 0; i < rounds; i += 1) await new Promise((resolve) => setImmediate(resolve)); };
const within = (promise, ms) => Promise.race([
  Promise.resolve(promise).then((value) => ({ settled: true, value }), (error) => ({ settled: true, error })),
  new Promise((resolve) => setTimeout(() => resolve({ settled: false }), ms)),
]);

// ---------------------------------------------------------------------------
// A native-view host (the 'view' browser host, Windows' default).

let nextContentsId = 7000;

class Debugger extends EventEmitter {
  constructor(owner) { super(); this.owner = owner; this.attached = false; this.calls = []; }
  isAttached() { return this.attached; }
  attach() { this.attached = true; }
  async sendCommand(method, params = {}, sessionId) {
    this.calls.push({ method, params, sessionId });
    const owner = this.owner;
    if (method === 'Target.getTargetInfo') return { targetInfo: { targetId: `target-${owner.id}` } };
    if (method === 'Page.getFrameTree') {
      return { frameTree: { frame: { id: `frame-${owner.id}`, loaderId: `loader-${owner.loads}`, url: owner.url, securityOrigin: 'https://page.example' }, childFrames: [] } };
    }
    if (method === 'Page.createIsolatedWorld') return { executionContextId: 77 };
    if (method === 'Runtime.evaluate') {
      const value = await owner.executeJavaScript(params.expression);
      return { result: value === undefined ? { type: 'undefined' } : { type: typeof value, value } };
    }
    if (method === 'Emulation.setDeviceMetricsOverride') {
      owner.metrics = { ...owner.metrics, width: params.width, height: params.height, deviceScaleFactor: params.deviceScaleFactor };
      return {};
    }
    if (method === 'Accessibility.getFullAXTree') return { nodes: [] };
    return {};
  }
}

class Contents extends EventEmitter {
  constructor(session) {
    super();
    this.id = ++nextContentsId;
    this.session = session;
    this.url = 'about:blank';
    this.loads = 0;
    this.loaded = [];
    this.destroyed = false;
    this.debugger = new Debugger(this);
    this.navigationHistory = { canGoBack: () => false, canGoForward: () => false };
    this.metrics = { width: 1440, height: 900, deviceScaleFactor: 1, scrollX: 0, scrollY: 0, documentWidth: 1440, documentHeight: 900 };
  }
  getType() { return this.type || 'browserView'; }
  getURL() { return this.url; }
  isDestroyed() { return this.destroyed; }
  isLoading() { return false; }
  isLoadingMainFrame() { return false; }
  setUserAgent() {}
  getZoomFactor() { return 1; }
  setZoomFactor() {}
  setWindowOpenHandler(handler) { this.windowOpenHandler = handler; }
  async loadURL(url) {
    this.loads += 1;
    this.loaded.push(url);
    this.emit('did-start-navigation', {}, url, false, true);
    this.url = url;
    this.emit('did-navigate', {}, url);
    this.emit('dom-ready');
    this.emit('did-finish-load');
    this.emit('did-stop-loading');
  }
  async executeJavaScript(script) {
    if (script.includes('location.href, readyState: document.readyState')) return { url: this.url, readyState: 'complete' };
    if (script.includes('width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio')) return { ...this.metrics };
    return { ok: true };
  }
  async executeJavaScriptInIsolatedWorld(_world, scripts) { return this.executeJavaScript(scripts[0].code); }
  stop() {}
  reload() {}
  close() { this.destroy(); }
  destroy() { if (this.destroyed) return; this.destroyed = true; this.emit('destroyed'); }
}

class View {
  constructor(options = {}) {
    this.options = options;
    this.webContents = options.webContents || new Contents(View.profile);
    this.attached = false;
    this.bounds = { x: 0, y: 0, width: 0, height: 0 };
  }
  setBounds(bounds) { this.bounds = { ...bounds }; }
  getBounds() { return { ...this.bounds }; }
  setBackgroundColor() {}
  setBorderRadius(radius) { this.radius = radius; }
}

class Window extends EventEmitter {
  constructor(options = {}) {
    super();
    Window.made.push(this);
    this.options = options;
    const page = new EventEmitter();
    page.ipc = new EventEmitter();
    page.sent = [];
    page.send = (channel, payload) => page.sent.push({ channel, payload });
    page.setWindowOpenHandler = () => {};
    page.isDestroyed = () => this.destroyed;
    this.webContents = page;
    this.destroyed = false;
    this.size = [options.width || 1200, options.height || 800];
    this.contentView = {
      children: [],
      addChildView: (view) => { view.attached = true; view.parent = this; this.contentView.children.push(view); },
      removeChildView: (view) => { view.attached = false; this.contentView.children = this.contentView.children.filter((child) => child !== view); },
    };
  }
  setBounds() {}
  setAlwaysOnTop() {}
  setAspectRatio() {}
  getContentSize() { return this.size; }
  setContentSize(width, height) { this.size = [width, height]; }
  isDestroyed() { return this.destroyed; }
  isVisible() { return false; }
  isMinimized() { return false; }
  show() {}
  focus() {}
  loadURL() { return Promise.resolve(); }
  destroy() { if (this.destroyed) return; this.destroyed = true; this.emit('closed'); }
}

Window.made = [];

function viewHost() {
  const profile = {
    isPersistent: () => true, setUserAgent() {}, setPermissionCheckHandler() {}, setPermissionRequestHandler() {},
    on(name, handler) { this[`on-${name}`] = handler; },
  };
  View.profile = profile;
  const win = new Window({ width: 1200, height: 800 });
  win.webContents = { getZoomFactor: () => 1, executeJavaScript: async () => 1, send() {} };
  const manager = new BrowserManager({
    win, WebContentsView: View, BrowserWindow: Window, session: { fromPartition: () => profile },
    chromeVersion: '150.0.0.0', platform: 'darwin', onState: () => {}, requestOpen: () => {},
  });
  return { manager, win, profile };
}

const PANE = { x: 700, y: 60, width: 480, height: 700 };

// ---------------------------------------------------------------------------
// A <webview> host (the 'webview' browser host, macOS' default). The app
// renderer is simulated: every page the shell publishes gets a guest mounted
// for it, as BrowserSurfaceHost does.

class Guest extends Contents {
  constructor(host, session, { gate = null } = {}) {
    super(session);
    this.type = 'webview';
    this.hostWebContents = host;
    this.gate = gate;
  }
  getType() { return 'webview'; }
  beginFrameSubscription(_onlyDirty, callback) { this.frameCallback = callback; }
  endFrameSubscription() { this.frameCallback = null; }
  setBackgroundThrottling(value) { this.throttling = value; }
  capturePage() { return Promise.resolve(frameImage()); }
  async executeJavaScript(script) {
    if (script.includes('location.href, readyState: document.readyState') && this.gate) await this.gate;
    return super.executeJavaScript(script);
  }
}

function frameImage() {
  return { isEmpty: () => false, getSize: () => ({ width: 1440, height: 900 }), resize() { return this; }, toJPEG: () => Buffer.from('jpeg') };
}

function webviewHost({ autoMount = true, host = 'webview', preferredHost = undefined } = {}) {
  const profile = {
    isPersistent: () => true, setUserAgent() {}, setPermissionCheckHandler() {}, setPermissionRequestHandler() {},
    on(name, handler) { this[`on-${name}`] = handler; },
  };
  const win = new Window({ width: 1200, height: 800 });
  win.webContents = { getZoomFactor: () => 1, executeJavaScript: async () => 1, send() {} };
  const guests = new Map();
  const mounted = new Map();
  let manager = null;
  const mount = (hostKey, options = {}) => {
    const guest = new Guest(win.webContents, profile, options);
    guests.set(guest.id, guest);
    manager.noteAttachedGuest(guest);
    mounted.set(hostKey, guest);
    guest.once('destroyed', () => { if (mounted.get(hostKey) === guest) mounted.delete(hostKey); });
    return { guest, reply: manager.registerGuest(hostKey, guest.id) };
  };
  const published = [];
  manager = new BrowserManager({
    win, WebContentsView: View, BrowserWindow: Window, session: { fromPartition: () => profile },
    chromeVersion: '150.0.0.0', platform: 'darwin', host, ...(preferredHost ? { preferredHost } : {}), onState: () => {}, requestOpen: () => {},
    webContentsFromId: (id) => guests.get(id) || null,
    onHosts: (hosts) => {
      published.push(hosts);
      if (!autoMount) return;
      for (const host of hosts) if (!mounted.has(host.hostKey)) queueMicrotask(() => { if (!mounted.has(host.hostKey)) mount(host.hostKey); });
    },
  });
  manager.guestAttachTimeoutMs = 400;
  return { manager, win, profile, guests, mounted, mount, published };
}

// ---------------------------------------------------------------------------
// The fake page with the globals the shell's page scripts use.

function page(url, build) {
  const fake = fakeBrowser({ url });
  const root = fake.loadRoot(url, build);
  const extras = {
    setTimeout, clearTimeout, performance: { now: () => performance.now() },
    MutationObserver: class { observe() {} disconnect() {} takeRecords() { return []; } },
  };
  Object.assign(root.context, extras);
  if (root.domGlobals) Object.assign(root.domGlobals, extras);
  const { document, view } = root.dom;
  document.addEventListener = () => {};
  document.removeEventListener = () => {};
  view.addEventListener = () => {};
  view.removeEventListener = () => {};
  awaitPromises(fake, root);
  return { fake, root, document, view };
}

// The fake page returns a script's value as is; CDP's awaitPromise waits for
// a promise first. Top frame only (the page world or the agent world).
function awaitPromises(fake, root) {
  const debug = fake.webContents.debugger;
  const send = debug.sendCommand.bind(debug);
  const isolated = new Set();
  debug.sendCommand = async (method, params = {}, sessionId) => {
    if (method !== 'Runtime.evaluate' || !params.awaitPromise || params.returnByValue === false || sessionId) {
      const reply = await send(method, params, sessionId);
      if (method === 'Page.createIsolatedWorld' && reply) isolated.add(reply.executionContextId);
      return reply;
    }
    await send(method, { ...params, expression: `(globalThis.__acceptancePending = (${params.expression}))` }, sessionId);
    const world = isolated.has(params.contextId) ? root.isolatedContext : root.context;
    try {
      const value = await world.__acceptancePending;
      return { result: value === undefined ? { type: 'undefined' } : { type: typeof value, value: JSON.parse(JSON.stringify(value)) } };
    } catch (error) {
      return { exceptionDetails: { text: String(error && error.message || error), exception: { description: String(error) } } };
    } finally {
      delete world.__acceptancePending;
    }
  };
}

function ax(nodeId, role, { name = '', value, backend = 0, children = [], props = {} } = {}) {
  return {
    nodeId: String(nodeId), role: { value: role }, name: { value: name },
    ...(value !== undefined ? { value: { value } } : {}),
    properties: Object.entries(props).map(([key, item]) => ({ name: key, value: { value: item } })),
    backendDOMNodeId: backend, childIds: children.map((child) => child.nodeId),
  };
}

// Elements the type and key scripts can focus, listen on and test with
// instanceof, as in a browser.
function makeEditable(root) {
  const { document, view } = root.dom;
  const tagged = (tag) => ({ [Symbol.hasInstance]: (value) => !!value && value.tagName === tag });
  view.HTMLInputElement = tagged('INPUT');
  view.HTMLTextAreaElement = tagged('TEXTAREA');
  view.HTMLSelectElement = tagged('SELECT');
  view.Event = class { constructor(type, init = {}) { this.type = type; Object.assign(this, init); } };
  view.InputEvent = view.Event;
  view.getSelection = () => ({ rangeCount: 0, removeAllRanges() {}, addRange() {} });
  // The fake DOM's selectors have no descendant combinator; the editor scripts
  // use one ('.view-lines .view-line').
  const descendants = (node) => (node.children || []).flatMap((child) => [child, ...descendants(child)]);
  const selectAll = (scope, selector) => {
    const found = new Set();
    for (const part of String(selector).split(',')) {
      const chain = part.trim().split(/\s+/u);
      for (const candidate of descendants(scope)) {
        if (!candidate.matches(chain[chain.length - 1])) continue;
        let index = chain.length - 2;
        for (let up = candidate.parentElement; up && index >= 0 && up !== scope.parentElement; up = up.parentElement) {
          if (up.matches(chain[index])) index -= 1;
        }
        if (index < 0) found.add(candidate);
      }
    }
    return descendants(scope).filter((candidate) => found.has(candidate));
  };
  const visit = (node) => {
    for (const child of node.children || []) {
      child.querySelectorAll = (selector) => selectAll(child, selector);
      child.querySelector = (selector) => selectAll(child, selector)[0] || null;
      child.focus = function focus() { document.activeElement = this; };
      child.scrollIntoView = () => {};
      child.addEventListener = () => {};
      child.removeEventListener = () => {};
      child.dispatchEvent = () => true;
      child.isContentEditable = false;
      visit(child);
    }
  };
  visit(document.documentElement);
}

// A page with a Monaco editor whose objects live in the page's world only.
function monacoPage({ readOnly = false, value = 'const answer = 41;' } = {}) {
  const made = page('https://code.example/', ({ h, body }) => {
    body.append(h('div', { class: 'monaco-editor', id: 'ed' },
      h('div', { class: 'view-lines' }, h('div', { class: 'view-line' }, 'const answer')),
      h('textarea', { class: 'inputarea', 'aria-label': 'Editor input' })));
  });
  makeEditable(made.root);
  const element = made.document.querySelector('.monaco-editor');
  const input = made.document.querySelector('textarea');
  made.document.activeElement = input;
  const state = { value, selected: false };
  const editor = {
    getDomNode: () => element,
    getModel: () => ({ getValue: () => state.value, getValueLength: () => state.value.length, getFullModelRange: () => ({ whole: true }) }),
    getRawOptions: () => ({ readOnly }),
    focus: () => { made.document.activeElement = input; },
    setSelection: () => { state.selected = true; },
    getSelection: () => ({ isEmpty: () => !state.selected }),
    executeEdits: () => { state.value = ''; state.selected = false; },
  };
  made.root.context.monaco = { editor: { getEditors: () => [editor] } };
  // Trusted text input lands in the editor (its selection replaced).
  const debug = made.fake.webContents.debugger;
  const send = debug.sendCommand.bind(debug);
  debug.sendCommand = (method, params = {}, sessionId) => {
    if (method === 'Input.insertText') {
      state.value = state.selected ? String(params.text) : state.value + String(params.text);
      state.selected = false;
      return Promise.resolve({});
    }
    return send(method, params, sessionId);
  };
  return { ...made, state };
}

// ===========================================================================
// Findings

test('finding 1: a page lost while it starts up never hangs the presentation queue (webview host)', async () => {
  const { manager, mount, published } = webviewHost({ autoMount: false });
  const entry = manager.get('chat-race');
  const hostKey = published.at(-1)[0].hostKey;
  const firstReady = entry.ready;
  const activation = manager.activate({ chatId: 'chat-race', bounds: PANE });
  let open;
  const gate = new Promise((resolve) => { open = resolve; });
  const first = mount(hostKey, { gate });
  assert.equal(first.reply.ok, true);
  await new Promise((resolve) => setTimeout(resolve, 10));
  first.guest.destroy(); // the app renderer reloads while the page sets up
  open();
  const second = mount(hostKey);
  assert.equal(second.reply.ok, true);
  assert.equal((await within(entry.ready, 2000)).value, true, 'the next guest comes up');
  assert.equal((await within(firstReady, 1500)).settled, true, 'the earlier readiness settles too');
  assert.equal((await within(activation, 1500)).settled, true, 'the activation waiting on it returns');
  assert.equal((await within(manager.hide('chat-race'), 1500)).settled, true, 'later presentation steps run');
  manager.destroy();
});

test('finding 2: the outline never shows a secret field\'s value, in a read or in a diff', async () => {
  const { fake, root, document } = page('https://shop.example/pay', ({ h, body }) => {
    body.append(
      h('input', { id: 'otp', autocomplete: 'one-time-code', $value: '482913' }),
      h('input', { id: 'card', autocomplete: 'cc-number', $value: '4111 1111 1111 1111' }),
      h('input', { id: 'cvv', name: 'cvv', $value: '737' }),
      h('input', { id: 'pw', type: 'text', name: 'password', $value: 'hunter2-Correct' }),
      h('input', { id: 'key', name: 'api_key', $value: 'k-29f8e1' }),
      h('input', { id: 'city', name: 'city', $value: 'Montevideo' }),
    );
  });
  const field = (id, name, value) => ax(id, 'textbox', { name, value, backend: fake.backendIdFor(document.getElementById(id)), props: { focusable: true } });
  const nodes = [field('otp', 'One-time code', '482913'), field('card', 'Card number', '4111 1111 1111 1111'),
    field('cvv', 'CVV', '737'), field('pw', 'Password', 'hunter2-Correct'), field('key', 'API key', 'k-29f8e1'), field('city', 'City', 'Montevideo')];
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Pay', children: nodes }), ...nodes];
  const first = await fake.manager.browserOutline(fake.entry, {});
  for (const secret of ['482913', '4111', '737', 'hunter2', 'k-29f8e1']) assert.doesNotMatch(first.outline, new RegExp(secret), secret);
  assert.match(first.outline, /textbox "City" \[e\d+\] value="Montevideo"/, 'an ordinary field keeps its value');
  assert.equal((first.outline.match(/value=\[redacted\]/gu) || []).length, 5);
  // A new value arrives; the diff since the first outline says it changed,
  // without the value.
  nodes[0].value = { value: '000111' };
  nodes[3].value = { value: 'hunter3-Correct' };
  const diff = await fake.manager.browserOutline(fake.entry, { sinceSnapshotId: first.outline_id });
  assert.doesNotMatch(JSON.stringify(diff), /000111|hunter3/);
});

test('finding 2, open: a secret field\'s inner text and its chosen option stay hidden too', async () => {
  const { fake, root, document } = page('https://shop.example/pay', ({ h, body }) => {
    body.append(
      h('input', { id: 'code', role: 'combobox', autocomplete: 'one-time-code', $value: '482913' }),
      h('select', { id: 'month', autocomplete: 'cc-exp-month' }, h('option', {}, '01'), h('option', { $selected: true }, '07')),
    );
  });
  const backend = (id) => fake.backendIdFor(document.getElementById(id));
  // An editable combobox <input> carries its inner editor's text; a closed
  // <select> lists its options with the chosen one selected.
  const inner = ax('t1', 'StaticText', { name: '482913' });
  const editorBox = ax('g1', 'generic', { children: [inner] });
  const code = ax('code', 'combobox', { name: 'Code', value: '482913', backend: backend('code'), props: { editable: 'plaintext', focusable: true }, children: [editorBox] });
  const optionA = ax('o1', 'option', { name: '01', backend: 0, props: { selected: false } });
  const optionB = ax('o2', 'option', { name: '07', backend: 0, props: { selected: true } });
  const popup = ax('p1', 'MenuListPopup', { children: [optionA, optionB] });
  const month = ax('month', 'combobox', { name: 'Expiry month', value: '07', backend: backend('month'), props: { focusable: true }, children: [popup] });
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Pay', children: [code, month] }), code, editorBox, inner, month, popup, optionA, optionB];
  const outline = await fake.manager.browserOutline(fake.entry, {});
  assert.doesNotMatch(outline.outline, /482913/, 'the combobox input\'s own text');
  assert.doesNotMatch(outline.outline, /"07" selected|07.*selected/, 'the chosen expiry month');
});

test('finding 3: uploads refuse a file reached through a linked folder and unlisted token stores', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'wv-acc-'));
  try {
    const kube = path.join(home, '.kube');
    fs.mkdirSync(kube);
    fs.writeFileSync(path.join(kube, 'config'), 'token: abc\n');
    fs.symlinkSync(kube, path.join(home, 'innocent'));
    assert.throws(() => actions.checkUploadPaths([path.join(home, 'innocent', 'config')]), /refused/);
    const gh = path.join(home, '.config', 'gh');
    fs.mkdirSync(gh, { recursive: true });
    fs.writeFileSync(path.join(gh, 'hosts.yml'), 'oauth_token: gho_x\n');
    assert.throws(() => actions.checkUploadPaths([path.join(gh, 'hosts.yml')]), /refused/);
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('finding 4: a popup an agent-driven page opens keeps its opener (createWindow) and becomes a tab', async () => {
  const { manager, profile } = viewHost();
  const tab = await manager.browserControl('browser.open', { chatId: 'chat-pop', visible: false });
  const entry = manager.browserEntries().find((candidate) => candidate.chatId === 'chat-pop');
  const reply = entry.view.webContents.windowOpenHandler({ url: 'https://accounts.example/oauth', disposition: 'new-window', frameName: '', features: '' });
  assert.equal(reply.action, 'allow', 'window.open must not return null to the page');
  assert.equal(typeof reply.createWindow, 'function');
  const made = new Contents(profile); // Chromium's webContents for the popup, opener attached
  assert.equal(reply.createWindow({ webContents: made, webPreferences: {} }), made);
  const popup = manager.browserTabs('chat-pop').find((candidate) => candidate.opened_by_tab_id === tab.id);
  assert.ok(popup, 'the popup is a tab of the chat');
  assert.equal(popup.id, made.id, 'the tab is the very webContents Chromium made');
  manager.destroy();
});

test('finding 4, open: a window.open popup of an agent-driven page can still be seen and used by the person, as on main', async () => {
  const { manager, profile } = viewHost();
  await manager.browserControl('browser.open', { chatId: 'chat-auth', visible: false });
  const entry = manager.browserEntries().find((candidate) => candidate.chatId === 'chat-auth');
  const before = Window.made.length;
  const reply = entry.view.webContents.windowOpenHandler({ url: 'https://accounts.example/approve', disposition: 'new-window', frameName: '', features: '' });
  assert.equal(reply.action, 'allow');
  let shown = !reply.createWindow && (!reply.overrideBrowserWindowOptions || reply.overrideBrowserWindowOptions.show !== false);
  if (reply.createWindow) {
    const made = new Contents(profile);
    reply.createWindow({ webContents: made, webPreferences: {} });
    await settle();
    // A sign-in that needs the person (two-factor, a captcha, a passkey)
    // happens in a window they can see.
    shown = Window.made.slice(before).some((win) => win.options.show !== false && win.contentView.children.some((view) => view.webContents === made));
  }
  assert.ok(shown, 'the popup is only a hidden tab');
  manager.destroy();
});

test('finding 4: reading a page the person is looking at does not take their popups away', async () => {
  const { manager } = viewHost();
  await manager.activate({ chatId: 'chat-look', bounds: PANE });
  const entry = manager.entries.get('chat-look');
  entry.agentActiveAt = 0;
  for (const method of ['browser.diagnostics', 'browser.list']) await manager.browserControl(method, { chatId: 'chat-look' });
  const reply = entry.view.webContents.windowOpenHandler({ url: 'https://docs.example/guide', disposition: 'foreground-tab' });
  assert.equal(reply.action, 'allow');
  assert.equal(reply.createWindow, undefined, 'a person\'s popup opens as its own window');
  assert.equal(reply.overrideBrowserWindowOptions.width, 980);
  manager.destroy();
});

test('finding 4: a popped-out page is in front of the person', () => {
  assert.equal(actions.agentDriven({ visible: false, pip: { window: {} }, agentActiveAt: 0 }), false);
  assert.equal(actions.agentDriven({ visible: false, pipWindow: { isDestroyed: () => false }, agentActiveAt: 0 }), false);
  assert.equal(actions.agentDriven({ visible: false, agentActiveAt: 0 }), true, 'a hidden page is the agent\'s');
});

test('finding 5: a compact ref used without its snapshot_id never reaches an outline element', async () => {
  const { fake, root, document } = page('https://shop.example/', ({ h, body }) => {
    body.append(h('button', { id: 'keep' }, 'Keep'), h('button', { id: 'delete' }, 'Delete everything'));
  });
  const keep = ax('k', 'button', { name: 'Keep', backend: fake.backendIdFor(document.getElementById('keep')), props: { focusable: true } });
  const del = ax('d', 'button', { name: 'Delete everything', backend: fake.backendIdFor(document.getElementById('delete')), props: { focusable: true } });
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Shop', children: [del, keep] }), del, keep];
  await fake.manager.browserOutline(fake.entry, {}); // e1, e2 now name outline elements
  const compact = await fake.manager.browserControl('browser.snapshot', { tabId: 4242, format: 'compact' }, { serialized: true });
  const refs = compact.elements.split('\n').map((line) => line.split(' ')[0]).filter(Boolean);
  assert.ok(refs.length >= 2, compact.elements);
  for (const ref of refs) {
    const target = await fake.manager.observationTarget(fake.entry, { elementRef: ref }).catch((error) => ({ error: error.message }));
    assert.equal(target.backendDOMNodeId, undefined, `${ref} of the compact snapshot resolved to an outline element`);
  }
});

test('finding 6: key handling reaches a Monaco editor\'s model through the page world', async () => {
  const { fake, state } = monacoPage();
  await fake.manager.refreshFrameTree(fake.entry);
  const length = await fake.manager.focusedTextLength(fake.entry, null);
  assert.deepEqual(length, { found: true, length: state.value.length });
  const selected = await fake.manager.selectAllBrowserTarget(fake.entry, null);
  assert.equal(selected.strategy, 'monaco-model');
  assert.equal(state.selected, true);
});

test('finding 6: typing into Monaco is verified against its model', async () => {
  const { fake, state } = monacoPage();
  await fake.manager.refreshFrameTree(fake.entry);
  const result = await fake.manager.browserControl('browser.type', { tabId: 4242, selector: '#ed', text: 'const answer = 42;' });
  assert.equal(state.value, 'const answer = 42;');
  assert.equal(result.verification, 'model');
  assert.equal(result.replacementVerified, true);
  assert.equal(result.valueLength, 18);
});

test('finding 6: a read-only Monaco editor is refused, not typed into', async () => {
  const { fake, state } = monacoPage({ readOnly: true, value: 'SELECT 1;' });
  await fake.manager.refreshFrameTree(fake.entry);
  await assert.rejects(fake.manager.browserControl('browser.type', { tabId: 4242, selector: '#ed', text: 'DROP TABLE x;' }), /read-only/);
  assert.equal(state.value, 'SELECT 1;');
});

test('finding 6: a CodeMirror instance (an expando of the page world) is used through the page world', async () => {
  const { fake, root, document } = page('https://code.example/', ({ h, body }) => {
    body.append(h('div', { class: 'CodeMirror', id: 'cm' }, h('textarea', { 'aria-label': 'Code' })));
  });
  makeEditable(root);
  const element = document.querySelector('.CodeMirror');
  document.activeElement = document.querySelector('textarea');
  let value = 'print(1)';
  let selected = false;
  const instance = {
    getValue: () => value, getOption: () => false, focus() {},
    execCommand: (name) => { if (name === 'selectAll') selected = true; },
    somethingSelected: () => selected, replaceSelection: (text) => { value = text; selected = false; },
  };
  // An expando the page set: visible to page-world code only.
  const debug = fake.webContents.debugger;
  const send = debug.sendCommand.bind(debug);
  const isolated = new Set();
  const pageObjects = new Set();
  let pageWorld = false;
  Object.defineProperty(element, 'CodeMirror', { get: () => (pageWorld ? instance : undefined) });
  debug.sendCommand = async (method, params = {}, sessionId) => {
    const inPage = (method === 'Runtime.evaluate' && !isolated.has(params.contextId)) ||
      (method === 'Runtime.callFunctionOn' && pageObjects.has(params.objectId));
    pageWorld = inPage;
    let pending;
    try { pending = send(method, params, sessionId); } finally { pageWorld = false; }
    const reply = await pending;
    if (method === 'Page.createIsolatedWorld') isolated.add(reply.executionContextId);
    if (method === 'DOM.resolveNode' && !isolated.has(params.executionContextId) && reply && reply.object) pageObjects.add(reply.object.objectId);
    return reply;
  };
  await fake.manager.refreshFrameTree(fake.entry);
  assert.deepEqual(await fake.manager.focusedTextLength(fake.entry, null), { found: true, length: 8 });
  assert.equal((await fake.manager.selectAllBrowserTarget(fake.entry, null)).strategy, 'codemirror-command');
  assert.equal((await fake.manager.deleteSelectedBrowserText(fake.entry, null, 'Backspace')).strategy, 'codemirror-selection');
  assert.equal(value, '');
});

test('finding 7: an explicit snapshot reads a value a script set without any mutation or event', async () => {
  const { fake, root, document } = page('https://shop.example/cart', ({ h, body }) => {
    body.append(h('input', { id: 'total', name: 'total', 'aria-label': 'Total', $value: '10.00' }));
  });
  const total = ax('t', 'textbox', { name: 'Total', value: '10.00', backend: fake.backendIdFor(document.getElementById('total')), props: { focusable: true } });
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Cart', children: [total] }), total];
  await fake.manager.refreshFrameTree(fake.entry);
  assert.match((await fake.manager.browserOutline(fake.entry, {})).outline, /10\.00/);
  document.getElementById('total').value = '42.00'; // page script: a property write
  total.value = { value: '42.00' };
  assert.match((await fake.manager.browserOutline(fake.entry, {})).outline, /42\.00/);
});

test('finding 7: an action whose only effect is a property write still reports the change', async () => {
  const { fake, root, document } = page('https://shop.example/cart', ({ h, body }) => {
    body.append(h('input', { id: 'total', name: 'total', 'aria-label': 'Total', $value: '10.00' }));
  });
  const total = ax('t', 'textbox', { name: 'Total', value: '10.00', backend: fake.backendIdFor(document.getElementById('total')), props: { focusable: true } });
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Cart', children: [total] }), total];
  await fake.manager.refreshFrameTree(fake.entry);
  await fake.manager.browserOutline(fake.entry, {});
  assert.equal(typeof document[Symbol.for('workass.activity')].n, 'number', 'the activity counter runs');
  const report = await actions.runAction(fake.manager, fake.entry, async () => {
    document.getElementById('total').value = '99.00';
    total.value = { value: '99.00' };
    return { ok: true, status: 'clicked' };
  }, { method: 'browser.click' });
  assert.notEqual(report.changes.none, true, 'the page did change');
  assert.match(report.changes.text, /99\.00/);
});

test('finding 8: resetting the page a person is looking at keeps the pane alive (native-view host)', async () => {
  const { manager, win } = viewHost();
  await manager.activate({ chatId: 'chat-reset', bounds: PANE });
  const before = manager.entries.get('chat-reset');
  assert.ok(win.contentView.children.includes(before.view));
  const result = await manager.browserControl('browser.close', { chatId: 'chat-reset' });
  assert.equal(result.status, 'reset');
  const fresh = manager.entries.get('chat-reset');
  assert.notEqual(fresh, before);
  assert.ok(win.contentView.children.includes(fresh.view), 'the fresh page is in the pane');
  assert.equal(await manager.resize('chat-reset', { ...PANE, width: 420 }), true, 'pane resizes still reach the page');
  manager.destroy();
});

test('finding 8: resetting the page a person is looking at keeps the pane alive (webview host)', async () => {
  const { manager } = webviewHost();
  await manager.activate({ chatId: 'chat-reset-w', bounds: PANE });
  const before = manager.entries.get('chat-reset-w');
  assert.equal(before.visible, true);
  const result = await manager.browserControl('browser.close', { chatId: 'chat-reset-w' });
  assert.equal(result.status, 'reset');
  const fresh = manager.entries.get('chat-reset-w');
  assert.notEqual(fresh, before);
  assert.equal(fresh.visible, true, 'the fresh page counts as in front of the person');
  assert.equal(await manager.resize('chat-reset-w', { ...PANE, width: 420 }), true, 'pane resizes still reach the page');
  manager.destroy();
});

test('finding 9: the tabs a page opened close with it', async () => {
  const { manager, profile } = viewHost();
  await manager.browserControl('browser.open', { chatId: 'chat-owner', visible: false });
  const owner = manager.browserEntries().find((candidate) => candidate.chatId === 'chat-owner');
  for (let index = 0; index < 2; index += 1) {
    const reply = owner.view.webContents.windowOpenHandler({ url: `https://pop.example/${index}`, disposition: 'foreground-tab' });
    if (reply.createWindow) reply.createWindow({ webContents: new Contents(profile), webPreferences: {} });
  }
  await settle();
  assert.equal(manager.browserEntries().filter((candidate) => candidate.extraTab).length, 2);
  manager.close('chat-owner');
  assert.deepEqual(manager.browserEntries().filter((candidate) => candidate.extraTab).map((candidate) => candidate.chatId), []);
  manager.destroy();
});

test('finding 10: the webview host starts only when the app renderer asks for it', async () => {
  // main.js: native views until the renderer asks (an older renderer never does).
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.doesNotMatch(source, /host:\s*browserHost\.host/, 'the shell must not start in the webview host on its own');
  assert.match(source, /preferredHost:\s*browserHost\.host/);
  assert.match(source, /rendererHostMode\(\)/);
  const { manager, published, mounted } = webviewHost({ host: 'view', preferredHost: 'webview' });
  // An older renderer: its pages are native views and come up without a guest.
  const older = await manager.browserControl('browser.open', { chatId: 'chat-old', visible: false });
  assert.equal(older.host, 'view');
  assert.equal(published.length, 0);
  // A renderer that can host pages asks; pages made from then on are hosted.
  assert.equal(manager.rendererHostMode(), 'webview');
  const hosted = await manager.browserControl('browser.open', { chatId: 'chat-new', visible: false });
  assert.equal(hosted.host, 'webview');
  assert.ok(mounted.size >= 1);
  // The page made before still works as a native view.
  await manager.activate({ chatId: manager.browserEntries().find((entry) => entry.chatId === 'chat-old').chatId, bounds: PANE });
  assert.ok(manager.win.contentView.children.length >= 1);
  // Windows keeps native views whatever the renderer says.
  const windows = webviewHost({ host: 'view', preferredHost: 'view' });
  assert.equal(windows.manager.rendererHostMode(), 'view');
  manager.destroy();
  windows.manager.destroy();
});

async function poppedOut() {
  const made = webviewHost();
  const entry = made.manager.get('chat-pip');
  assert.equal(await entry.ready, true);
  const before = Window.made.length;
  await made.manager.popOut('chat-pip');
  const pip = Window.made.slice(before).find((win) => win.options.alwaysOnTop);
  assert.ok(pip, 'the pop-out window opened');
  pip.webContents.emit('did-finish-load');
  await settle();
  return { ...made, entry, pip };
}

test('finding 11: the pop-out window follows the page across an app renderer reload (webview host)', async () => {
  const { manager, entry, pip, mounted } = await poppedOut();
  const first = entry.view.webContents;
  assert.equal(typeof first.frameCallback, 'function', 'frames flow from the page');
  first.destroy(); // the app renderer reloads
  await settle();
  assert.equal(await entry.ready, true);
  await settle();
  const second = mounted.get(entry.hostKey);
  assert.ok(second && second !== first);
  assert.equal(typeof second.frameCallback, 'function', 'the window shows the page that came back');
  const sent = pip.webContents.sent.length;
  second.frameCallback(frameImage(), { x: 0, y: 0, width: 10, height: 10 });
  await new Promise((resolve) => setTimeout(resolve, 120));
  assert.ok(pip.webContents.sent.slice(sent).some((message) => message.channel === 'workass-pip:frame'));
  manager.destroy();
});

test('finding 12: the pop-out window takes typing, hover, drags and double clicks (webview host)', async () => {
  const { manager, entry, pip } = await poppedOut();
  const guest = entry.view.webContents;
  const [onInput] = pip.webContents.ipc.listeners('workass-pip:input');
  assert.equal(typeof onInput, 'function');
  const send = (input) => onInput({ sender: pip.webContents }, input);
  send({ type: 'mouseMoved', x: 0.5, y: 0.5, buttons: 0 });
  send({ type: 'mousePressed', x: 0.1, y: 0.1, button: 0, buttons: 1, clickCount: 2 });
  send({ type: 'mouseMoved', x: 0.3, y: 0.1, buttons: 1 });
  send({ type: 'mouseReleased', x: 0.3, y: 0.1, button: 0, buttons: 0, clickCount: 2 });
  send({ type: 'keyDown', key: 'a', code: 'KeyA', keyCode: 65, text: 'a' });
  send({ type: 'keyUp', key: 'a', code: 'KeyA', keyCode: 65 });
  send({ type: 'insertText', text: 'ñandú' });
  await settle(10);
  const input = guest.debugger.calls.filter((call) => call.method.startsWith('Input.'));
  const mouse = input.filter((call) => call.method === 'Input.dispatchMouseEvent').map((call) => call.params);
  assert.ok(mouse.some((event) => event.type === 'mouseMoved' && event.buttons === 0), 'hover');
  assert.ok(mouse.some((event) => event.type === 'mouseMoved' && event.buttons === 1 && event.button === 'left'), 'a drag or a text selection');
  assert.ok(mouse.some((event) => event.type === 'mousePressed' && event.clickCount === 2), 'a double click');
  const keys = input.filter((call) => call.method === 'Input.dispatchKeyEvent').map((call) => call.params);
  assert.ok(keys.some((event) => event.type === 'keyDown' && event.text === 'a'), 'typing');
  assert.ok(keys.some((event) => event.type === 'keyUp' && event.key === 'a'));
  assert.ok(input.some((call) => call.method === 'Input.insertText' && call.params.text === 'ñandú'), 'composed text');
  // Nothing but input reaches the page from the window.
  const count = guest.debugger.calls.length;
  send({ type: 'Runtime.evaluate', expression: 'alert(1)' });
  onInput({ sender: {} }, { type: 'keyDown', key: 'b', code: 'KeyB', keyCode: 66, text: 'b' });
  await settle(10);
  assert.equal(guest.debugger.calls.length, count);
  manager.destroy();
});

test('finding 14: top-frame commands honour the caller\'s time limit; a frame without a script context is refused', async () => {
  const entry = { targetSessions: new Map(), view: { webContents: { debugger: { sendCommand: () => new Promise(() => {}) } } } };
  const bounded = BrowserManager.prototype.ownedSessionCommand.call({}, entry, 'Runtime.evaluate', { expression: 'new Promise(() => {})' }, null, 60);
  const outcome = await within(bounded, 600);
  assert.equal(outcome.settled, true, 'a top-frame command with a limit gives up');
  assert.ok(outcome.error);

  const { fake, root, document } = page('https://shop.example/', ({ h, body }) => body.append(h('p', { id: 'top' }, 'top')));
  fake.setFrame({ id: 'child', parentId: 'root', frameUrl: 'https://shop.example/child', kind: 'same-process', build: ({ h, body }) => body.append(h('p', {}, 'child')) });
  fake.entry.executionContexts.delete(`${fake.entry.rootTargetId}:child`);
  await fake.manager.refreshFrameTree(fake.entry);
  const child = fake.entry.frames.get('child');
  assert.ok(child && child.contextId == null);
  fake.entry.refMap = { documentKey: fake.manager.documentKey(fake.entry), byKey: new Map(), byRef: new Map([['e1', { frameId: 'child', loaderId: child.loaderId, backendNodeId: 1, role: 'generic', name: '' }]]), next: 2 };
  root.context.whereAmI = 'top';
  const result = await actions.browserEvaluate(fake.manager, fake.entry, { expression: 'typeof whereAmI === "string" ? whereAmI : "child"', elementRef: 'e1' });
  assert.notEqual(result.page_data, '"top"', 'the expression ran in the parent frame');
  assert.equal(result.status, 'unavailable');
  void document;
});

test('finding 15: a dialog in the middle of a batch stops it and says how far it got', async () => {
  const { fake, view } = page('https://shop.example/', ({ h, body }) => body.append(h('p', {}, 'page')));
  let scrolls = 0;
  view.scrollBy = () => { scrolls += 1; };
  let resume;
  const paused = new Promise((resolve) => { resume = resolve; });
  const debug = fake.webContents.debugger;
  const send = debug.sendCommand.bind(debug);
  debug.sendCommand = (method, params = {}, sessionId) => {
    if (method === 'Runtime.evaluate' && String(params.expression).includes('window.scrollBy(') && scrolls === 1) {
      // The second scroll's page script opens an alert: the page pauses.
      scrolls += 1;
      actions.dialogOpened(fake.entry, { type: 'alert', message: 'Saved' }, null);
      return paused.then(() => ({ result: { value: { changed: false } } }));
    }
    return send(method, params, sessionId);
  };
  const steps = [{ action: 'scroll', x: 0, y: 10 }, { action: 'scroll', x: 0, y: 20 }, { action: 'scroll', x: 0, y: 30 }];
  let report;
  try {
    report = await fake.manager.browserControl('browser.batch', { tabId: 4242, actions: steps }, { report: true });
  } finally {
    actions.dialogClosed(fake.entry, { result: true });
    resume();
  }
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(report.status, 'dialog_opened');
  assert.deepEqual(report.batch && report.batch.completed_indexes, [0]);
  assert.equal(report.batch.dialog_index, 1);
  assert.deepEqual(report.batch.unexecuted_indexes, [2]);
  assert.equal(scrolls, 2, 'nothing after the dialog ran once it was answered');
});

test('finding 16: an agent-driven page cannot fill the disk with downloads', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wv-acc-dl-'));
  try {
    const entry = { chatId: 'chat-dl', visible: false, agentActiveAt: 0 };
    const items = [];
    for (let index = 0; index < 14; index += 1) {
      const item = new EventEmitter();
      Object.assign(item, {
        getFilename: () => `file-${index}.bin`, getURL: () => 'https://dl.example/f', getTotalBytes: () => 1024,
        getReceivedBytes: () => 0, setSavePath(file) { this.savePath = file; }, cancel() { this.cancelled = true; },
      });
      actions.takeDownload({ downloadsRoot: root }, entry, item);
      items.push(item);
    }
    assert.ok(items.filter((item) => item.cancelled).length >= 4, 'a burst of downloads is cut off');
    assert.ok((entry.events || []).some((event) => event.type === 'download_blocked'));
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('finding 17: a navigation that runs out of time reports the page as loading instead of failing', async () => {
  const { fake } = page('https://slow.example/', ({ h, body }) => body.append(h('p', {}, 'old')));
  fake.entry.mainDocumentReady = false;
  fake.entry.loading = true;
  const navigate = async () => ({ id: 4242, loading: true });
  const started = Date.now();
  const outcome = await fake.manager.serialize(fake.entry,
    () => actions.runAction(fake.manager, fake.entry, navigate, { before: null, method: 'browser.navigate' }), { deadlineMs: 3000 })
    .then((value) => ({ value }), (error) => ({ error }));
  assert.equal(outcome.error, undefined, outcome.error && outcome.error.message);
  assert.equal(outcome.value.page.loading, true);
  assert.ok(Date.now() - started < 3000);
});

test('finding 18: restoring a page after an app reload never authorizes it like a navigate command', async () => {
  const { manager, mounted } = webviewHost();
  const entry = manager.get('chat-art');
  assert.equal(await entry.ready, true);
  const first = entry.view.webContents;
  const artifact = 'http://127.0.0.1:8799/workass/artifacts/@mac/proj/notes.html';
  first.url = artifact;
  first.emit('did-navigate', {}, artifact); // the page sent itself there
  assert.equal(manager.consumeArtifactNavigation(first, artifact), false);
  first.destroy(); // the app renderer reloads
  await settle();
  assert.equal(await entry.ready, true);
  await settle();
  const second = mounted.get(entry.hostKey);
  assert.ok(second && second !== first);
  assert.ok(second.loaded.includes(artifact), 'the page comes back at its URL');
  assert.equal(manager.consumeArtifactNavigation(second, artifact), false);
  manager.destroy();
});

// ===========================================================================
// Parity with main, on both hosts

test('parity: uploads still take an ordinary file (macOS /tmp and /var aliases included), never a hard link', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wv-acc-up-'));
  try {
    const file = path.join(dir, 'invoice.pdf');
    fs.writeFileSync(file, '%PDF-1.4\n');
    assert.deepEqual(actions.checkUploadPaths([file]).map((item) => path.basename(item.file)), ['invoice.pdf']);
    if (fs.existsSync('/tmp') && fs.realpathSync('/tmp') !== '/tmp') {
      const viaAlias = fs.mkdtempSync('/tmp/wv-acc-alias-');
      try {
        fs.writeFileSync(path.join(viaAlias, 'photo.png'), 'png');
        assert.equal(actions.checkUploadPaths([path.join(viaAlias, 'photo.png')]).length, 1);
      } finally { fs.rmSync(viaAlias, { recursive: true, force: true }); }
    }
    fs.linkSync(file, path.join(dir, 'other-name.pdf'));
    assert.throws(() => actions.checkUploadPaths([file]), /hard links/);
    assert.throws(() => actions.checkUploadPaths([path.join(dir, '.env')]), /refused|no such file/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('parity: downloads of a hidden agent page save into the chat folder; a person\'s keep the save dialog', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wv-acc-dl-'));
  try {
    const item = new EventEmitter();
    Object.assign(item, {
      getFilename: () => '../report.csv', getURL: () => 'https://dl.example/r', getTotalBytes: () => 10,
      getReceivedBytes: () => 0, setSavePath(file) { this.savePath = file; }, cancel() { this.cancelled = true; },
    });
    assert.equal(actions.takeDownload({ downloadsRoot: root }, { chatId: 'chat-a', visible: false, agentActiveAt: 0 }, item), true);
    assert.equal(path.dirname(item.savePath), actions.downloadsDirFor(root, 'chat-a'));
    assert.equal(path.basename(item.savePath), 'report.csv');
    const person = new EventEmitter();
    Object.assign(person, { getFilename: () => 'x', getURL: () => 'https://x', getTotalBytes: () => 1, getReceivedBytes: () => 0, setSavePath() { throw new Error('no'); }, cancel() {} });
    assert.equal(actions.takeDownload({ downloadsRoot: root }, { chatId: 'chat-a', visible: true, agentActiveAt: 0 }, person), false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('parity: a JS dialog pauses the page tools and is answered through CDP', async () => {
  const { fake } = page('https://shop.example/', ({ h, body }) => body.append(h('p', {}, 'page')));
  const debug = fake.webContents.debugger;
  const send = debug.sendCommand.bind(debug);
  const answered = [];
  debug.sendCommand = (method, params = {}, sessionId) => {
    if (method === 'Page.handleJavaScriptDialog') { answered.push(params); actions.dialogClosed(fake.entry, { result: params.accept }); return Promise.resolve({}); }
    return send(method, params, sessionId);
  };
  assert.equal((await actions.browserDialog(fake.manager, fake.entry, { accept: true })).status, 'no_dialog');
  actions.dialogOpened(fake.entry, { type: 'confirm', message: 'Delete token=abc123 for real?' }, null);
  await assert.rejects(fake.manager.browserControl('browser.click', { tabId: 4242, selector: 'p' }), /confirm dialog is open/);
  assert.doesNotMatch(JSON.stringify(fake.entry.dialog.public), /abc123/, 'the dialog message is redacted');
  const reply = await actions.browserDialog(fake.manager, fake.entry, { accept: false });
  assert.equal(reply.status, 'dismissed');
  assert.deepEqual(answered, [{ accept: false }]);
  assert.equal(fake.entry.dialog, null);
});

test('parity: secret values stay out of the json and compact snapshots, the outline and read', async () => {
  const { fake, root, document } = page('https://bank.example/login', ({ h, body }) => {
    body.append(
      h('input', { id: 'user', name: 'user', 'aria-label': 'User', $value: 'ada' }),
      h('input', { id: 'pw', type: 'password', name: 'pw', 'aria-label': 'Password', $value: 'hunter2-SECRET' }),
      h('input', { id: 'otp', autocomplete: 'one-time-code', 'aria-label': 'Code', $value: '991177' }),
      h('p', {}, 'Your key: sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWX'),
    );
  });
  const field = (id, name, value) => ax(id, 'textbox', { name, value, backend: fake.backendIdFor(document.getElementById(id)), props: { focusable: true } });
  const nodes = [field('user', 'User', 'ada'), field('pw', 'Password', '••••••••••••••'), field('otp', 'Code', '991177'),
    ax('txt', 'StaticText', { name: 'Your key: sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWX' })];
  root.axNodes = [ax('root', 'RootWebArea', { name: 'Login', children: nodes }), ...nodes];
  const outputs = {
    outline: await fake.manager.browserOutline(fake.entry, {}),
    json: await fake.manager.browserControl('browser.snapshot', { tabId: 4242 }, { serialized: true }),
    compact: await fake.manager.browserControl('browser.snapshot', { tabId: 4242, format: 'compact' }, { serialized: true }),
    read: await fake.manager.browserControl('browser.read', { tabId: 4242 }, { serialized: true }),
  };
  for (const [name, output] of Object.entries(outputs)) {
    const text = JSON.stringify(output);
    for (const secret of ['hunter2-SECRET', '991177', 'sk-ant-api03-ABCDEFGHIJ']) assert.doesNotMatch(text, new RegExp(secret), `${name} shows ${secret}`);
  }
  assert.match(JSON.stringify(outputs.outline), /ada/, 'ordinary values stay');
});

test('parity: only a navigate command authorizes an artifact URL (native-view host)', async () => {
  const { manager } = viewHost();
  await manager.activate({ chatId: 'chat-nav', bounds: PANE });
  const entry = manager.entries.get('chat-nav');
  const wc = entry.view.webContents;
  const artifact = 'http://127.0.0.1:8799/workass/artifacts/@mac/proj/report.html';
  await manager.command('chat-nav', 'navigate', artifact);
  assert.equal(manager.consumeArtifactNavigation(wc, artifact), true);
  assert.equal(manager.consumeArtifactNavigation(wc, artifact), false, 'once');
  wc.emit('did-navigate', {}, `${artifact}?again`);
  assert.equal(manager.consumeArtifactNavigation(wc, `${artifact}?again`), false, 'a page-initiated navigation is never authorized');
  manager.destroy();
});

test('parity: a tab keeps its id and its CDP route across an app renderer reload (webview host)', async () => {
  const { manager, mounted } = webviewHost();
  const opened = await manager.browserControl('browser.open', { chatId: 'chat-id', visible: false });
  const entry = manager.browserEntries().find((candidate) => candidate.chatId === 'chat-id');
  const first = entry.view.webContents;
  first.destroy();
  await settle();
  assert.equal(await entry.ready, true);
  await settle();
  const second = mounted.get(entry.hostKey);
  assert.notEqual(second, first);
  assert.equal(manager.browserTabs('chat-id')[0].id, opened.id);
  await manager.executeCDP({ tabId: opened.id }, 'Page.reload', {});
  assert.ok(second.debugger.calls.some((call) => call.method === 'Page.reload'), 'the old tab id drives the new page');
  manager.destroy();
});

test('parity: an app renderer reload leaves a native-view page alive, same tab, not reloaded (native-view host)', async () => {
  const { manager, win } = viewHost();
  const shown = await manager.activate({ chatId: 'chat-keep', bounds: PANE });
  const entry = manager.entries.get('chat-keep');
  const wc = entry.view.webContents;
  const loads = wc.loads;
  await manager.hideAll(); // the renderer that presented it starts reloading
  assert.equal(win.contentView.children.includes(entry.view), false, 'nothing stays on screen without its controls');
  const again = await manager.activate({ chatId: 'chat-keep', bounds: PANE });
  assert.equal(manager.entries.get('chat-keep').view.webContents, wc);
  assert.equal(wc.loads, loads, 'the page was not loaded again');
  assert.equal(again.tabId ?? manager.tabIdOf(entry), shown.tabId ?? manager.tabIdOf(entry));
  assert.ok(win.contentView.children.includes(entry.view));
  manager.destroy();
});

test('parity: evaluate\'s page_data passes credential redaction', async () => {
  const { fake } = page('https://shop.example/', ({ h, body }) => body.append(h('p', {}, 'page')));
  await fake.manager.refreshFrameTree(fake.entry);
  const result = await actions.browserEvaluate(fake.manager, fake.entry, { expression: '"key=sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWX"' });
  assert.equal(result.ok, true);
  assert.doesNotMatch(result.page_data, /sk-ant-api03-ABCDEFGHIJ/);
});

test('parity: Monaco\'s full text reaches the json snapshot', async () => {
  const { fake } = monacoPage({ value: 'line one\nline two\nline three' });
  const snapshot = await fake.manager.browserControl('browser.snapshot', { tabId: 4242 }, { serialized: true });
  const editor = (snapshot.editors || []).find((candidate) => candidate.kind === 'monaco');
  assert.ok(editor, JSON.stringify(snapshot.editors));
  assert.equal(editor.text, 'line one\nline two\nline three');
});

test('parity: the person\'s own popups and downloads keep main\'s behaviour on the webview host', async () => {
  const { manager } = webviewHost();
  await manager.activate({ chatId: 'chat-wv', bounds: PANE });
  const entry = manager.entries.get('chat-wv');
  entry.agentActiveAt = 0;
  const reply = entry.view.webContents.windowOpenHandler({ url: 'https://docs.example/', disposition: 'foreground-tab' });
  assert.equal(reply.action, 'allow');
  assert.equal(reply.createWindow, undefined);
  manager.destroy();
});
