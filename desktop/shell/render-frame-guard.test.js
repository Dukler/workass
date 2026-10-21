'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { shouldBlockFrameNavigation, frameUrlChain, navigationOrigin, installRenderFrameGuard } = require('./render-frame-guard');

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
  assert.equal(shouldBlockFrameNavigation({}), false);
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

test('a shell without the WebRTC policy API still installs the navigation guard', () => {
  const contents = new EventEmitter();
  contents.setWebRTCIPHandlingPolicy = () => { throw new Error('unsupported'); };
  const lines = [];
  installRenderFrameGuard(contents, (line) => lines.push(line));
  assert.equal(contents.listenerCount('will-frame-navigate'), 1);
  assert.match(lines[0], /webrtc policy unavailable/);
});
