'use strict';

// HTML pages in the chat render in sandboxed `srcdoc` frames with no network
// (desktop/docs/mocks/html-render/PLAN.md, "Security model"). Their CSP stops
// every fetch, but no CSP stops a frame from navigating itself
// (`location = …`, a meta refresh, a link click the page's bootstrap missed),
// and a navigation is a request that can carry data. The shell cancels every
// navigation of a render frame and of anything nested inside one.
//
// A render frame is an `about:srcdoc` document whose frame is not itself inside
// another render frame. Its own (re)load of `about:srcdoc` is allowed: that is
// the renderer setting or replacing the page. Everything else is refused:
//   • a render frame navigating anywhere but about:srcdoc;
//   • any frame (srcdoc, about:blank, a URL) nested under a render frame.
// The main frame, the browser pane (its own webContents, never consulted here)
// and a visualization frame loaded from /workass/artifacts/ are untouched.

const SRCDOC = 'about:srcdoc';

function isSrcdoc(url) {
  return String(url || '').split('#')[0] === SRCDOC;
}

/**
 * @param {{ isMainFrame: boolean, url: string, frameUrls: string[], initiatorUrls?: string[],
 *   frameUnknown?: boolean, initiatorUnknown?: boolean }} details
 *   frameUrls[0] is the navigating frame's current URL, then its ancestors up
 *   to (and including) the main frame. initiatorUrls is the same chain for the
 *   frame that started the navigation. frameUnknown / initiatorUnknown say a
 *   chain could not be read up to the main frame (a lookup threw, the frame
 *   was gone, a parent was unreadable).
 *
 * Measured in Electron 38: when a render frame navigates itself to another
 * site, the event's `frame` is the new cross-process frame, whose URL is still
 * empty; the `initiator` is the render frame. So anything a render frame (or a
 * frame inside one) initiates is refused, whatever frame it targets.
 *
 * The guard fails closed: a navigation it cannot attribute is cancelled. The
 * renderer runs a page's scripts only because this guard exists, so "could not
 * tell" must never mean "let it through" (review 2026-10-21: without the guard
 * a scripted page got out by five self-navigation vectors, typed text included).
 */
function shouldBlockFrameNavigation({ isMainFrame, url, frameUrls, initiatorUrls, frameUnknown, initiatorUnknown } = {}) {
  if (initiatorUnknown) return true;
  if (Array.isArray(initiatorUrls) && initiatorUrls.some(isSrcdoc)) return true;
  if (isMainFrame) return false;
  const chain = Array.isArray(frameUrls) ? frameUrls : [];
  // A subframe navigation whose frame cannot be identified is cancelled. A
  // subframe always has the main frame above it; one with no parent at all is
  // detached, and where it sat is no longer known.
  if (frameUnknown || chain.length < 2) return true;
  if (chain.slice(1).some(isSrcdoc)) return true;
  return isSrcdoc(chain[0]) && !isSrcdoc(url);
}

// WebFrameMain → { urls: [its url, parent url, …], complete }. complete is true
// only when the walk reached the main frame (a frame with no parent): a frame
// torn down mid-walk throws, and a partial chain may be missing exactly the
// render frame it sits in.
function readFrameChain(frame) {
  const urls = [];
  if (!frame || typeof frame !== 'object') return { urls, complete: false };
  let current = frame;
  for (let depth = 0; depth < 32; depth++) {
    let parent;
    try {
      urls.push(String(current.url || ''));
      parent = current.parent;
    } catch {
      return { urls, complete: false };
    }
    if (!parent) return { urls, complete: true };
    current = parent;
  }
  return { urls, complete: false };
}

// The URL chain alone, as far as it could be read.
function frameUrlChain(frame) {
  return readFrameChain(frame).urls;
}

// Only the origin of a refused target is logged: its path and query are page
// data, which may be exactly what the page tried to send.
function navigationOrigin(url) {
  try {
    const parsed = new URL(url);
    return parsed.origin && parsed.origin !== 'null' ? parsed.origin : `${parsed.protocol}`;
  } catch {
    return 'invalid-url';
  }
}

// Reading event.frame / event.initiator can itself throw once a frame is
// disposed, and an exception escaping a main-process listener stops the shell
// behind Electron's modal error dialog. Every read is guarded, and a read that
// throws is reported as such: it is not the same as "there is no frame".
function readFrame(event, key) {
  try {
    return { value: event[key], threw: false };
  } catch {
    return { value: null, threw: true };
  }
}

// The frame being navigated is unknown unless its chain reaches the main frame.
function navigatingChain(read) {
  return read.threw ? { urls: [], complete: false } : readFrameChain(read.value);
}

// The initiator may legitimately be null (a navigation the browser started);
// only a lookup that throws, or a chain that stops short, is unknown.
function initiatorChain(read) {
  if (read.threw) return { urls: [], complete: false };
  if (read.value === null || read.value === undefined) return { urls: [], complete: true };
  return readFrameChain(read.value);
}

// webContents → the exact listener attached to it. The preload asks for this
// (workass-render-frames:guard) before it tells the renderer that pages may
// run scripts, so the answer is what is attached, not what was intended.
const installedGuards = new WeakMap();

function installRenderFrameGuard(webContents, log = (line) => console.error(line)) {
  const guard = (event) => {
    try {
      const details = event && typeof event === 'object' ? event : {};
      const main = readFrame(details, 'isMainFrame');
      const target = readFrame(details, 'url');
      const frame = navigatingChain(readFrame(details, 'frame'));
      const initiator = initiatorChain(readFrame(details, 'initiator'));
      const url = target.threw ? '' : target.value;
      if (!shouldBlockFrameNavigation({
        isMainFrame: !main.threw && !!main.value,
        url,
        frameUrls: frame.urls,
        initiatorUrls: initiator.urls,
        frameUnknown: main.threw || !frame.complete,
        initiatorUnknown: !initiator.complete,
      })) return;
      event.preventDefault();
      log(`[shell] render frame navigation blocked ${navigationOrigin(url)}`);
    } catch (err) {
      // Fail closed: whatever went wrong, still try to cancel the navigation.
      try { event.preventDefault(); } catch { /* the event is gone */ }
      log(`[shell] render frame guard error: ${err && err.message ? err.message : err}`);
    }
  };
  webContents.on('will-frame-navigate', guard);
  installedGuards.set(webContents, guard);
  // WebRTC is the one way out that no CSP covers. The frame bootstrap removes
  // RTCPeerConnection; this keeps any connection that survives off direct UDP.
  // Voice input uses getUserMedia only, which this policy does not affect.
  try {
    webContents.setWebRTCIPHandlingPolicy('disable_non_proxied_udp');
  } catch (err) {
    log(`[shell] webrtc policy unavailable: ${err && err.message ? err.message : err}`);
  }
  return true;
}

// Whether this webContents has the navigation guard attached right now.
function hasRenderFrameGuard(webContents) {
  if (!webContents || typeof webContents !== 'object') return false;
  const guard = installedGuards.get(webContents);
  if (!guard) return false;
  try {
    if (typeof webContents.isDestroyed === 'function' && webContents.isDestroyed()) return false;
    if (typeof webContents.listeners !== 'function') return false;
    return webContents.listeners('will-frame-navigate').includes(guard);
  } catch {
    return false;
  }
}

module.exports = {
  shouldBlockFrameNavigation, frameUrlChain, readFrameChain, navigationOrigin, installRenderFrameGuard, hasRenderFrameGuard,
};
