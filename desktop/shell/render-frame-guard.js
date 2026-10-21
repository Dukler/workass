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
 * @param {{ isMainFrame: boolean, url: string, frameUrls: string[], initiatorUrls?: string[] }} details
 *   frameUrls[0] is the navigating frame's current URL, then its ancestors up
 *   to (and including) the main frame. initiatorUrls is the same chain for the
 *   frame that started the navigation.
 *
 * Measured in Electron 38: when a render frame navigates itself to another
 * site, the event's `frame` is the new cross-process frame, whose URL is still
 * empty; the `initiator` is the render frame. So anything a render frame (or a
 * frame inside one) initiates is refused, whatever frame it targets.
 */
function shouldBlockFrameNavigation({ isMainFrame, url, frameUrls, initiatorUrls } = {}) {
  if (Array.isArray(initiatorUrls) && initiatorUrls.some(isSrcdoc)) return true;
  if (isMainFrame) return false;
  const chain = Array.isArray(frameUrls) ? frameUrls : [];
  if (chain.slice(1).some(isSrcdoc)) return true;
  return isSrcdoc(chain[0]) && !isSrcdoc(url);
}

// WebFrameMain → [its url, parent url, …]. A frame torn down mid-walk throws;
// an unknown chain is treated as the frame alone.
function frameUrlChain(frame) {
  const urls = [];
  let current = frame;
  for (let depth = 0; current && depth < 32; depth++) {
    try {
      urls.push(String(current.url || ''));
      current = current.parent;
    } catch {
      break;
    }
  }
  return urls;
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
// behind Electron's modal error dialog. Every read is guarded.
function readFrame(event, key) {
  try {
    return event[key];
  } catch {
    return null;
  }
}

function installRenderFrameGuard(webContents, log = (line) => console.error(line)) {
  webContents.on('will-frame-navigate', (event) => {
    try {
      const details = event && typeof event === 'object' ? event : {};
      const frameUrls = frameUrlChain(readFrame(details, 'frame'));
      const initiatorUrls = frameUrlChain(readFrame(details, 'initiator'));
      if (!shouldBlockFrameNavigation({ isMainFrame: !!details.isMainFrame, url: details.url, frameUrls, initiatorUrls })) return;
      event.preventDefault();
      log(`[shell] render frame navigation blocked ${navigationOrigin(details.url)}`);
    } catch (err) {
      log(`[shell] render frame guard error: ${err && err.message ? err.message : err}`);
    }
  });
  // WebRTC is the one way out that no CSP covers. The frame bootstrap removes
  // RTCPeerConnection; this keeps any connection that survives off direct UDP.
  // Voice input uses getUserMedia only, which this policy does not affect.
  try {
    webContents.setWebRTCIPHandlingPolicy('disable_non_proxied_udp');
  } catch (err) {
    log(`[shell] webrtc policy unavailable: ${err && err.message ? err.message : err}`);
  }
}

module.exports = { shouldBlockFrameNavigation, frameUrlChain, navigationOrigin, installRenderFrameGuard };
