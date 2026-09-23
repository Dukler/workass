'use strict';

const crypto = require('node:crypto');
const DEFAULT_PARTITION = 'persist:workass-browser';
const DEFAULT_URL = 'about:blank';
const {
  DEFAULT_VIEWPORT, MAX_CAPTURE_PIXELS, MAX_PNG_BYTES, assertCaptureBounds, captureRasterStep, pngDimensions, presentationFor, validateViewport,
} = require('./browser-viewport');
const {
  addDiagnostic, diagnosticsAfter, mapFramePointThroughQuad, observationScript,
  observedTargetGeometryScript, redactPageMessage, targetExpression, truncateUtf8Bytes,
} = require('./browser-observation');

const MAX_BROWSER_SNAPSHOT_BYTES = 64 * 1024;
const SNAPSHOT_FRAME_LIMIT = 100;
const SNAPSHOT_EDITOR_LIMIT = 50;
const AX_NODE_LIMIT = 500;

function finiteMetric(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number : null;
}

function screenshotMetricStamp(metrics, mode) {
  const stamp = {
    width: metrics.width,
    height: metrics.height,
    deviceScaleFactor: metrics.deviceScaleFactor,
    scrollX: metrics.scrollX,
    scrollY: metrics.scrollY,
  };
  if (mode !== 'viewport') {
    stamp.documentWidth = metrics.documentWidth;
    stamp.documentHeight = metrics.documentHeight;
  }
  return stamp;
}

function normalizeCDPSessionId(value) {
  return value == null || value === '' ? null : String(value);
}

function frameTreeRows(tree) {
  const rows = [];
  const visit = (node, parentFrameId = null) => {
    const frame = node && node.frame;
    if (!frame || !frame.id) return;
    const actualParent = frame.parentId == null ? parentFrameId : String(frame.parentId);
    rows.push({ frame, parentFrameId: actualParent });
    for (const child of Array.isArray(node.childFrames) ? node.childFrames : []) visit(child, frame.id);
  };
  visit(tree);
  return rows;
}

function unwrapCDPValue(response) {
  const remote = response && response.result;
  if (response && response.exceptionDetails) throw new Error('browser frame evaluation failed');
  if (remote && Object.prototype.hasOwnProperty.call(remote, 'value')) return remote.value;
  if (remote && remote.type === 'undefined') return undefined;
  return null;
}

function safeChatId(value) {
  const id = String(value || '').trim();
  if (!id || id.length > 200) throw new Error('invalid browser chat id');
  return id;
}

function safeBounds(raw, contentSize) {
  const input = raw && typeof raw === 'object' ? raw : {};
  const maxW = Math.max(1, Math.floor(Number(contentSize && contentSize[0]) || 1));
  const maxH = Math.max(1, Math.floor(Number(contentSize && contentSize[1]) || 1));
  const x = Math.max(0, Math.min(maxW - 1, Math.floor(Number(input.x) || 0)));
  const y = Math.max(0, Math.min(maxH - 1, Math.floor(Number(input.y) || 0)));
  const width = Math.max(1, Math.min(maxW - x, Math.floor(Number(input.width) || 1)));
  const height = Math.max(1, Math.min(maxH - y, Math.floor(Number(input.height) || 1)));
  return { x, y, width, height };
}

function captureRasterBounds(rect) {
  return {
    minWidth: Math.max(1, Math.floor(rect.width)), maxWidth: Math.ceil(rect.width),
    minHeight: Math.max(1, Math.floor(rect.height)), maxHeight: Math.ceil(rect.height),
  };
}

function matchesCaptureRaster(dimensions, bounds) {
  return dimensions.width >= bounds.minWidth && dimensions.width <= bounds.maxWidth &&
    dimensions.height >= bounds.minHeight && dimensions.height <= bounds.maxHeight;
}

// Schemes that carry no authority, so their tail must never be pattern-matched
// into a hostname.
const OPAQUE_SCHEME_RE = /^(file|data|javascript|blob|about|chrome|chrome-extension|devtools|view-source|mailto|tel|sms):/i;
const AUTHORITY_SCHEME_RE = /^([a-z][a-z0-9+.-]*):\/\//i;

function unsupportedSchemeMessage(scheme) {
  if (String(scheme).toLowerCase() === 'file') return localPathMessage();
  return `the Workass browser opens http and https URLs only; ${String(scheme).toLowerCase()}: is not supported`;
}

function localPathMessage() {
  return 'the Workass browser does not open local files; host the file with workass_host_artifact and open the URL it returns';
}

// Resolve to { url } or { error }. One rule with two failure modes: a user
// opening the pane must still get a browser, while a caller asking for an
// unsupported scheme has to be told what to do instead. Rewriting
// file:///Users/x/mock.html into https://file/... produced ERR_NAME_NOT_RESOLVED,
// which reads as a DNS fault and sends the caller hunting in the wrong place.
function resolveBrowserURL(value) {
  const raw = String(value || '').trim();
  if (!raw || raw === DEFAULT_URL) return { url: DEFAULT_URL };
  if (/^https?:\/\//i.test(raw)) return { url: raw };
  if (/^(localhost|127\.0\.0\.1|\[::1\])(?::\d+)?(?:\/|$)/i.test(raw)) return { url: `http://${raw}` };
  const scheme = (AUTHORITY_SCHEME_RE.exec(raw) || OPAQUE_SCHEME_RE.exec(raw) || [])[1];
  if (scheme) return { error: unsupportedSchemeMessage(scheme) };
  // A bare filesystem path fails the same way: it has dots, so it would become
  // https://Users/... rather than anything a browser can resolve.
  if (/^[/\\]/.test(raw) || /^[a-z]:[\\/]/i.test(raw)) return { error: localPathMessage() };
  if (/\s/.test(raw) || !raw.includes('.')) return { url: `https://www.google.com/search?q=${encodeURIComponent(raw)}` };
  return { url: `https://${raw}` };
}

function normalizeBrowserURL(value) {
  const resolved = resolveBrowserURL(value);
  if (resolved.error) throw new Error(resolved.error);
  return resolved.url;
}

function cleanUserAgent(chromeVersion, platform = process.platform) {
  const major = String(chromeVersion || '').split('.')[0].replace(/\D/g, '') || '140';
  const os = platform === 'win32'
    ? 'Windows NT 10.0; Win64; x64'
    : platform === 'linux'
      ? 'X11; Linux x86_64'
      : 'Macintosh; Intel Mac OS X 10_15_7';
  return `Mozilla/5.0 (${os}) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${major}.0.0.0 Safari/537.36`;
}

const BROWSER_MODIFIER_ALIASES = new Map([
  ['alt', 'alt'], ['option', 'alt'],
  ['control', 'control'], ['ctrl', 'control'],
  ['meta', 'meta'], ['command', 'meta'], ['cmd', 'meta'], ['super', 'meta'],
  ['shift', 'shift'],
]);

const BROWSER_KEY_ALIASES = new Map([
  ['esc', 'Escape'], ['escape', 'Escape'],
  ['return', 'Enter'], ['enter', 'Enter'],
  ['spacebar', 'Space'], ['space', 'Space'],
  ['backspace', 'Backspace'], ['delete', 'Delete'], ['del', 'Delete'],
  ['tab', 'Tab'], ['home', 'Home'], ['end', 'End'],
  ['pageup', 'PageUp'], ['pagedown', 'PageDown'],
  ['arrowup', 'ArrowUp'], ['up', 'ArrowUp'],
  ['arrowdown', 'ArrowDown'], ['down', 'ArrowDown'],
  ['arrowleft', 'ArrowLeft'], ['left', 'ArrowLeft'],
  ['arrowright', 'ArrowRight'], ['right', 'ArrowRight'],
]);

// Workass accepts the familiar compact shortcut spelling used by agents
// (`Meta+A`, `Control+Shift+P`). Electron wants the key and modifiers in
// separate fields; passing the whole string as keyCode silently emits no real
// shortcut while still looking like a successful send.
function parseBrowserKey(value, platform = process.platform) {
  const raw = String(value || '').trim();
  if (!raw) throw new Error('browser key is required');
  if (raw.length > 128 || /[\r\n\0]/u.test(raw)) throw new Error('browser key is invalid');

  const parts = raw.split('+').map((part) => part.trim());
  if (parts.some((part) => !part)) throw new Error(`invalid browser shortcut: ${raw}`);
  const keyRaw = parts.pop();
  const modifiers = [];
  for (const part of parts) {
    const normalized = part.toLowerCase().replace(/[\s_-]+/gu, '');
    const modifier = normalized === 'commandorcontrol' || normalized === 'cmdorctrl' || normalized === 'mod'
      ? (platform === 'darwin' ? 'meta' : 'control')
      : BROWSER_MODIFIER_ALIASES.get(normalized);
    if (!modifier) throw new Error(`unsupported browser modifier: ${part}`);
    if (!modifiers.includes(modifier)) modifiers.push(modifier);
  }

  const loweredKey = keyRaw.toLowerCase().replace(/[\s_-]+/gu, '');
  const keyCode = BROWSER_KEY_ALIASES.get(loweredKey)
    || (/^[a-z]$/iu.test(keyRaw) ? keyRaw.toUpperCase() : keyRaw);
  if (!keyCode || keyCode.length > 64) throw new Error('browser key is invalid');
  return { keyCode, modifiers };
}

const CDP_MODIFIER_BITS = Object.freeze({ alt: 1, control: 2, meta: 4, shift: 8 });
const CDP_NAMED_KEYS = new Map([
  ['Backspace', { key: 'Backspace', code: 'Backspace', virtualKeyCode: 8 }],
  ['Tab', { key: 'Tab', code: 'Tab', virtualKeyCode: 9 }],
  ['Enter', { key: 'Enter', code: 'Enter', virtualKeyCode: 13, text: '\r' }],
  ['Shift', { key: 'Shift', code: 'ShiftLeft', virtualKeyCode: 16 }],
  ['Control', { key: 'Control', code: 'ControlLeft', virtualKeyCode: 17 }],
  ['Alt', { key: 'Alt', code: 'AltLeft', virtualKeyCode: 18 }],
  ['Escape', { key: 'Escape', code: 'Escape', virtualKeyCode: 27 }],
  ['Space', { key: ' ', code: 'Space', virtualKeyCode: 32, text: ' ' }],
  ['PageUp', { key: 'PageUp', code: 'PageUp', virtualKeyCode: 33 }],
  ['PageDown', { key: 'PageDown', code: 'PageDown', virtualKeyCode: 34 }],
  ['End', { key: 'End', code: 'End', virtualKeyCode: 35 }],
  ['Home', { key: 'Home', code: 'Home', virtualKeyCode: 36 }],
  ['ArrowLeft', { key: 'ArrowLeft', code: 'ArrowLeft', virtualKeyCode: 37 }],
  ['ArrowUp', { key: 'ArrowUp', code: 'ArrowUp', virtualKeyCode: 38 }],
  ['ArrowRight', { key: 'ArrowRight', code: 'ArrowRight', virtualKeyCode: 39 }],
  ['ArrowDown', { key: 'ArrowDown', code: 'ArrowDown', virtualKeyCode: 40 }],
  ['Delete', { key: 'Delete', code: 'Delete', virtualKeyCode: 46 }],
  ['Meta', { key: 'Meta', code: 'MetaLeft', virtualKeyCode: 91 }],
]);
const CDP_PUNCTUATION_KEYS = new Map([
  [';', { code: 'Semicolon', virtualKeyCode: 186, shifted: ':' }],
  ['=', { code: 'Equal', virtualKeyCode: 187, shifted: '+' }],
  [',', { code: 'Comma', virtualKeyCode: 188, shifted: '<' }],
  ['-', { code: 'Minus', virtualKeyCode: 189, shifted: '_' }],
  ['.', { code: 'Period', virtualKeyCode: 190, shifted: '>' }],
  ['/', { code: 'Slash', virtualKeyCode: 191, shifted: '?' }],
  ['`', { code: 'Backquote', virtualKeyCode: 192, shifted: '~' }],
  ['[', { code: 'BracketLeft', virtualKeyCode: 219, shifted: '{' }],
  ['\\', { code: 'Backslash', virtualKeyCode: 220, shifted: '|' }],
  [']', { code: 'BracketRight', virtualKeyCode: 221, shifted: '}' }],
  ["'", { code: 'Quote', virtualKeyCode: 222, shifted: '"' }],
]);

// CDP targets the owned page without requiring a focused BrowserWindow.
// Electron's webContents.sendInputEvent only works while the containing window
// is focused, which made its success-looking return value especially dangerous
// for agent-driven shortcuts. Editing commands add page-side verification where
// a detached Chromium target cannot perform the default selection/deletion.
function cdpBrowserKey(parsed) {
  const modifiers = parsed.modifiers.reduce((bits, modifier) => bits | (CDP_MODIFIER_BITS[modifier] || 0), 0);
  const shift = parsed.modifiers.includes('shift');
  const nonShiftModifier = parsed.modifiers.some((modifier) => modifier !== 'shift');
  let definition = CDP_NAMED_KEYS.get(parsed.keyCode);

  if (!definition && /^[A-Z]$/u.test(parsed.keyCode)) {
    const lower = parsed.keyCode.toLowerCase();
    definition = {
      key: shift ? parsed.keyCode : lower,
      code: `Key${parsed.keyCode}`,
      virtualKeyCode: parsed.keyCode.charCodeAt(0),
      text: shift ? parsed.keyCode : lower,
      unmodifiedText: lower,
    };
  } else if (!definition && /^[0-9]$/u.test(parsed.keyCode)) {
    definition = {
      key: parsed.keyCode,
      code: `Digit${parsed.keyCode}`,
      virtualKeyCode: parsed.keyCode.charCodeAt(0),
      text: parsed.keyCode,
    };
  } else if (!definition && CDP_PUNCTUATION_KEYS.has(parsed.keyCode)) {
    const punctuation = CDP_PUNCTUATION_KEYS.get(parsed.keyCode);
    const key = shift ? punctuation.shifted : parsed.keyCode;
    definition = { key, code: punctuation.code, virtualKeyCode: punctuation.virtualKeyCode, text: key, unmodifiedText: parsed.keyCode };
  } else if (!definition && /^F(?:[1-9]|1[0-9]|2[0-4])$/u.test(parsed.keyCode)) {
    const number = Number(parsed.keyCode.slice(1));
    definition = { key: parsed.keyCode, code: parsed.keyCode, virtualKeyCode: 111 + number };
  }

  definition ||= { key: parsed.keyCode, code: parsed.keyCode, virtualKeyCode: 0 };
  const unmodifiedText = definition.unmodifiedText ?? definition.text;
  const text = nonShiftModifier ? undefined : definition.text;
  return {
    modifiers,
    key: definition.key,
    code: definition.code,
    windowsVirtualKeyCode: definition.virtualKeyCode,
    ...(unmodifiedText ? { unmodifiedText } : {}),
    ...(text ? { text } : {}),
  };
}

class BrowserManager {
  constructor({ win, WebContentsView, BrowserWindow, nativeImage, session, partition = DEFAULT_PARTITION, chromeVersion, platform, onState, requestOpen, onLifecycle }) {
    if (!win || !WebContentsView || !session) throw new Error('browser manager dependencies missing');
    this.win = win;
    this.WebContentsView = WebContentsView;
    this.BrowserWindow = BrowserWindow || null;
    this.nativeImage = nativeImage || null;
    this.partition = partition;
    this.artifactNavigations = new WeakMap();
    this.platform = platform || process.platform;
    this.profile = session.fromPartition(partition, { cache: true });
    this.userAgent = cleanUserAgent(chromeVersion, platform);
    this.onState = typeof onState === 'function' ? onState : () => {};
    this.requestOpen = typeof requestOpen === 'function' ? requestOpen : () => {};
    this.onLifecycle = typeof onLifecycle === 'function' ? onLifecycle : () => {};
    this.entries = new Map();
    this.activeId = null;
    this.attachedView = null;
    this.presentationChain = Promise.resolve();
    this.agentControl = false;
    this.cdpListeners = new Set();
    this.childSessions = new Map();
    this.typeProbeSeq = 0;

    try { this.profile.setUserAgent(this.userAgent); } catch { /* best effort */ }
    // Remote pages never receive ambient device capabilities. Login itself does
    // not require these permissions; explicit browser permission UX can be added
    // later without silently granting camera/mic/location to arbitrary sites.
    try { this.profile.setPermissionCheckHandler(() => false); } catch { /* older Electron */ }
    try { this.profile.setPermissionRequestHandler((_wc, _permission, callback) => callback(false)); } catch { /* older Electron */ }
  }

  publicState(entry) {
    const wc = entry.view.webContents;
    const history = wc.navigationHistory;
    return {
      chatId: entry.chatId,
      url: redactPageMessage(entry.url || DEFAULT_URL),
      title: redactPageMessage(entry.title || ''),
      loading: !!entry.loading,
      error: entry.error ? redactPageMessage(entry.error) : null,
      canGoBack: !!(history && history.canGoBack && history.canGoBack()),
      canGoForward: !!(history && history.canGoForward && history.canGoForward()),
      cdpAttached: !!(wc.debugger && wc.debugger.isAttached && wc.debugger.isAttached()),
      persistent: !!(wc.session && wc.session.isPersistent && wc.session.isPersistent()),
      agentControl: this.agentControl,
      viewport: { ...entry.viewport },
      effectiveViewport: entry.effectiveViewport ? { ...entry.effectiveViewport } : null,
      viewportGeneration: entry.viewportGeneration,
      documentGeneration: entry.documentGeneration,
      visible: entry.visible === true,
      presentationBounds: entry.presentationBounds ? { ...entry.presentationBounds } : null,
      presentationScale: entry.presentationScale,
    };
  }

  setAgentControlReady(ready) {
    this.agentControl = ready === true;
    for (const entry of this.entries.values()) this.publish(entry);
  }

  // Main-process artifact authorization needs the exact native views mounted
  // by this manager; callers never receive their browsing credentials.
  consumeArtifactNavigation(contents, targetURL) {
    const expected = this.artifactNavigations.get(contents);
    if (!expected || expected.split('#')[0] !== targetURL.split('#')[0]) return false;
    this.artifactNavigations.delete(contents);
    return true;
  }

  ownedWebContents() {
    return this.browserEntries().map((entry) => entry.view && entry.view.webContents).filter(Boolean);
  }

  onCDP(listener) {
    if (typeof listener !== 'function') return () => {};
    this.cdpListeners.add(listener);
    return () => this.cdpListeners.delete(listener);
  }

  emitCDP(event) {
    for (const listener of this.cdpListeners) {
      try { listener(event); } catch { /* one adapter must not break the rest */ }
    }
  }

  publish(entry) {
    const state = this.publicState(entry);
    try { this.onState(state); } catch { /* renderer may be reloading */ }
    return state;
  }

  lifecycle(entry, stage, details = {}) {
    try {
      this.onLifecycle({ stage, chatId: entry.chatId, tabId: Number(entry.view.webContents.id), ...details });
    } catch { /* test/diagnostic observers cannot interrupt browser work */ }
  }

  bind(entry) {
    const wc = entry.view.webContents;
    const syncURL = (_event, url) => {
      entry.url = String(url || wc.getURL() || DEFAULT_URL);
      entry.error = null;
      this.publish(entry);
    };
    wc.on('did-start-navigation', (_event, _url, inPlace, isMainFrame) => {
      if (inPlace || isMainFrame === false) return;
      entry.mainNavigationPending = true;
      entry.documentGeneration += 1;
      entry.interactionGeneration += 1;
      entry.frameTreeGeneration += 1;
      entry.observations.clear();
      entry.screenshots.clear();
      this.invalidateFrames(entry, null, 'document_replaced');
      this.interruptWaiters(entry, 'navigation');
    });
    wc.on('did-start-loading', () => { entry.loading = true; entry.error = null; this.publish(entry); });
    wc.on('did-stop-loading', () => {
      entry.loading = false;
      entry.url = wc.getURL() || entry.url || DEFAULT_URL;
      this.publish(entry);
    });
    wc.on('did-navigate', syncURL);
    wc.on('did-navigate-in-page', syncURL);
    wc.on('did-finish-load', () => {
      if (!entry.initialDocumentReady || wc.isDestroyed?.() || this.entries.get(entry.chatId) !== entry) return;
      const scale = this.attachedView === entry.view ? entry.presentationScale : 1;
      void this.applyAndReadDeviceEmulation(entry, entry.viewport, scale).then((effective) => {
        if (wc.isDestroyed?.() || this.entries.get(entry.chatId) !== entry) return;
        entry.effectiveViewport = effective;
        this.publish(entry);
      }).catch((error) => {
        if (wc.isDestroyed?.() || this.entries.get(entry.chatId) !== entry) return;
        entry.error = redactPageMessage(error && error.message || error);
        this.publish(entry);
      });
    });
    wc.on('page-title-updated', (_event, title) => { entry.title = String(title || ''); this.publish(entry); });
    wc.on('did-fail-load', (_event, code, description, url, isMainFrame) => {
      if (!isMainFrame || code === -3) return; // ERR_ABORTED is normal during redirects.
      entry.loading = false;
      entry.url = String(url || entry.url || DEFAULT_URL);
      entry.error = String(description || `load failed (${code})`);
      this.publish(entry);
    });
    wc.on('render-process-gone', (_event, details) => {
      entry.loading = false;
      this.interruptWaiters(entry, 'renderer_lost');
      this.invalidateFrames(entry, null, 'renderer_lost');
      entry.error = `browser renderer stopped (${String(details && details.reason || 'unknown')})`;
      this.publish(entry);
    });
    wc.setWindowOpenHandler(({ url }) => ({
      action: /^https?:\/\//i.test(String(url || '')) ? 'allow' : 'deny',
      overrideBrowserWindowOptions: {
        width: 980,
        height: 760,
        title: 'workass browser',
        webPreferences: {
          partition: this.partition,
          nodeIntegration: false,
          contextIsolation: true,
          sandbox: true,
        },
      },
    }));
    if (wc.debugger && typeof wc.debugger.on === 'function') {
      wc.debugger.on('message', (_event, method, params, sessionId) => {
        const currentSessionId = normalizeCDPSessionId(sessionId);
        const currentSession = currentSessionId == null || entry.targetSessions.has(currentSessionId);
        if (currentSession && !String(method || '').startsWith('Target.')) {
          this.emitCDP({ tabId: Number(wc.id), method, params: params || {}, sessionId: currentSessionId });
        }
        void this.handleCDPMessage(entry, method, params || {}, currentSessionId).catch((error) => {
          if (this.isCurrentEntry(entry)) {
            this.lifecycle(entry, 'owned-frame-cdp-event-failed', {
              reason: redactPageMessage(error && error.message || error, 256),
            });
          }
        });
        const sourceFrame = this.frameForSession(entry, currentSessionId);
        // Root CDP events remain owned by this exact WebContents even during
        // the brief navigation interval where the old root frame is invalidated
        // and the replacement frame has not yet appeared in Page.getFrameTree.
        // Child-session diagnostics still require a live verified frame binding.
        const frameIsCurrent = currentSessionId == null
          ? !!entry.rootTargetId && !entry.closed
          : !!sourceFrame && !sourceFrame.detached && entry.targetSessions.get(currentSessionId)?.frameId === sourceFrame.id;
        if (frameIsCurrent && method === 'Runtime.consoleAPICalled' && ['warning', 'error'].includes(String(params && params.type || ''))) {
          const values = Array.isArray(params.args) ? params.args.slice(0, 20).map((arg) => {
            if (arg && ['string', 'number', 'boolean'].includes(arg.type)) return String(arg.value ?? arg.description ?? '');
            return '[object]';
          }) : [];
          this.recordDiagnostic(entry, `console.${params.type}`, values.join(' ').slice(0, 8192));
        } else if (frameIsCurrent && method === 'Runtime.exceptionThrown') {
          const details = params && params.exceptionDetails || {};
          const exception = details.exception || {};
          this.recordDiagnostic(entry, 'exception', String(details.text || exception.description || 'runtime exception'));
        }
      });
      wc.debugger.on('detach', (_event, reason) => {
        this.emitCDP({ tabId: Number(wc.id), detach: true, reason: String(reason || 'detached') });
        entry.cdpReady = false;
        this.interruptWaiters(entry, 'debugger_lost');
        this.invalidateFrames(entry, null, 'debugger_lost');
      });
    }
  }

  isCurrentEntry(entry, token = entry.lifecycleToken) {
    return !!entry && !entry.closed && entry.lifecycleToken === token && this.entries.get(entry.chatId) === entry;
  }

  assertCurrentEntry(entry, token = entry.lifecycleToken) {
    if (!this.isCurrentEntry(entry, token)) throw new Error('browser tab was closed during the operation');
  }

  frameForSession(entry, sessionId) {
    if (sessionId == null) {
      return Array.from(entry.frames.values()).find((frame) => frame.targetId === entry.rootTargetId && frame.parentFrameId == null) || null;
    }
    const target = entry.targetSessions.get(sessionId);
    return target ? entry.frames.get(target.frameId) || null : null;
  }

  frameForTarget(entry, targetId) {
    return Array.from(entry.frames.values()).find((frame) => frame.targetId === targetId && !frame.detached) || null;
  }

  interruptWaiters(entry, reason, frameIds = null, targetIds = null) {
    for (const waiter of Array.from(entry.waits || [])) {
      if ((frameIds || targetIds) && !frameIds?.has(waiter.frameId) &&
          !(waiter.frameId == null && targetIds?.has(waiter.targetId))) continue;
      try { waiter.interrupt(reason); } catch { /* waiter cleanup is isolated */ }
    }
  }

  frameSubtreeIds(entry, rootFrameId) {
    const ids = new Set([String(rootFrameId)]);
    let grew = true;
    while (grew) {
      grew = false;
      for (const frame of entry.frames.values()) {
        if (frame.parentFrameId && ids.has(frame.parentFrameId) && !ids.has(frame.id)) {
          ids.add(frame.id);
          grew = true;
        }
      }
    }
    return ids;
  }

  clearFrameContexts(entry, frameIds) {
    for (const [key] of Array.from(entry.executionContexts.entries())) {
      const separator = key.indexOf(':');
      if (separator >= 0 && frameIds.has(key.slice(separator + 1))) entry.executionContexts.delete(key);
    }
  }

  async retireFrameSubtree(entry, rootFrameId, reason, { preserveRoot = false } = {}) {
    const ids = this.frameSubtreeIds(entry, rootFrameId);
    this.invalidateFrames(entry, rootFrameId, reason);
    this.clearFrameContexts(entry, ids);

    const sessionRows = Array.from(entry.targetSessions.entries())
      .filter(([, target]) => ids.has(target.frameId) && !(preserveRoot && target.frameId === String(rootFrameId)))
      .sort((a, b) => {
        const depth = (frameId) => {
          let value = 0;
          let frame = entry.frames.get(frameId);
          while (frame?.parentFrameId) { value += 1; frame = entry.frames.get(frame.parentFrameId); }
          return value;
        };
        return depth(a[1].frameId) - depth(b[1].frameId);
      });
    for (const [sessionId] of sessionRows) {
      // detachOwnedSession removes the complete nested session subtree before
      // its first await. A later protocol detach event therefore cannot detach
      // the same obsolete session a second time.
      if (entry.targetSessions.has(sessionId)) await this.detachOwnedSession(entry, sessionId, reason);
    }
    for (const frameId of ids) {
      if (preserveRoot && frameId === String(rootFrameId)) continue;
      entry.frames.delete(frameId);
    }
    return ids;
  }

  invalidateFrames(entry, frameId = null, reason = 'frame_changed') {
    const invalid = new Set();
    if (frameId == null) {
      for (const frame of entry.frames.values()) invalid.add(frame.id);
    } else {
      invalid.add(String(frameId));
      let grew = true;
      while (grew) {
        grew = false;
        for (const frame of entry.frames.values()) {
          if (frame.parentFrameId && invalid.has(frame.parentFrameId) && !invalid.has(frame.id)) {
            invalid.add(frame.id);
            grew = true;
          }
        }
      }
    }
    for (const id of invalid) {
      const frame = entry.frames.get(id);
      if (!frame) continue;
      frame.generation = ++entry.frameGenerationSequence;
      frame.unavailableReason = reason;
      frame.axAvailable = false;
      if (reason === 'document_replaced' || reason === 'renderer_lost' || reason === 'debugger_lost' ||
          reason === 'frame_detached' || reason === 'target_destroyed' || reason === 'target_crashed') {
        frame.detached = true;
        frame.contextId = null;
      }
    }
    if (invalid.size) {
      entry.frameTreeGeneration += 1;
      entry.interactionGeneration += 1;
      entry.screenshots.clear();
    }
    return invalid;
  }

  async detachOwnedSession(entry, sessionId, reason = 'target_detached') {
    const accepted = entry.targetSessions.get(sessionId);
    if (!accepted) return false;
    entry.targetSessions.delete(sessionId);
    this.childSessions.delete(String(Number(entry.view.webContents.id)) + ':' + accepted.targetId);
    const explicitDetach = reason === 'explicit_detach';
    this.invalidateFrames(entry, accepted.frameId, explicitDetach ? 'target_detached' : reason);
    const frame = entry.frames.get(accepted.frameId);
    const parent = frame?.parentFrameId ? entry.frames.get(frame.parentFrameId) : null;
    if (frame && (reason === 'target_detached' || explicitDetach) && parent && !parent.detached) {
      frame.targetId = parent.targetId;
      frame.sessionId = parent.sessionId || null;
      frame.contextId = null;
      frame.detached = false;
      frame.unavailableReason = 'owned_frame_target_detached';
    }
    const debug = entry.view.webContents.debugger;
    for (const key of Array.from(entry.executionContexts.keys())) if (key.startsWith(accepted.targetId + ':')) entry.executionContexts.delete(key);
    const nested = Array.from(entry.targetSessions.entries()).filter(([, child]) => this.isDescendantFrame(entry, child.frameId, accepted.frameId));
    for (const [nestedId, nestedTarget] of nested) {
      entry.targetSessions.delete(nestedId);
      this.childSessions.delete(String(Number(entry.view.webContents.id)) + ':' + nestedTarget.targetId);
      for (const key of Array.from(entry.executionContexts.keys())) if (key.startsWith(nestedTarget.targetId + ':')) entry.executionContexts.delete(key);
      this.invalidateFrames(entry, nestedTarget.frameId, explicitDetach ? 'target_detached' : 'target_destroyed');
      if (explicitDetach) {
        try { await debug.sendCommand('Target.detachFromTarget', { sessionId: nestedId }, nestedTarget.parentSessionId || undefined); } catch { /* parent target may already be detached */ }
      }
    }
    // A Target.detachedFromTarget event means the protocol session is already
    // gone. Sending Target.detachFromTarget again races Chromium's target
    // teardown; only an explicit manager request issues that command.
    if (explicitDetach) {
      try { await debug.sendCommand('Target.detachFromTarget', { sessionId }, accepted.parentSessionId || undefined); }
      catch { /* target already detached or debugger closing */ }
    }
    return true;
  }

  async readFrameTree(entry, sessionId = null) {
    if (!this.isCurrentEntry(entry)) return false;
    const debug = entry.view.webContents.debugger;
    let response;
    try { response = await debug.sendCommand('Page.getFrameTree', {}, sessionId || undefined); }
    catch { return false; }
    const tree = response && response.frameTree;
    return tree || false;
  }

  recordFrameTree(entry, tree, sessionId = null) {
    if (!this.isCurrentEntry(entry) || !tree) return false;
    const target = sessionId ? entry.targetSessions.get(sessionId) : null;
    const targetId = target ? target.targetId : entry.rootTargetId;
    if (!targetId) return false;
    const rows = frameTreeRows(tree);
    if (!rows.length) return false;
    const seen = new Set();
    for (const { frame: raw, parentFrameId } of rows) {
      const id = String(raw.id);
      seen.add(id);
      const previous = entry.frames.get(id);
      const loaderId = String(raw.loaderId || '');
      const sameDocument = previous && previous.loaderId === loaderId && !previous.detached;
      const record = sameDocument ? previous : {
        id,
        parentFrameId,
        loaderId,
        url: String(raw.url || ''),
        securityOrigin: String(raw.securityOrigin || ''),
        generation: ++entry.frameGenerationSequence,
        targetId,
        sessionId: sessionId || null,
        contextId: null,
        access: 'owned_cdp_frame',
        unavailableReason: null,
        detached: false,
      };
      record.parentFrameId = parentFrameId;
      record.loaderId = loaderId;
      record.url = String(raw.url || '');
      record.securityOrigin = String(raw.securityOrigin || '');
      record.detached = false;
      if (!sameDocument) {
        record.targetId = targetId;
        record.sessionId = sessionId || null;
        record.contextId = null;
        record.generation = ++entry.frameGenerationSequence;
        record.unavailableReason = null;
        entry.frameTreeGeneration += 1;
      }
      const contextId = entry.executionContexts.get(targetId + ':' + id);
      if (contextId != null) {
        record.contextId = contextId;
        record.unavailableReason = null;
      }
      entry.frames.set(id, record);
    }
    const disappeared = [];
    for (const [id, frame] of entry.frames) {
      if (frame.targetId === targetId && !seen.has(id)) disappeared.push(id);
    }
    for (const id of disappeared) {
      this.invalidateFrames(entry, id, 'frame_detached');
      entry.frames.delete(id);
    }
    return true;
  }

  async refreshFrameTree(entry, sessionId = null) {
    const tree = await this.readFrameTree(entry, sessionId);
    return this.recordFrameTree(entry, tree, sessionId);
  }

  async enableOwnedSession(entry, sessionId = null) {
    const debug = entry.view.webContents.debugger;
    for (const domain of ['Page', 'Runtime', 'DOM', 'Accessibility']) {
      try { await debug.sendCommand(domain + '.enable', {}, sessionId || undefined); }
      catch { /* an unavailable optional domain remains explicitly unavailable */ }
    }
    await this.refreshFrameTree(entry, sessionId);
    try {
      await debug.sendCommand('Target.setAutoAttach', {
        autoAttach: true,
        waitForDebuggerOnStart: false,
        flatten: true,
        filter: [{ type: 'iframe', exclude: false }, { type: '*', exclude: true }],
      }, sessionId || undefined);
      if (!sessionId) entry.autoAttachAvailable = true;
    } catch {
      if (!sessionId) entry.autoAttachAvailable = false;
    }
  }

  async rejectAttachedTarget(entry, parentSessionId, childSessionId) {
    try {
      await entry.view.webContents.debugger.sendCommand(
        'Target.detachFromTarget', { sessionId: childSessionId }, parentSessionId || undefined,
      );
    } catch { /* unowned target is immediately abandoned */ }
  }

  async acceptAttachedTarget(entry, params, parentSessionId) {
    const targetInfo = params && params.targetInfo;
    const childSessionId = params && params.sessionId;
    if (!this.isCurrentEntry(entry) || !targetInfo || !childSessionId || targetInfo.type !== 'iframe') {
      if (childSessionId) await this.rejectAttachedTarget(entry, parentSessionId, childSessionId);
      if (this.isCurrentEntry(entry)) this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'target_type_or_entry_unavailable' });
      return false;
    }
    const parentTargetId = parentSessionId
      ? entry.targetSessions.get(parentSessionId)?.targetId
      : entry.rootTargetId;
    if (!parentTargetId) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId);
      this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'parent_target_unowned' });
      return false;
    }
    // TargetInfo does not provide a portable parentTargetId for iframe
    // targets. Bind the session only through the exact frame id in the
    // parent's authoritative Page.getFrameTree response.
    const parentTree = await this.readFrameTree(entry, parentSessionId || null);
    this.recordFrameTree(entry, parentTree, parentSessionId || null);
    const response = await this.readFrameTree(entry, childSessionId);
    if (!response) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId);
      this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'child_frame_tree_unavailable' });
      return false;
    }
    const root = response.frame;
    const frameId = String(root && root.id || '');
    let candidate = frameId && entry.frames.get(frameId);
    let parentTreeContainsChild = frameId
      ? frameTreeRows(parentTree).some(({ frame }) => String(frame.id) === frameId)
      : false;
    const claimedParent = String(root.parentId || targetInfo.parentFrameId || targetInfo.openerFrameId || candidate?.parentFrameId || '');
    const targetParentFrame = String(targetInfo.parentFrameId || '');
    // Cross-origin OOPIF nodes are not consistently enumerated in their
    // parent's Page.getFrameTree. The flattened auto-attach session lineage
    // proves which owned target emitted this event; the child's authoritative
    // FrameTree parentId must then resolve to a frame already bound to that
    // exact parent target. Never infer ownership from a URL.
    for (let attempt = 0; claimedParent && attempt < 50; attempt += 1) {
      const parentRecord = entry.frames.get(claimedParent);
      if (parentRecord && !parentRecord.detached && parentRecord.targetId === parentTargetId) break;
      await new Promise((resolve) => setTimeout(resolve, 50));
      const refreshedTree = await this.readFrameTree(entry, parentSessionId || null);
      this.recordFrameTree(entry, refreshedTree, parentSessionId || null);
      parentTreeContainsChild = frameTreeRows(refreshedTree).some(({ frame }) => String(frame.id) === frameId);
      candidate = entry.frames.get(frameId) || null;
    }
    const parentRecord = claimedParent && entry.frames.get(claimedParent);
    if (!frameId || !claimedParent || (root.parentId && targetParentFrame && String(root.parentId) !== targetParentFrame) ||
        !parentRecord || parentRecord.detached || parentRecord.targetId !== parentTargetId ||
        (candidate && (candidate.parentFrameId !== claimedParent || candidate.targetId !== parentTargetId))) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId);
      if (candidate && candidate.targetId === parentTargetId) candidate.unavailableReason = 'owned_frame_target_identity_unverified';
      const parentSession = parentSessionId ? entry.targetSessions.get(parentSessionId) : null;
      this.lifecycle(entry, 'owned-frame-target-rejected', {
        reason: !frameId ? 'child_frame_identity_missing' : !candidate ? 'parent_frame_identity_unverified' : 'parent_frame_identity_mismatch',
        childFrameIdPresent: !!frameId,
        candidateFound: !!candidate,
        parentSessionPresent: !!parentSessionId,
        parentSessionOwned: !parentSessionId || !!parentSession,
        parentSessionIsRoot: !parentSessionId || parentSession?.targetId === entry.rootTargetId,
        parentTreeAvailable: !!parentTree,
        parentTreeContainsChild,
      });
      return false;
    }
    if (!candidate) {
      candidate = {
        id: frameId,
        parentFrameId: claimedParent,
        loaderId: '', url: '', securityOrigin: '',
        generation: ++entry.frameGenerationSequence,
        targetId: parentTargetId,
        sessionId: parentSessionId || null,
        contextId: null,
        access: 'owned_cdp_frame',
        unavailableReason: null,
        detached: false,
      };
      entry.frames.set(frameId, candidate);
      entry.frameTreeGeneration += 1;
      entry.interactionGeneration += 1;
      entry.screenshots.clear();
    }
    const targetId = String(targetInfo.targetId || '');
    for (let attempt = 0; frameId && targetId && root.loaderId && candidate?.loaderId &&
      String(root.loaderId) !== String(candidate.loaderId) && attempt < 50; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 50));
      await this.refreshFrameTree(entry, parentSessionId || null);
      candidate = entry.frames.get(frameId) || null;
    }
    if (!targetId || !candidate || (root.loaderId && candidate.loaderId && String(root.loaderId) !== String(candidate.loaderId))) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId);
      if (candidate) candidate.unavailableReason = 'owned_frame_target_identity_unverified';
      this.lifecycle(entry, 'owned-frame-target-rejected', {
        reason: 'target_or_loader_identity_unverified',
        candidateFound: !!candidate,
        targetIdPresent: !!targetId,
        childLoaderPresent: !!root.loaderId,
        parentLoaderPresent: !!candidate?.loaderId,
        loaderIdsMatch: !root.loaderId || !candidate?.loaderId || String(root.loaderId) === String(candidate.loaderId),
      });
      return false;
    }
    const accepted = { targetId, frameId, parentSessionId: parentSessionId || null, sessionId: childSessionId };
    entry.targetSessions.set(childSessionId, accepted);
    this.childSessions.set(String(Number(entry.view.webContents.id)) + ':' + targetId, childSessionId);
    candidate.targetId = targetId;
    candidate.sessionId = childSessionId;
    candidate.loaderId = String(root.loaderId || candidate.loaderId || '');
    candidate.access = 'owned_oopif';
    candidate.unavailableReason = null;
    candidate.detached = false;
    this.lifecycle(entry, 'owned-frame-target-accepted', { access: 'owned_oopif' });
    await this.enableOwnedSession(entry, childSessionId);
    if (!this.isCurrentEntry(entry)) {
      await this.detachOwnedSession(entry, childSessionId, 'entry_closed');
      this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'entry_closed_during_session_enable' });
      return false;
    }
    return true;
  }

  async handleCDPMessage(entry, method, params, sessionId) {
    if (!this.isCurrentEntry(entry)) return;
    if (method === 'Target.attachedToTarget') {
      this.lifecycle(entry, 'owned-frame-target-event', {
        type: String(params?.targetInfo?.type || 'unknown'),
        parentSessionPresent: !!sessionId,
        parentSessionOwned: !sessionId || entry.targetSessions.has(sessionId),
      });
      const task = this.acceptAttachedTarget(entry, params, sessionId);
      entry.pendingTargetTasks.add(task);
      try { await task; }
      finally { entry.pendingTargetTasks.delete(task); }
      return;
    }
    if (method === 'Target.detachedFromTarget') {
      const detached = String(params && params.sessionId || '');
      if (detached) {
        if (entry.targetSessions.has(detached)) void this.detachOwnedSession(entry, detached, 'target_detached');
      }
      return;
    }
    if (method === 'Target.targetDestroyed') {
      const targetId = String(params && params.targetId || '');
      const sessions = Array.from(entry.targetSessions.entries()).filter(([, target]) => target.targetId === targetId);
      for (const [id, target] of sessions) {
        entry.targetSessions.delete(id);
        this.childSessions.delete(String(Number(entry.view.webContents.id)) + ':' + target.targetId);
        this.invalidateFrames(entry, target.frameId, 'target_destroyed');
      }
      return;
    }
    if (method === 'Target.targetCrashed') {
      const targetId = String(params && params.targetId || '');
      for (const [id, target] of Array.from(entry.targetSessions.entries())) {
        if (target.targetId !== targetId) continue;
        entry.targetSessions.delete(id);
        this.childSessions.delete(String(Number(entry.view.webContents.id)) + ':' + target.targetId);
        this.invalidateFrames(entry, target.frameId, 'target_crashed');
      }
      return;
    }
    const target = sessionId ? entry.targetSessions.get(sessionId) : null;
    if (sessionId && !target) return;
    const targetId = target ? target.targetId : entry.rootTargetId;
    if (method === 'Page.frameAttached') {
      const frameId = String(params && params.frameId || '');
      const parentFrameId = String(params && params.parentFrameId || '');
      const parent = parentFrameId && entry.frames.get(parentFrameId);
      if (!frameId || !parent || parent.detached || parent.targetId !== targetId) {
        void this.refreshFrameTree(entry, sessionId || null);
        return;
      }
      const previous = entry.frames.get(frameId);
      if (previous && previous.parentFrameId !== parentFrameId) this.invalidateFrames(entry, frameId, 'frame_identity_changed');
      entry.frames.set(frameId, previous && previous.parentFrameId === parentFrameId ? previous : {
        id: frameId, parentFrameId, loaderId: '', url: '', securityOrigin: '',
        generation: ++entry.frameGenerationSequence, targetId, sessionId: sessionId || null,
        contextId: null, access: 'owned_cdp_frame', unavailableReason: null, detached: false,
      });
      entry.frameTreeGeneration += 1;
      entry.interactionGeneration += 1;
      entry.screenshots.clear();
      return;
    }
    if (method === 'Page.frameNavigated') {
      const raw = params && params.frame;
      const frameId = String(raw && raw.id || '');
      const frame = frameId && entry.frames.get(frameId);
      if (!frame || (frame.detached && frame.unavailableReason !== 'document_replaced') ||
          (frame.targetId !== targetId && target?.frameId !== frameId)) {
        await this.refreshFrameTree(entry, sessionId || null);
        return;
      }
      const loaderId = String(raw.loaderId || '');
      if (frame.loaderId !== loaderId) {
        if (frame.parentFrameId == null) {
          if (!entry.mainNavigationPending) {
            entry.documentGeneration += 1;
            entry.observations.clear();
            entry.screenshots.clear();
            this.interruptWaiters(entry, 'navigation');
          }
          entry.mainNavigationPending = false;
        }
        await this.retireFrameSubtree(entry, frameId, 'document_replaced', { preserveRoot: true });
        const current = entry.frames.get(frameId);
        if (current) {
          current.parentFrameId = raw.parentId == null ? null : String(raw.parentId);
          current.loaderId = loaderId;
          current.url = String(raw.url || '');
          current.securityOrigin = String(raw.securityOrigin || '');
          current.contextId = null;
          current.detached = false;
          current.unavailableReason = null;
        }
      } else {
        frame.url = String(raw.url || '');
        frame.securityOrigin = String(raw.securityOrigin || '');
      }
      await this.refreshFrameTree(entry, sessionId || null);
      return;
    }
    if (method === 'Page.frameDetached' || method === 'Page.frameSubtreeWillBeDetached') {
      const frameId = String(params && (params.frameId || params.rootFrameId) || '');
      const frame = frameId && entry.frames.get(frameId);
      const parent = frame?.parentFrameId ? entry.frames.get(frame.parentFrameId) : null;
      const sourceOwnsFrame = frame && (frame.targetId === targetId || target?.frameId === frameId || parent?.targetId === targetId);
      if (!sourceOwnsFrame) {
        return;
      }
      await this.retireFrameSubtree(entry, frameId, 'frame_detached');
      return;
    }
    if (method === 'Runtime.executionContextCreated') {
      const context = params && params.context;
      const frameId = String(context && context.auxData && context.auxData.frameId || '');
      if (!frameId || context.auxData?.isDefault !== true || !targetId) return;
      const key = targetId + ':' + frameId;
      entry.executionContexts.set(key, context.id);
      const frame = entry.frames.get(frameId);
      if (frame && frame.targetId === targetId) {
        if (frame.contextId !== context.id) frame.generation = ++entry.frameGenerationSequence;
        frame.contextId = context.id;
        frame.detached = false;
        frame.unavailableReason = null;
      }
      return;
    }
    if (method === 'Runtime.executionContextDestroyed') {
      const contextId = params && params.executionContextId;
      const affectedFrameIds = new Set();
      for (const [key, id] of entry.executionContexts) {
        const separator = key.indexOf(':');
        const ownerTargetId = key.slice(0, separator);
        if (id !== contextId || ownerTargetId !== targetId) continue;
        const frameId = key.slice(separator + 1);
        entry.executionContexts.delete(key);
        affectedFrameIds.add(frameId);
        const frame = entry.frames.get(frameId);
        if (frame && frame.targetId === ownerTargetId) {
          frame.contextId = null;
          frame.generation = ++entry.frameGenerationSequence;
          frame.unavailableReason = 'execution_context_lost';
          frame.axAvailable = false;
          entry.interactionGeneration += 1;
        }
      }
      if (affectedFrameIds.size) {
        const root = this.frameForSession(entry, null);
        const targetFilter = root && affectedFrameIds.has(root.id) ? new Set([targetId]) : null;
        this.interruptWaiters(entry, 'execution_context_lost', affectedFrameIds, targetFilter);
      }
      return;
    }
    if (method === 'Runtime.executionContextsCleared') {
      const affectedFrameIds = new Set();
      for (const frame of entry.frames.values()) {
        if (frame.targetId === targetId) {
          affectedFrameIds.add(frame.id);
          frame.contextId = null;
          frame.generation = ++entry.frameGenerationSequence;
          frame.unavailableReason = 'execution_context_lost';
          frame.axAvailable = false;
        }
      }
      for (const key of Array.from(entry.executionContexts.keys())) if (key.startsWith(targetId + ':')) entry.executionContexts.delete(key);
      if (affectedFrameIds.size) {
        const root = this.frameForSession(entry, null);
        const targetFilter = root && affectedFrameIds.has(root.id) ? new Set([targetId]) : null;
        this.interruptWaiters(entry, 'execution_context_lost', affectedFrameIds, targetFilter);
      }
      return;
    }
  }

  isDescendantFrame(entry, candidateId, ancestorId) {
    let frame = entry.frames.get(candidateId);
    const seen = new Set();
    while (frame && frame.parentFrameId && !seen.has(frame.id)) {
      if (frame.parentFrameId === ancestorId) return true;
      seen.add(frame.id);
      frame = entry.frames.get(frame.parentFrameId);
    }
    return false;
  }

  create(chatId) {
    const view = new this.WebContentsView({
      webPreferences: {
        partition: this.partition,
        nodeIntegration: false,
        contextIsolation: true,
        sandbox: true,
        navigateOnDragDrop: false,
      },
    });
    const entry = {
      chatId, view, url: DEFAULT_URL, title: '', loading: false, error: null,
      viewport: { ...DEFAULT_VIEWPORT }, effectiveViewport: null, viewportGeneration: 0,
      documentGeneration: 0, visible: false, presentationBounds: null, presentationScale: 1,
      captureHost: null, captureHostAttached: false, initialDocumentReady: false,
      operationChain: Promise.resolve(), observationSequence: 0, observations: new Map(),
      screenshots: new Map(), diagnostics: [], diagnosticBytes: 0, diagnosticSequence: 0,
      closed: false, lifecycleToken: Symbol('browser-entry'), interactionGeneration: 0,
      frameTreeGeneration: 0, frameGenerationSequence: 0, frames: new Map(),
      targetSessions: new Map(), rootTargetId: null, cdpReady: false,
      pendingTargetTasks: new Set(), mainNavigationPending: false,
      autoAttachAvailable: false, waits: new Set(), executionContexts: new Map(),
    };
    try { view.setBackgroundColor('#ffffff'); } catch { /* older Electron */ }
    try { view.webContents.setUserAgent(this.userAgent); } catch { /* best effort */ }
    this.entries.set(chatId, entry);
    this.bind(entry);
    const lifecycleToken = entry.lifecycleToken;
    entry.ready = this.initializeEntry(entry, lifecycleToken).catch((error) => {
      if (entry.closed || entry.lifecycleToken !== lifecycleToken || this.entries.get(entry.chatId) !== entry) return false;
      entry.error = redactPageMessage(error && error.message || error);
      this.publish(entry);
      return false;
    });
    return entry;
  }

  recordDiagnostic(entry, kind, text) {
    const record = addDiagnostic(entry, kind, text);
    if (record) this.publish(entry);
    return record;
  }

  async initializeEntry(entry, lifecycleToken = entry.lifecycleToken) {
    const wc = entry.view.webContents;
    // A fresh WebContentsView can have no committed document or render widget
    // yet. Give this same owned page a real initial document on its lazily
    // created, non-focusing host before viewport emulation is applied. User
    // navigation still happens only after these desktop metrics are verified.
    await this.ensureCaptureHost(entry);
    this.assertCurrentEntry(entry, lifecycleToken);
    await this.attachToCaptureHost(entry, false);
    this.assertCurrentEntry(entry, lifecycleToken);
    this.lifecycle(entry, 'before-initial-blank-navigation', { initialDocumentReady: entry.initialDocumentReady });
    await wc.loadURL(DEFAULT_URL);
    this.assertCurrentEntry(entry, lifecycleToken);
    this.lifecycle(entry, 'after-initial-blank-navigation');
    this.lifecycle(entry, 'before-initial-document-readiness');
    const initialDocument = await wc.executeJavaScript('({ url: location.href, readyState: document.readyState })');
    this.assertCurrentEntry(entry, lifecycleToken);
    if (!initialDocument || initialDocument.readyState === 'loading' || !String(initialDocument.url || '').startsWith(DEFAULT_URL)) {
      throw new Error('browser initial about:blank document did not become ready');
    }
    entry.initialDocumentReady = true;
    this.lifecycle(entry, 'after-initial-document-readiness', {
      url: String(initialDocument.url), readyState: initialDocument.readyState,
      initialDocumentReady: entry.initialDocumentReady,
    });
    this.lifecycle(entry, 'before-page-debugger-attach');
    await this.ensureCDP(entry);
    this.assertCurrentEntry(entry, lifecycleToken);
    this.lifecycle(entry, 'after-page-debugger-attach');
    this.lifecycle(entry, 'before-metrics', { ...entry.viewport, initialDocumentReady: entry.initialDocumentReady });
    const effective = await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
    this.assertCurrentEntry(entry, lifecycleToken);
    entry.effectiveViewport = effective;
    entry.viewportGeneration = 1;
    this.lifecycle(entry, 'after-metrics', { ...effective, initialDocumentReady: entry.initialDocumentReady });
    try { await wc.debugger.sendCommand('Runtime.enable'); } catch { /* metrics and basic page control remain available */ }
    return true;
  }

  async readEffectiveViewport(entry) {
    const effective = await entry.view.webContents.executeJavaScript('({ width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio, scrollX, scrollY, documentWidth: document.documentElement?.scrollWidth || 0, documentHeight: document.documentElement?.scrollHeight || 0 })');
    if (!effective || !Number.isFinite(Number(effective.width)) || !Number.isFinite(Number(effective.height)) || !Number.isFinite(Number(effective.deviceScaleFactor))) {
      throw new Error('browser page did not report effective viewport metrics');
    }
    return {
      width: Number(effective.width), height: Number(effective.height), deviceScaleFactor: Number(effective.deviceScaleFactor),
      scrollX: Number(effective.scrollX) || 0, scrollY: Number(effective.scrollY) || 0,
      documentWidth: Number(effective.documentWidth) || 0, documentHeight: Number(effective.documentHeight) || 0,
    };
  }

  async applyDeviceEmulation(entry, viewport = entry.viewport, scale = 1) {
    if (entry.initialDocumentReady !== true) {
      throw new Error('browser viewport emulation requires a ready initial about:blank document');
    }
    const webContents = entry.view.webContents;
    if (!webContents?.debugger?.isAttached?.()) throw new Error('browser viewport metrics require the owned CDP session');
    await webContents.debugger.sendCommand('Emulation.setDeviceMetricsOverride', {
      width: viewport.width,
      height: viewport.height,
      deviceScaleFactor: viewport.deviceScaleFactor,
      mobile: false,
      scale: Number.isFinite(Number(scale)) && Number(scale) > 0 ? Number(scale) : 1,
    });
  }

  async applyAndReadDeviceEmulation(entry, viewport = entry.viewport, scale = 1) {
    await this.applyDeviceEmulation(entry, viewport, scale);
    const effective = await this.readEffectiveViewport(entry);
    this.assertEffectiveViewport(viewport, effective);
    return effective;
  }

  assertEffectiveViewport(requested, effective) {
    if (effective.width !== requested.width || effective.height !== requested.height || effective.deviceScaleFactor !== requested.deviceScaleFactor) {
      throw new Error(`browser viewport metrics did not take effect: requested ${requested.width}x${requested.height} DPR ${requested.deviceScaleFactor}, observed ${effective.width}x${effective.height} DPR ${effective.deviceScaleFactor}`);
    }
  }

  async nativeHostDeviceScaleFactor() {
    const shellContents = this.win && this.win.webContents;
    if (!shellContents || typeof shellContents.executeJavaScript !== 'function') return 1;
    try {
      const zoom = Number(shellContents.getZoomFactor?.()) || 1;
      const deviceScaleFactor = Number(await shellContents.executeJavaScript('window.devicePixelRatio')) / zoom;
      return Number.isFinite(deviceScaleFactor) && deviceScaleFactor > 0 ? deviceScaleFactor : 1;
    } catch {
      return 1;
    }
  }

  serialize(entry, operation) {
    const next = entry.operationChain.catch(() => {}).then(() => {
      this.assertCurrentEntry(entry);
      return operation();
    });
    entry.operationChain = next.catch(() => {});
    return next;
  }

  async waitForOwnedTargetTasks(entry) {
    const deadline = Date.now() + 1000;
    while (true) {
      this.assertCurrentEntry(entry);
      const pending = Array.from(entry.pendingTargetTasks || []);
      if (!pending.length) {
        await new Promise((resolve) => setImmediate(resolve));
        if (!entry.pendingTargetTasks?.size) return;
      } else {
        const remaining = deadline - Date.now();
        if (remaining <= 0) throw new Error('owned iframe setup did not settle; browser navigation was not started');
        let timer;
        const finished = await Promise.race([
          Promise.allSettled(pending).then(() => true),
          new Promise((resolve) => { timer = setTimeout(() => resolve(false), Math.min(remaining, 50)); }),
        ]);
        clearTimeout(timer);
        if (!finished && Date.now() >= deadline && entry.pendingTargetTasks.size) {
          throw new Error('owned iframe setup did not settle; browser navigation was not started');
        }
      }
    }
  }

  serializePresentation(operation) {
    const next = this.presentationChain.catch(() => {}).then(operation);
    this.presentationChain = next.catch(() => {});
    return next;
  }

  async setViewport(entry, viewport) {
    return this.serializePresentation(() => this.setViewportSerialized(entry, viewport));
  }

  async setViewportSerialized(entry, viewport) {
    this.assertCurrentEntry(entry);
    const next = validateViewport(viewport.width, viewport.height);
    if (entry.viewport.width === next.width && entry.viewport.height === next.height && entry.viewport.deviceScaleFactor === next.deviceScaleFactor) {
      const effective = await this.readEffectiveViewport(entry);
      this.assertEffectiveViewport(next, effective);
      entry.effectiveViewport = effective;
      this.publish(entry);
      return this.tabInfo(entry);
    }
    const fitted = entry.visible && entry.presentationBounds ? presentationFor(next, entry.presentationBounds) : null;
    const effective = await this.applyAndReadDeviceEmulation(entry, next, fitted?.scale || 1);
    entry.viewport = next;
    entry.effectiveViewport = effective;
    entry.viewportGeneration += 1;
    entry.screenshots.clear();
    if (this.attachedView === entry.view) await this.attach(entry);
    else if (entry.captureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
      try { entry.captureHost.setBounds({ x: 0, y: 0, width: next.width, height: next.height }); } catch { /* the hidden surface keeps the page metrics */ }
      entry.view.setBounds({ x: 0, y: 0, width: next.width, height: next.height });
    }
    this.publish(entry);
    return this.tabInfo(entry);
  }

  async resetViewport(entry) {
    return this.setViewport(entry, DEFAULT_VIEWPORT);
  }

  get(chatId) {
    const id = safeChatId(chatId);
    return this.entries.get(id) || this.create(id);
  }

  async ensureCDP(entry) {
    if (entry.cdpReady === true) return true;
    const debug = entry.view.webContents.debugger;
    if (!debug || typeof debug.attach !== 'function') return false;
    try {
      if (!debug.isAttached()) debug.attach('1.3');
      const target = await debug.sendCommand('Target.getTargetInfo');
      const targetId = String(target && target.targetInfo && target.targetInfo.targetId || '');
      if (targetId) entry.rootTargetId = targetId;
      entry.cdpReady = true;
      await this.enableOwnedSession(entry);
      return true;
    } catch {
      entry.cdpReady = false;
      return false;
    }
  }

  // Build-health probe: initialize the persistent Chromium profile and attach
  // CDP without showing a view. Electron may start with the browser rail closed,
  // but rebuild health still needs to prove the browser process is usable.
  async probe() {
    const entry = this.get('__workass-health__');
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    return this.publish(entry);
  }

  async ensureCaptureHost(entry) {
    this.assertCurrentEntry(entry);
    if (entry.captureHost && !entry.captureHost.isDestroyed()) return entry.captureHost;
    if (!this.BrowserWindow) throw new Error('hidden browser capture host is unavailable in this shell');
    const host = new this.BrowserWindow({
      width: entry.viewport.width,
      height: entry.viewport.height,
      show: false,
      focusable: false,
      webPreferences: {
        partition: this.partition,
        nodeIntegration: false,
        contextIsolation: true,
        sandbox: true,
      },
    });
    entry.captureHost = host;
    host.on('closed', () => {
      if (entry.captureHost === host) {
        entry.captureHost = null;
        entry.captureHostAttached = false;
      }
    });
    try {
      await host.loadURL('about:blank');
      this.assertCurrentEntry(entry);
    } catch (error) {
      entry.captureHost = null;
      try { if (!host.isDestroyed()) host.destroy(); } catch { /* host never became usable */ }
      if (!this.isCurrentEntry(entry)) throw new Error('browser tab was closed during capture-host initialization');
      throw new Error(`hidden browser capture host failed to initialize: ${String(error && error.message || error)}`);
    }
    return host;
  }

  async attachToCaptureHost(entry, preserveVisible = false) {
    this.assertCurrentEntry(entry);
    const host = entry.captureHost;
    if (!host || host.isDestroyed()) throw new Error('hidden browser capture host is unavailable');
    if (this.attachedView === entry.view) {
      try { this.win.contentView.removeChildView(entry.view); } catch { /* window is already closing */ }
      this.attachedView = null;
    }
    if (!entry.captureHostAttached) {
      host.contentView.addChildView(entry.view);
      entry.captureHostAttached = true;
    }
    entry.view.setBounds({ x: 0, y: 0, width: entry.viewport.width, height: entry.viewport.height });
    if (entry.initialDocumentReady) {
      await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
      this.assertCurrentEntry(entry);
    }
    entry.visible = preserveVisible === true;
  }

  async attach(entry) {
    this.assertCurrentEntry(entry);
    if (this.attachedView !== entry.view) {
      if (this.attachedView) {
        const previous = this.browserEntries().find((candidate) => candidate.view === this.attachedView);
        if (previous) {
          previous.visible = false;
          try { this.win.contentView.removeChildView(this.attachedView); } catch { /* owning window is already closing */ }
          if (previous.captureHost && !previous.captureHost.isDestroyed() && !previous.captureHostAttached) {
            previous.captureHost.contentView.addChildView(previous.view);
            previous.captureHostAttached = true;
            previous.view.setBounds({ x: 0, y: 0, width: previous.viewport.width, height: previous.viewport.height });
            await this.applyAndReadDeviceEmulation(previous, previous.viewport, 1);
            this.assertCurrentEntry(entry);
          }
        } else {
          try { this.win.contentView.removeChildView(this.attachedView); } catch { /* owning window is already closing */ }
        }
        this.attachedView = null;
      }
      if (entry.captureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
        entry.captureHost.contentView.removeChildView(entry.view);
        entry.captureHostAttached = false;
      }
      this.win.contentView.addChildView(entry.view);
      this.attachedView = entry.view;
    }
    entry.visible = true;
    if (entry.presentationBounds) {
      const fitted = presentationFor(entry.viewport, entry.presentationBounds);
      entry.presentationScale = fitted.scale || 1;
      entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, entry.presentationScale);
      this.assertCurrentEntry(entry);
      entry.view.setBounds(fitted.bounds);
    }
  }

  activate(options) {
    return this.serializePresentation(() => this.activateSerialized(options));
  }

  async activateSerialized({ chatId, conversationId, bounds, url }) {
    const id = safeChatId(chatId);
    // Adopt any background entry the agent opened for this conversation (keyed by
    // its conversation id) so the user's visible view and the agent's browser are
    // ONE view, never a duplicate.
    if (conversationId) this.adoptBackground(id, String(conversationId));
    const entry = this.get(id);
    if (conversationId) entry.conversationId = String(conversationId);
    this.activeId = entry.chatId;
    entry.presentationBounds = safeBounds(bounds, this.win.getContentSize());
    entry.bounds = entry.presentationBounds;
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    // Opening the pane must always leave the user with a browser, so an
    // unsupported URL becomes a visible pane error instead of a failed open.
    const resolved = resolveBrowserURL(url);
    const target = resolved.error ? DEFAULT_URL : resolved.url;
    if (resolved.error) entry.error = resolved.error;
    await this.attach(entry);
    if (target !== DEFAULT_URL && (!entry.view.webContents.getURL() || entry.view.webContents.getURL() === DEFAULT_URL)) {
      entry.loading = true;
      entry.url = target;
      this.artifactNavigations.set(entry.view.webContents, target);
      void entry.view.webContents.loadURL(target).catch((err) => {
        entry.loading = false;
        entry.error = String(err && err.message || err);
        this.publish(entry);
      });
    }
    return this.publish(entry);
  }

  resize(chatId, bounds) {
    return this.serializePresentation(() => this.resizeSerialized(chatId, bounds));
  }

  async resizeSerialized(chatId, bounds) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry || this.activeId !== id) return false;
    entry.presentationBounds = safeBounds(bounds, this.win.getContentSize());
    entry.bounds = entry.presentationBounds;
    if (this.attachedView === entry.view) await this.attach(entry);
    return true;
  }

  hide(chatId) {
    return this.serializePresentation(() => this.hideSerialized(chatId));
  }

  async hideSerialized(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry || this.attachedView !== entry.view) return false;
    try { this.win.contentView.removeChildView(entry.view); } catch { /* owning window already gone */ }
    this.attachedView = null;
    this.activeId = null;
    if (entry.captureHost && !entry.captureHost.isDestroyed()) await this.attachToCaptureHost(entry, false);
    else entry.visible = false;
    return true;
  }

  async command(chatId, command, value) {
    const entry = this.get(chatId);
    const wc = entry.view.webContents;
    switch (command) {
      case 'navigate': {
        if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
        const target = normalizeBrowserURL(value);
        // Only (re)attach a view that is ALREADY the visible one — an agent
        // navigating a background browser (a chat the user isn't viewing) must
        // never yank that view into the window and steal the screen.
        if (this.attachedView === entry.view) {
          await this.serializePresentation(async () => {
            this.assertCurrentEntry(entry);
            if (this.attachedView === entry.view) await this.attach(entry);
          });
        }
        await this.waitForOwnedTargetTasks(entry);
        entry.loading = true;
        entry.url = target;
        entry.error = null;
        this.publish(entry);
        this.artifactNavigations.set(wc, target);
        this.lifecycle(entry, 'before-user-navigation');
        await wc.loadURL(target);
        await this.refreshFrameTree(entry);
        const scale = this.attachedView === entry.view ? entry.presentationScale : 1;
        entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, scale);
        this.publish(entry);
        this.lifecycle(entry, 'after-user-navigation');
        break;
      }
      case 'back': if (wc.navigationHistory.canGoBack()) wc.navigationHistory.goBack(); break;
      case 'forward': if (wc.navigationHistory.canGoForward()) wc.navigationHistory.goForward(); break;
      case 'reload': wc.reload(); break;
      case 'stop': wc.stop(); break;
      default: throw new Error(`unknown browser command: ${String(command)}`);
    }
    return this.publish(entry);
  }

  browserEntries() {
    return Array.from(this.entries.values()).filter((entry) => entry.chatId !== '__workass-health__');
  }

  tabInfo(entry) {
    const wc = entry.view.webContents;
    return {
      id: Number(wc.id),
      ...this.publicState(entry),
      chatId: entry.chatId,
      conversationId: entry.conversationId || null,
      url: redactPageMessage(wc.getURL() || entry.url || DEFAULT_URL),
      title: redactPageMessage(entry.title || ''),
      active: this.activeId === entry.chatId,
      viewport: { ...entry.viewport },
      effectiveViewport: entry.effectiveViewport ? { ...entry.effectiveViewport } : null,
      viewportGeneration: entry.viewportGeneration,
      documentGeneration: entry.documentGeneration,
      visible: entry.visible === true,
      supportedCaptureModes: ['viewport', 'full_page', 'clip'],
    };
  }

  entryBelongsToChat(entry, chatId) {
    const owner = String(chatId || '').trim();
    return !owner || entry.chatId === owner || entry.conversationId === owner;
  }

  browserTabs(chatId = '') {
    return this.browserEntries()
      .filter((entry) => this.entryBelongsToChat(entry, chatId))
      .map((entry) => this.tabInfo(entry));
  }

  entryForTab(tabId) {
    const id = Number(tabId);
    if (!Number.isInteger(id) || id <= 0) throw new Error('invalid browser tab id');
    const entry = this.browserEntries().find((candidate) => Number(candidate.view.webContents.id) === id);
    if (!entry) throw new Error(`browser tab not found: ${id}`);
    return entry;
  }

  ownedEntryForTab(tabId, chatId) {
    const entry = this.entryForTab(tabId);
    const owner = String(chatId || '').trim();
    if (owner && !this.entryBelongsToChat(entry, owner)) {
      throw new Error(`Workass browser tab ${Number(tabId)} belongs to another chat`);
    }
    return entry;
  }

  // The agent may open a browser for a conversation the user is NOT currently
  // viewing; that entry lives in the background keyed by the conversation id.
  // When the user finally opens that chat, the renderer activates with the UI
  // tab id — re-key the existing entry to it instead of spawning a second view.
  adoptBackground(tabId, conversationId) {
    if (this.entries.has(tabId)) return;
    for (const [key, entry] of this.entries) {
      if (key === tabId || key === '__workass-health__') continue;
      if (entry.conversationId === conversationId || entry.chatId === conversationId) {
        this.entries.delete(key);
        entry.chatId = tabId;
        this.entries.set(tabId, entry);
        if (this.activeId === key) this.activeId = tabId;
        return;
      }
    }
  }

  // browser.open is opt-in and per-chat: ask the renderer to mark the OWNING
  // chat's pane (never steal the active view) and ensure a live entry exists so
  // the agent can drive the browser immediately — visible or in the background.
  // The renderer adopts the background entry when the user opens that chat.
  openConversation(params) {
    const conversationId = String(params.chatId || '').trim();
    if (params.tabId != null) return this.ownedEntryForTab(params.tabId, conversationId);
    if (!conversationId) {
      // No conversation context (should not happen from the daemon): reuse the
      // active/first entry rather than orphan a new background view.
      if (this.activeId && this.activeId !== '__workass-health__') return this.entries.get(this.activeId);
      const entries = this.browserEntries();
      if (entries.length) return entries[0];
      throw new Error('no Workass browser tab is available');
    }
    if (params.visible !== false) this.requestOpen(conversationId);
    const matched = this.browserEntries().find((entry) => entry.conversationId === conversationId || entry.chatId === conversationId);
    if (matched) return matched;
    const entry = this.create(conversationId);
    entry.conversationId = conversationId;
    return entry;
  }

  async controlEntry(params = {}) {
    const conversationId = String(params.chatId || '').trim();
    if (params.tabId != null) return this.ownedEntryForTab(params.tabId, conversationId);
    if (conversationId) {
      const matched = this.browserEntries().find((entry) => entry.conversationId === conversationId || entry.chatId === conversationId);
      if (matched) return matched;
      throw new Error(`no Workass browser tab belongs to chat ${conversationId}; call browser.open first`);
    }
    if (this.activeId && this.activeId !== '__workass-health__') return this.entries.get(this.activeId);
    const entries = this.browserEntries();
    if (entries.length) return entries[0];
    throw new Error('no Workass browser tab is available');
  }

  async executeCDP(target, method, commandParams = {}) {
    const entry = this.entryForTab(target && target.tabId);
    await this.ensureCDP(entry);
    this.assertCurrentEntry(entry);
    const debug = entry.view.webContents.debugger;
    const commandName = String(method || '');
    if (['Target.attachToTarget', 'Target.setAutoAttach', 'Target.getTargets', 'Target.detachFromTarget'].includes(commandName)) {
      throw new Error('browser target management is restricted to the owned frame tree');
    }
    let sessionId = target && target.sessionId;
    if (!sessionId && target && target.targetId) {
      if (String(target.targetId) !== String(entry.rootTargetId)) {
        const owned = Array.from(entry.targetSessions.entries()).find(([, candidate]) => candidate.targetId === String(target.targetId));
        sessionId = owned && owned[0];
        if (!sessionId) throw new Error('browser child target is not in this tab owned frame tree');
      }
    }
    if (sessionId) {
      const owned = entry.targetSessions.get(sessionId);
      if (!owned || (target?.targetId && String(target.targetId) !== owned.targetId)) {
        throw new Error('browser child session is not owned by this tab and frame');
      }
    }
    return debug.sendCommand(commandName, commandParams || {}, sessionId || undefined);
  }

  async evaluateFrame(entry, frameId, expression, options = {}) {
    this.assertCurrentEntry(entry);
    const explicitFrameId = frameId != null;
    const requestedFrameId = explicitFrameId ? String(frameId) : null;
    let frame = explicitFrameId ? entry.frames.get(requestedFrameId) : this.frameForSession(entry, null);
    if (explicitFrameId && !frame) {
      throw new Error('browser frame is stale or no longer owned; refresh the snapshot');
    }
    if (!explicitFrameId && frame && frame.parentFrameId == null && frame.detached) {
      await this.refreshFrameTree(entry);
      frame = this.frameForSession(entry, null) || frame;
    }
    if (frame && frame.detached) {
      throw new Error('browser frame observation is stale');
    }
    if (!frame || (!frame.sessionId && frame.contextId == null)) {
      if (!explicitFrameId || frame.parentFrameId == null) {
        return entry.view.webContents.executeJavaScript(expression, true);
      }
      throw new Error('browser frame execution context is unavailable');
    }
    const target = frame.sessionId ? entry.targetSessions.get(frame.sessionId) : null;
    if (frame.sessionId && (!target || target.frameId !== frame.id || target.targetId !== frame.targetId)) {
      throw new Error('browser frame target is no longer owned');
    }
    const params = {
      expression: String(expression),
      returnByValue: options.returnByValue !== false,
      awaitPromise: options.awaitPromise !== false,
      ...(frame.contextId != null ? { contextId: frame.contextId } : {}),
    };
    const response = await entry.view.webContents.debugger.sendCommand(
      'Runtime.evaluate', params, frame.sessionId || undefined,
    );
    return unwrapCDPValue(response);
  }

  async dispatchBrowserKey(entry, parsed, { commands } = {}) {
    const tabId = this.tabInfo(entry).id;
    const key = cdpBrowserKey(parsed);
    const down = { type: key.text ? 'keyDown' : 'rawKeyDown', ...key };
    if (Array.isArray(commands) && commands.length) down.commands = commands;
    await this.executeCDP({ tabId }, 'Input.dispatchKeyEvent', down);
    const { text: _text, ...keyUp } = key;
    await this.executeCDP({ tabId }, 'Input.dispatchKeyEvent', { type: 'keyUp', ...keyUp });
  }

  async exposeBackendTarget(entry, target) {
    const frame = target.frameId ? entry.frames.get(target.frameId) : this.frameForSession(entry, null);
    if (!frame || frame.detached) throw new Error('observed frame is stale; refresh the snapshot');
    const sessionId = frame.sessionId || undefined;
    const resolved = await entry.view.webContents.debugger.sendCommand('DOM.resolveNode', {
      backendNodeId: target.backendDOMNodeId,
      ...(frame.contextId != null ? { executionContextId: frame.contextId } : {}),
    }, sessionId);
    const objectId = resolved?.object?.objectId;
    if (!objectId) throw new Error('observed node is stale; refresh the snapshot');
    try {
      const installed = await entry.view.webContents.debugger.sendCommand('Runtime.callFunctionOn', {
        objectId,
        functionDeclaration: "function(){ if(!this||!this.isConnected)return false; globalThis[Symbol.for('workass.browser.action-target')]=this; return true; }",
        returnByValue: true,
      }, sessionId);
      if (installed?.result?.value !== true) throw new Error('observed node is stale; refresh the snapshot');
    } finally {
      try { await entry.view.webContents.debugger.sendCommand('Runtime.releaseObject', { objectId }, sessionId); } catch { /* context owns the remote object */ }
    }
    return {
      clear: () => this.evaluateFrame(entry, target.frameId, "delete globalThis[Symbol.for('workass.browser.action-target')]"),
    };
  }

  async frameOwnerIsDeepActive(entry, parent, child) {
    if (!parent || !child || child.parentFrameId !== parent.id || parent.detached || child.detached) return false;
    if (child.sessionId) {
      const owned = entry.targetSessions.get(child.sessionId);
      if (!owned || owned.frameId !== child.id || owned.targetId !== child.targetId) return false;
    } else if (child.targetId !== parent.targetId) {
      return false;
    }
    const debug = entry.view.webContents.debugger;
    const sessionId = parent.sessionId || undefined;
    let ownerObjectId = null;
    try {
      const owner = await debug.sendCommand('DOM.getFrameOwner', { frameId: child.id }, sessionId);
      const backendNodeId = Number(owner && owner.backendNodeId);
      if (!Number.isSafeInteger(backendNodeId) || backendNodeId <= 0) return false;
      const resolved = await debug.sendCommand('DOM.resolveNode', {
        backendNodeId,
        ...(parent.contextId != null ? { executionContextId: parent.contextId } : {}),
      }, sessionId);
      ownerObjectId = resolved?.object?.objectId || null;
      if (!ownerObjectId) return false;
      const active = await debug.sendCommand('Runtime.callFunctionOn', {
        objectId: ownerObjectId,
        functionDeclaration: `function() {
          let active = this.ownerDocument?.activeElement || null;
          const seen = new Set();
          while (active && !seen.has(active)) {
            seen.add(active);
            const nested = active.shadowRoot?.activeElement || null;
            if (!nested || nested === active) break;
            active = nested;
          }
          return active === this;
        }`,
        returnByValue: true,
      }, sessionId);
      return active?.result?.value === true;
    } catch {
      return false;
    } finally {
      if (ownerObjectId) {
        try { await debug.sendCommand('Runtime.releaseObject', { objectId: ownerObjectId }, sessionId); }
        catch { /* a parent navigation already released this frame owner */ }
      }
    }
  }

  async focusedFrame(entry) {
    let frame = this.frameForSession(entry, null);
    if (!frame || frame.detached) return frame;
    for (let depth = 0; depth < 64; depth += 1) {
      let state;
      try {
        state = await this.evaluateFrame(entry, frame.id, `(/* workass-browser-focused-frame-state */ () => {
          let active = document.activeElement;
          const seen = new Set();
          while (active && !seen.has(active)) {
            seen.add(active);
            const nested = active.shadowRoot?.activeElement || null;
            if (!nested || nested === active) break;
            active = nested;
          }
          return {
            hasFocus: document.hasFocus() === true,
            activeFrame: active?.localName === 'iframe' || active?.localName === 'frame',
          };
        })()`);
      } catch { return frame; }
      if (state?.hasFocus !== true || state.activeFrame !== true) return frame;
      const children = Array.from(entry.frames.values()).filter((candidate) =>
        candidate.parentFrameId === frame.id && !candidate.detached);
      const matches = [];
      for (const child of children) {
        if (await this.frameOwnerIsDeepActive(entry, frame, child)) matches.push(child);
      }
      if (matches.length !== 1) return frame;
      try {
        if (await this.evaluateFrame(entry, matches[0].id, 'document.hasFocus()') !== true) return frame;
      } catch { return frame; }
      frame = matches[0];
    }
    return frame;
  }

  async focusedTextLength(entry, frameId = null) {
    return this.evaluateFrame(entry, frameId, `(/* workass-browser-key-text-length */ () => {
      let el = document.activeElement;
      const seen = new Set();
      while (el && !seen.has(el)) {
        seen.add(el);
        const nested = el.shadowRoot?.activeElement || null;
        if (!nested || nested === el) break;
        el = nested;
      }
      if (!el || el === document.body || el === document.documentElement) return { found: false };
      const editorRoot = el.closest?.('.monaco-editor,.CodeMirror,.cm-editor') || null;
      try {
        if (editorRoot?.classList.contains('monaco-editor')) {
          const api = globalThis.monaco?.editor;
          const editors = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          const editor = editors.find((candidate) => candidate?.getDomNode?.() === editorRoot);
          const value = editor?.getModel?.()?.getValue?.();
          if (typeof value === 'string') return { found: true, length: value.length };
        }
        if (editorRoot?.classList.contains('CodeMirror') && editorRoot.CodeMirror) {
          return { found: true, length: String(editorRoot.CodeMirror.getValue() || '').length };
        }
        const view = el.ownerDocument.defaultView;
        if (!editorRoot && (el instanceof view.HTMLInputElement || el instanceof view.HTMLTextAreaElement)) {
          if (String(el.type || '').toLowerCase() === 'password') return { found: false };
          return { found: true, length: String(el.value || '').length };
        }
        if (el.isContentEditable) return { found: true, length: String(el.innerText || el.textContent || '').length };
      } catch { /* readback is optional; CDP key input remains primary */ }
      return { found: false };
    })()`);
  }

  async selectAllBrowserTarget(entry, frameId = null) {
    return this.evaluateFrame(entry, frameId, `(/* workass-browser-select-all */ () => {
      const document = globalThis.document;
      const view = document.defaultView;
      let el = document.activeElement;
      while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
      if (!el || el === document.body || el === document.documentElement) {
        return { found: false, selectionVerified: false, strategy: 'none' };
      }
      const editorRoot = el.closest?.('.monaco-editor,.CodeMirror,.cm-editor') || null;
      try {
        if (editorRoot?.classList.contains('monaco-editor')) {
          const api = globalThis.monaco?.editor;
          const editors = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          const editor = editors.find((candidate) => candidate?.getDomNode?.() === editorRoot);
          const model = editor?.getModel?.();
          const range = model?.getFullModelRange?.();
          if (editor && range) {
            editor.focus();
            editor.setSelection(range);
            return { found: true, selectionVerified: true, strategy: 'monaco-model' };
          }
        }
        if (editorRoot?.classList.contains('CodeMirror') && editorRoot.CodeMirror) {
          editorRoot.CodeMirror.execCommand('selectAll');
          return { found: true, selectionVerified: true, strategy: 'codemirror-command' };
        }
        if (el.isContentEditable) {
          const selection = view.getSelection();
          const range = document.createRange();
          range.selectNodeContents(el);
          selection.removeAllRanges();
          selection.addRange(range);
          const empty = !String(el.innerText || el.textContent || '');
          return {
            found: true,
            selectionVerified: empty || (selection.rangeCount === 1 && !selection.getRangeAt(0).collapsed),
            strategy: 'contenteditable-range',
          };
        }
        if (!editorRoot && (el instanceof view.HTMLInputElement || el instanceof view.HTMLTextAreaElement)) {
          el.setSelectionRange(0, String(el.value || '').length);
          return {
            found: true,
            selectionVerified: el.selectionStart === 0 && el.selectionEnd === String(el.value || '').length,
            strategy: 'form-control-range',
          };
        }
      } catch { /* keyboard dispatch remains the fallback */ }
      return { found: true, selectionVerified: false, strategy: 'keyboard-fallback' };
    })()`);
  }

  async deleteSelectedBrowserText(entry, frameId, keyCode) {
    const command = JSON.stringify(keyCode === 'Delete' ? 'forwardDelete' : 'delete');
    return this.evaluateFrame(entry, frameId, `(/* workass-browser-delete-selection */ () => {
      const document = globalThis.document;
      const view = document.defaultView;
      let el = document.activeElement;
      while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
      if (!el || el === document.body || el === document.documentElement) {
        return { found: false, deletionVerified: false, strategy: 'none' };
      }
      const editorRoot = el.closest?.('.monaco-editor,.CodeMirror,.cm-editor') || null;
      try {
        if (editorRoot?.classList.contains('monaco-editor')) {
          const api = globalThis.monaco?.editor;
          const editors = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          const editor = editors.find((candidate) => candidate?.getDomNode?.() === editorRoot);
          const model = editor?.getModel?.();
          const selection = editor?.getSelection?.();
          if (editor && model && selection && !selection.isEmpty()) {
            const before = model.getValueLength();
            editor.executeEdits('workass-browser-key', [{ range: selection, text: '', forceMoveMarkers: true }]);
            return { found: true, deletionVerified: model.getValueLength() < before, strategy: 'monaco-model' };
          }
        }
        if (editorRoot?.classList.contains('CodeMirror') && editorRoot.CodeMirror?.somethingSelected?.()) {
          const before = String(editorRoot.CodeMirror.getValue()).length;
          editorRoot.CodeMirror.replaceSelection('');
          return {
            found: true,
            deletionVerified: String(editorRoot.CodeMirror.getValue()).length < before,
            strategy: 'codemirror-selection',
          };
        }
        if (el.isContentEditable) {
          const selection = view.getSelection();
          if (selection?.rangeCount && !selection.getRangeAt(0).collapsed) {
            const before = String(el.innerText || el.textContent || '').length;
            view.document.execCommand(${command}, false, null);
            const after = String(el.innerText || el.textContent || '').length;
            return { found: true, deletionVerified: after < before, strategy: 'contenteditable-command' };
          }
        }
        if (!editorRoot && (el instanceof view.HTMLInputElement || el instanceof view.HTMLTextAreaElement)) {
          const start = Number(el.selectionStart);
          const end = Number(el.selectionEnd);
          if (Number.isInteger(start) && Number.isInteger(end) && end > start) {
            const before = String(el.value || '');
            const next = before.slice(0, start) + before.slice(end);
            const prototype = el instanceof view.HTMLTextAreaElement ? view.HTMLTextAreaElement.prototype : view.HTMLInputElement.prototype;
            const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
            if (typeof setter === 'function') setter.call(el, next);
            else el.value = next;
            el.setSelectionRange(start, start);
            let event;
            try { event = new view.InputEvent('input', { bubbles: true, inputType: ${command} === 'forwardDelete' ? 'deleteContentForward' : 'deleteContentBackward' }); }
            catch { event = new view.Event('input', { bubbles: true }); }
            el.dispatchEvent(event);
            return { found: true, deletionVerified: String(el.value || '') === next, strategy: 'form-control-selection' };
          }
        }
      } catch { /* the CDP key event remains the primary attempt */ }
      return { found: false, deletionVerified: false, strategy: 'keyboard-only' };
    })()`);
  }

  async attachTarget(tabId, targetId) {
    const entry = this.entryForTab(tabId);
    await this.ensureCDP(entry);
    const owned = Array.from(entry.targetSessions.entries()).find(([, target]) => target.targetId === String(targetId));
    if (!owned) throw new Error('browser target is not an owned iframe in this tab');
    return { sessionId: owned[0] };
  }

  async detachTarget(tabId, targetId) {
    const entry = this.entryForTab(tabId);
    const owned = Array.from(entry.targetSessions.entries()).find(([, target]) => target.targetId === String(targetId));
    if (!owned) return {};
    await this.detachOwnedSession(entry, owned[0], 'explicit_detach');
    return {};
  }

  async submitBrowserType(entry, locator) {
    const target = targetExpression(locator);
    const submit = await this.evaluateFrame(entry, locator.frameId ?? null, `(() => {
      const el = ${target};
      if (!el) return { found: false };
      el.focus();
      if (el.form && typeof el.form.requestSubmit === 'function') {
        el.form.requestSubmit();
        return { found: true, submitted: true, strategy: 'form' };
      }
      return { found: true, submitted: false, strategy: 'enter-key' };
    })()`);
    if (!submit || submit.found !== true) throw new Error('browser type target disappeared before submit');
    if (!submit.submitted) {
      await this.dispatchBrowserKey(entry, parseBrowserKey('Enter', this.platform));
      submit.submitted = true;
    }
    return submit;
  }

  async browserType(entry, params = {}, locator = { selector: String(params.selector || '') }) {
    const wc = entry.view.webContents;
    const selectorText = String(params.selector || '');
    const text = String(params.text ?? '');
    const selector = targetExpression(locator);
    const value = JSON.stringify(text);
    const evaluate = (script) => this.evaluateFrame(entry, locator.frameId ?? null, script);
    const probeKey = `__workassBrowserTypeProbe${++this.typeProbeSeq}`;
    const encodedProbeKey = JSON.stringify(probeKey);

    // Ordinary form controls keep the deterministic value-set path, but use
    // the native prototype setter so React/Vue value trackers observe the
    // input event. Hidden editor textareas and contenteditable surfaces must be
    // driven like a user: focus, select all, then insert native text.
    const prepared = await evaluate(`(/* workass-browser-type-prepare */ () => {
      const el = ${selector};
      if (!el) return { found: false };
      const view = el.ownerDocument.defaultView;
      const input = el instanceof view.HTMLInputElement;
      const textarea = el instanceof view.HTMLTextAreaElement;
      const blockedInputTypes = new Set(['button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit']);
      const formControl = textarea || (input && !blockedInputTypes.has(String(el.type || '').toLowerCase()));
      const editorRoot = el.matches?.('.monaco-editor,.CodeMirror,.cm-editor')
        ? el
        : el.closest('.monaco-editor,.CodeMirror,.cm-editor');
      const monacoEditor = (() => {
        if (!editorRoot?.classList.contains('monaco-editor')) return null;
        try {
          const api = globalThis.monaco?.editor;
          const editors = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          return editors.find((candidate) => candidate?.getDomNode?.() === editorRoot) || null;
        } catch { return null; }
      })();
      const nativeEditor = !!editorRoot || el.isContentEditable || (!formControl && el.getAttribute('role') === 'textbox');
      el.scrollIntoView({ block: 'center', inline: 'center' });
      if (monacoEditor) monacoEditor.focus();
      else if (editorRoot && el === editorRoot) {
        const editorInput = editorRoot.querySelector('textarea:not([readonly]),[contenteditable]:not([contenteditable="false"])');
        if (editorInput) editorInput.focus();
        else el.focus();
      } else el.focus();
      const activeElement = () => {
        let active = document.activeElement;
        while (active?.shadowRoot?.activeElement) active = active.shadowRoot.activeElement;
        return active;
      };
      const focused = () => activeElement() === el || (!!editorRoot && editorRoot.contains(activeElement()));
      const editorReadOnly = monacoEditor?.getRawOptions?.()?.readOnly === true;
      if (editorReadOnly || (!editorRoot && (el.disabled || el.readOnly))) {
        return { found: true, editable: false, focused: focused(), reason: 'disabled or read-only' };
      }
      if (!formControl && !nativeEditor) {
        return { found: true, editable: false, focused: focused(), reason: 'target is not editable' };
      }
      if (!nativeEditor) {
        const before = String(el.value ?? '');
        const prototype = textarea ? view.HTMLTextAreaElement.prototype : view.HTMLInputElement.prototype;
        const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
        if (typeof setter !== 'function') {
          return { found: true, editable: false, focused: focused(), reason: 'value setter is unavailable' };
        }
        setter.call(el, ${value});
        let inputEvent;
        try {
          inputEvent = new view.InputEvent('input', { bubbles: true, inputType: 'insertReplacementText', data: ${value} });
        } catch {
          inputEvent = new view.Event('input', { bubbles: true });
        }
        el.dispatchEvent(inputEvent);
        el.dispatchEvent(new view.Event('change', { bubbles: true }));
        return new Promise((resolve) => setTimeout(() => {
          const retained = String(el.value ?? '') === ${value};
          resolve({
            found: true, editable: true, strategy: 'value', focused: focused(),
            changed: before !== String(el.value ?? ''), replacementVerified: retained,
            valueLength: String(el.value ?? '').length,
          });
        }, 0));
      }

      const readModel = () => {
        try {
          const api = globalThis.monaco?.editor;
          if (!api) return null;
          if (typeof api.getEditors === 'function' && editorRoot) {
            const matches = api.getEditors().filter((editor) => editor?.getDomNode?.() === editorRoot);
            if (matches.length === 1) return String(matches[0].getModel()?.getValue?.() ?? '');
          }
          if (typeof api.getModels === 'function') {
            const models = api.getModels().filter((model) => !model?.isDisposed?.());
            if (models.length === 1) return String(models[0].getValue());
          }
        } catch { /* optional Monaco readback */ }
        return null;
      };
      const readVisible = () => {
        const root = editorRoot || el;
        const lines = root.querySelectorAll('.view-lines .view-line,.CodeMirror-code pre,.cm-content .cm-line');
        if (lines.length) return Array.from(lines).map((line) => String(line.textContent || '')).join('\\n');
        if (el.isContentEditable) return String(el.innerText || el.textContent || '');
        return '';
      };
      const probe = {
        el,
        beforeModel: readModel(),
        beforeVisible: readVisible(),
        beforeInputEvents: 0,
        inputEvents: 0,
        dataLengths: [],
        inputTypes: [],
      };
      probe.onBeforeInput = (event) => {
        probe.beforeInputEvents += 1;
        probe.dataLengths.push(typeof event.data === 'string' ? event.data.length : null);
        probe.inputTypes.push(String(event.inputType || ''));
      };
      probe.onInput = (event) => {
        probe.inputEvents += 1;
        probe.dataLengths.push(typeof event.data === 'string' ? event.data.length : null);
        probe.inputTypes.push(String(event.inputType || ''));
      };
      el.addEventListener('beforeinput', probe.onBeforeInput, true);
      el.addEventListener('input', probe.onInput, true);
      globalThis[${encodedProbeKey}] = probe;
      let selectionVerified = false;
      let selectionStrategy = 'keyboard-fallback';
      try {
        if (editorRoot?.classList.contains('monaco-editor')) {
          const api = globalThis.monaco?.editor;
          const editors = typeof api?.getEditors === 'function' ? api.getEditors() : [];
          const editor = editors.find((candidate) => candidate?.getDomNode?.() === editorRoot);
          const model = editor?.getModel?.();
          const range = model?.getFullModelRange?.();
          if (editor && range) {
            editor.focus();
            editor.setSelection(range);
            selectionVerified = true;
            selectionStrategy = 'monaco-model';
          }
        }
        if (!selectionVerified && editorRoot?.classList.contains('CodeMirror') && editorRoot.CodeMirror) {
          editorRoot.CodeMirror.execCommand('selectAll');
          selectionVerified = true;
          selectionStrategy = 'codemirror-command';
        }
        if (!selectionVerified && el.isContentEditable) {
          const selection = view.getSelection();
          const range = document.createRange();
          range.selectNodeContents(el);
          selection.removeAllRanges();
          selection.addRange(range);
          const empty = !String(el.innerText || el.textContent || '');
          selectionVerified = empty || (selection.rangeCount === 1 && !selection.getRangeAt(0).collapsed);
          selectionStrategy = 'contenteditable-range';
        }
      } catch { /* CDP keyboard selection remains the fallback */ }
      return {
        found: true, editable: true, strategy: 'native', focused: focused(),
        editor: editorRoot ? (editorRoot.classList.contains('monaco-editor') ? 'monaco' : 'code-editor') : 'contenteditable',
        selectionVerified, selectionStrategy,
      };
    })()`);

    if (!prepared || prepared.found !== true) return { found: false, status: locator.path ? 'stale' : 'missing', reason: locator.path ? 'observed element was replaced; refresh the snapshot' : 'selector target is missing' };
    if (prepared.editable !== true) {
      throw new Error(`browser type target is not editable${prepared.reason ? `: ${prepared.reason}` : ''}`);
    }
    if (prepared.focused !== true) throw new Error('browser type target could not be focused');

    if (prepared.strategy === 'value') {
      if (prepared.replacementVerified !== true) throw new Error('browser type target did not retain the replacement text');
      let submitted = false;
      if (params.submit === true) submitted = (await this.submitBrowserType(entry, locator)).submitted === true;
      entry.interactionGeneration += 1;
      return { ...prepared, submitted };
    }
    if (prepared.strategy !== 'native') throw new Error('browser type selected an unknown edit strategy');

    const cleanupProbe = async () => {
      try {
        await evaluate(`(() => {
          const probe = globalThis[${encodedProbeKey}];
          if (probe?.el) {
            probe.el.removeEventListener('beforeinput', probe.onBeforeInput, true);
            probe.el.removeEventListener('input', probe.onInput, true);
          }
          delete globalThis[${encodedProbeKey}];
        })()`);
      } catch { /* navigation or renderer teardown owns the cleanup */ }
    };

    let verification;
    try {
      if (prepared.selectionVerified !== true) {
        await this.dispatchBrowserKey(entry, parseBrowserKey('CommandOrControl+A', this.platform), { commands: ['selectAll'] });
      }
      // Cross the renderer event loop once so the editor consumes Select All
      // before the replacement text reaches its hidden input surface.
      await evaluate('(/* workass-browser-input-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
      if (text) {
        await this.executeCDP({ tabId: this.tabInfo(entry).id }, 'Input.insertText', { text });
      } else {
        await this.dispatchBrowserKey(entry, parseBrowserKey('Backspace', this.platform));
        await evaluate('(/* workass-browser-delete-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
        await this.deleteSelectedBrowserText(entry, locator.frameId ?? null, 'Backspace');
      }

      verification = await evaluate(`(/* workass-browser-type-verify */ async () => {
        const expected = ${value};
        const normalize = (raw) => String(raw ?? '').replace(/\\r\\n?/gu, '\\n').replace(/\\u00a0/gu, ' ');
        const compact = (raw) => normalize(raw).replace(/\\s+/gu, ' ').trim();
        const visibleMatches = (visible) => {
          const observed = compact(visible);
          const wanted = compact(expected);
          if (!wanted) return !observed;
          return !!observed && wanted.includes(observed);
        };
        const readState = () => {
          const probe = globalThis[${encodedProbeKey}];
          const el = ${selector};
          if (!probe || !el) return { found: false };
          const editorRoot = el.closest('.monaco-editor,.CodeMirror,.cm-editor');
          let model = null;
          try {
            const api = globalThis.monaco?.editor;
            if (api && typeof api.getEditors === 'function' && editorRoot) {
              const matches = api.getEditors().filter((editor) => editor?.getDomNode?.() === editorRoot);
              if (matches.length === 1) model = String(matches[0].getModel()?.getValue?.() ?? '');
            }
            if (model === null && api && typeof api.getModels === 'function') {
              const models = api.getModels().filter((candidate) => !candidate?.isDisposed?.());
              if (models.length === 1) model = String(models[0].getValue());
            }
          } catch { /* optional Monaco readback */ }
          const root = editorRoot || el;
          const lines = root.querySelectorAll('.view-lines .view-line,.CodeMirror-code pre,.cm-content .cm-line');
          const visible = lines.length
            ? Array.from(lines).map((line) => String(line.textContent || '')).join('\\n')
            : (el.isContentEditable ? String(el.innerText || el.textContent || '') : '');
          const emptyContentEditable = el.isContentEditable && expected === ''
            && String(el.textContent || '').replace(/\u200b/gu, '') === '';
          const exact = model !== null
            ? normalize(model) === normalize(expected)
            : (el.isContentEditable
              ? (expected === '' ? emptyContentEditable : normalize(visible) === normalize(expected))
              : false);
          const changed = model !== null
            ? normalize(model) !== normalize(probe.beforeModel)
            : normalize(visible) !== normalize(probe.beforeVisible);
          const inputAccepted = probe.beforeInputEvents > 0 || probe.inputEvents > 0;
          return {
            found: true,
            focused: document.activeElement === el || (!!editorRoot && editorRoot.contains(document.activeElement)),
            changed,
            inputAccepted,
            exact,
            visibleMatch: visibleMatches(visible),
            beforeInputEvents: probe.beforeInputEvents,
            inputEvents: probe.inputEvents,
            valueLength: model !== null ? model.length : null,
            verification: exact ? (model !== null ? 'model' : 'contenteditable') : (visibleMatches(visible) ? 'visible-fragment' : 'none'),
          };
        };
        let state = readState();
        for (let attempt = 0; attempt < 20 && state.found && !state.exact && !state.visibleMatch; attempt += 1) {
          await new Promise((resolve) => setTimeout(resolve, 25));
          state = readState();
        }
        const probe = globalThis[${encodedProbeKey}];
        if (probe?.el) {
          probe.el.removeEventListener('beforeinput', probe.onBeforeInput, true);
          probe.el.removeEventListener('input', probe.onInput, true);
        }
        delete globalThis[${encodedProbeKey}];
        return state;
      })()`);
    } catch (error) {
      await cleanupProbe();
      throw error;
    }

    if (!verification || verification.found !== true) throw new Error('browser type target disappeared during replacement');
    if (verification.focused !== true) throw new Error('browser type target lost focus during replacement');
    if (verification.inputAccepted !== true && verification.exact !== true) {
      throw new Error('browser type replacement did not reach the editor input');
    }
    if (verification.exact !== true && verification.visibleMatch !== true) {
      throw new Error('browser type could not observe the replacement in the editor');
    }
    if (verification.changed !== true && verification.exact !== true) {
      throw new Error('browser type did not change the editor');
    }

    let submitted = false;
    if (params.submit === true) submitted = (await this.submitBrowserType(entry, locator)).submitted === true;
    entry.interactionGeneration += 1;
    return {
      found: true,
      editable: true,
      focused: true,
      strategy: 'native',
      changed: verification.changed === true,
      replacementVerified: verification.exact === true,
      observed: verification.visibleMatch === true,
      verification: verification.verification,
      valueLength: verification.valueLength,
      submitted,
    };
  }

  async backendNodeKeys(entry, frame, backendDOMNodeId) {
    const debug = entry.view.webContents.debugger;
    const sessionId = frame.sessionId || undefined;
    let resolved;
    try {
      resolved = await debug.sendCommand('DOM.resolveNode', {
        backendNodeId: Number(backendDOMNodeId),
        ...(frame.contextId != null ? { executionContextId: frame.contextId } : {}),
      }, sessionId);
    } catch { return []; }
    const objectId = resolved && resolved.object && resolved.object.objectId;
    if (!objectId) return [];
    try {
      const response = await debug.sendCommand('Runtime.callFunctionOn', {
        objectId,
        functionDeclaration: "function(){ const refs=globalThis[Symbol.for('workass.browser.observed-node-refs')]; return refs&&refs.get(this)?Array.from(refs.get(this)):[]; }",
        returnByValue: true,
      }, sessionId);
      return Array.isArray(response && response.result && response.result.value)
        ? response.result.value.map(String)
        : [];
    } catch { return []; }
    finally {
      try { await debug.sendCommand('Runtime.releaseObject', { objectId }, sessionId); } catch { /* resolved node released with its context */ }
    }
  }

  async browserSnapshot(entry) {
    this.assertCurrentEntry(entry);
    await this.ensureCDP(entry);
    await this.refreshFrameTree(entry);
    const documentGeneration = entry.documentGeneration;
    const viewportGeneration = entry.viewportGeneration;
    const interactionGeneration = entry.interactionGeneration;
    const frameTreeGeneration = entry.frameTreeGeneration;
    const snapshotId = crypto.randomUUID();
    const liveFrames = Array.from(entry.frames.values())
      .filter((frame) => !frame.detached)
      .sort((a, b) => Number(a.parentFrameId != null) - Number(b.parentFrameId != null))
      .slice(0, SNAPSHOT_FRAME_LIMIT);
    const frameSignature = JSON.stringify(liveFrames.map((frame) => [
      frame.id, frame.parentFrameId, frame.generation, frame.loaderId, frame.targetId, frame.sessionId, frame.contextId,
    ]).sort((a, b) => String(a[0]).localeCompare(String(b[0]))));
    const rootFrame = liveFrames.find((frame) => frame.parentFrameId == null && frame.targetId === entry.rootTargetId) || null;
    const sources = [];
    let textSource = null;
    const traversalErrors = [];
    let visitedElements = 0;
    let observedAxTree = false;
    let axNodesScanned = 0;
    let axNodesTruncated = false;
    let axNodesTruncatedCount = 0;
    let textBytesScanned = 0;
    let textLengthExact = true;
    let editorRootCount = 0;
    let editorsTruncated = false;
    let frameListTruncated = entry.frames.size > SNAPSHOT_FRAME_LIMIT;
    let nodeCount = 0;
    let semanticTruncatedCount = 0;

    const observe = async (frame) => {
      const script = observationScript({ snapshotId, maxNodes: 500, maxText: 32 * 1024, rootOnly: true });
      if (frame) return this.evaluateFrame(entry, frame.id, script);
      return entry.view.webContents.executeJavaScript(script, true);
    };

    for (const frame of liveFrames.length ? liveFrames : [null]) {
      const source = { frame, raw: null, refs: [], semantic: [], interactive: [], editors: [], frames: [] };
      if (frame && frame.unavailableReason && frame.contextId == null && frame.sessionId == null) {
        frame.access = 'inaccessible';
        source.error = frame.unavailableReason;
        sources.push(source);
        continue;
      }
      try {
        const raw = await observe(frame);
        if (!raw || typeof raw !== 'object' || raw.snapshotId !== snapshotId) {
          source.error = 'frame_observation_unavailable';
          sources.push(source);
          continue;
        }
        source.raw = raw;
        source.refs = Array.isArray(raw.refs) ? raw.refs : [];
        source.semantic = Array.isArray(raw.semantic) ? raw.semantic : [];
        source.interactive = Array.isArray(raw.interactive) ? raw.interactive : [];
        source.editors = Array.isArray(raw.editors) ? raw.editors : [];
        source.frames = Array.isArray(raw.frames) ? raw.frames : [];
        const isRootObservation = !frame || frame.id === rootFrame?.id;
        if (isRootObservation) textSource = raw;
        visitedElements += Number(raw.traversal?.visitedElements) || 0;
        if (raw.traversal?.errors) traversalErrors.push(...raw.traversal.errors.slice(0, 32));
        if (isRootObservation) {
          textBytesScanned = Number(raw.textBytesScanned) || textBytesScanned;
          textLengthExact = raw.textLengthExact === true;
        }
        editorRootCount += Number(raw.editorRootCount) || source.editors.length;
        editorsTruncated = editorsTruncated || raw.editorsTruncated === true;
        nodeCount += Number(raw.nodeCount) || source.semantic.length;
        semanticTruncatedCount += Number(raw.semanticTruncatedCount) || 0;
        sources.push(source);
      } catch (error) {
        source.error = frame && frame.parentFrameId != null ? 'frame_execution_context_unavailable' : 'document_observation_failed';
        source.errorName = String(error && error.name || 'Error');
        sources.push(source);
      }
    }

    const combinedRefs = [];
    const semantic = [];
    const interactive = [];
    const editors = [];
    const domFrameOwners = new Map();
    let helperFrameCount = 0;
    for (const source of sources) {
      const offset = combinedRefs.length;
      for (const ref of source.refs) {
        combinedRefs.push({
          path: Array.isArray(ref.path) ? ref.path : [],
          nodeKey: ref.nodeKey == null ? null : String(ref.nodeKey),
          selector: String(ref.selector || ''),
          frameId: source.frame?.id || null,
          frameGeneration: source.frame?.generation ?? null,
          frameLoaderId: source.frame?.loaderId || '',
          targetId: source.frame?.targetId || entry.rootTargetId,
          sessionId: source.frame?.sessionId || null,
          contextId: source.frame?.contextId ?? null,
          backendDOMNodeId: null,
        });
      }
      for (const node of source.semantic) {
        const refIndex = Number(node.refIndex);
        if (!Number.isInteger(refIndex) || refIndex < 0 || refIndex >= source.refs.length) continue;
        semantic.push({ ...node, refIndex: offset + refIndex, frameId: source.frame?.id || null });
      }
      for (const node of source.interactive) {
        const refIndex = Number(node.refIndex);
        if (!Number.isInteger(refIndex) || refIndex < 0 || refIndex >= source.refs.length) continue;
        interactive.push({ ...node, refIndex: offset + refIndex, frameId: source.frame?.id || null });
      }
      for (const editor of source.editors) {
        editors.push({ ...editor, frameId: source.frame?.id || null });
      }
      for (const boundary of source.frames) {
        helperFrameCount += 1;
        const refIndex = Number(boundary.refIndex);
        const ref = Number.isInteger(refIndex) ? combinedRefs[offset + refIndex] : null;
        if (ref) domFrameOwners.set((source.frame?.id || '') + ':' + String(ref.nodeKey), boundary);
      }
    }

    const axByFrame = new Map();
    for (const source of sources) {
      const frame = source.frame;
      if (!frame || !frame.id || !source.raw) continue;
      const sessionId = frame.sessionId || undefined;
      let response;
      try {
        response = await entry.view.webContents.debugger.sendCommand(
          'Accessibility.getFullAXTree', { frameId: frame.id }, sessionId,
        );
      } catch {
        frame.axAvailable = false;
        continue;
      }
      const axNodes = Array.isArray(response && response.nodes) ? response.nodes : [];
      frame.axAvailable = true;
      observedAxTree = true;
      const key = frame.id;
      const frameNodes = [];
      const byNodeKey = new Map();
      for (const ref of combinedRefs) {
        if (ref.frameId === frame.id && ref.nodeKey) byNodeKey.set(ref.nodeKey, ref);
      }
      for (let axIndex = 0; axIndex < axNodes.length; axIndex += 1) {
        if (axNodesScanned >= AX_NODE_LIMIT) {
          axNodesTruncated = true;
          axNodesTruncatedCount += Math.max(1, axNodes.length - axIndex);
          break;
        }
        axNodesScanned += 1;
        const axNode = axNodes[axIndex];
        if (!axNode || axNode.ignored === true || axNode.backendDOMNodeId == null) continue;
        const backendDOMNodeId = Number(axNode.backendDOMNodeId);
        if (!Number.isSafeInteger(backendDOMNodeId) || backendDOMNodeId <= 0) continue;
        const values = Object.fromEntries((Array.isArray(axNode.properties) ? axNode.properties : [])
          .filter((property) => property && typeof property.name === 'string')
          .map((property) => [property.name, property.value && property.value.value]));
        const keys = await this.backendNodeKeys(entry, frame, backendDOMNodeId);
        let ref = keys.map((nodeKey) => byNodeKey.get(nodeKey)).find(Boolean) || null;
        if (!ref) {
          ref = {
            path: null, nodeKey: null, selector: '', frameId: frame.id,
            frameGeneration: frame.generation, frameLoaderId: frame.loaderId,
            targetId: frame.targetId, sessionId: frame.sessionId || null,
            contextId: frame.contextId ?? null, backendDOMNodeId,
          };
          combinedRefs.push(ref);
          frameNodes.push(ref);
        } else {
          ref.backendDOMNodeId = backendDOMNodeId;
        }
        const refIndex = combinedRefs.indexOf(ref);
        const role = String(axNode.role && axNode.role.value || '');
        const name = String(axNode.name && axNode.name.value || '');
        const description = String(axNode.description && axNode.description.value || '');
        const existing = semantic.find((node) => node.refIndex === refIndex);
        const axFields = {
          role: role || existing?.role || 'generic',
          name: name || existing?.name || '',
          description,
          checked: typeof values.checked === 'boolean' ? values.checked : existing?.checked ?? null,
          expanded: typeof values.expanded === 'boolean' ? values.expanded : existing?.expanded ?? null,
          selected: typeof values.selected === 'boolean' ? values.selected : existing?.selected ?? null,
          disabled: values.disabled === true || existing?.disabled === true,
          editable: values.editable === true || existing?.editable === true,
          focused: values.focused === true || existing?.focused === true,
          frameId: frame.id,
        };
        if (existing) Object.assign(existing, axFields);
        else {
          const actionableRoles = new Set(['button', 'checkbox', 'combobox', 'link', 'menuitem', 'radio', 'searchbox', 'slider', 'spinbutton', 'switch', 'tab', 'textbox']);
          if (!name && !actionableRoles.has(role)) continue;
          semantic.push({
            refIndex, tag: '', selector: '', ariaLabel: null, text: '', type: null,
            readOnly: values.readonly === true, editor: null, actionable: actionableRoles.has(role),
            scrollable: false, bounds: null, boundsAvailable: false, boundsUnavailableReason: 'ax_geometry_not_resolved',
            valueLength: null, ...axFields,
          });
          nodeCount += 1;
          if (actionableRoles.has(role)) interactive.push({
            refIndex, role: axFields.role, name: axFields.name, text: '', selector: '',
            ariaLabel: null, readOnly: values.readonly === true, editor: null,
            focused: axFields.focused, disabled: axFields.disabled,
          });
        }
      }
      axByFrame.set(key, {
        scanned: Math.min(axNodes.length, AX_NODE_LIMIT),
        returned: frameNodes.length,
        truncated: axNodesTruncated,
      });
    }
    semanticTruncatedCount += axNodesTruncatedCount;

    const frameRefById = new Map();
    for (const frame of liveFrames) frameRefById.set(frame.id, 'fr_' + crypto.randomBytes(12).toString('base64url'));
    const frameRows = [];
    const matchedDomFrameBoundaries = new Set();
    const elementRefByNodeKey = new Map();
    const refs = new Map();
    const publicRefByIndex = new Map();
    for (let refIndex = 0; refIndex < combinedRefs.length; refIndex += 1) {
      const source = combinedRefs[refIndex];
      const frame = source.frameId ? entry.frames.get(source.frameId) : null;
      const elementRef = 'el_' + crypto.randomBytes(18).toString('base64url');
      refs.set(elementRef, {
        path: source.path,
        nodeKey: source.nodeKey,
        selector: source.selector,
        backendDOMNodeId: source.backendDOMNodeId,
        frameId: source.frameId,
        frameGeneration: source.frameGeneration,
        frameLoaderId: source.frameLoaderId,
        targetId: source.targetId,
        sessionId: source.sessionId,
        contextId: source.contextId,
        documentGeneration,
        snapshotId,
      });
      publicRefByIndex.set(refIndex, elementRef);
      if (source.nodeKey) elementRefByNodeKey.set((source.frameId || '') + ':' + source.nodeKey, elementRef);
      if (frame && frame.parentFrameId) {
        // A CDP backend identity is the only cross-surface join; labels/selectors are never used to bind frames.
      }
    }
    for (const frame of liveFrames) {
      if (frame.parentFrameId == null) continue;
      const parent = entry.frames.get(frame.parentFrameId);
      let ownerBackendId = null;
      if (parent && frame.id) {
        try {
          const owner = await entry.view.webContents.debugger.sendCommand(
            'DOM.getFrameOwner', { frameId: frame.id }, parent.sessionId || undefined,
          );
          ownerBackendId = Number(owner && owner.backendNodeId) || null;
        } catch { /* owner unavailable leaves the boundary explicit */ }
      }
      let parentElementRef = null;
      let sourceBoundary = null;
      if (ownerBackendId && parent) {
        const keys = await this.backendNodeKeys(entry, parent, ownerBackendId);
        for (const nodeKey of keys) {
          parentElementRef = elementRefByNodeKey.get(parent.id + ':' + nodeKey) || null;
          const boundary = domFrameOwners.get(parent.id + ':' + nodeKey) || null;
          if (boundary) {
            sourceBoundary = sourceBoundary || boundary;
            matchedDomFrameBoundaries.add(boundary);
          }
          if (parentElementRef) break;
        }
      }
      const accessible = !!(frame.sessionId || frame.contextId != null) && !frame.unavailableReason;
      frameRows.push({
        frame_ref: frameRefById.get(frame.id),
        parent_frame_ref: frameRefById.get(frame.parentFrameId) || null,
        parent_element_ref: parentElementRef,
        name: redactPageMessage(sourceBoundary?.name || ''),
        selector: redactPageMessage(sourceBoundary?.selector || ''),
        url: redactPageMessage(frame.url || ''),
        bounds: sourceBoundary?.bounds || null,
        boundsAvailable: sourceBoundary?.boundsAvailable === true,
        accessible,
        access: frame.sessionId ? 'owned_oopif' : frame.contextId != null ? 'same_target_context' : 'inaccessible',
        limitation: accessible ? null : redactPageMessage(frame.unavailableReason || (entry.autoAttachAvailable ? 'owned_frame_execution_context_unavailable' : 'owned_frame_target_unavailable')),
        axTreeAvailable: frame.axAvailable === true,
      });
    }
    for (const source of sources) {
      for (const boundary of source.frames) {
        if (matchedDomFrameBoundaries.has(boundary)) continue;
        frameRows.push({
          frame_ref: 'fr_' + crypto.randomBytes(12).toString('base64url'),
          parent_frame_ref: source.frame ? frameRefById.get(source.frame.id) || null : null,
          parent_element_ref: publicRefByIndex.get(Number(boundary.refIndex)) || null,
          name: redactPageMessage(boundary.name || ''),
          selector: redactPageMessage(boundary.selector || ''),
          url: '',
          bounds: boundary.bounds || null,
          boundsAvailable: boundary.boundsAvailable === true,
          accessible: boundary.accessible === true,
          access: boundary.access || 'unavailable',
          limitation: redactPageMessage(boundary.limitation || 'owned_frame_identity_unavailable'),
          axTreeAvailable: false,
        });
      }
    }

    for (const node of semantic) {
      const refIndex = Number(node.refIndex);
      node.element_ref = publicRefByIndex.get(refIndex) || null;
      node.frame_ref = frameRefById.get(node.frameId) || null;
      delete node.refIndex;
      delete node.frameId;
      node.name = redactPageMessage(node.name || '', 1024);
      node.text = redactPageMessage(node.text || '', 1024);
      node.selector = redactPageMessage(node.selector || '', 2048);
      node.ariaLabel = node.ariaLabel ? redactPageMessage(node.ariaLabel, 1024) : node.ariaLabel;
      node.description = node.description ? redactPageMessage(node.description, 1024) : '';
    }
    const semanticByRef = new Map(semantic.map((node) => [node.element_ref, node]));
    for (const node of interactive) {
      const refIndex = Number(node.refIndex);
      node.element_ref = publicRefByIndex.get(refIndex) || null;
      node.frame_ref = frameRefById.get(node.frameId) || null;
      delete node.refIndex;
      delete node.frameId;
      node.name = redactPageMessage(node.name || '', 1024);
      node.text = redactPageMessage(node.text || '', 1024);
      node.selector = redactPageMessage(node.selector || '', 2048);
      node.ariaLabel = node.ariaLabel ? redactPageMessage(node.ariaLabel, 1024) : node.ariaLabel;
      const semanticNode = semanticByRef.get(node.element_ref);
      if (semanticNode) {
        node.role = semanticNode.role;
        node.name = semanticNode.name;
        node.disabled = semanticNode.disabled;
      }
    }
    for (const editor of editors) {
      editor.selector = redactPageMessage(editor.selector || '', 2048);
      editor.kind = redactPageMessage(editor.kind || '', 80);
      editor.text = redactPageMessage(String(editor.text || ''), 4096);
      editor.truncated = editor.truncated === true || String(editor.text || '').length > 4096;
      editor.frame_ref = frameRefById.get(editor.frameId) || null;
      delete editor.frameId;
    }

    const scroll = textSource && textSource.scroll || {};
    const metrics = {
      width: finiteMetric(scroll.width ?? entry.effectiveViewport?.width),
      height: finiteMetric(scroll.height ?? entry.effectiveViewport?.height),
      device_scale_factor: finiteMetric(scroll.deviceScaleFactor ?? entry.effectiveViewport?.deviceScaleFactor),
    };
    const text = textSource ? redactPageMessage(textSource.text || '', 32 * 1024) : '';
    const result = {
      url: redactPageMessage(textSource?.url || entry.url || DEFAULT_URL, 2048),
      title: redactPageMessage(textSource?.title || entry.title || '', 1024),
      text,
      editors,
      interactive,
      snapshot_id: snapshotId,
      document_generation: documentGeneration,
      viewport_generation: viewportGeneration,
      requested_viewport: { ...entry.viewport },
      effective_viewport: metrics,
      scroll: {
        x: finiteMetric(scroll.x), y: finiteMetric(scroll.y),
        width: finiteMetric(scroll.width), height: finiteMetric(scroll.height),
        device_scale_factor: finiteMetric(scroll.deviceScaleFactor),
      },
      loading: entry.loading === true,
      error: entry.error ? redactPageMessage(entry.error, 1024) : null,
      semantic,
      frames: frameRows,
      text_length: Number(textSource?.textLength) || 0,
      text_length_exact: textSource?.textLengthExact === true && textLengthExact,
      text_bytes_returned: Buffer.byteLength(text, 'utf8'),
      text_bytes_scanned: Number(textSource?.textBytesScanned) || textBytesScanned,
      text_truncated_bytes: textSource?.textTruncatedBytes ?? null,
      text_truncated: textSource?.textTruncated === true || !textSource,
      semantic_node_count: nodeCount || semantic.length,
      semantic_returned: semantic.length,
      semantic_truncated: semanticTruncatedCount > 0 || sources.some((source) => source.raw?.semanticTruncated === true) || axNodesTruncated,
      semantic_truncated_count: semanticTruncatedCount,
      editor_root_count: editorRootCount,
      editors_truncated: editorsTruncated,
      accessibility: {
        source: observedAxTree ? 'dom+owned-cdp-ax' : 'dom',
        axTreeAvailable: observedAxTree,
        limitation: observedAxTree ? null : 'owned_cdp_ax_tree_unavailable',
        frames_scanned: liveFrames.length,
        frames_unavailable: frameRows.filter((frame) => frame.accessible !== true).length,
        ax_nodes_scanned: axNodesScanned,
        ax_nodes_truncated: axNodesTruncated,
        ax_nodes_truncated_count: axNodesTruncatedCount,
      },
      traversal: {
        visitedElements,
        maxElements: sources.reduce((max, source) => Math.max(max, Number(source.raw?.traversal?.maxElements) || 0), 0),
        truncated: sources.some((source) => source.raw?.traversal?.truncated === true) || frameListTruncated,
        depthTruncated: sources.some((source) => source.raw?.traversal?.depthTruncated === true),
        frameListTruncated: sources.some((source) => source.raw?.traversal?.frameListTruncated === true) || frameListTruncated,
        errorCount: traversalErrors.length + sources.filter((source) => source.error).length,
        errors: traversalErrors.slice(0, 32),
        nodeCountExact: textLengthExact && !frameListTruncated && !sources.some((source) => source.raw?.traversal?.truncated === true),
      },
      truncation: {
        text: textSource?.textTruncated === true || !textSource,
        semantic_nodes: semanticTruncatedCount > 0 || axNodesTruncated,
        editors: editorsTruncated,
        frames: frameListTruncated,
        total_bytes: 0,
      },
    };

    const byteSize = () => Buffer.byteLength(JSON.stringify(result), 'utf8');
    const originalTextBytes = Buffer.byteLength(String(result.text || ''), 'utf8');
    const budget = {
      semanticRowsRemoved: 0,
      interactiveRowsRemoved: 0,
      editorRowsRemoved: 0,
      frameRowsRemoved: 0,
      stringBytesRemoved: 0,
    };
    const stringFields = () => {
      const fields = [[result, 'url'], [result, 'title'], [result, 'text'], [result, 'error']];
      for (const editor of result.editors) for (const key of ['selector', 'kind', 'text']) fields.push([editor, key]);
      for (const node of result.semantic) for (const key of ['role', 'name', 'description', 'tag', 'selector', 'ariaLabel', 'text', 'type', 'boundsUnavailableReason']) fields.push([node, key]);
      for (const node of result.interactive) for (const key of ['role', 'name', 'selector', 'ariaLabel', 'text', 'type']) fields.push([node, key]);
      for (const frame of result.frames) for (const key of ['name', 'selector', 'url', 'access', 'limitation']) fields.push([frame, key]);
      fields.push([result.accessibility, 'source'], [result.accessibility, 'limitation']);
      for (const error of result.traversal.errors) fields.push([error, 'stage'], [error, 'name']);
      return fields.filter(([object, key]) => typeof object?.[key] === 'string' && object[key].length > 0);
    };
    const shrinkField = (object, key, removeBytes) => {
      const value = String(object[key] || '');
      const current = Buffer.byteLength(value, 'utf8');
      if (!current) return 0;
      const next = truncateUtf8Bytes(value, Math.max(0, current - removeBytes));
      const removed = current - Buffer.byteLength(next, 'utf8');
      if (!removed) return 0;
      object[key] = next;
      budget.stringBytesRemoved += removed;
      return removed;
    };
    const removeSemanticRow = () => {
      const removed = result.semantic.pop();
      if (!removed) return false;
      budget.semanticRowsRemoved += 1;
      if (removed.element_ref) {
        result.interactive = result.interactive.filter((node) => node.element_ref !== removed.element_ref);
      }
      result.semantic_truncated = true;
      result.semantic_truncated_count += 1;
      result.truncation.semantic_nodes = true;
      return true;
    };
    const removeAnyInteractiveRow = () => {
      if (!result.interactive.length) return false;
      result.interactive.pop();
      budget.interactiveRowsRemoved += 1;
      return true;
    };
    let serializedBytes = byteSize();
    while (serializedBytes > MAX_BROWSER_SNAPSHOT_BYTES - 16) {
      let bytesToRemove = serializedBytes - (MAX_BROWSER_SNAPSHOT_BYTES - 16);
      const fields = stringFields().sort((a, b) =>
        Buffer.byteLength(String(b[0][b[1]]), 'utf8') - Buffer.byteLength(String(a[0][a[1]]), 'utf8'));
      let removedBytes = 0;
      for (const [object, key] of fields) {
        if (bytesToRemove <= 0) break;
        const removed = shrinkField(object, key, bytesToRemove);
        removedBytes += removed;
        bytesToRemove -= removed;
      }
      if (removedBytes) {
        result.truncation.strings = true;
        serializedBytes = byteSize();
      } else if (removeAnyInteractiveRow()) {
        result.truncation.interactive_nodes = true;
        serializedBytes = byteSize();
      } else if (removeSemanticRow()) {
        // The row is no longer actionable through this snapshot.
        serializedBytes = byteSize();
      } else if (result.editors.length) {
        result.editors.pop();
        budget.editorRowsRemoved += 1;
        result.editors_truncated = true;
        result.truncation.editors = true;
        serializedBytes = byteSize();
      } else if (result.frames.length) {
        result.frames.pop();
        budget.frameRowsRemoved += 1;
        result.truncation.frames = true;
        serializedBytes = byteSize();
      } else {
        break;
      }
    }
    const returnedTextBytes = Buffer.byteLength(String(result.text || ''), 'utf8');
    const managerTextBytesRemoved = Math.max(0, originalTextBytes - returnedTextBytes);
    if (managerTextBytesRemoved) {
      result.text_truncated = true;
      result.text_length_exact = false;
      result.truncation.text = true;
      result.text_bytes_returned = returnedTextBytes;
      if (Number.isFinite(Number(result.text_truncated_bytes))) {
        result.text_truncated_bytes = Number(result.text_truncated_bytes) + managerTextBytesRemoved;
      } else {
        result.text_truncated_bytes = managerTextBytesRemoved;
      }
    }
    result.semantic_returned = result.semantic.length;
    result.semantic_truncated_count = Math.max(Number(semanticTruncatedCount) || 0, Number(result.semantic_truncated_count) || 0);
    result.truncation.semantic_rows_removed = budget.semanticRowsRemoved;
    result.truncation.interactive_rows_removed = budget.interactiveRowsRemoved;
    result.truncation.editor_rows_removed = budget.editorRowsRemoved;
    result.truncation.frame_rows_removed = budget.frameRowsRemoved;
    result.truncation.string_bytes_removed = budget.stringBytesRemoved;
    result.truncation.total_bytes = 0;
    // Leave room for the byte-count value itself, whose decimal width is part
    // of the serialized response.
    while (byteSize() > MAX_BROWSER_SNAPSHOT_BYTES) {
      if (removeAnyInteractiveRow()) {
        result.truncation.interactive_nodes = true;
      } else if (removeSemanticRow()) {
        // The row is no longer actionable through this snapshot.
      } else if (result.editors.length) {
        result.editors.pop();
        budget.editorRowsRemoved += 1;
        result.editors_truncated = true;
        result.truncation.editors = true;
      } else if (result.frames.length) {
        result.frames.pop();
        budget.frameRowsRemoved += 1;
        result.truncation.frames = true;
      } else {
        result.text = '';
        result.text_truncated = true;
        result.text_length_exact = false;
        result.text_bytes_returned = 0;
        result.truncation.text = true;
      }
      result.semantic_returned = result.semantic.length;
      result.truncation.semantic_rows_removed = budget.semanticRowsRemoved;
      result.truncation.interactive_rows_removed = budget.interactiveRowsRemoved;
      result.truncation.editor_rows_removed = budget.editorRowsRemoved;
      result.truncation.frame_rows_removed = budget.frameRowsRemoved;
      if (!result.semantic.length && !result.interactive.length && !result.editors.length && !result.frames.length && !result.text) break;
    }
    result.truncation.total_bytes = byteSize();
    result.truncation.total_bytes = byteSize();
    while (byteSize() > MAX_BROWSER_SNAPSHOT_BYTES && result.semantic.length) {
      removeSemanticRow();
      result.semantic_returned = result.semantic.length;
      result.truncation.semantic_rows_removed = budget.semanticRowsRemoved;
      result.truncation.total_bytes = byteSize();
    }
    result.text_bytes_returned = Buffer.byteLength(String(result.text || ''), 'utf8');
    if (result.text_bytes_returned < originalTextBytes) {
      result.text_truncated = true;
      result.text_length_exact = false;
      result.truncation.text = true;
      if (Number.isFinite(Number(result.text_truncated_bytes))) {
        result.text_truncated_bytes = Math.max(0, Number(result.text_truncated_bytes));
      }
    }
    if (byteSize() > MAX_BROWSER_SNAPSHOT_BYTES) throw new Error('browser snapshot metadata exceeds the 64 KiB response limit');

    const liveElementRefs = new Set();
    for (const node of result.semantic) if (node.element_ref) liveElementRefs.add(node.element_ref);
    for (const node of result.interactive) if (node.element_ref) liveElementRefs.add(node.element_ref);
    for (const frame of result.frames) if (frame.parent_element_ref) liveElementRefs.add(frame.parent_element_ref);
    for (const elementRef of Array.from(refs.keys())) if (!liveElementRefs.has(elementRef)) refs.delete(elementRef);

    await this.refreshFrameTree(entry);
    const currentSignature = JSON.stringify(Array.from(entry.frames.values())
      .filter((frame) => !frame.detached)
      .sort((a, b) => Number(a.parentFrameId != null) - Number(b.parentFrameId != null))
      .slice(0, SNAPSHOT_FRAME_LIMIT)
      .map((frame) => [frame.id, frame.parentFrameId, frame.generation, frame.loaderId, frame.targetId, frame.sessionId, frame.contextId])
      .sort((a, b) => String(a[0]).localeCompare(String(b[0]))));
    if (documentGeneration !== entry.documentGeneration || viewportGeneration !== entry.viewportGeneration ||
        interactionGeneration !== entry.interactionGeneration || frameTreeGeneration !== entry.frameTreeGeneration ||
        frameSignature !== currentSignature) {
      throw new Error('browser document or frame changed during snapshot; refresh the snapshot');
    }
    entry.observations.set(snapshotId, {
      refs, documentGeneration, viewportGeneration,
      frameTreeGeneration, interactionGeneration,
      scroll: { x: result.scroll.x || 0, y: result.scroll.y || 0 },
    });
    while (entry.observations.size > 8) entry.observations.delete(entry.observations.keys().next().value);
    return result;
  }

  async observationTarget(entry, params, { coordinates = false } = {}) {
    const selectorPresent = Object.prototype.hasOwnProperty.call(params, 'selector');
    const refPresent = Object.prototype.hasOwnProperty.call(params, 'elementRef');
    const snapshotPresent = Object.prototype.hasOwnProperty.call(params, 'snapshotId');
    const xPresent = Object.prototype.hasOwnProperty.call(params, 'x');
    const yPresent = Object.prototype.hasOwnProperty.call(params, 'y');
    const screenshotPresent = Object.prototype.hasOwnProperty.call(params, 'screenshotId');
    const hasSelector = typeof params.selector === 'string' && params.selector.trim() !== '';
    const hasRef = typeof params.elementRef === 'string' && params.elementRef.trim() !== '';
    const hasCoordinates = xPresent || yPresent || screenshotPresent;
    if (selectorPresent && !hasSelector) throw new Error('selector must be a non-empty string');
    if (refPresent && !hasRef) throw new Error('element_ref must be a non-empty string');
    if (snapshotPresent && (typeof params.snapshotId !== 'string' || !params.snapshotId.trim())) {
      throw new Error('snapshot_id must be a non-empty string');
    }
    if (hasRef !== snapshotPresent) {
      throw new Error('element_ref and snapshot_id must be supplied together');
    }
    if (hasCoordinates && !coordinates) throw new Error('this browser action does not accept screenshot coordinates');
    const forms = Number(hasSelector) + Number(hasRef) + Number(coordinates && hasCoordinates);
    if (forms !== 1) throw new Error('choose exactly one browser target: selector, element_ref with snapshot_id, or screenshot coordinates');
    if (hasSelector) {
      const selector = params.selector.trim();
      let count;
      try { count = await entry.view.webContents.executeJavaScript('document.querySelectorAll(' + JSON.stringify(selector) + ').length'); }
      catch { return { status: 'missing', reason: 'selector is invalid' }; }
      if (count === 0) return { status: 'missing', reason: 'selector did not match an element' };
      if (count !== 1) return { status: 'ambiguous', reason: 'selector matched ' + count + ' elements; use a more specific selector or an observed element_ref' };
      const root = this.frameForSession(entry, null);
      return { selector, frameId: root?.id || null };
    }
    if (hasRef) {
      const observation = entry.observations.get(params.snapshotId);
      if (!observation || observation.documentGeneration !== entry.documentGeneration) {
        return { status: 'stale', reason: 'browser observation is stale; refresh the snapshot' };
      }
      const ref = observation.refs.get(params.elementRef);
      if (!ref || ref.documentGeneration !== entry.documentGeneration || ref.snapshotId !== params.snapshotId) {
        return { status: 'stale', reason: 'element_ref was not present in that snapshot; refresh the snapshot' };
      }
      if (ref.frameId) {
        const frame = entry.frames.get(ref.frameId);
        if (!frame || frame.detached || frame.generation !== ref.frameGeneration ||
            frame.loaderId !== ref.frameLoaderId || frame.targetId !== ref.targetId ||
            frame.sessionId !== ref.sessionId || (ref.contextId != null && frame.contextId !== ref.contextId)) {
          return { status: 'stale', reason: 'observed frame or document was replaced; refresh the snapshot' };
        }
      }
      return {
        path: ref.path,
        nodeKey: ref.nodeKey,
        selector: ref.selector,
        backendDOMNodeId: ref.backendDOMNodeId,
        frameId: ref.frameId,
        frameGeneration: ref.frameGeneration,
        frameLoaderId: ref.frameLoaderId,
        targetId: ref.targetId,
        sessionId: ref.sessionId,
        contextId: ref.contextId,
        snapshotId: params.snapshotId,
      };
    }
    return { coordinates: true };
  }

  async mapFramePointToRoot(entry, frameId, point) {
    let frame = entry.frames.get(String(frameId));
    if (!frame || frame.detached) return { status: 'stale', reason: 'observed frame was replaced; refresh the snapshot' };
    let mappedPoint = { x: Number(point.x), y: Number(point.y) };
    while (frame.parentFrameId) {
      const parent = entry.frames.get(frame.parentFrameId);
      if (!parent || parent.detached) return { status: 'stale', reason: 'observed frame parent was removed; refresh the snapshot' };
      const sessionId = parent.sessionId || undefined;
      let owner;
      let quads;
      let viewport;
      try {
        owner = await entry.view.webContents.debugger.sendCommand('DOM.getFrameOwner', { frameId: frame.id }, sessionId);
        quads = await entry.view.webContents.debugger.sendCommand('DOM.getContentQuads', { backendNodeId: owner.backendNodeId }, sessionId);
        viewport = await this.evaluateFrame(entry, frame.id, '({ width: innerWidth, height: innerHeight })');
      } catch {
        return { status: 'unavailable', reason: 'owned_frame_geometry_unavailable' };
      }
      const quadValues = Array.isArray(quads?.quads) ? quads.quads[0] : null;
      if (!owner?.backendNodeId || !Array.isArray(quadValues) || quadValues.length < 8) {
        return { status: 'unavailable', reason: 'owned_frame_geometry_unavailable' };
      }
      const quad = {
        p1: { x: quadValues[0], y: quadValues[1] }, p2: { x: quadValues[2], y: quadValues[3] },
        p3: { x: quadValues[4], y: quadValues[5] }, p4: { x: quadValues[6], y: quadValues[7] },
      };
      const mapped = mapFramePointThroughQuad(mappedPoint, viewport, quad);
      if (!mapped.available) return { status: 'unavailable', reason: mapped.reason };
      let ownerObjectId = null;
      try {
        const resolvedOwner = await entry.view.webContents.debugger.sendCommand('DOM.resolveNode', {
          backendNodeId: owner.backendNodeId,
        }, sessionId);
        ownerObjectId = resolvedOwner?.object?.objectId || null;
        if (!ownerObjectId) return { status: 'unavailable', reason: 'owned_frame_owner_unavailable' };
        const hitTest = await entry.view.webContents.debugger.sendCommand('Runtime.callFunctionOn', {
          objectId: ownerObjectId,
          functionDeclaration: `function(x,y) {
            const deep=(doc,px,py)=>{let hit=doc?.elementFromPoint?.(px,py)||null;const seen=new Set();while(hit&&!seen.has(hit)){seen.add(hit);const nested=hit.shadowRoot?.elementFromPoint?.(px,py);if(!nested||nested===hit)break;hit=nested;}return hit;};
            return deep(this.ownerDocument,x,y)===this;
          }`,
          arguments: [{ value: mapped.x }, { value: mapped.y }],
          returnByValue: true,
        }, sessionId);
        if (hitTest?.result?.value !== true) return { status: 'blocked', reason: 'frame_boundary_covered' };
      } catch (error) {
        return {
          status: 'unavailable',
          reason: 'owned_frame_hit_test_unavailable',
          detail: redactPageMessage(String(error && error.message || error), 256),
        };
      } finally {
        if (ownerObjectId) {
          try { await entry.view.webContents.debugger.sendCommand('Runtime.releaseObject', { objectId: ownerObjectId }, sessionId); }
          catch { /* the parent context may have navigated during hit testing */ }
        }
      }
      mappedPoint = { x: mapped.x, y: mapped.y };
      frame = parent;
    }
    return { status: 'ready', ...mappedPoint };
  }

  async geometryForTarget(entry, target) {
    if (target.backendDOMNodeId != null) {
      const frame = target.frameId ? entry.frames.get(target.frameId) : this.frameForSession(entry, null);
      if (!frame || frame.detached) return { status: 'stale', reason: 'observed frame is unavailable; refresh the snapshot' };
      let temporary;
      try {
        temporary = await this.exposeBackendTarget(entry, target);
        return await this.evaluateFrame(entry, frame.id, observedTargetGeometryScript({ ...target, temporaryTarget: true }, { mapFrames: false }));
      } catch {
        return { status: 'stale', reason: 'observed node could not be checked' };
      } finally {
        try { await temporary?.clear(); } catch { /* navigation already removed the frame context */ }
      }
    }
    const frameId = target.frameId;
    try {
      return await this.evaluateFrame(entry, frameId, observedTargetGeometryScript(target, { mapFrames: false }));
    } catch {
      return { status: 'stale', reason: target.path ? 'observed node was replaced; refresh the snapshot' : 'browser target could not be inspected' };
    }
  }

  async readInteractionStamp(entry) {
    this.assertCurrentEntry(entry);
    await this.refreshFrameTree(entry);
    const frames = [];
    for (const frame of Array.from(entry.frames.values()).filter((item) => !item.detached)) {
      let scroll = null;
      try { scroll = await this.evaluateFrame(entry, frame.id, '({ x: scrollX, y: scrollY, width: innerWidth, height: innerHeight })'); }
      catch { /* unavailable frames remain represented by their identity */ }
      frames.push([frame.id, frame.generation, frame.loaderId, frame.targetId, frame.sessionId,
        finiteMetric(scroll?.x), finiteMetric(scroll?.y), finiteMetric(scroll?.width), finiteMetric(scroll?.height)]);
    }
    let top = null;
    try { top = await this.readEffectiveViewport(entry); } catch { /* caller reports an unavailable metric */ }
    frames.sort((a, b) => String(a[0]).localeCompare(String(b[0])));
    return {
      documentGeneration: entry.documentGeneration,
      viewportGeneration: entry.viewportGeneration,
      frameTreeGeneration: entry.frameTreeGeneration,
      interactionGeneration: entry.interactionGeneration,
      metrics: {
        width: finiteMetric(top?.width), height: finiteMetric(top?.height),
        deviceScaleFactor: finiteMetric(top?.deviceScaleFactor),
        scrollX: finiteMetric(top?.scrollX), scrollY: finiteMetric(top?.scrollY),
        documentWidth: finiteMetric(top?.documentWidth), documentHeight: finiteMetric(top?.documentHeight),
      },
      topScroll: { x: finiteMetric(top?.scrollX), y: finiteMetric(top?.scrollY) },
      frames,
    };
  }

  async browserClick(entry, params = {}) {
    this.assertCurrentEntry(entry);
    const target = await this.observationTarget(entry, params, { coordinates: true });
    if (target.status) return { ok: false, status: target.status, reason: target.reason };
    let point;
    let frameLocalPoint = null;
    if (target.coordinates) {
      if (typeof params.x !== 'number' || !Number.isFinite(params.x) ||
          typeof params.y !== 'number' || !Number.isFinite(params.y) ||
          typeof params.screenshotId !== 'string' || !params.screenshotId) {
        throw new Error('screenshot-coordinate click requires finite numeric x, y, and screenshot_id');
      }
      const image = entry.screenshots.get(params.screenshotId);
      if (!image) return { ok: false, status: 'stale', reason: 'screenshot_id is unavailable; take a fresh screenshot' };
      const currentStamp = await this.readInteractionStamp(entry);
      if (image.documentGeneration !== entry.documentGeneration ||
          image.viewportGeneration !== entry.viewportGeneration ||
          image.frameTreeGeneration !== currentStamp.frameTreeGeneration ||
          image.interactionGeneration !== currentStamp.interactionGeneration ||
          JSON.stringify(image.frameStamp) !== JSON.stringify(currentStamp.frames) ||
          JSON.stringify(image.metricsStamp) !== JSON.stringify(screenshotMetricStamp(currentStamp.metrics, image.mode))) {
        return { ok: false, status: 'stale', reason: 'screenshot observation changed; take a fresh screenshot' };
      }
      if (params.x < 0 || params.y < 0 || params.x >= image.imageWidth || params.y >= image.imageHeight) {
        return { ok: false, status: 'missing', reason: 'coordinates are outside the screenshot image' };
      }
      const docX = image.cssRect.x + params.x * image.pixelToCss.x;
      const docY = image.cssRect.y + params.y * image.pixelToCss.y;
      point = { x: docX - currentStamp.topScroll.x, y: docY - currentStamp.topScroll.y };
      if (point.x < 0 || point.y < 0 || point.x >= currentStamp.metrics.width || point.y >= currentStamp.metrics.height) {
        return { ok: false, status: 'blocked', reason: 'screenshot point is outside the current visible viewport' };
      }
      const inspected = await entry.view.webContents.executeJavaScript('(() => {' +
        'const x=' + JSON.stringify(point.x) + ';const y=' + JSON.stringify(point.y) + ';' +
        'const deep=(doc,px,py)=>{let hit=doc.elementFromPoint(px,py);const seen=new Set();while(hit&&!seen.has(hit)){seen.add(hit);const nested=hit.shadowRoot?.elementFromPoint?.(px,py);if(!nested||nested===hit)break;hit=nested;}return hit;};' +
        'const target=deep(document,x,y);if(!target)return {status:"missing"};' +
        'for(let node=target;node&&node.nodeType===1;node=node.parentElement||node.getRootNode?.()?.host||null){if(node.disabled||node.getAttribute("aria-disabled")==="true"||node.hasAttribute("inert"))return {status:"disabled"};}' +
        'return {status:"ready",tag:String(target.tagName||"").toLowerCase(),role:target.getAttribute("role")||null,name:String(target.getAttribute("aria-label")||target.innerText||target.textContent||"").trim().slice(0,160)};' +
        '})()');
      if (!inspected || inspected.status !== 'ready') {
        return { ok: false, status: inspected?.status || 'blocked', reason: inspected?.status === 'disabled' ? 'the screenshot target is disabled' : 'no target is present at those screenshot coordinates' };
      }
      point.target = inspected;
    } else {
      const frame = target.frameId ? entry.frames.get(target.frameId) : this.frameForSession(entry, null);
      const frameGeneration = frame?.generation ?? null;
      const inspected = await this.geometryForTarget(entry, target);
      if (!inspected || inspected.status !== 'ready') {
        const status = inspected?.status || 'missing';
        const reason = inspected?.reason || (status === 'stale' ? 'observed element was replaced; refresh the snapshot'
          : status === 'disabled' ? 'target is disabled'
            : status === 'blocked' ? 'target is covered or outside the visible viewport'
              : status === 'unavailable' ? 'target geometry is unavailable in its owned frame'
                : 'target is missing or not visible');
        return { ok: false, status, reason };
      }
      point = inspected;
      if (target.frameId) {
        frameLocalPoint = inspected;
        const mapped = await this.mapFramePointToRoot(entry, target.frameId, point);
        if (mapped.status !== 'ready') return { ok: false, status: mapped.status, reason: mapped.reason };
        point = { ...point, x: mapped.x, y: mapped.y };
        const currentFrame = entry.frames.get(target.frameId);
        if (!currentFrame || currentFrame.generation !== frameGeneration || currentFrame.detached) {
          return { ok: false, status: 'stale', reason: 'observed frame changed during target validation; refresh the snapshot' };
        }
      }
      const effective = await this.readEffectiveViewport(entry);
      if (!Number.isFinite(point.x) || !Number.isFinite(point.y) ||
          point.x < 0 || point.y < 0 || point.x >= effective.width || point.y >= effective.height) {
        return { ok: false, status: 'blocked', reason: 'target is outside the visible page viewport' };
      }
    }
    const tabId = Number(entry.view.webContents.id);
    const targetFrame = target.frameId ? entry.frames.get(target.frameId) : null;
    const inputTarget = targetFrame?.access === 'owned_oopif' && targetFrame.sessionId
      ? { tabId, targetId: targetFrame.targetId, sessionId: targetFrame.sessionId }
      : { tabId };
    const inputPoint = inputTarget.sessionId ? frameLocalPoint : point;
    await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mouseMoved', x: inputPoint.x, y: inputPoint.y });
    await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mousePressed', x: inputPoint.x, y: inputPoint.y, button: 'left', clickCount: 1 });
    await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mouseReleased', x: inputPoint.x, y: inputPoint.y, button: 'left', clickCount: 1 });
    entry.interactionGeneration += 1;
    const reportedTarget = point.target || { tag: point.tag, name: point.name };
    if (reportedTarget && typeof reportedTarget.name === 'string') reportedTarget.name = redactPageMessage(reportedTarget.name, 160);
    return { ok: true, status: 'clicked', trusted: true, point: { x: point.x, y: point.y }, target: reportedTarget };
  }

  async capturePng(entry, captureParams, expectedSize, options = {}) {
    const wc = entry.view.webContents;
    const request = () => wc.debugger.sendCommand('Page.captureScreenshot', captureParams);
    const decode = (result) => {
      const png = Buffer.from(String(result && result.data || ''), 'base64');
      if (!png.length) throw new Error('browser capture returned an empty image');
      const dimensions = pngDimensions(png);
      if (expectedSize && !matchesCaptureRaster(dimensions, expectedSize)) {
        throw new Error(`browser capture dimensions were ${dimensions.width}x${dimensions.height}; expected ${expectedSize.minWidth}..${expectedSize.maxWidth} by ${expectedSize.minHeight}..${expectedSize.maxHeight} pixels for the requested CSS rect`);
      }
      return { result, png, dimensions };
    };

    const expandedExtent = options.expandedExtent;
    if (expandedExtent && (expandedExtent.width > entry.viewport.width || expandedExtent.height > entry.viewport.height)) {
      const original = {
        attached: this.attachedView === entry.view,
        captureHostAttached: entry.captureHostAttached,
        visible: entry.visible,
        presentationScale: entry.presentationScale || 1,
        viewBounds: entry.view.getBounds(),
        captureHost: entry.captureHost,
        captureHostContentSize: entry.captureHost && !entry.captureHost.isDestroyed() &&
          typeof entry.captureHost.getContentSize === 'function' ? entry.captureHost.getContentSize() : null,
      };
      let captured = null;
      let captureError = null;
      let restoreError = null;
      try {
        await this.ensureCaptureHost(entry);
        await this.attachToCaptureHost(entry, original.visible);
        const host = entry.captureHost;
        if (typeof host.setContentSize === 'function') host.setContentSize(expandedExtent.width, expandedExtent.height);
        else host.setBounds({ x: 0, y: 0, width: expandedExtent.width, height: expandedExtent.height });
        entry.view.setBounds({ x: 0, y: 0, width: expandedExtent.width, height: expandedExtent.height });
        await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
        await new Promise((resolve) => setTimeout(resolve, 50));

        const assertPageState = async (phase) => {
          const observed = await this.readEffectiveViewport(entry);
          const expected = options.expectedPageState;
          if (expected) {
            for (const key of ['width', 'height', 'deviceScaleFactor', 'scrollX', 'scrollY', 'documentWidth', 'documentHeight']) {
              if (observed[key] !== expected[key]) {
                throw new Error(`expanded full-page capture changed ${key} during ${phase}: expected ${expected[key]}, observed ${observed[key]}`);
              }
            }
          }
          return observed;
        };
        await assertPageState('hidden-surface preparation');
        captured = decode(await request());
        await assertPageState('capture');
      } catch (error) {
        captureError = error;
      }

      try {
        const host = entry.captureHost;
        if (host && !host.isDestroyed() && original.captureHostContentSize && typeof host.setContentSize === 'function') {
          host.setContentSize(original.captureHostContentSize[0], original.captureHostContentSize[1]);
        }
        if (original.attached) {
          await this.attach(entry);
        } else if (original.captureHostAttached && host && !host.isDestroyed()) {
          await this.attachToCaptureHost(entry, original.visible);
          entry.view.setBounds(original.viewBounds);
          await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
        } else {
          if (this.attachedView === entry.view) {
            try { this.win.contentView.removeChildView(entry.view); } catch { /* restore the prior detached state */ }
            this.attachedView = null;
          }
          if (entry.captureHostAttached && host && !host.isDestroyed()) {
            try { host.contentView.removeChildView(entry.view); } catch { /* restore the prior detached state */ }
          }
          entry.captureHostAttached = false;
          entry.visible = original.visible;
          entry.view.setBounds(original.viewBounds);
          await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
        }
        if (!original.captureHost && entry.captureHost && !entry.captureHost.isDestroyed()) {
          const temporaryHost = entry.captureHost;
          entry.captureHost = null;
          entry.captureHostAttached = false;
          temporaryHost.destroy();
        }
        const restored = await this.readEffectiveViewport(entry);
        const expected = options.expectedPageState;
        if (expected) {
          for (const key of ['width', 'height', 'deviceScaleFactor', 'scrollX', 'scrollY', 'documentWidth', 'documentHeight']) {
            if (restored[key] !== expected[key]) {
              throw new Error(`expanded full-page capture failed to restore ${key}: expected ${expected[key]}, observed ${restored[key]}`);
            }
          }
        }
      } catch (error) {
        restoreError = error;
      }
      if (captureError || restoreError) {
        const captureMessage = captureError ? String(captureError.message || captureError) : '';
        const restoreMessage = restoreError ? `; surface restoration failed: ${String(restoreError.message || restoreError)}` : '';
        throw new Error(`expanded hidden-host full-page capture failed${captureMessage ? `: ${captureMessage}` : ''}${restoreMessage}`);
      }
      return { ...captured, surface: 'hidden_host' };
    }

    let firstError = null;
    try {
      const captured = decode(await request());
      return { ...captured, surface: entry.captureHostAttached ? 'hidden_host' : 'web_contents' };
    } catch (error) {
      firstError = error;
    }

    const hiddenOrMinimized = entry.visible !== true ||
      (typeof this.win.isMinimized === 'function' && this.win.isMinimized()) ||
      (typeof this.win.isVisible === 'function' && !this.win.isVisible());
    if (!hiddenOrMinimized || wc.isDestroyed?.()) {
      throw new Error(`browser capture failed on the visible page surface: ${String(firstError && firstError.message || firstError)}`);
    }

    const wasAttached = this.attachedView === entry.view;
    const wasCaptureHostAttached = entry.captureHostAttached;
    const wasVisible = entry.visible;
    let fallbackError = null;
    try {
      await this.ensureCaptureHost(entry);
      await this.attachToCaptureHost(entry, wasVisible);
      await new Promise((resolve) => setTimeout(resolve, 50));
      const captured = decode(await request());
      return { ...captured, surface: 'hidden_host' };
    } catch (error) {
      fallbackError = error;
    } finally {
      if (wasAttached) {
        await this.attach(entry);
      } else if (wasCaptureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
        await this.attachToCaptureHost(entry, wasVisible);
      } else if (entry.captureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
        try { entry.captureHost.contentView.removeChildView(entry.view); } catch { /* best-effort restoration of a detached page */ }
        entry.captureHostAttached = false;
        entry.visible = wasVisible;
        await this.applyAndReadDeviceEmulation(entry, entry.viewport, entry.presentationScale || 1);
      }
    }
    throw new Error(`browser capture failed on the first surface (${String(firstError && firstError.message || firstError)}) and the same-page hidden host (${String(fallbackError && fallbackError.message || fallbackError)})`);
  }

  browserScreenshot(entry, params = {}) {
    return this.serializePresentation(() => this.browserScreenshotSerialized(entry, params));
  }

  async browserScreenshotSerialized(entry, params = {}) {
    this.assertCurrentEntry(entry);
    const wc = entry.view.webContents;
    const mode = String(params.mode || 'viewport');
    if (!['viewport', 'full_page', 'clip'].includes(mode)) throw new Error('mode must be viewport, full_page, or clip');
    if (mode === 'clip' && (!params.clip || typeof params.clip !== 'object')) throw new Error('clip mode requires a CSS clip rectangle');
    if (mode !== 'clip' && params.clip != null) throw new Error('clip is only accepted with mode=clip');
    if (entry.loading) throw new Error('browser page is navigating; wait for navigation before taking a screenshot');
    await this.ensureCDP(entry);
    const beforeStamp = await this.readInteractionStamp(entry);
    if (entry.loading) throw new Error('browser page is navigating; wait for navigation before taking a screenshot');
    const beforeMetricsStamp = screenshotMetricStamp(beforeStamp.metrics, mode);
    const effective = beforeStamp.metrics;
    this.assertEffectiveViewport(entry.viewport, effective);
    let cssRect;
    let fullPageRasterExtent = null;
    let fullPageHostScale = 1;
    let captureParams = { format: 'png', fromSurface: true, captureBeyondViewport: false };
    if (mode === 'viewport') {
      cssRect = { x: effective.scrollX, y: effective.scrollY, width: entry.viewport.width, height: entry.viewport.height };
      captureParams.clip = { ...cssRect, scale: 1 };
    } else if (mode === 'full_page') {
      const metrics = await wc.debugger.sendCommand('Page.getLayoutMetrics');
      const content = metrics && (metrics.cssContentSize || metrics.contentSize);
      cssRect = { x: 0, y: 0, width: Number(content?.width), height: Number(content?.height) };
      captureParams.captureBeyondViewport = true;
      fullPageHostScale = await this.nativeHostDeviceScaleFactor();
      const rasterStep = captureRasterStep(fullPageHostScale);
      const alignExtent = (extent) => Math.ceil(Math.ceil(extent) / rasterStep) * rasterStep;
      fullPageRasterExtent = { width: alignExtent(cssRect.width), height: alignExtent(cssRect.height) };
      // Chromium rounds the temporary native view size in host DIPs but derives
      // the requested bitmap size from this clip. Align the integer raster to
      // the host scale so both extents match and Chromium's tiling fallback is
      // not invoked for Retina/fractional display scales.
      captureParams.clip = { ...cssRect, ...fullPageRasterExtent, scale: 1 };
    } else {
      const validated = assertCaptureBounds(params.clip, 'browser screenshot clip');
      cssRect = { x: validated.x, y: validated.y, width: validated.width, height: validated.height };
      captureParams.captureBeyondViewport = true;
      captureParams.clip = { ...cssRect, scale: 1 };
    }
    try { assertCaptureBounds(cssRect, `browser ${mode} screenshot`); }
    catch (error) { throw new Error(`${error.message}; requested extent is ${cssRect.width}x${cssRect.height} CSS pixels`); }
    const expectedSize = fullPageRasterExtent
      ? { minWidth: fullPageRasterExtent.width, maxWidth: fullPageRasterExtent.width, minHeight: fullPageRasterExtent.height, maxHeight: fullPageRasterExtent.height }
      : captureRasterBounds(cssRect);
    const captureSurfaceOptions = mode === 'full_page'
      ? {
        expandedExtent: {
          width: Math.max(entry.viewport.width, Math.ceil(cssRect.x + fullPageRasterExtent.width)),
          height: Math.max(entry.viewport.height, Math.ceil(cssRect.y + fullPageRasterExtent.height)),
        },
        expectedPageState: effective,
      }
      : {};
    if (fullPageRasterExtent) {
      try { assertCaptureBounds({ x: 0, y: 0, ...fullPageRasterExtent }, 'browser full-page raster extent'); }
      catch (error) { throw new Error(`${error.message}; requested integer raster extent is ${fullPageRasterExtent.width}x${fullPageRasterExtent.height} pixels`); }
    }
    if (captureSurfaceOptions.expandedExtent) {
      try {
        assertCaptureBounds({ x: 0, y: 0, ...captureSurfaceOptions.expandedExtent }, 'browser full-page native capture surface');
      } catch (error) {
        throw new Error(`${error.message}; expanded same-page host would be ${captureSurfaceOptions.expandedExtent.width}x${captureSurfaceOptions.expandedExtent.height} CSS pixels`);
      }
      const nativeSurfaceExtent = {
        width: Math.ceil(captureSurfaceOptions.expandedExtent.width * fullPageHostScale),
        height: Math.ceil(captureSurfaceOptions.expandedExtent.height * fullPageHostScale),
      };
      try {
        assertCaptureBounds({ x: 0, y: 0, ...nativeSurfaceExtent }, 'browser full-page native surface raster');
      } catch (error) {
        throw new Error(`${error.message}; expanded same-page host would allocate ${nativeSurfaceExtent.width}x${nativeSurfaceExtent.height} native pixels at host scale ${fullPageHostScale}`);
      }
    }
    const captured = await this.capturePng(entry, captureParams, expectedSize, captureSurfaceOptions);
    this.assertCurrentEntry(entry);
    const afterStamp = await this.readInteractionStamp(entry);
    const afterMetricsStamp = screenshotMetricStamp(afterStamp.metrics, mode);
    if (beforeStamp.documentGeneration !== afterStamp.documentGeneration ||
        beforeStamp.viewportGeneration !== afterStamp.viewportGeneration ||
        beforeStamp.frameTreeGeneration !== afterStamp.frameTreeGeneration ||
        beforeStamp.interactionGeneration !== afterStamp.interactionGeneration ||
        JSON.stringify(beforeStamp.frames) !== JSON.stringify(afterStamp.frames) ||
        JSON.stringify(beforeMetricsStamp) !== JSON.stringify(afterMetricsStamp) ||
        entry.loading) {
      const changed = [];
      for (const key of ['documentGeneration', 'viewportGeneration', 'frameTreeGeneration', 'interactionGeneration']) {
        if (beforeStamp[key] !== afterStamp[key]) changed.push(key);
      }
      if (JSON.stringify(beforeMetricsStamp) !== JSON.stringify(afterMetricsStamp)) changed.push('metrics');
      if (entry.loading) changed.push('navigation_in_progress');
      if (JSON.stringify(beforeStamp.frames) !== JSON.stringify(afterStamp.frames)) changed.push('frame_identity_or_scroll');
      throw new Error(`browser screenshot became stale while the document, frame, scroll, or viewport changed (${changed.join(',') || 'stamp'}); take a fresh screenshot`);
    }
    const png = captured.png;
    if (!png.length) throw new Error('browser screenshot returned an empty image');
    if (png.length > MAX_PNG_BYTES) throw new Error(`browser screenshot is ${png.length} PNG bytes; maximum is ${MAX_PNG_BYTES}; request a smaller clip`);
    const dimensions = pngDimensions(png);
    if (dimensions.width * dimensions.height > MAX_CAPTURE_PIXELS) throw new Error(`browser screenshot is ${dimensions.width}x${dimensions.height} pixels; maximum is ${MAX_CAPTURE_PIXELS}; request a smaller clip`);
    if (!matchesCaptureRaster(dimensions, expectedSize)) {
      throw new Error(`browser screenshot dimensions were ${dimensions.width}x${dimensions.height}; expected ${expectedSize.minWidth}..${expectedSize.maxWidth} by ${expectedSize.minHeight}..${expectedSize.maxHeight} pixels for the requested CSS rect`);
    }
    if (mode === 'viewport' && (dimensions.width !== 1440 && entry.viewport.width === 1440 || dimensions.height !== 900 && entry.viewport.height === 900)) {
      throw new Error('default browser screenshot did not preserve the 1440x900 logical viewport');
    }
    const screenshotId = crypto.randomUUID();
    const metadata = {
      screenshot_id: screenshotId,
      tab: this.tabInfo(entry),
      mode,
      viewport: { ...entry.viewport },
      effective_viewport: { width: effective.width, height: effective.height, device_scale_factor: effective.deviceScaleFactor },
      image: { width: dimensions.width, height: dimensions.height },
      css_capture_rect: cssRect,
      scroll_origin: { x: mode === 'viewport' ? effective.scrollX : 0, y: mode === 'viewport' ? effective.scrollY : 0 },
      document_generation: entry.documentGeneration,
      viewport_generation: entry.viewportGeneration,
      frame_tree_generation: beforeStamp.frameTreeGeneration,
      interaction_generation: beforeStamp.interactionGeneration,
      pixel_to_css: { x: cssRect.width / dimensions.width, y: cssRect.height / dimensions.height },
      capture_surface: captured.surface,
    };
    entry.screenshots.set(screenshotId, {
      documentGeneration: entry.documentGeneration, viewportGeneration: entry.viewportGeneration,
      imageWidth: dimensions.width, imageHeight: dimensions.height, cssRect,
      pixelToCss: metadata.pixel_to_css, observedScroll: { x: effective.scrollX, y: effective.scrollY },
      frameTreeGeneration: beforeStamp.frameTreeGeneration,
      interactionGeneration: beforeStamp.interactionGeneration,
      frameStamp: beforeStamp.frames,
      metricsStamp: beforeMetricsStamp,
      mode,
    });
    while (entry.screenshots.size > 20) entry.screenshots.delete(entry.screenshots.keys().next().value);
    return { mimeType: 'image/png', base64: png.toString('base64'), metadata, tab: metadata.tab };
  }

  async browserWait(entry, params = {}) {
    const condition = String(params.condition || '');
    if (!['dom_ready', 'load', 'element_visible', 'element_hidden'].includes(condition)) {
      throw new Error('condition must be dom_ready, load, element_visible, or element_hidden');
    }
    const selector = String(params.selector || '');
    if (condition.startsWith('element_') && !selector.trim()) throw new Error('element conditions require selector');
    if (!condition.startsWith('element_') && selector) throw new Error('selector is only accepted for element conditions');
    const timeout = params.timeoutMs == null ? 5000 : Number(params.timeoutMs);
    if (!Number.isInteger(timeout) || timeout < 0 || timeout > 15000) throw new Error('timeout_ms must be an integer from 0 to 15000');
    this.assertCurrentEntry(entry);
    const script = '(() => {' +
      'const condition=' + JSON.stringify(condition) + ';const selector=' + JSON.stringify(selector) + ';' +
      'const state=()=>({url:location.href,title:document.title,readyState:document.readyState,loading:document.readyState!=="complete",scroll:{x:scrollX,y:scrollY},width:innerWidth,height:innerHeight});' +
      'const visible=(el)=>{if(!el)return false;const rect=el.getBoundingClientRect();const style=el.ownerDocument.defaultView.getComputedStyle(el);return rect.width>0&&rect.height>0&&style.display!=="none"&&style.visibility!=="hidden"&&Number(style.opacity||1)>0;};' +
      'let done=false;let invalid=false;' +
      'if(condition==="dom_ready")done=document.readyState==="interactive"||document.readyState==="complete";' +
      'else if(condition==="load")done=document.readyState==="complete";' +
      'else{let matches=[];try{matches=Array.from(document.querySelectorAll(selector));}catch{invalid=true;}done=condition==="element_visible"?matches.some(visible):(matches.length===0||matches.every((el)=>!visible(el)));}' +
      'return {done,invalid,state:state()};' +
      '})()';
    const safeState = (value) => ({
      url: redactPageMessage(value?.url || entry.url || DEFAULT_URL, 2048),
      title: redactPageMessage(value?.title || entry.title || '', 1024),
      readyState: ['loading', 'interactive', 'complete'].includes(value?.readyState) ? value.readyState : null,
      loading: value?.loading === true,
      scroll: { x: finiteMetric(value?.scroll?.x), y: finiteMetric(value?.scroll?.y) },
      width: finiteMetric(value?.width),
      height: finiteMetric(value?.height),
    });

    return new Promise((resolve) => {
      let settled = false;
      let timeoutTimer = null;
      let pollTimer = null;
      let stopPendingEvaluation = () => {};
      let lastState = safeState(null);
      const cleanup = () => {
        if (timeoutTimer) clearTimeout(timeoutTimer);
        if (pollTimer) clearTimeout(pollTimer);
        entry.waits.delete(waiter);
      };
      const finish = (value) => {
        if (settled) return;
        settled = true;
        cleanup();
        stopPendingEvaluation();
        let tab;
        try { tab = this.tabInfo(entry); }
        catch { tab = { id: Number(entry.view.webContents.id), chatId: redactPageMessage(entry.chatId, 256), closed: true }; }
        resolve({ ...value, state: safeState(value.state || lastState), tab });
      };
      const waiter = {
        frameId: this.frameForSession(entry, null)?.id || null,
        targetId: entry.rootTargetId,
        interrupt: (reason) => finish({ success: false, timedOut: false, reason: String(reason || 'browser_wait_interrupted'), state: lastState }),
      };
      entry.waits.add(waiter);
      timeoutTimer = setTimeout(() => finish({
        success: false, timedOut: true, reason: 'condition was not observed before the host deadline', state: lastState,
      }), Math.max(1, timeout));
      const poll = async () => {
        if (settled) return;
        let stop;
        const stopped = new Promise((resolveStopped) => { stop = resolveStopped; });
        stopPendingEvaluation = () => stop({ stopped: true });
        const evaluation = this.evaluateFrame(entry, this.frameForSession(entry, null)?.id ?? null, script)
          .then((value) => ({ value }))
          .catch(() => ({ failed: true }));
        const outcome = await Promise.race([evaluation, stopped]);
        if (settled || outcome?.stopped) return;
        if (outcome?.failed) {
          finish({ success: false, timedOut: false, reason: 'execution_context_lost', state: lastState });
          return;
        }
        lastState = safeState(outcome.value?.state);
        if (outcome.value?.invalid) {
          finish({ success: false, timedOut: false, reason: 'selector_invalid', state: lastState });
          return;
        }
        if (outcome.value?.done === true) {
          finish({ success: true, timedOut: false, reason: '', state: lastState });
          return;
        }
        pollTimer = setTimeout(poll, Math.min(100, Math.max(1, timeout)));
      };
      poll();
    });
  }

  validateBatch(actions, observeAfter) {
    if (!Array.isArray(actions) || actions.length < 1 || actions.length > 20) throw new Error('browser batch requires 1-20 actions');
    if (observeAfter != null && typeof observeAfter !== 'boolean') throw new Error('observe_after must be a boolean');
    const allowedFields = {
      click: new Set(['action', 'selector', 'elementRef', 'snapshotId', 'x', 'y', 'screenshotId']),
      type: new Set(['action', 'selector', 'elementRef', 'snapshotId', 'text', 'submit']),
      scroll: new Set(['action', 'x', 'y', 'elementRef', 'snapshotId']),
      key: new Set(['action', 'key']),
      snapshot: new Set(['action']),
    };
    return actions.map((action, index) => {
      if (!action || typeof action !== 'object' || Array.isArray(action) ||
          (Object.getPrototypeOf(action) !== Object.prototype && Object.getPrototypeOf(action) !== null)) {
        throw new Error('browser batch action ' + index + ' must be a plain object');
      }
      const name = action.action;
      if (typeof name !== 'string' || !allowedFields[name]) {
        throw new Error('unsupported browser batch action at index ' + index + ': ' + String(name || ''));
      }
      for (const field of Object.keys(action)) {
        if (!allowedFields[name].has(field)) throw new Error('browser batch action ' + index + ' has unsupported field: ' + field);
      }
      const selectorPresent = Object.prototype.hasOwnProperty.call(action, 'selector');
      const refPresent = Object.prototype.hasOwnProperty.call(action, 'elementRef');
      const snapshotPresent = Object.prototype.hasOwnProperty.call(action, 'snapshotId');
      const validateTargetPair = () => {
        if (refPresent !== snapshotPresent) throw new Error('browser batch ' + name + ' ' + index + ' element_ref and snapshot_id must be supplied together');
        if (refPresent && (typeof action.elementRef !== 'string' || !action.elementRef.trim() ||
            typeof action.snapshotId !== 'string' || !action.snapshotId.trim())) {
          throw new Error('browser batch ' + name + ' ' + index + ' has an invalid observed target pair');
        }
        if (selectorPresent && (typeof action.selector !== 'string' || !action.selector.trim())) {
          throw new Error('browser batch ' + name + ' ' + index + ' selector must be a non-empty string');
        }
      };
      let parsedKey = null;
      if (name === 'click') {
        validateTargetPair();
        const hasSelector = selectorPresent;
        const hasRef = refPresent;
        const coordinateFields = ['x', 'y', 'screenshotId'];
        const coordinateCount = coordinateFields.filter((field) => Object.prototype.hasOwnProperty.call(action, field)).length;
        if (coordinateCount && coordinateCount !== 3) throw new Error('browser batch click ' + index + ' requires x, y, and screenshot_id together');
        if (coordinateCount) {
          if (typeof action.x !== 'number' || !Number.isFinite(action.x) ||
              typeof action.y !== 'number' || !Number.isFinite(action.y) ||
              typeof action.screenshotId !== 'string' || !action.screenshotId.trim()) {
            throw new Error('browser batch click ' + index + ' coordinates or screenshot_id are invalid');
          }
        }
        if (Number(hasSelector) + Number(hasRef) + Number(coordinateCount === 3) !== 1) {
          throw new Error('browser batch click ' + index + ' requires exactly one target form');
        }
      } else if (name === 'type') {
        validateTargetPair();
        if (typeof action.text !== 'string') throw new Error('browser batch type ' + index + ' requires text');
        if (Number(selectorPresent) + Number(refPresent) !== 1) throw new Error('browser batch type ' + index + ' requires exactly one target form');
        if (Object.prototype.hasOwnProperty.call(action, 'submit') && typeof action.submit !== 'boolean') {
          throw new Error('browser batch type ' + index + ' submit must be boolean');
        }
      } else if (name === 'scroll') {
        validateTargetPair();
        for (const key of ['x', 'y']) {
          if (Object.prototype.hasOwnProperty.call(action, key) && (typeof action[key] !== 'number' || !Number.isFinite(action[key]))) {
            throw new Error('browser batch scroll ' + index + ' ' + key + ' must be finite numeric');
          }
        }
      } else if (name === 'key') {
        if (typeof action.key !== 'string' || !action.key.trim()) throw new Error('browser batch key ' + index + ' requires key');
        parsedKey = parseBrowserKey(action.key, this.platform);
      }
      const normalized = { ...action, action: name };
      if (parsedKey) Object.defineProperty(normalized, 'parsedKey', { value: parsedKey, enumerable: false });
      return normalized;
    });
  }

  isFailedBatchAction(result) {
    if (!result || typeof result !== 'object') return false;
    if (result.ok === false || result.found === false) return true;
    return ['stale', 'blocked', 'disabled', 'ambiguous', 'missing', 'unavailable', 'not_scrollable'].includes(String(result.status || ''));
  }

  async browserControl(method, params = {}, internal = {}) {
    if (method === 'browser.list') return { tabs: this.browserTabs(params.chatId) };
    if (method === 'browser.open') {
      const entry = this.openConversation(params);
      if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
      if (params.url) await this.serialize(entry, () => this.command(entry.chatId, 'navigate', params.url));
      return this.tabInfo(entry);
    }
    const entry = await this.controlEntry(params);
    const wc = entry.view.webContents;
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    const serializedMethods = new Set([
      'browser.navigate', 'browser.back', 'browser.forward', 'browser.reload', 'browser.click',
      'browser.type', 'browser.scroll', 'browser.key', 'browser.setViewport', 'browser.resetViewport',
      'browser.batch', 'browser.snapshot', 'browser.screenshot',
    ]);
    if (!internal.serialized && serializedMethods.has(method)) {
      return this.serialize(entry, () => this.browserControl(method, params, { serialized: true }));
    }
    if (method === 'browser.setViewport') return this.setViewport(entry, params);
    if (method === 'browser.resetViewport') return this.resetViewport(entry);
    if (method === 'browser.navigate') {
      await this.command(entry.chatId, 'navigate', params.url);
      return this.tabInfo(entry);
    }
    if (method === 'browser.back') { await this.command(entry.chatId, 'back'); return this.tabInfo(entry); }
    if (method === 'browser.forward') { await this.command(entry.chatId, 'forward'); return this.tabInfo(entry); }
    if (method === 'browser.reload') { await this.command(entry.chatId, 'reload'); return this.tabInfo(entry); }
    if (method === 'browser.snapshot') return this.browserSnapshot(entry);
    if (method === 'browser.click') return this.browserClick(entry, params);
    if (method === 'browser.type') {
      const target = await this.observationTarget(entry, params);
      if (target.status) return { found: false, status: target.status, reason: target.reason };
      if (target.backendDOMNodeId == null) return this.browserType(entry, params, target);
      const temporary = await this.exposeBackendTarget(entry, target);
      try {
        return await this.browserType(entry, params, { ...target, temporaryTarget: true });
      } finally {
        try { await temporary.clear(); } catch { /* navigation or close already removed the owner context */ }
      }
    }
    if (method === 'browser.scroll') {
      const x = Number.isFinite(Number(params.x)) ? Number(params.x) : 0;
      const y = Number.isFinite(Number(params.y)) ? Number(params.y) : 0;
      if (params.elementRef || params.snapshotId) {
        const targetParams = {};
        for (const field of ['selector', 'elementRef', 'snapshotId']) {
          if (Object.prototype.hasOwnProperty.call(params, field)) targetParams[field] = params[field];
        }
        const target = await this.observationTarget(entry, targetParams);
        if (target.status) return { changed: false, status: target.status, reason: target.reason };
        const temporary = target.backendDOMNodeId != null ? await this.exposeBackendTarget(entry, target) : null;
        const locator = temporary ? { ...target, temporaryTarget: true } : target;
        const expression = targetExpression(locator);
        const script = '(() => {' +
          'const el=' + expression + ';if(!el||!el.isConnected)return {changed:false,status:"stale",reason:"observed scroll region was replaced; refresh the snapshot"};' +
          'const doc=el.ownerDocument,view=doc.defaultView,root=doc.scrollingElement||doc.documentElement;' +
          'const parent=(node)=>node?.parentElement||node?.getRootNode?.()?.host||null;' +
          'let scroller=el,found=null;' +
          'while(scroller&&scroller!==root){const style=view.getComputedStyle(scroller);const vertical=/(auto|scroll|overlay)/u.test(String(style.overflowY||style.overflow||""))&&scroller.scrollHeight>scroller.clientHeight;const horizontal=/(auto|scroll|overlay)/u.test(String(style.overflowX||style.overflow||""))&&scroller.scrollWidth>scroller.clientWidth;if(vertical||horizontal){found=scroller;break;}scroller=parent(scroller);}' +
          'const before={x:view.scrollX,y:view.scrollY,elementX:0,elementY:0};' +
          'if(found){before.elementX=found.scrollLeft;before.elementY=found.scrollTop;found.scrollBy(' + JSON.stringify(x) + ',' + JSON.stringify(y) + ');}' +
          'else if(root&&((root.scrollHeight>root.clientHeight)||(root.scrollWidth>root.clientWidth))){view.scrollBy(' + JSON.stringify(x) + ',' + JSON.stringify(y) + ');}' +
          'else return {changed:false,status:"not_scrollable",reason:"the observed target has no scrollable composed ancestor"};' +
          'const after={x:view.scrollX,y:view.scrollY,elementX:found?found.scrollLeft:0,elementY:found?found.scrollTop:0};' +
          'return {changed:before.x!==after.x||before.y!==after.y||before.elementX!==after.elementX||before.elementY!==after.elementY,before,after,nested:!!found};' +
          '})()';
        try {
          const result = await this.evaluateFrame(entry, target.frameId, script);
          if (result?.changed) entry.interactionGeneration += 1;
          return result;
        } finally {
          try { await temporary?.clear(); } catch { /* navigation already removed the frame context */ }
        }
      }
      const result = await wc.executeJavaScript('(() => { const before = { x: scrollX, y: scrollY }; window.scrollBy(' + JSON.stringify(x) + ', ' + JSON.stringify(y) + '); const after = { x: scrollX, y: scrollY }; return { changed: before.x !== after.x || before.y !== after.y, before, after }; })()');
      if (result?.changed) entry.interactionGeneration += 1;
      return result;
    }
    if (method === 'browser.key') {
      const parsed = internal.parsedKey || parseBrowserKey(params.key, this.platform);
      const focusedFrame = await this.focusedFrame(entry);
      const focusedFrameId = focusedFrame?.id ?? null;
      let deletionBefore = null;
      if (parsed.keyCode === 'Backspace' || parsed.keyCode === 'Delete') {
        try { deletionBefore = await this.focusedTextLength(entry, focusedFrameId); }
        catch { /* a key may still be dispatched if optional target readback is unavailable */ }
      }
      await this.dispatchBrowserKey(entry, parsed);
      const selectAll = parsed.keyCode === 'A'
        && (parsed.modifiers.includes('meta') || parsed.modifiers.includes('control'))
        && !parsed.modifiers.includes('alt');
      const selection = selectAll ? await this.selectAllBrowserTarget(entry, focusedFrame?.id ?? null) : null;
      let deletion = null;
      if (parsed.keyCode === 'Backspace' || parsed.keyCode === 'Delete') {
        await this.evaluateFrame(entry, focusedFrameId, '(/* workass-browser-key-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
        deletion = await this.deleteSelectedBrowserText(entry, focusedFrameId, parsed.keyCode);
        if (deletion?.deletionVerified !== true && deletionBefore?.found === true) {
          try {
            const after = await this.focusedTextLength(entry, focusedFrameId);
            if (after?.found === true && Number(after.length) < Number(deletionBefore.length)) {
              deletion = { found: true, deletionVerified: true, strategy: 'native-key-event' };
            }
          } catch { /* report only the verification already returned by the target helper */ }
        }
      }
      entry.interactionGeneration += 1;
      return {
        sent: true, key: parsed.keyCode, modifiers: parsed.modifiers,
        ...(selection ? { selectionVerified: selection.selectionVerified === true, selectionStrategy: selection.strategy } : {}),
        ...(deletion?.found ? { deletionVerified: deletion.deletionVerified === true, deletionStrategy: deletion.strategy } : {}),
      };
    }
    if (method === 'browser.batch') {
      const actions = this.validateBatch(params.actions, params.observeAfter);
      const results = [];
      const completedIndexes = [];
      for (let index = 0; index < actions.length; index += 1) {
        const action = actions[index];
        const name = action.action;
        try {
          const result = await this.browserControl(`browser.${name}`, { ...action, tabId: this.tabInfo(entry).id }, { serialized: true, parsedKey: action.parsedKey });
          results.push({ index, action: name, result });
          if (this.isFailedBatchAction(result)) {
            return { results, completed_indexes: completedIndexes, failed_index: index, unexecuted_indexes: actions.slice(index + 1).map((_item, offset) => index + offset + 1), observe_after: null, observe_after_error: null };
          }
          completedIndexes.push(index);
        } catch (error) {
          results.push({ index, action: name, error: redactPageMessage(String(error && error.message || error), 1024) });
          return { results, completed_indexes: completedIndexes, failed_index: index, unexecuted_indexes: actions.slice(index + 1).map((_item, offset) => index + offset + 1), observe_after: null, observe_after_error: null };
        }
      }
      let observed = null;
      let observeAfterError = null;
      if (params.observeAfter === true) {
        try { observed = await this.browserControl('browser.snapshot', { tabId: this.tabInfo(entry).id }, { serialized: true }); }
        catch (error) { observeAfterError = redactPageMessage(String(error && error.message || error), 1024); }
      }
      return { results, completed_indexes: completedIndexes, failed_index: null, unexecuted_indexes: [], observe_after: observed, observe_after_error: observeAfterError };
    }
    if (method === 'browser.screenshot') {
      return this.browserScreenshot(entry, params);
    }
    if (method === 'browser.wait') return this.browserWait(entry, params);
    if (method === 'browser.diagnostics') {
      const after = params.afterSequence == null ? 0 : Number(params.afterSequence);
      const limit = params.limit == null ? 20 : Number(params.limit);
      if (!Number.isInteger(after) || after < 0) throw new Error('after_sequence must be a nonnegative integer');
      if (!Number.isInteger(limit) || limit < 1 || limit > 100) throw new Error('diagnostics limit must be an integer from 1 to 100');
      return { tab: this.tabInfo(entry), ...diagnosticsAfter(entry, after, limit) };
    }
    throw new Error(`unknown browser control method: ${String(method)}`);
  }

  close(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry) return false;
    entry.closed = true;
    entry.lifecycleToken = Symbol('closed-browser-entry');
    this.interruptWaiters(entry, 'tab_closed');
    this.invalidateFrames(entry, null, 'entry_closed');
    if (this.attachedView === entry.view) {
      try { this.win.contentView.removeChildView(entry.view); } catch { /* window is already closing */ }
      this.attachedView = null;
      if (this.activeId === id) this.activeId = null;
      entry.visible = false;
    }
    if (entry.captureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
      try { entry.captureHost.contentView.removeChildView(entry.view); } catch { /* host is already closing */ }
      entry.captureHostAttached = false;
    }
    this.entries.delete(id);
    entry.observations.clear();
    entry.screenshots.clear();
    entry.diagnostics.length = 0;
    entry.diagnosticBytes = 0;
    entry.documentGeneration += 1;
    entry.interactionGeneration += 1;
    const tabId = Number(entry.view.webContents.id);
    for (const [sessionId, target] of Array.from(entry.targetSessions.entries())) {
      this.childSessions.delete(`${tabId}:${target.targetId}`);
      try {
        void entry.view.webContents.debugger.sendCommand('Target.detachFromTarget', { sessionId }, target.parentSessionId || undefined).catch(() => {});
      } catch { /* debugger may already be gone */ }
      entry.targetSessions.delete(sessionId);
    }
    entry.executionContexts.clear();
    entry.frames.clear();
    try { entry.view.webContents.close(); } catch { /* already gone */ }
    try { if (entry.captureHost && !entry.captureHost.isDestroyed()) entry.captureHost.destroy(); } catch { /* host teardown is idempotent */ }
    entry.captureHost = null;
    return true;
  }

  destroy() {
    for (const id of Array.from(this.entries.keys())) this.close(id);
    this.cdpListeners.clear();
    this.childSessions.clear();
  }
}

module.exports = { BrowserManager, DEFAULT_PARTITION, cleanUserAgent, normalizeBrowserURL, parseBrowserKey, resolveBrowserURL, safeBounds };
