'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const path = require('node:path');
const Module = require('node:module');
const {
  shouldBlockFrameNavigation, frameUrlChain, readFrameChain, navigationOrigin, installRenderFrameGuard, hasRenderFrameGuard,
} = require('./render-frame-guard');

const APP = 'https://127.0.0.1:8798/';

test('a render frame cannot navigate away from its page', () => {
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.example/?pw=1', frameUrls: ['about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:blank', frameUrls: ['about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: `${APP}index.html`, frameUrls: ['about:srcdoc', APP] }), true);
});

test('a navigation a render frame initiates is refused even when the target frame has no URL yet', () => {
  // Electron 38: a render frame navigating itself cross-site reports the new,
  // still-empty frame; only its initiator is the about:srcdoc page.
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.invalid/?chat=1', frameUrls: ['', APP], initiatorUrls: ['about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: true, url: 'https://evil.invalid/', frameUrls: [APP], initiatorUrls: ['about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.invalid/', frameUrls: ['', APP], initiatorUrls: ['about:blank', 'about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: `${APP}workass/artifacts/chart-1a2b/`, frameUrls: ['', APP], initiatorUrls: [APP] }), false);
});

test('the renderer may load or replace the page itself', () => {
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:srcdoc', frameUrls: ['about:blank', APP] }), false);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:srcdoc', frameUrls: ['', APP] }), false);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:srcdoc', frameUrls: ['about:srcdoc', APP] }), false);
});

test('anything nested under a render frame is refused, srcdoc and about:blank included', () => {
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.example/', frameUrls: ['about:blank', 'about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:srcdoc', frameUrls: ['about:blank', 'about:srcdoc', APP] }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'about:srcdoc#x', frameUrls: ['', 'about:srcdoc', 'about:srcdoc', APP] }), true);
});

test('the main frame and visualization frames are untouched', () => {
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: true, url: 'https://example.com/', frameUrls: [APP] }), false);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: `${APP}workass/artifacts/chart-1a2b/`, frameUrls: ['about:blank', APP] }), false);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: `${APP}workass/artifacts/chart-1a2b/next.html`, frameUrls: [`${APP}workass/artifacts/chart-1a2b/`, APP] }), false);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: true }), false);
});

test('a navigation the guard cannot attribute is cancelled (fail closed)', () => {
  // No frame at all: a subframe nobody can name.
  assert.equal(shouldBlockFrameNavigation({}), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.invalid/', frameUrls: [] }), true);
  // A subframe with no parent is detached: where it sat is no longer known.
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.invalid/', frameUrls: [''] }), true);
  // A chain that stopped short of the main frame may be missing its render frame.
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: 'https://evil.invalid/', frameUrls: ['about:blank'], frameUnknown: true }), true);
  // An initiator that could not be read may be a render frame.
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: false, url: `${APP}workass/artifacts/chart-1a2b/`, frameUrls: ['', APP], initiatorUnknown: true }), true);
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: true, url: 'https://evil.invalid/', frameUrls: [APP], initiatorUnknown: true }), true);
  // A browser-started navigation has no initiator; that is known, not unknown.
  assert.equal(shouldBlockFrameNavigation({ isMainFrame: true, url: APP, frameUrls: [APP], initiatorUrls: [] }), false);
});

test('a frame chain is complete only when it reaches the main frame', () => {
  const main = { url: APP, parent: null };
  assert.deepEqual(readFrameChain({ url: 'about:blank', parent: { url: 'about:srcdoc', parent: main } }), { urls: ['about:blank', 'about:srcdoc', APP], complete: true });
  const tornParent = { url: 'about:blank', get parent() { throw new Error('destroyed'); } };
  assert.deepEqual(readFrameChain(tornParent), { urls: ['about:blank'], complete: false });
  assert.deepEqual(readFrameChain({ url: 'about:blank', parent: { get url() { throw new Error('destroyed'); }, parent: main } }), { urls: ['about:blank'], complete: false });
  assert.deepEqual(readFrameChain(null), { urls: [], complete: false });
  let deep = main;
  for (let index = 0; index < 40; index++) deep = { url: 'about:blank', parent: deep };
  assert.equal(readFrameChain(deep).complete, false, 'a chain past the depth bound is not trusted');
});

test('the frame chain is read through WebFrameMain parents and survives a torn-down frame', () => {
  const main = { url: APP, parent: null };
  const page = { url: 'about:srcdoc', parent: main };
  const nested = { url: 'about:blank', parent: page };
  assert.deepEqual(frameUrlChain(nested), ['about:blank', 'about:srcdoc', APP]);
  const broken = { get url() { throw new Error('destroyed'); } };
  assert.deepEqual(frameUrlChain(broken), []);
  assert.deepEqual(frameUrlChain(null), []);
});

test('only the origin of a refused target is logged, never its path or query', () => {
  assert.equal(navigationOrigin('https://evil.example/collect?pw=hunter2'), 'https://evil.example');
  assert.equal(navigationOrigin('mailto:someone@example.com'), 'mailto:');
  assert.equal(navigationOrigin('not a url'), 'invalid-url');
});

test('the installed guard cancels a render-frame navigation and locks WebRTC to proxied traffic', () => {
  const contents = new EventEmitter();
  let policy = '';
  contents.setWebRTCIPHandlingPolicy = (value) => { policy = value; };
  const lines = [];
  installRenderFrameGuard(contents, (line) => lines.push(line));
  assert.equal(policy, 'disable_non_proxied_udp');

  const main = { url: APP, parent: null };
  let prevented = 0;
  const blocked = { url: 'https://evil.example/steal?pw=1', isMainFrame: false, frame: { url: 'about:srcdoc', parent: main }, preventDefault: () => { prevented++; } };
  contents.emit('will-frame-navigate', blocked);
  assert.equal(prevented, 1);
  assert.deepEqual(lines, ['[shell] render frame navigation blocked https://evil.example']);

  const speculative = { url: 'https://evil.invalid/?chat=1', isMainFrame: false, frame: { url: '', parent: main }, initiator: { url: 'about:srcdoc', parent: main }, preventDefault: () => { prevented++; } };
  contents.emit('will-frame-navigate', speculative);
  assert.equal(prevented, 2);
  assert.equal(lines[1], '[shell] render frame navigation blocked https://evil.invalid');
  prevented = 1;

  const allowed = { url: 'about:srcdoc', isMainFrame: false, frame: { url: 'about:blank', parent: main }, initiator: main, preventDefault: () => { prevented++; } };
  contents.emit('will-frame-navigate', allowed);
  const top = { url: 'https://example.com/', isMainFrame: true, frame: main, preventDefault: () => { prevented++; } };
  contents.emit('will-frame-navigate', top);
  assert.equal(prevented, 1);
});

test('a disposed frame never throws out of the guard (that would stop the shell behind a modal error)', () => {
  const contents = new EventEmitter();
  contents.setWebRTCIPHandlingPolicy = () => {};
  const lines = [];
  installRenderFrameGuard(contents, (line) => lines.push(line));
  const disposed = {
    url: 'https://evil.invalid/', isMainFrame: false,
    get frame() { throw new Error('Render frame was disposed before WebFrameMain could be accessed'); },
    get initiator() { return { url: 'about:srcdoc', parent: { url: APP, parent: null } }; },
    preventDefault() { this.prevented = true; },
  };
  assert.doesNotThrow(() => contents.emit('will-frame-navigate', disposed));
  assert.equal(disposed.prevented, true, 'the initiator alone still refuses it');
  const broken = { url: 'https://evil.invalid/', isMainFrame: false, frame: null, initiator: null, preventDefault() { throw new Error('gone'); } };
  broken.initiator = { url: 'about:srcdoc', parent: null };
  assert.doesNotThrow(() => contents.emit('will-frame-navigate', broken));
  assert.match(lines.at(-1), /render frame guard error: gone/);
});

test('a shell without the WebRTC policy API still installs the navigation guard', () => {
  const contents = new EventEmitter();
  contents.setWebRTCIPHandlingPolicy = () => { throw new Error('unsupported'); };
  const lines = [];
  installRenderFrameGuard(contents, (line) => lines.push(line));
  assert.equal(contents.listenerCount('will-frame-navigate'), 1);
  assert.match(lines[0], /webrtc policy unavailable/);
});

function guardedContents() {
  const contents = new EventEmitter();
  contents.setWebRTCIPHandlingPolicy = () => {};
  const lines = [];
  installRenderFrameGuard(contents, (line) => lines.push(line));
  return { contents, lines };
}

function navigation(fields) {
  const event = { prevented: false, preventDefault() { this.prevented = true; } };
  for (const [key, value] of Object.entries(fields)) {
    if (typeof value === 'function') Object.defineProperty(event, key, { get: value, enumerable: true });
    else event[key] = value;
  }
  return event;
}

const throws = () => { throw new Error('Render frame was disposed before WebFrameMain could be accessed'); };

test('the installed guard cancels a navigation when both frame lookups throw', () => {
  const { contents } = guardedContents();
  const event = navigation({ url: 'https://evil.invalid/?typed=hunter2', isMainFrame: false, frame: throws, initiator: throws });
  assert.doesNotThrow(() => contents.emit('will-frame-navigate', event));
  assert.equal(event.prevented, true);
});

test('the installed guard cancels when the frame is gone or its parents cannot be read', () => {
  const { contents } = guardedContents();
  const gone = navigation({ url: 'https://evil.invalid/', isMainFrame: false, frame: null, initiator: null });
  contents.emit('will-frame-navigate', gone);
  assert.equal(gone.prevented, true, 'a subframe with no frame to name');
  const torn = navigation({ url: 'https://evil.invalid/', isMainFrame: false, frame: { url: 'about:blank', get parent() { return throws(); } }, initiator: null });
  contents.emit('will-frame-navigate', torn);
  assert.equal(torn.prevented, true, 'a nested frame whose render-frame parent is unreadable');
  const unreadableInitiator = navigation({ url: `${APP}workass/artifacts/chart-1a2b/`, isMainFrame: false, frame: { url: '', parent: { url: APP, parent: null } }, initiator: throws });
  contents.emit('will-frame-navigate', unreadableInitiator);
  assert.equal(unreadableInitiator.prevented, true, 'an initiator that cannot be read may be a render frame');
  const unreadableKind = navigation({ url: 'https://evil.invalid/', isMainFrame: throws, frame: { url: '', parent: { url: APP, parent: null } }, initiator: null });
  contents.emit('will-frame-navigate', unreadableKind);
  assert.equal(unreadableKind.prevented, true, 'a navigation that cannot say whether it is the main frame');
});

test('the installed guard still lets the app load and visualizations navigate', () => {
  const { contents } = guardedContents();
  const main = { url: APP, parent: null };
  const reload = navigation({ url: APP, isMainFrame: true, frame: main, initiator: null });
  contents.emit('will-frame-navigate', reload);
  assert.equal(reload.prevented, false, 'a browser-started main-frame load has no initiator');
  const artifact = navigation({ url: `${APP}workass/artifacts/chart-1a2b/`, isMainFrame: false, frame: { url: 'about:blank', parent: main }, initiator: main });
  contents.emit('will-frame-navigate', artifact);
  assert.equal(artifact.prevented, false);
});

test('an error inside the guard still cancels the navigation', () => {
  const { contents, lines } = guardedContents();
  const event = navigation({ isMainFrame: false, frame: { url: 'about:srcdoc', parent: { url: APP, parent: null } }, initiator: null });
  Object.defineProperty(event, 'url', { get() { return { toString() { throw new Error('bad url'); } }; } });
  contents.emit('will-frame-navigate', event);
  assert.equal(event.prevented, true);
  assert.match(lines.at(-1), /render frame guard error: bad url/);
});

test('the guard reports itself installed only while its listener is attached', () => {
  const contents = new EventEmitter();
  contents.setWebRTCIPHandlingPolicy = () => {};
  assert.equal(hasRenderFrameGuard(contents), false, 'never installed');
  assert.equal(installRenderFrameGuard(contents, () => {}), true);
  assert.equal(hasRenderFrameGuard(contents), true);
  contents.removeAllListeners('will-frame-navigate');
  assert.equal(hasRenderFrameGuard(contents), false, 'a removed listener is not a guard');
  const destroyed = new EventEmitter();
  destroyed.setWebRTCIPHandlingPolicy = () => {};
  destroyed.isDestroyed = () => true;
  installRenderFrameGuard(destroyed, () => {});
  assert.equal(hasRenderFrameGuard(destroyed), false);
  assert.equal(hasRenderFrameGuard(null), false);
  assert.equal(hasRenderFrameGuard(new EventEmitter()), false, 'another webContents');
});

// Load preload.js against a stand-in electron module and return what it exposed.
function loadPreload(sendSync) {
  const exposed = {};
  const sent = [];
  const electron = {
    contextBridge: { exposeInMainWorld: (name, api) => { exposed[name] = api; } },
    ipcRenderer: {
      invoke: async () => null,
      on: () => {},
      removeListener: () => {},
      sendSync: (channel, ...args) => { sent.push(channel); return sendSync(channel, ...args); },
    },
  };
  const preloadPath = require.resolve('./preload.js');
  const originalLoad = Module._load;
  Module._load = function load(request, parent, isMain) {
    if (request === 'electron') return electron;
    return originalLoad.call(this, request, parent, isMain);
  };
  try {
    delete require.cache[preloadPath];
    require(preloadPath);
  } finally {
    Module._load = originalLoad;
    delete require.cache[preloadPath];
  }
  return { exposed, sent };
}

test('the preload lets pages run scripts only when the main process confirms the guard', () => {
  const confirmed = loadPreload((channel) => channel === 'workass-render-frames:guard');
  assert.deepEqual(confirmed.exposed.workassRenderFrames, { supported: true, navigationGuard: true });
  assert.deepEqual(confirmed.sent, ['workass-render-frames:guard']);

  for (const [label, reply] of [
    ['no guard on this window', () => false],
    ['a main process without the handler', () => undefined],
    ['a truthy non-boolean', () => 'yes'],
    ['the IPC failing', () => { throw new Error('reply was never sent'); }],
  ]) {
    const { exposed } = loadPreload(reply);
    assert.equal(exposed.workassRenderFrames.navigationGuard, false, label);
    assert.equal(exposed.workassRenderFrames.supported, true, label);
  }
});

test('main attaches the guard before the first load and answers the preload from what is attached', () => {
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  assert.doesNotMatch(preload, /navigationGuard:\s*true/);
  assert.match(main, /ipcMain\.on\('workass-render-frames:guard', \(event\) => \{\s*event\.returnValue = own\(event\) && hasRenderFrameGuard\(event\.sender\);/);
  assert.match(main, /ipcMain\.removeAllListeners\('workass-render-frames:guard'\)/);
  const install = main.indexOf('installRenderFrameGuard(win.webContents)');
  const load = main.indexOf('win.loadURL(url)');
  assert.ok(install > 0 && load > 0 && install < load, 'the guard is attached before the window loads anything');
  assert.equal(main.split('installRenderFrameGuard(win.webContents)').length, 2, 'attached exactly once');
});
