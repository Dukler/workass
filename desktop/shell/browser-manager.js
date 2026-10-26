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
const {
  attachNodeFacts, browserSnapshotForParams, detachNodeFacts, dropSecretFieldAxRows,
} = require('./browser-snapshot-view');
const { browserRead } = require('./browser-read');
const { openGuestPip } = require('./browser-pip');
const { buildOutline, diffOutlines, outlineSecretField, pageLines } = require('./browser-outline');
const {
  POPUP_TAB_LIMIT, agentDriven, browserDialog, browserDrag, browserEvaluate, browserHover, browserSelect, browserUpload,
  dialogBlockedError, dialogClosed, dialogOpened, markAgentActive, noteActivity, pushEvent, readActivity,
  runAction, takeDownload, touchActivity,
} = require('./browser-actions');

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

// Owned iframe (OOPIF) target setup. Chromium never answers a command that is
// in flight on a flattened child session when that child goes away (its frame
// is removed, navigates cross-site or its renderer stops), and Electron only
// rejects pending commands when the ROOT debugger detaches. Every attach task
// is therefore bounded and cancellable; nothing may wait on one forever.
const OWNED_TARGET_SETUP_TIMEOUT_MS = 5000;
// Navigation lets setup that is about to finish commit its binding instead of
// cutting it mid-command, then abandons whatever is still pending: the new
// document replaces those frames anyway.
const NAVIGATION_TARGET_SETUP_WAIT_MS = 1000;
const ABANDONED_TARGET_SETTLE_MS = 250;
// A page whose subframe never finishes loading never fires its load event, so
// Electron's loadURL() never resolves. Navigation returns once the main
// document is usable instead of holding the browser's operation queue forever.
const NAVIGATION_LOAD_GRACE_MS = 5000;
const NAVIGATION_COMMIT_TIMEOUT_MS = 20000;
// Observation commands on a child session: released as soon as the session
// is retired, and capped so a renderer that stopped answering cannot hold the
// browser's serialized operation queue (and every later navigation) forever.
const CHILD_SESSION_COMMAND_TIMEOUT_MS = 10000;
// Workass's own JavaScript world in every page and frame. Nothing Workass
// runs in a page runs in the page's world: the page can neither see it (no
// globals, observers, listeners or timers of ours) nor change what it sees.
// Only workass_browser_evaluate runs the agent's own code in the page.
const AGENT_WORLD_NAME = 'workass-agent';
// Electron's equivalent, only for a page without a CDP session yet.
const AGENT_WORLD_ID = 1999;
// One input command (a mouse, key or drag event) that the page does not
// take within this is given up on, and whatever it left pressed is released.
const INPUT_COMMAND_TIMEOUT_MS = 5000;
const INPUT_RELEASE_TIMEOUT_MS = 1000;
// A one-line <select> opens a native menu: agent input cannot operate it,
// and on macOS it holds the window's input until it closes.
// Calls that only look at a page: they do not make it agent-driven (which
// changes who gets its popups and downloads).
const READ_ONLY_METHODS = new Set(['browser.list', 'browser.snapshot', 'browser.read', 'browser.screenshot', 'browser.diagnostics',
  'browser.controlStatus', 'browser.receipt', 'browser.wait']);
const NATIVE_MENU_REFUSAL = Object.freeze({ ok: false, status: 'native_menu',
  reason: 'a select list opens a native menu that agent input cannot operate; choose its option with workass_browser_select' });
const MENU_OPENING_KEYS = new Set(['Space', ' ', 'Enter', 'ArrowDown', 'ArrowUp']);
// The page world's part of editing a code editor: the editor objects
// (Monaco's, CodeMirror's) exist only there. Runs on the element the agent
// world found (this), finds its editor, and does one thing to it.
const EDITOR_PAGE_FUNCTION = `function (op) {
  const root = this && this.closest ? this.closest('.monaco-editor,.CodeMirror') : null;
  if (!root) return { editor: null };
  let monaco = null;
  let cm = null;
  try {
    if (root.classList.contains('monaco-editor')) {
      const api = globalThis.monaco && globalThis.monaco.editor;
      const list = api && typeof api.getEditors === 'function' ? api.getEditors() : [];
      monaco = list.find((candidate) => candidate && candidate.getDomNode && candidate.getDomNode() === root) || null;
    } else if (root.CodeMirror && typeof root.CodeMirror.getValue === 'function') {
      cm = root.CodeMirror;
    }
  } catch (_) { return { editor: null }; }
  if (!monaco && !cm) return { editor: null };
  const kind = monaco ? 'monaco' : 'codemirror';
  try {
    const model = monaco ? monaco.getModel() : null;
    const value = () => (monaco ? String(model ? model.getValue() : '') : String(cm.getValue()));
    if (op === 'inspect') {
      const readOnly = monaco ? !!(monaco.getRawOptions && monaco.getRawOptions().readOnly) : cm.getOption && cm.getOption('readOnly') === true;
      return { editor: kind, readOnly, value: value(), length: value().length };
    }
    if (op === 'value') return { editor: kind, value: value(), length: value().length };
    if (op === 'focus') { (monaco || cm).focus(); return { editor: kind, done: true }; }
    if (op === 'selectAll') {
      if (monaco) {
        const range = model && model.getFullModelRange ? model.getFullModelRange() : null;
        if (!range) return { editor: kind, done: false };
        monaco.focus();
        monaco.setSelection(range);
        return { editor: kind, done: true, strategy: 'monaco-model' };
      }
      cm.execCommand('selectAll');
      return { editor: kind, done: true, strategy: 'codemirror-command' };
    }
    if (op === 'deleteSelection') {
      if (monaco) {
        const selection = monaco.getSelection ? monaco.getSelection() : null;
        if (!model || !selection || selection.isEmpty()) return { editor: kind, done: false };
        const before = model.getValueLength();
        monaco.executeEdits('workass-browser-key', [{ range: selection, text: '', forceMoveMarkers: true }]);
        return { editor: kind, done: model.getValueLength() < before, strategy: 'monaco-model' };
      }
      if (!cm.somethingSelected || !cm.somethingSelected()) return { editor: kind, done: false };
      const before = String(cm.getValue()).length;
      cm.replaceSelection('');
      return { editor: kind, done: String(cm.getValue()).length < before, strategy: 'codemirror-selection' };
    }
  } catch (_) { return { editor: kind, error: true }; }
  return { editor: kind };
}`;
// The focused element (through open shadow roots), in the agent world.
const ACTIVE_ELEMENT_EXPRESSION = `(() => {
  let el = document.activeElement;
  const seen = new Set();
  while (el && !seen.has(el)) { seen.add(el); const nested = el.shadowRoot && el.shadowRoot.activeElement; if (!nested || nested === el) break; el = nested; }
  return el && el !== document.body && el !== document.documentElement ? el : null;
})()`;
const STALE_CONTEXT = /Cannot find context|context with specified id|Execution context was destroyed|Cannot find default execution context|No frame with given id/u;
const SNAPSHOT_CHILD_FRAMES_BUDGET_MS = 8000;
// How long a call waits for the app renderer to mount (or, after a reload,
// mount again) a page's <webview> before it reports that the page is not up.
const GUEST_ATTACH_TIMEOUT_MS = 20000;
const EDITOR_PAGE_CALL_TIMEOUT_MS = 3000;
// A page an agent drove stays painting this long after its last call.
const PAINT_IDLE_MS = 4000;
// The page outline: frames read per outline, their shared time budget, how
// many read at once, refs per document, outlines kept for paging and diffs,
// and the default page size.
const OUTLINE_FRAME_LIMIT = 120;
const OUTLINE_FRAMES_BUDGET_MS = 8000;
const OUTLINE_READ_CONCURRENCY = 8;
const OUTLINE_REF_LIMIT = 20000;
const OUTLINE_HISTORY = 6;
const OUTLINE_PAGE_BYTES = 20000;
const OUTLINE_SECRET_CHECK_LIMIT = 400;
// No agent call may hold a tab's queue longer than this (the CLI gives up at
// 30 s); navigation and batches get a little more.
const OPERATION_DEADLINE_MS = 25000;
const LONG_OPERATION_DEADLINE_MS = 28500;
const CAPTURE_TIMEOUT_MS = 12000;
// Reasons that mean the child session no longer exists, so no protocol
// command may be sent to it (or to detach it) anymore.
const OWNED_TARGET_GONE_REASONS = new Set(['target_gone', 'target_crashed', 'debugger_lost']);

class OwnedTargetSetupCancelled extends Error {
  constructor(reason) {
    super(`owned iframe setup cancelled: ${reason}`);
    this.reason = reason;
  }
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

function safeRadius(value) {
  const radius = Math.round(Number(value) || 0);
  return Math.max(0, Math.min(32, radius));
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
  constructor({
    win, WebContentsView, BrowserWindow, nativeImage, session, partition = DEFAULT_PARTITION, chromeVersion, platform,
    onState, requestOpen, onLifecycle, host = 'view', webContentsFromId = null, onHosts = null, pipPreload = '',
    downloadsDir = '',
  }) {
    if (!win || !WebContentsView || !session) throw new Error('browser manager dependencies missing');
    this.win = win;
    this.WebContentsView = WebContentsView;
    this.BrowserWindow = BrowserWindow || null;
    this.nativeImage = nativeImage || null;
    this.partition = partition;
    // 'webview': every page is the guest of a <webview> the app renderer
    // mounts, so it moves and resizes with the HTML around it. 'view': the
    // page is a WebContentsView stacked over the window (the escape hatch).
    this.hostMode = host === 'webview' ? 'webview' : 'view';
    this.webContentsFromId = typeof webContentsFromId === 'function' ? webContentsFromId : () => null;
    this.onHosts = typeof onHosts === 'function' ? onHosts : () => {};
    this.pipPreload = String(pipPreload || '');
    // Guests the app window attached through will-attach-webview (hardened).
    this.attachedGuests = new WeakSet();
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
    // Bumped when the renderer that presented the page goes away: a request it
    // sent before that never puts the page back on screen.
    this.presentationEpoch = 0;
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
    // Files an agent's page downloads go to a folder of the chat's own.
    this.downloadsRoot = String(downloadsDir || '');
    // A page an agent drives keeps producing frames through a frame
    // subscription only while the app window is not on screen (holdPainting).
    if (this.hostMode === 'webview' && typeof win.on === 'function') {
      for (const event of ['hide', 'show', 'minimize', 'restore']) {
        win.on(event, () => { for (const entry of this.entries.values()) this.syncPaintSource(entry); });
      }
    }
    // The app losing focus ends any pointer press an agent left down, so no
    // page keeps the pointer captured.
    if (typeof win.on === 'function') {
      win.on('blur', () => { for (const entry of this.entries.values()) if (entry.pointerDown) void this.releaseStuckInput(entry); });
    }
    if (typeof this.profile.on === 'function') {
      this.profile.on('will-download', (_event, item, contents) => {
        const entry = Array.from(this.entries.values()).find((candidate) => candidate.view && candidate.view.webContents === contents);
        if (entry && !entry.closed) takeDownload(this, entry, item);
      });
    }
  }

  publicState(entry) {
    const wc = this.liveContents(entry);
    const history = wc && !wc.isDestroyed?.() ? wc.navigationHistory : null;
    const live = !!wc && !wc.isDestroyed?.();
    return {
      chatId: entry.chatId,
      url: redactPageMessage(entry.url || DEFAULT_URL),
      title: redactPageMessage(entry.title || ''),
      loading: !!entry.loading,
      error: entry.error ? redactPageMessage(entry.error) : null,
      canGoBack: !!(history && history.canGoBack && history.canGoBack()),
      canGoForward: !!(history && history.canGoForward && history.canGoForward()),
      cdpAttached: !!(live && wc.debugger && wc.debugger.isAttached && wc.debugger.isAttached()),
      persistent: !!(live && wc.session && wc.session.isPersistent && wc.session.isPersistent()),
      agentControl: this.agentControl,
      viewport: { ...entry.viewport },
      effectiveViewport: entry.effectiveViewport ? { ...entry.effectiveViewport } : null,
      viewportGeneration: entry.viewportGeneration,
      documentGeneration: entry.documentGeneration,
      visible: entry.visible === true,
      presentationBounds: entry.presentationBounds ? { ...entry.presentationBounds } : null,
      presentationScale: entry.presentationScale,
      poppedOut: !!(entry.pipWindow && !entry.pipWindow.isDestroyed()),
      host: this.isHosted(entry) ? 'webview' : 'view',
      ...(this.isHosted(entry) ? { hostKey: entry.hostKey, tabId: entry.tabId ?? null, attached: live } : {}),
    };
  }

  // The page's webContents, or null while a webview host is between guests
  // (the app renderer reloaded and has not mounted the page's <webview> yet).
  liveContents(entry) {
    const wc = entry && entry.view ? entry.view.webContents : null;
    return wc || null;
  }

  // A tab keeps one id for its whole life. In the webview host the page's
  // webContents is replaced when the app renderer reloads, so the id is the
  // first guest's (webContents ids are never reused in a process).
  tabIdOf(entry) {
    if (entry.tabId != null) return Number(entry.tabId);
    const wc = this.liveContents(entry);
    return wc ? Number(wc.id) : 0;
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
      this.onLifecycle({ stage, chatId: entry.chatId, tabId: this.tabIdOf(entry), ...details });
    } catch { /* test/diagnostic observers cannot interrupt browser work */ }
  }

  bind(entry) {
    const wc = entry.view.webContents;
    // A webview host replaces the page's webContents when the app renderer
    // reloads; a late event from the replaced one must not touch the tab.
    const current = () => entry.view.webContents === wc;
    const on = (name, handler) => wc.on(name, (...args) => { if (current()) handler(...args); });
    const syncURL = (_event, url) => {
      entry.url = String(url || wc.getURL() || DEFAULT_URL);
      entry.error = null;
      this.publish(entry);
    };
    on('did-start-navigation', (_event, _url, inPlace, isMainFrame) => {
      if (inPlace || isMainFrame === false) return;
      entry.mainNavigationPending = true;
      entry.appliedMetrics = null;
      entry.documentGeneration += 1;
      entry.interactionGeneration += 1;
      entry.frameTreeGeneration += 1;
      entry.observations.clear();
      entry.screenshots.clear();
      this.invalidateFrames(entry, null, 'document_replaced');
      this.interruptWaiters(entry, 'navigation');
    });
    // A page whose subframes never finish keeps loading forever; its main
    // document is still ready to observe once DOMContentLoaded has fired.
    on('did-start-navigation', (_event, _url, inPlace, isMainFrame) => {
      if (!inPlace && isMainFrame !== false) { entry.mainDocumentReady = false; entry.mainCommitPending = true; }
    });
    on('dom-ready', () => { entry.mainDocumentReady = true; entry.mainCommitPending = false; });
    on('did-start-loading', () => { entry.loading = true; entry.error = null; touchActivity(entry); this.publish(entry); });
    on('did-stop-loading', () => {
      touchActivity(entry);
      // A navigation that ended without a new document (a download, a 204,
      // a cancel) leaves the old one in place, still usable.
      if (entry.mainCommitPending) { entry.mainCommitPending = false; entry.mainDocumentReady = true; }
      entry.loading = false;
      entry.url = wc.getURL() || entry.url || DEFAULT_URL;
      this.publish(entry);
      this.retryDeferredViewport(entry);
    });
    on('did-navigate', (event, url) => {
      syncURL(event, url);
      pushEvent(entry, { type: 'navigated', url: redactPageMessage(String(url || ''), 1024) });
      this.pinGuestZoom(entry);
      this.resyncViewSize(entry);
    });
    // Leaving a page whose beforeunload asks to stay: an agent's navigation
    // goes ahead (and says so); a person's keeps Electron's default.
    on('will-prevent-unload', (event) => {
      if (!agentDriven(entry)) return;
      event.preventDefault();
      pushEvent(entry, { type: 'beforeunload_accepted' });
    });
    // A guest follows the app's zoom (and a pinch or a zoom shortcut while it
    // has focus); the page itself always lays out at 1:1.
    if (this.isHosted(entry)) on('zoom-changed', () => this.pinGuestZoom(entry));
    on('did-navigate-in-page', (event, url, isMainFrame) => {
      syncURL(event, url);
      if (isMainFrame !== false) pushEvent(entry, { type: 'url_changed', url: redactPageMessage(String(url || ''), 1024) });
    });
    on('did-finish-load', () => {
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
    on('page-title-updated', (_event, title) => { entry.title = String(title || ''); this.publish(entry); });
    on('did-fail-load', (_event, code, description, url, isMainFrame) => {
      if (!isMainFrame || code === -3) return; // ERR_ABORTED is normal during redirects.
      entry.loading = false;
      entry.url = String(url || entry.url || DEFAULT_URL);
      entry.error = String(description || `load failed (${code})`);
      this.publish(entry);
    });
    on('render-process-gone', (_event, details) => {
      entry.loading = false;
      entry.appliedMetrics = null;
      this.interruptWaiters(entry, 'renderer_lost');
      this.cancelOwnedTargetTasks(entry, () => true, 'renderer_lost');
      this.invalidateFrames(entry, null, 'renderer_lost');
      entry.error = `browser renderer stopped (${String(details && details.reason || 'unknown')})`;
      this.publish(entry);
      // A hosted page's renderer that died is brought back in place (same tab,
      // same URL), a few times a minute at most.
      if (this.isHosted(entry) && String(details && details.reason || '') !== 'clean-exit') {
        const now = Date.now();
        entry.crashReloads = (entry.crashReloads || []).filter((at) => now - at < 60000);
        if (entry.crashReloads.length < 3) {
          entry.crashReloads.push(now);
          const timer = setTimeout(() => {
            if (!current() || wc.isDestroyed?.() || !this.isCurrentEntry(entry)) return;
            try { wc.reload(); } catch { /* the guest is going away */ }
          }, 400);
          if (typeof timer?.unref === 'function') timer.unref();
        }
      }
    });
    wc.setWindowOpenHandler((details) => {
      const url = String(details && details.url || '');
      // A page an agent is driving opens links and popups as tabs of the
      // same chat, which the agent can list and drive.
      // The popup keeps its opener (window.opener, postMessage: sign-in and
      // payment popups) and its request (a form's POST): Chromium creates
      // its webContents, Workass only hosts it.
      if (current() && agentDriven(entry) && /^https?:\/\//i.test(url)) {
        if (!this.popupTabAllowed(entry, url)) return { action: 'deny' };
        return {
          action: 'allow',
          overrideBrowserWindowOptions: { show: false, webPreferences: this.popupWebPreferences() },
          createWindow: (options) => this.adoptPopup(entry, url, String(details.disposition || ''), options),
        };
      }
      return this.windowOpenPolicy(url);
    });
    if (wc.debugger && typeof wc.debugger.on === 'function') this.bindDebugger(entry, wc, current);
  }

  windowOpenPolicy(url) {
    return {
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
    };
  }

  bindDebugger(entry, wc, current) {
    {
      wc.debugger.on('message', (_event, method, params, sessionId) => {
        if (!current()) return;
        const currentSessionId = normalizeCDPSessionId(sessionId);
        noteActivity(entry, method, params || {}, currentSessionId);
        const currentSession = currentSessionId == null || entry.targetSessions.has(currentSessionId);
        if (currentSession && !String(method || '').startsWith('Target.')) {
          this.emitCDP({ tabId: this.tabIdOf(entry), method, params: params || {}, sessionId: currentSessionId });
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
        if (!current()) return;
        this.emitCDP({ tabId: this.tabIdOf(entry), detach: true, reason: String(reason || 'detached') });
        entry.cdpReady = false;
        this.interruptWaiters(entry, 'debugger_lost');
        this.cancelOwnedTargetTasks(entry, () => true, 'debugger_lost');
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
    this.releaseSessionCommands(entry, sessionId);
    this.childSessions.delete(String(this.tabIdOf(entry)) + ':' + accepted.targetId);
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
      this.releaseSessionCommands(entry, nestedId);
      this.childSessions.delete(String(this.tabIdOf(entry)) + ':' + nestedTarget.targetId);
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
    // A child session retired while its Page.getFrameTree was in flight must
    // never have its frames recorded as the root target's.
    if (sessionId && !target) return false;
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

  async enableOwnedSession(entry, sessionId = null, task = null) {
    const debug = entry.view.webContents.debugger;
    const step = (promise) => this.ownedTargetStep(task, promise);
    for (const domain of ['Page', 'Runtime', 'DOM', 'Accessibility']) {
      try { await step(() => debug.sendCommand(domain + '.enable', {}, sessionId || undefined)); }
      catch (error) {
        if (error instanceof OwnedTargetSetupCancelled) throw error;
        /* an unavailable optional domain remains explicitly unavailable */
      }
    }
    const tree = await step(() => this.readFrameTree(entry, sessionId));
    this.recordFrameTree(entry, tree, sessionId);
    try {
      await step(() => debug.sendCommand('Target.setAutoAttach', {
        autoAttach: true,
        waitForDebuggerOnStart: false,
        flatten: true,
        filter: [{ type: 'iframe', exclude: false }, { type: '*', exclude: true }],
      }, sessionId || undefined));
      if (!sessionId) entry.autoAttachAvailable = true;
    } catch (error) {
      if (error instanceof OwnedTargetSetupCancelled) throw error;
      if (!sessionId) entry.autoAttachAvailable = false;
    }
  }

  // Races one setup step against its task's cancellation (timeout, target
  // gone, navigation, entry closed). The step starts only while the task is
  // live, so a cancelled setup sends nothing more; an abandoned protocol
  // promise may never settle, and nothing waits on it after this.
  ownedTargetStep(task, work) {
    const start = () => (typeof work === 'function' ? work() : work);
    if (!task) return start();
    if (task.cancelled) return Promise.reject(new OwnedTargetSetupCancelled(task.cancelled));
    let promise;
    try { promise = Promise.resolve(start()); } catch (error) { promise = Promise.reject(error); }
    return Promise.race([promise, task.cancelSignal]);
  }

  // A target auto-attached with waitForDebuggerOnStart stays paused until its
  // session says so; every outcome resumes it. Fire-and-forget: a renderer
  // that stopped responding must not hold setup open, and a detach resumes it
  // too.
  resumeWaitingTarget(entry, task) {
    if (!task || !task.waitingForDebugger || task.resumed || !task.sessionId) return;
    task.resumed = true;
    try {
      void Promise.resolve(entry.view.webContents.debugger.sendCommand('Runtime.runIfWaitingForDebugger', {}, task.sessionId))
        .catch(() => { /* the target is already gone or running */ });
    } catch { /* debugger already closed */ }
  }

  // Chromium drops the reply to a command in flight on a child session that
  // goes away, so an owned-frame observation must not await it unbounded.
  ownedSessionCommand(entry, method, params, sessionId, timeoutMs = null) {
    if (sessionId && !entry.targetSessions.has(sessionId)) return Promise.reject(new Error('browser frame target is no longer owned'));
    const command = Promise.resolve(entry.view.webContents.debugger.sendCommand(method, params, sessionId || undefined));
    if (!sessionId) {
      // The top frame's commands are bounded too when the caller gives a
      // limit (evaluate's 10 s, the outline's budget).
      if (!(Number(timeoutMs) > 0)) return command;
      let timer = null;
      return Promise.race([command, new Promise((_resolve, reject) => {
        timer = setTimeout(() => reject(new Error(`browser page did not answer ${method} within ${Math.max(1, Math.round(Number(timeoutMs) / 1000))}s`)), Number(timeoutMs));
      })]).finally(() => clearTimeout(timer));
    }
    const waiters = entry.sessionCommandWaiters || (entry.sessionCommandWaiters = new Map());
    const pending = waiters.get(sessionId) || new Set();
    waiters.set(sessionId, pending);
    return new Promise((resolve, reject) => {
      let timer = null;
      const finish = (settle, value) => {
        clearTimeout(timer);
        pending.delete(fail);
        if (!pending.size && waiters.get(sessionId) === pending) waiters.delete(sessionId);
        settle(value);
      };
      const fail = (message) => finish(reject, new Error(message));
      pending.add(fail);
      const capMs = this.childSessionCommandTimeoutMs ?? CHILD_SESSION_COMMAND_TIMEOUT_MS;
      const limitMs = Math.max(1, Math.min(capMs, Number(timeoutMs) > 0 ? Number(timeoutMs) : capMs));
      timer = setTimeout(() => fail(`browser frame did not answer ${method} within ${Math.round(limitMs / 1000)}s`), limitMs);
      command.then((value) => finish(resolve, value), (error) => finish(reject, error));
    });
  }

  releaseSessionCommands(entry, sessionId) {
    const pending = entry.sessionCommandWaiters?.get(sessionId);
    if (!pending) return;
    entry.sessionCommandWaiters.delete(sessionId);
    for (const fail of Array.from(pending)) fail('browser frame target went away; refresh the snapshot');
  }

  detachChildSessionQuietly(entry, sessionId, parentSessionId) {
    if (!sessionId) return;
    try {
      void Promise.resolve(entry.view.webContents.debugger.sendCommand(
        'Target.detachFromTarget', { sessionId }, parentSessionId || undefined,
      )).catch(() => { /* the session is already gone */ });
    } catch { /* debugger already closed */ }
  }

  startOwnedTargetTask(entry, params, parentSessionId) {
    const targetInfo = params && params.targetInfo || {};
    const task = {
      sessionId: String(params && params.sessionId || ''),
      targetId: String(targetInfo.targetId || ''),
      parentSessionId: parentSessionId || null,
      waitingForDebugger: params && params.waitingForDebugger === true,
      startedAt: Date.now(),
      frameId: null,
      accepted: false,
      resumed: false,
      cancelled: null,
      settled: false,
      timer: null,
      rejectCancel: null,
    };
    task.cancelSignal = new Promise((_resolve, reject) => { task.rejectCancel = reject; });
    task.cancelSignal.catch(() => { /* observed through ownedTargetStep */ });
    task.timer = setTimeout(() => this.cancelOwnedTargetTask(entry, task, 'setup_timeout'),
      this.ownedTargetSetupTimeoutMs ?? OWNED_TARGET_SETUP_TIMEOUT_MS);
    if (typeof task.timer?.unref === 'function') task.timer.unref();
    entry.pendingTargetTasks.add(task);
    task.promise = Promise.resolve()
      .then(() => this.acceptAttachedTarget(entry, params, parentSessionId, task))
      .catch((error) => {
        const reason = error instanceof OwnedTargetSetupCancelled ? error.reason : 'setup_failed';
        if (!task.cancelled) task.cancelled = reason;
        this.abandonOwnedTargetTask(entry, task, task.cancelled, error instanceof OwnedTargetSetupCancelled ? null : error);
        return false;
      })
      .finally(() => {
        clearTimeout(task.timer);
        task.settled = true;
        entry.pendingTargetTasks.delete(task);
      });
    return task;
  }

  cancelOwnedTargetTask(entry, task, reason) {
    if (!task || task.settled || task.cancelled) return false;
    task.cancelled = reason;
    task.rejectCancel(new OwnedTargetSetupCancelled(reason));
    return true;
  }

  cancelOwnedTargetTasks(entry, predicate, reason) {
    let cancelled = 0;
    for (const task of Array.from(entry.pendingTargetTasks || [])) {
      if (predicate(task) && this.cancelOwnedTargetTask(entry, task, reason)) cancelled += 1;
    }
    return cancelled;
  }

  // Settles a cancelled setup without waiting on the page: resume a paused
  // target, release a session that is still attached, and leave the frame
  // explicitly unavailable for observations.
  abandonOwnedTargetTask(entry, task, reason, error = null) {
    const sessionGone = OWNED_TARGET_GONE_REASONS.has(reason);
    if (!sessionGone) this.resumeWaitingTarget(entry, task);
    const accepted = task.sessionId ? entry.targetSessions.get(task.sessionId) : null;
    if (accepted) {
      // Accepted mid-enable: retire the session subtree (synchronous before
      // its first await, and no protocol command for a non-explicit reason).
      void this.detachOwnedSession(entry, task.sessionId, reason);
    }
    // Setup of a child below this one cannot outlive its parent session.
    this.cancelOwnedTargetTasks(entry, (child) => child !== task && child.parentSessionId === task.sessionId, 'target_gone');
    if (!sessionGone) this.detachChildSessionQuietly(entry, task.sessionId, task.parentSessionId);
    const frame = task.frameId ? entry.frames.get(task.frameId) : null;
    const parent = frame?.parentFrameId ? entry.frames.get(frame.parentFrameId) : null;
    // Never touch a frame another (newer) session has since bound.
    const ownedByThisSetup = frame && (frame.sessionId === task.sessionId ||
      (frame.sessionId || null) === task.parentSessionId);
    if (ownedByThisSetup) {
      this.invalidateFrames(entry, frame.id, reason);
      if (parent && !parent.detached) {
        frame.targetId = parent.targetId;
        frame.sessionId = parent.sessionId || null;
        frame.access = 'owned_cdp_frame';
      } else {
        frame.sessionId = null;
      }
      frame.contextId = null;
      frame.unavailableReason = reason;
    }
    this.lifecycle(entry, 'owned-frame-target-abandoned', {
      reason,
      elapsedMs: Date.now() - task.startedAt,
      accepted: task.accepted,
      frameMarked: !!ownedByThisSetup,
      sessionDetached: !sessionGone,
      resumed: task.resumed,
      ...(error ? { error: redactPageMessage(error && error.message || error, 256) } : {}),
    });
  }

  async rejectAttachedTarget(entry, parentSessionId, childSessionId, task = null) {
    this.resumeWaitingTarget(entry, task);
    try {
      await this.ownedTargetStep(task, () => entry.view.webContents.debugger.sendCommand(
        'Target.detachFromTarget', { sessionId: childSessionId }, parentSessionId || undefined,
      ));
    } catch (error) {
      if (error instanceof OwnedTargetSetupCancelled) throw error;
      /* unowned target is immediately abandoned */
    }
  }

  async acceptAttachedTarget(entry, params, parentSessionId, task = null) {
    const step = (promise) => this.ownedTargetStep(task, promise);
    const pause = () => step(() => new Promise((resolve) => setTimeout(resolve, 50)));
    const targetInfo = params && params.targetInfo;
    const childSessionId = params && params.sessionId;
    if (!this.isCurrentEntry(entry) || !targetInfo || !childSessionId || targetInfo.type !== 'iframe') {
      if (childSessionId) await this.rejectAttachedTarget(entry, parentSessionId, childSessionId, task);
      if (this.isCurrentEntry(entry)) this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'target_type_or_entry_unavailable' });
      return false;
    }
    const parentTargetId = parentSessionId
      ? entry.targetSessions.get(parentSessionId)?.targetId
      : entry.rootTargetId;
    if (!parentTargetId) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId, task);
      this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'parent_target_unowned' });
      return false;
    }
    // TargetInfo does not provide a portable parentTargetId for iframe
    // targets. Bind the session only through the exact frame id in the
    // parent's authoritative Page.getFrameTree response.
    const parentTree = await step(() => this.readFrameTree(entry, parentSessionId || null));
    this.recordFrameTree(entry, parentTree, parentSessionId || null);
    const response = await step(() => this.readFrameTree(entry, childSessionId));
    if (!response) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId, task);
      this.lifecycle(entry, 'owned-frame-target-rejected', { reason: 'child_frame_tree_unavailable' });
      return false;
    }
    const root = response.frame;
    const frameId = String(root && root.id || '');
    if (task && frameId) task.frameId = frameId;
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
      await pause();
      const refreshedTree = await step(() => this.readFrameTree(entry, parentSessionId || null));
      this.recordFrameTree(entry, refreshedTree, parentSessionId || null);
      parentTreeContainsChild = frameTreeRows(refreshedTree).some(({ frame }) => String(frame.id) === frameId);
      candidate = entry.frames.get(frameId) || null;
    }
    const parentRecord = claimedParent && entry.frames.get(claimedParent);
    if (!frameId || !claimedParent || (root.parentId && targetParentFrame && String(root.parentId) !== targetParentFrame) ||
        !parentRecord || parentRecord.detached || parentRecord.targetId !== parentTargetId ||
        (candidate && (candidate.parentFrameId !== claimedParent || candidate.targetId !== parentTargetId))) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId, task);
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
    // The parent's record of a cross-site child describes the document the
    // child had in the parent's renderer (its initial empty document, or the
    // page before it moved into its own target). Once the child runs in its
    // own target the parent no longer lists it at all, so that record never
    // catches up: waiting for it rejected every frame of a page whose parent
    // was still busy when its children attached (2026-10-26: 36 of 36
    // cross-site frames of one page). The child's own frame tree, whose
    // parentId resolved above to a live frame of this exact parent target
    // (the flattened session lineage proves which target announced it), is
    // the authority for the child's current document.
    if (frameId && targetId && candidate && root.loaderId && candidate.loaderId &&
        String(root.loaderId) !== String(candidate.loaderId)) {
      candidate.loaderId = String(root.loaderId);
      candidate.url = String(root.url || candidate.url || '');
      candidate.securityOrigin = String(root.securityOrigin || candidate.securityOrigin || '');
      candidate.generation = ++entry.frameGenerationSequence;
      entry.frameTreeGeneration += 1;
      entry.interactionGeneration += 1;
      entry.screenshots.clear();
    }
    if (!targetId || !candidate || (root.loaderId && candidate.loaderId && String(root.loaderId) !== String(candidate.loaderId))) {
      await this.rejectAttachedTarget(entry, parentSessionId, childSessionId, task);
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
    if (task) task.accepted = true;
    this.childSessions.set(String(this.tabIdOf(entry)) + ':' + targetId, childSessionId);
    candidate.targetId = targetId;
    candidate.sessionId = childSessionId;
    candidate.loaderId = String(root.loaderId || candidate.loaderId || '');
    candidate.access = 'owned_oopif';
    candidate.unavailableReason = null;
    candidate.detached = false;
    this.lifecycle(entry, 'owned-frame-target-accepted', { access: 'owned_oopif' });
    await this.enableOwnedSession(entry, childSessionId, task);
    this.resumeWaitingTarget(entry, task);
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
      await this.startOwnedTargetTask(entry, params, sessionId).promise;
      return;
    }
    if (method === 'Target.detachedFromTarget') {
      const detached = String(params && params.sessionId || '');
      if (detached) {
        this.cancelOwnedTargetTasks(entry, (task) => task.sessionId === detached || task.parentSessionId === detached, 'target_gone');
        if (entry.targetSessions.has(detached)) void this.detachOwnedSession(entry, detached, 'target_detached');
      }
      return;
    }
    if (method === 'Target.targetDestroyed') {
      const targetId = String(params && params.targetId || '');
      if (targetId) this.cancelOwnedTargetTasks(entry, (task) => task.targetId === targetId, 'target_gone');
      const sessions = Array.from(entry.targetSessions.entries()).filter(([, target]) => target.targetId === targetId);
      for (const [id, target] of sessions) {
        entry.targetSessions.delete(id);
        this.releaseSessionCommands(entry, id);
        this.childSessions.delete(String(this.tabIdOf(entry)) + ':' + target.targetId);
        this.invalidateFrames(entry, target.frameId, 'target_destroyed');
      }
      return;
    }
    if (method === 'Target.targetCrashed') {
      const targetId = String(params && params.targetId || '');
      if (targetId) this.cancelOwnedTargetTasks(entry, (task) => task.targetId === targetId, 'target_crashed');
      for (const [id, target] of Array.from(entry.targetSessions.entries())) {
        if (target.targetId !== targetId) continue;
        entry.targetSessions.delete(id);
        this.releaseSessionCommands(entry, id);
        this.childSessions.delete(String(this.tabIdOf(entry)) + ':' + target.targetId);
        this.invalidateFrames(entry, target.frameId, 'target_crashed');
      }
      return;
    }
    const target = sessionId ? entry.targetSessions.get(sessionId) : null;
    if (sessionId && !target) return;
    const targetId = target ? target.targetId : entry.rootTargetId;
    if (method === 'Page.javascriptDialogOpening') { dialogOpened(entry, params, sessionId); this.publish(entry); return; }
    if (method === 'Page.javascriptDialogClosed') { dialogClosed(entry, params); this.publish(entry); return; }
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
      // A frame swapped into another renderer (its cross-site document now
      // runs in its own target, or back) is not removed: it keeps its frame
      // id and its new target binds it. Only the emitting target's view of it
      // is gone. Treating the swap as a removal retired 33 of 36 cross-site
      // frames right after their targets were bound.
      if (method === 'Page.frameDetached' && params && params.reason === 'swap') {
        if (frame.targetId === targetId && !(target && target.frameId === frameId)) {
          frame.contextId = null;
          frame.generation = ++entry.frameGenerationSequence;
          frame.unavailableReason = 'owned_frame_target_pending';
          entry.frameTreeGeneration += 1;
          entry.interactionGeneration += 1;
          entry.screenshots.clear();
        }
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

  // Whether a page is the guest of a <webview> in the app window (the
  // webview host's own pages) rather than a native view (the view host's
  // pages, and in either host the popups pages open, whose webContents
  // Chromium makes with their opener).
  isHosted(entry) {
    return this.hostMode === 'webview' && !!(entry && entry.hostKey);
  }

  create(chatId) {
    if (this.hostMode === 'webview') return this.createHosted(chatId);
    const view = new this.WebContentsView({
      webPreferences: {
        partition: this.partition,
        nodeIntegration: false,
        contextIsolation: true,
        sandbox: true,
        navigateOnDragDrop: false,
      },
    });
    const entry = this.newEntry(chatId, view);
    try { view.setBackgroundColor('#ffffff'); } catch { /* older Electron */ }
    try { view.webContents.setUserAgent(this.userAgent); } catch { /* best effort */ }
    entry.tabId = Number(view.webContents.id);
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

  // ---------------------------------------------------------------------------
  // The webview host. The app renderer mounts one <webview> per page (main.js
  // hardens it in will-attach-webview) and hands its guest webContents back
  // with registerGuest. The page then moves, resizes and composites with the
  // HTML around it. Every agent path below runs on the guest unchanged.
  //
  // A reload or crash of the app renderer destroys every guest. The tab keeps
  // its id, URL and place: the reloaded renderer mounts it again, the new
  // guest is registered under the same host key, and the page is loaded back
  // at its last URL in the same persistent session (sign-ins intact). Agent
  // calls made meanwhile wait for it, bounded.
  createHosted(chatId) {
    const entry = this.newEntry(chatId, { webContents: null, hosted: true });
    entry.hostKey = 'wkb-' + crypto.randomBytes(9).toString('base64url');
    entry.guestGeneration = 0;
    this.entries.set(chatId, entry);
    entry.ready = this.awaitGuest(entry);
    this.publishHosts();
    return entry;
  }

  hostList() {
    return Array.from(this.entries.values())
      .filter((entry) => !entry.closed && entry.hostKey)
      .map((entry) => ({
        hostKey: entry.hostKey,
        chatId: entry.chatId,
        tabId: entry.tabId ?? null,
        viewport: { ...entry.viewport },
        visible: entry.visible === true,
        poppedOut: !!(entry.pipWindow && !entry.pipWindow.isDestroyed()),
      }))
      .sort((a, b) => a.hostKey.localeCompare(b.hostKey));
  }

  publishHosts() {
    if (this.hostMode !== 'webview') return;
    try { this.onHosts(this.hostList()); } catch { /* renderer may be reloading */ }
  }

  awaitGuest(entry) {
    const lifecycleToken = entry.lifecycleToken;
    const generation = (entry.guestWaitGeneration || 0) + 1;
    entry.guestWaitGeneration = generation;
    return new Promise((resolve) => {
      const timeoutMs = this.guestAttachTimeoutMs ?? GUEST_ATTACH_TIMEOUT_MS;
      const timer = setTimeout(() => {
        if (entry.guestWaiter?.generation !== generation) return;
        entry.guestWaiter = null;
        if (!this.isCurrentEntry(entry, lifecycleToken)) return resolve(false);
        entry.error = 'the browser page did not come up in the Workass window; open the browser pane, or try again in a moment';
        this.publish(entry);
        // A later registration still brings the page up for the next call.
        entry.ready = this.awaitGuest(entry);
        resolve(false);
      }, timeoutMs);
      if (typeof timer?.unref === 'function') timer.unref();
      entry.guestWaiter = {
        generation,
        settle: (value) => { clearTimeout(timer); entry.guestWaiter = null; resolve(value); },
      };
    });
  }

  // main.js reports every guest the app window attached after hardening it.
  noteAttachedGuest(contents) {
    if (contents && typeof contents === 'object') this.attachedGuests.add(contents);
  }

  // The app renderer hands over the guest it mounted for a host key.
  registerGuest(hostKey, webContentsId) {
    if (this.hostMode !== 'webview') return { ok: false, reason: 'this shell hosts pages in native views' };
    const entry = Array.from(this.entries.values()).find((candidate) => candidate.hostKey === String(hostKey || ''));
    if (!entry || entry.closed) return { ok: false, reason: 'unknown browser page' };
    const guest = this.webContentsFromId(Number(webContentsId));
    if (!guest || guest.isDestroyed?.() || !this.attachedGuests.has(guest)) return { ok: false, reason: 'not a page this window attached' };
    if (typeof guest.getType === 'function' && guest.getType() !== 'webview') return { ok: false, reason: 'not a webview guest' };
    if (guest.hostWebContents && guest.hostWebContents !== this.win.webContents) return { ok: false, reason: 'guest belongs to another window' };
    if (guest.session && this.profile && guest.session !== this.profile) return { ok: false, reason: 'guest is not in the browser profile' };
    if (entry.view.webContents === guest) return { ok: true, tabId: this.tabIdOf(entry), already: true };
    for (const other of this.entries.values()) {
      if (other !== entry && other.view.webContents === guest) return { ok: false, reason: 'guest already belongs to another page' };
    }
    if (entry.view.webContents && !entry.view.webContents.isDestroyed?.()) {
      return { ok: false, reason: 'this page already has a live guest' };
    }
    void this.adoptGuest(entry, guest);
    return { ok: true, tabId: entry.tabId ?? Number(guest.id) };
  }

  async adoptGuest(entry, guest) {
    const lifecycleToken = entry.lifecycleToken;
    const restoring = entry.guestGeneration > 0;
    entry.guestGeneration += 1;
    entry.view.webContents = guest;
    if (entry.tabId == null) entry.tabId = Number(guest.id);
    entry.cdpReady = false;
    entry.rootTargetId = null;
    entry.initialDocumentReady = false;
    entry.appliedMetrics = null;
    entry.focusEmulated = false;
    entry.focusChange = null;
    entry.captureInFlight = null;
    entry.paintSubscription = null;
    entry.frameSubscribers = null;
    try { guest.setUserAgent(this.userAgent); } catch { /* best effort */ }
    this.pinGuestZoom(entry);
    this.bind(entry);
    guest.once('destroyed', () => this.guestGone(entry, guest));
    this.lifecycle(entry, 'guest-attached', { restoring, generation: entry.guestGeneration });
    let ready = false;
    try {
      await this.initializeGuest(entry, lifecycleToken);
      ready = true;
      if (restoring && /^https?:\/\//i.test(String(entry.restoreURL || ''))) {
        const target = entry.restoreURL;
        entry.restoreURL = null;
        entry.loading = true;
        entry.url = target;
        // The page's own last URL: loaded again, never authorized the way a
        // navigate command authorizes its target.
        this.publish(entry);
        try {
          await this.loadNavigationTarget(entry, target);
          entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
        } catch (error) {
          entry.error = redactPageMessage(error && error.message || error);
        }
      }
    } catch (error) {
      if (this.isCurrentEntry(entry, lifecycleToken) && entry.view.webContents === guest) {
        entry.error = redactPageMessage(error && error.message || error);
      }
    }
    if (!this.isCurrentEntry(entry, lifecycleToken) || entry.view.webContents !== guest) return;
    this.publish(entry);
    this.publishHosts();
    if (entry.guestWaiter) entry.guestWaiter.settle(ready);
    else entry.ready = Promise.resolve(ready);
  }

  async initializeGuest(entry, lifecycleToken) {
    const wc = entry.view.webContents;
    // The renderer mounts the guest on about:blank; make sure that document
    // committed before metrics are applied, as the native host does.
    if (!wc.getURL() || wc.isLoadingMainFrame?.()) {
      await wc.loadURL(DEFAULT_URL);
      this.assertCurrentEntry(entry, lifecycleToken);
    }
    const initialDocument = await wc.executeJavaScript('({ url: location.href, readyState: document.readyState })');
    this.assertCurrentEntry(entry, lifecycleToken);
    if (!initialDocument || initialDocument.readyState === 'loading') {
      throw new Error('browser initial document did not become ready');
    }
    entry.initialDocumentReady = true;
    if (!await this.ensureCDP(entry)) throw new Error('browser page debugger is unavailable');
    this.assertCurrentEntry(entry, lifecycleToken);
    entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
    this.assertCurrentEntry(entry, lifecycleToken);
    if (!entry.viewportGeneration) entry.viewportGeneration = 1;
    try { await wc.debugger.sendCommand('Runtime.enable'); } catch { /* metrics and basic page control remain available */ }
    return true;
  }

  // The page lays out at 1:1 whatever the app's zoom is (a guest follows the
  // app's zoom unless told otherwise).
  pinGuestZoom(entry) {
    if (!this.isHosted(entry)) return;
    const wc = this.liveContents(entry);
    if (!wc || wc.isDestroyed?.() || typeof wc.getZoomFactor !== 'function') return;
    try { if (Math.abs(wc.getZoomFactor() - 1) > 1e-6) wc.setZoomFactor(1); } catch { /* closing */ }
  }

  // The app renderer reloaded or crashed (or the page's element went away
  // without a close): keep the tab, wait for its next guest.
  guestGone(entry, guest) {
    if (entry.closed || entry.view.webContents !== guest || this.entries.get(entry.chatId) !== entry) return;
    entry.view.webContents = null;
    entry.cdpReady = false;
    entry.rootTargetId = null;
    entry.initialDocumentReady = false;
    entry.appliedMetrics = null;
    entry.loading = false;
    entry.visible = false;
    this.interruptWaiters(entry, 'renderer_lost');
    this.cancelOwnedTargetTasks(entry, () => true, 'renderer_lost');
    this.invalidateFrames(entry, null, 'renderer_lost');
    for (const [sessionId] of Array.from(entry.targetSessions.entries())) this.releaseSessionCommands(entry, sessionId);
    entry.targetSessions.clear();
    entry.executionContexts.clear();
    entry.frames.clear();
    entry.observations.clear();
    entry.screenshots.clear();
    entry.documentGeneration += 1;
    entry.interactionGeneration += 1;
    entry.frameTreeGeneration += 1;
    entry.focusEmulated = false;
    entry.focusChange = null;
    entry.captureInFlight = null;
    if (/^https?:\/\//i.test(String(entry.url || ''))) entry.restoreURL = entry.url;
    this.lifecycle(entry, 'guest-lost', { restoreURL: !!entry.restoreURL });
    entry.ready = this.awaitGuest(entry);
    this.publish(entry);
    this.publishHosts();
  }

  newEntry(chatId, view) {
    return {
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

  // The agent world's execution context in a frame (CDP
  // Page.createIsolatedWorld), one per document.
  async agentContextId(entry, frame) {
    if (!frame) throw new Error('browser frame execution context is unavailable');
    const forDocument = `${frame.loaderId || ''}|${frame.contextId ?? ''}`;
    if (frame.agentWorld && frame.agentWorld.forDocument === forDocument) return frame.agentWorld.id;
    const created = await this.ownedSessionCommand(entry, 'Page.createIsolatedWorld',
      { frameId: frame.id, worldName: AGENT_WORLD_NAME, grantUniveralAccess: false }, frame.sessionId || null);
    const id = Number(created && created.executionContextId);
    if (!Number.isSafeInteger(id)) throw new Error('browser frame execution context is unavailable');
    frame.agentWorld = { forDocument, id };
    return id;
  }

  // Runs one command against a frame's agent world, making the world again
  // once if the document replaced it in between.
  async withAgentContext(entry, frame, run) {
    for (let attempt = 0; ; attempt += 1) {
      const contextId = await this.agentContextId(entry, frame);
      try {
        return await run(contextId);
      } catch (error) {
        if (attempt > 0 || !STALE_CONTEXT.test(String(error && error.message))) throw error;
        frame.agentWorld = null;
      }
    }
  }

  // A code editor's full text lives only in the editor's own objects
  // (Monaco's models, CodeMirror's instance), which exist only in the page's
  // world. This one read-only call asks the editor API for it, and only on a
  // page that has such an editor; the agent world reads everything else.
  async readEditorModels(entry, frame, editors, rowLists = []) {
    for (const editor of editors) {
      if (!editor || typeof editor.selector !== 'string' || !editor.selector) continue;
      const model = await this.editorPageCall(entry, frame ? frame.id : null, `document.querySelector(${JSON.stringify(editor.selector)})`, 'inspect');
      if (!model || !model.editor || typeof model.value !== 'string') continue;
      editor.text = model.value.slice(0, 12000);
      editor.valueLength = model.value.length;
      editor.truncated = model.value.length > 12000;
      editor.readOnly = editor.readOnly === true || model.readOnly === true;
      // The element rows for the same editor say the same.
      for (const rows of rowLists) {
        for (const row of Array.isArray(rows) ? rows : []) {
          if (!row || !row.editor || row.selector !== editor.selector) continue;
          row.text = model.value.trim().replace(/\s+/gu, ' ').slice(0, 240);
          row.readOnly = row.readOnly === true || model.readOnly === true;
        }
      }
    }
  }

  // One call into the page's own world for a code editor's API: the agent
  // world finds the element (elementExpression); only EDITOR_PAGE_FUNCTION
  // runs in the page world, on that element. Returns null when there is no
  // such editor or the call cannot be made.
  async editorPageCall(entry, frameId, elementExpression, op) {
    const frame = frameId != null ? entry.frames.get(String(frameId)) : this.frameForSession(entry, null);
    if (!frame) return null;
    const sessionId = frame.sessionId || null;
    const release = (objectId) => this.ownedSessionCommand(entry, 'Runtime.releaseObject', { objectId }, sessionId).catch(() => {});
    let agentObject = null;
    let pageObject = null;
    try {
      const handle = await this.withAgentContext(entry, frame, (contextId) => this.ownedSessionCommand(entry, 'Runtime.evaluate', {
        expression: String(elementExpression), contextId, returnByValue: false,
      }, sessionId, EDITOR_PAGE_CALL_TIMEOUT_MS));
      agentObject = handle && handle.result && handle.result.objectId;
      if (!agentObject) return null;
      const described = await this.ownedSessionCommand(entry, 'DOM.describeNode', { objectId: agentObject }, sessionId, EDITOR_PAGE_CALL_TIMEOUT_MS);
      const backendNodeId = Number(described && described.node && described.node.backendNodeId);
      if (!(backendNodeId > 0)) return null;
      const resolved = await this.ownedSessionCommand(entry, 'DOM.resolveNode', {
        backendNodeId, ...(frame.contextId != null ? { executionContextId: frame.contextId } : {}),
      }, sessionId, EDITOR_PAGE_CALL_TIMEOUT_MS);
      pageObject = resolved && resolved.object && resolved.object.objectId;
      if (!pageObject) return null;
      const called = await this.ownedSessionCommand(entry, 'Runtime.callFunctionOn', {
        objectId: pageObject, functionDeclaration: EDITOR_PAGE_FUNCTION, arguments: [{ value: String(op) }], returnByValue: true,
      }, sessionId, EDITOR_PAGE_CALL_TIMEOUT_MS);
      const value = called && called.result && called.result.value;
      return value && typeof value === 'object' ? value : null;
    } catch {
      return null;
    } finally {
      if (agentObject) await release(agentObject);
      if (pageObject) await release(pageObject);
    }
  }

  // Script on the top document, in the agent world. Over the root CDP
  // session: Electron's executeJavaScript() also waits until the page stops
  // loading, which a page with a never-finishing subframe never does.
  async evaluateAgentWorld(entry, expression, userGesture = false) {
    const wc = entry.view.webContents;
    const root = wc.debugger?.isAttached?.() === true ? this.frameForSession(entry, null) : null;
    if (root) {
      return this.withAgentContext(entry, root, (contextId) => Promise.resolve(wc.debugger.sendCommand('Runtime.evaluate', {
        expression: String(expression), contextId, returnByValue: true, awaitPromise: true, userGesture: userGesture === true,
      })).then(unwrapCDPValue));
    }
    if (typeof wc.executeJavaScriptInIsolatedWorld === 'function') {
      return wc.executeJavaScriptInIsolatedWorld(AGENT_WORLD_ID, [{ code: String(expression) }], userGesture);
    }
    throw new Error('browser page execution context is unavailable');
  }

  async readEffectiveViewport(entry) {
    const effective = await this.evaluateAgentWorld(entry, '({ width: innerWidth, height: innerHeight, deviceScaleFactor: devicePixelRatio, scrollX, scrollY, documentWidth: document.documentElement?.scrollWidth || 0, documentHeight: document.documentElement?.scrollHeight || 0 })');
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
    const applied = Number.isFinite(Number(scale)) && Number(scale) > 0 ? Number(scale) : 1;
    entry.appliedMetrics = null;
    // dontSetVisibleSize: by default Chromium also resizes the page's view to
    // the emulated size, so until the next setBounds the page was painted at
    // its full 1440 × 900 layout, unscaled and spilling past its frame over
    // the chat. That was the floating browser "expanding" on every drag step
    // (2026-10-25, real app). The view's size is always set explicitly here.
    await webContents.debugger.sendCommand('Emulation.setDeviceMetricsOverride', {
      width: viewport.width,
      height: viewport.height,
      deviceScaleFactor: viewport.deviceScaleFactor,
      mobile: false,
      scale: applied,
      dontSetVisibleSize: true,
    });
    entry.appliedMetrics = { width: viewport.width, height: viewport.height, deviceScaleFactor: viewport.deviceScaleFactor, scale: applied };
  }

  // Every drag step of the floating browser used to re-apply the same metrics
  // (a CDP round trip, and with the view resized to 1440 × 900 each time, the
  // flicker and the ballooning). Metrics that are already in place are kept.
  metricsApplied(entry, viewport, scale) {
    const applied = entry.appliedMetrics;
    return !!applied && !entry.deferredViewport && applied.width === viewport.width && applied.height === viewport.height &&
      applied.deviceScaleFactor === viewport.deviceScaleFactor && Math.abs(applied.scale - scale) < 1e-9;
  }

  // The renderer measures in CSS pixels; the window places views in DIPs. They
  // differ by the app's zoom (View > Zoom In, Cmd +), and without this the page
  // drifted off its frame at any zoom but 100%. Bounds arrive unrounded and are
  // rounded once, here; the size from the size, so a moving frame never
  // changes the page's size.
  hostBounds(bounds) {
    if (!bounds || typeof bounds !== 'object') return bounds;
    const zoom = Number(this.win && this.win.webContents && this.win.webContents.getZoomFactor && this.win.webContents.getZoomFactor()) || 1;
    const at = (value) => Math.round((Number(value) || 0) * zoom);
    return { x: at(bounds.x), y: at(bounds.y), width: at(bounds.width), height: at(bounds.height) };
  }

  // The floating page has the frame's rounded corners (approved mock
  // browser-pip-v1 · B, "only the page, with rounded corners"). Rounding the
  // view also clips it: whatever the page paints stays inside its bounds and
  // can never spill over the chat.
  applyRadius(entry, radius) {
    const next = safeRadius(radius);
    if (entry.appliedRadius === next || typeof entry.view.setBorderRadius !== 'function') return;
    try {
      entry.view.setBorderRadius(next);
      entry.appliedRadius = next;
    } catch {
      /* an older Electron draws it square */
    }
  }

  // The renderer that presented the page is gone (a reload, a crash): nothing
  // may stay on screen without the controls it drew around the page. The page
  // leaves the window at once, not after whatever presentation step is in
  // flight, and nothing that renderer asked for before can bring it back. A
  // reloaded renderer presents it again, with its controls.
  hideAll() {
    this.presentationEpoch += 1;
    if (this.hostMode === 'webview') {
      // A hosted page is drawn by the renderer that is going away; nothing to
      // take off screen. Its guest goes with it and is replaced on the next
      // mount.
      for (const entry of this.entries.values()) if (this.isHosted(entry)) entry.visible = false;
      if (!this.attachedView) { this.activeId = null; return Promise.resolve(); }
    }
    const shown = this.attachedView;
    const entry = shown ? this.browserEntries().find((candidate) => candidate.view === shown) : null;
    if (shown) {
      try { this.win.contentView.removeChildView(shown); } catch { /* owning window already gone */ }
      this.attachedView = null;
      this.activeId = null;
    }
    if (entry) entry.visible = false;
    return this.serializePresentation(async () => {
      if (!entry || !this.isCurrentEntry(entry) || this.attachedView === entry.view || entry.pipWindow) return;
      if (entry.captureHost && !entry.captureHost.isDestroyed()) await this.attachToCaptureHost(entry, false);
    });
  }

  // A new document can come up believing its view is the emulated 1440 × 900
  // while the view itself is still the frame's size: the page then painted
  // unscaled, spilling past its frame, until something resized the view (seen
  // in the real app after every navigation of the floating page). Setting the
  // same bounds is a no-op, so the size is stepped and put back in one task;
  // only the final bounds are ever composited.
  resyncViewSize(entry) {
    if (this.isHosted(entry)) return;
    try {
      const bounds = entry.view.getBounds();
      if (!bounds || bounds.width < 2 || bounds.height < 2) return;
      entry.view.setBounds({ ...bounds, width: bounds.width - 1 });
      entry.view.setBounds(bounds);
    } catch {
      /* the view is closing */
    }
  }

  async applyAndReadDeviceEmulation(entry, viewport = entry.viewport, scale = 1) {
    await this.applyDeviceEmulation(entry, viewport, scale);
    const effective = await this.readVerifiedViewport(entry, viewport, scale);
    this.assertEffectiveViewport(viewport, effective);
    return effective;
  }

  // A same-site frame nested inside cross-site frames shares the main frame's
  // renderer and renders at the native screen scale. Until the next visual
  // properties sync, the main frame's devicePixelRatio can then report that
  // native scale although the page itself renders at the emulated one. When
  // only the scale disagrees, measure what the emulation controls: the raster
  // of a small CSS-pixel capture of the main frame (which also resyncs it). A
  // main frame that really renders at another scale still fails.
  async readVerifiedViewport(entry, requested, scale = 1) {
    const effective = await this.readEffectiveViewport(entry);
    if (effective.width !== requested.width || effective.height !== requested.height ||
        effective.deviceScaleFactor === requested.deviceScaleFactor) {
      return effective;
    }
    const rasterScale = await this.mainFrameRasterScale(entry, effective);
    const expected = [requested.deviceScaleFactor, requested.deviceScaleFactor * (Number(scale) > 0 ? Number(scale) : 1)];
    if (rasterScale == null || !expected.some((value) => Math.abs(rasterScale - value) < 0.05)) {
      this.lifecycle(entry, 'viewport-scale-mismatch', { reported: effective.deviceScaleFactor, raster: rasterScale });
      return effective;
    }
    const reread = await this.readEffectiveViewport(entry);
    this.lifecycle(entry, 'viewport-scale-verified-by-raster', {
      reported: effective.deviceScaleFactor, raster: rasterScale,
      resynced: reread.deviceScaleFactor === requested.deviceScaleFactor,
    });
    return { ...reread, deviceScaleFactor: requested.deviceScaleFactor };
  }

  async mainFrameRasterScale(entry, effective) {
    const size = 10;
    let timer = null;
    try {
      const capture = Promise.resolve(entry.view.webContents.debugger.sendCommand('Page.captureScreenshot', {
        format: 'png',
        clip: { x: Number(effective.scrollX) || 0, y: Number(effective.scrollY) || 0, width: size, height: size, scale: 1 },
      }));
      const response = await Promise.race([
        capture,
        new Promise((resolve) => { timer = setTimeout(() => resolve(null), 3000); }),
      ]);
      if (!response || !response.data) return null;
      const { width } = pngDimensions(Buffer.from(String(response.data), 'base64'));
      return width / size;
    } catch {
      return null;
    } finally {
      clearTimeout(timer);
    }
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

  // One operation at a time per tab, each bounded: an operation that does not
  // finish by its deadline is reported and the queue moves on without it (a
  // wedged page cannot hold every later call).
  serialize(entry, operation, { deadlineMs = null } = {}) {
    const hold = {};
    const deadlineLimitMs = deadlineMs ?? this.operationDeadlineMs ?? OPERATION_DEADLINE_MS;
    let deadlineAt = null;
    const run = entry.operationChain.catch(() => {}).then(() => {
      this.assertCurrentEntry(entry);
      // The action report (browser-actions.js) fits its waits and reads in
      // what is left of this; it ends with the operation.
      deadlineAt = Date.now() + deadlineLimitMs;
      entry.operationDeadlineAt = deadlineAt;
      return this.withPainting(entry, operation, hold);
    });
    run.then(() => {}, () => {}).then(() => {
      if (deadlineAt && entry.operationDeadlineAt === deadlineAt) entry.operationDeadlineAt = null;
    });
    // Whatever the operation left pressed (it failed between a press and its
    // release) is released as soon as it ends.
    run.then(() => {}, () => {}).then(() => {
      if (entry.pointerDown || entry.inputTimedOut) void this.releaseStuckInput(entry);
    });
    const limitMs = deadlineLimitMs;
    let timer = null;
    const bounded = Promise.race([
      run,
      new Promise((_resolve, reject) => {
        timer = setTimeout(() => {
          entry.wedgedAt = Date.now();
          if (deadlineAt && entry.operationDeadlineAt === deadlineAt) entry.operationDeadlineAt = null;
          this.lifecycle(entry, 'operation-deadline', { limitMs });
          // The tab moves on: it no longer stays emulated focused and
          // painting for an operation that may never end, and its input is
          // put back to rest.
          if (hold.release) hold.release();
          void this.releaseStuckInput(entry);
          reject(new Error(`the browser did not finish this within ${Math.round(limitMs / 1000)}s; the page may be stuck. Try again, or reset the tab with workass_browser_close`));
        }, limitMs);
        if (typeof timer?.unref === 'function') timer.unref();
      }),
    ]).finally(() => clearTimeout(timer));
    run.then(() => { entry.wedgedAt = null; }, () => {});
    entry.operationChain = bounded.catch(() => {});
    return bounded;
  }

  // Webview host: a guest that is not on screen paints no frames, so a
  // capture would wait for one forever and focus() would fire nothing. While
  // an agent drives a page it is emulated focused, which Chromium treats like
  // a captured page and keeps painting; a few seconds after the last call it
  // goes back to ordinary throttling (after t3code's DesktopBrowserHost).
  async withPainting(entry, operation, hold = {}) {
    if (!this.isHosted(entry)) return operation();
    this.holdPainting(entry);
    // Released once: when the operation ends, or at its deadline.
    hold.release = () => { if (hold.released) return; hold.released = true; this.releasePainting(entry); };
    try {
      if (entry.focusChange) await entry.focusChange;
      return await operation();
    } finally {
      hold.release();
    }
  }

  holdPainting(entry) {
    entry.paintHolds = (entry.paintHolds || 0) + 1;
    clearTimeout(entry.paintIdleTimer);
    entry.paintIdleTimer = null;
    entry.painting = true;
    if (!entry.focusEmulated) this.setFocusEmulation(entry, true);
    this.syncPaintSource(entry);
  }

  releasePainting(entry) {
    entry.paintHolds = Math.max(0, (entry.paintHolds || 0) - 1);
    if (entry.paintHolds || entry.closed) return;
    clearTimeout(entry.paintIdleTimer);
    entry.paintIdleTimer = setTimeout(() => {
      entry.paintIdleTimer = null;
      if (entry.paintHolds || entry.captureInFlight) return;
      entry.painting = false;
      if (!entry.pip) this.setFocusEmulation(entry, false);
      this.syncPaintSource(entry);
    }, this.paintIdleMs ?? PAINT_IDLE_MS);
    if (typeof entry.paintIdleTimer?.unref === 'function') entry.paintIdleTimer.unref();
  }

  // Whether the app window is on screen: its <webview> guests then produce
  // frames on their own.
  hostWindowShowing() {
    const win = this.win;
    try { return !!win && !win.isDestroyed?.() && win.isVisible?.() === true && win.isMinimized?.() !== true; } catch { return false; }
  }

  // While Workass itself is hidden or minimized a page produces no frames:
  // pointer moves then wait for a frame that never comes (about a second
  // each), timers slow down and captures stall. A frame subscriber keeps it
  // producing frames while an agent drives it, only then (it costs a frame
  // copy per frame), and copies out only each frame's damaged region.
  syncPaintSource(entry) {
    const want = entry.painting === true && !entry.closed && !this.hostWindowShowing();
    if (want && !entry.paintSubscription) entry.paintSubscription = this.addFrameSubscriber(entry, () => {}, { full: false });
    else if (!want && entry.paintSubscription) { entry.paintSubscription(); entry.paintSubscription = null; }
  }

  // One Electron frame subscription per page, shared by everyone who needs
  // the page's frames (the separate window, an agent driving it). Whole
  // frames are copied out only while a subscriber wants them (the separate
  // window); otherwise only each frame's damaged region, which for a page
  // that is not changing is next to nothing.
  addFrameSubscriber(entry, callback, { full = true } = {}) {
    const wc = this.liveContents(entry);
    if (!wc || wc.isDestroyed?.() || typeof wc.beginFrameSubscription !== 'function') return () => {};
    if (entry.frameSubscribers?.contents !== wc) entry.frameSubscribers = { contents: wc, callbacks: new Map(), active: false, full: false };
    const group = entry.frameSubscribers;
    const sync = () => {
      const wantFull = Array.from(group.callbacks.values()).some(Boolean);
      if (!group.callbacks.size) {
        if (!group.active) return;
        group.active = false;
        try { if (!wc.isDestroyed()) wc.endFrameSubscription(); } catch { /* already ended */ }
        return;
      }
      if (group.active && group.full === wantFull) return;
      try {
        if (group.active && !wc.isDestroyed()) wc.endFrameSubscription();
        wc.beginFrameSubscription(!wantFull, (image, dirty) => {
          for (const each of Array.from(group.callbacks.keys())) { try { each(image, dirty); } catch { /* one consumer cannot stop the others */ } }
        });
        group.active = true;
        group.full = wantFull;
      } catch { group.active = false; /* no frames for this page */ }
    };
    group.callbacks.set(callback, full);
    sync();
    return () => {
      group.callbacks.delete(callback);
      sync();
    };
  }

  setFocusEmulation(entry, enabled) {
    const wc = this.liveContents(entry);
    entry.focusEmulated = enabled;
    if (!wc || wc.isDestroyed?.() || !wc.debugger?.isAttached?.()) { entry.focusChange = null; return; }
    const change = Promise.resolve(wc.debugger.sendCommand('Emulation.setFocusEmulationEnabled', { enabled }))
      .then(() => undefined, () => undefined)
      .finally(() => { if (entry.focusChange === change) entry.focusChange = null; });
    entry.focusChange = change;
  }

  // Before a navigation: give owned iframe setup that is already running a
  // bounded moment to commit, then abandon the rest and navigate anyway. Only
  // tasks present now are awaited, so an ad slot that keeps attaching new
  // frames cannot hold navigation open, and frame setup never refuses it.
  async waitForOwnedTargetTasks(entry, budgetMs = this.navigationTargetSetupWaitMs ?? NAVIGATION_TARGET_SETUP_WAIT_MS) {
    this.assertCurrentEntry(entry);
    const waiting = Array.from(entry.pendingTargetTasks || []).filter((task) => !task.settled && !task.cancelled);
    if (!waiting.length) return { waited: 0, abandoned: 0 };
    const startedAt = Date.now();
    const bounded = (promises, ms) => {
      let timer;
      return Promise.race([
        Promise.allSettled(promises),
        new Promise((resolve) => { timer = setTimeout(resolve, ms); }),
      ]).finally(() => clearTimeout(timer));
    };
    await bounded(waiting.map((task) => task.promise), budgetMs);
    this.assertCurrentEntry(entry);
    const stuck = waiting.filter((task) => !task.settled);
    for (const task of stuck) this.cancelOwnedTargetTask(entry, task, 'setup_abandoned_navigation');
    if (stuck.length) {
      this.lifecycle(entry, 'owned-frame-setup-abandoned-for-navigation', {
        abandoned: stuck.length, waitedMs: Date.now() - startedAt,
      });
      // Cancellation settles each task on its next turn without touching the
      // page; the bound only guards against a future unguarded await.
      await bounded(stuck.map((task) => task.promise), ABANDONED_TARGET_SETTLE_MS);
      this.assertCurrentEntry(entry);
    }
    return { waited: Date.now() - startedAt, abandoned: stuck.length };
  }

  // loadURL() over a page that is still loading rejects the NEW load with the
  // replaced page's main-frame ERR_ABORTED. Stop the old load first and let
  // that failure (or the stop) be reported before loading. Bounded: a subframe
  // whose request never answers may never report a stop.
  async stopLoadingBeforeNavigation(entry) {
    const wc = entry.view.webContents;
    if (typeof wc.isLoading !== 'function' || !wc.isLoading()) return;
    await new Promise((resolve) => {
      let timer = null;
      const onFail = (_event, _code, _description, _url, isMainFrame) => { if (isMainFrame !== false) done(); };
      const done = () => {
        clearTimeout(timer);
        wc.removeListener('did-stop-loading', done);
        wc.removeListener('did-fail-load', onFail);
        resolve();
      };
      timer = setTimeout(done, 500);
      wc.on('did-stop-loading', done);
      wc.on('did-fail-load', onFail);
      try { wc.stop(); } catch { done(); }
    });
    this.assertCurrentEntry(entry);
  }

  // Electron's loadURL() resolves on the main frame's load event, which a
  // subframe that never finishes loading (a hung ad request) withholds
  // forever. Wait for the load, but once the main document is ready give it
  // only a short grace; a main document that never commits is reported.
  async loadNavigationTarget(entry, target) {
    const wc = entry.view.webContents;
    const graceMs = this.navigationLoadGraceMs ?? NAVIGATION_LOAD_GRACE_MS;
    const commitTimeoutMs = this.navigationCommitTimeoutMs ?? NAVIGATION_COMMIT_TIMEOUT_MS;
    let committed = false;
    let settle;
    const outcome = new Promise((resolve, reject) => { settle = { resolve, reject }; });
    const timers = [];
    const onNavigate = () => { committed = true; };
    const onDomReady = () => {
      if (!committed) return; // a late DOMContentLoaded of the document being replaced
      timers.push(setTimeout(() => settle.resolve('load_pending'), graceMs));
    };
    wc.on('did-navigate', onNavigate);
    wc.on('dom-ready', onDomReady);
    timers.push(setTimeout(() => {
      if (committed) settle.resolve('load_pending');
      else settle.reject(new Error(`browser navigation did not commit within ${Math.round(commitTimeoutMs / 1000)}s; the page is still loading in the background. Take a snapshot later to see whether it arrived, or navigate again to retry`));
    }, commitTimeoutMs));
    let load;
    try {
      load = Promise.resolve(wc.loadURL(target));
      load.then(() => settle.resolve('loaded'), (error) => settle.reject(error));
      const result = await outcome;
      if (result !== 'loaded') {
        this.lifecycle(entry, 'user-navigation-load-pending', { committed });
      }
      return result;
    } finally {
      for (const timer of timers) clearTimeout(timer);
      wc.removeListener('did-navigate', onNavigate);
      wc.removeListener('dom-ready', onDomReady);
      // A load that finishes, fails or is replaced later is reported through
      // the page events; it must not surface as an unhandled rejection.
      if (load) load.catch(() => {});
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

  async setViewportSerialized(entry, viewport, present = true) {
    this.assertCurrentEntry(entry);
    const next = validateViewport(viewport.width, viewport.height);
    if (entry.viewport.width === next.width && entry.viewport.height === next.height && entry.viewport.deviceScaleFactor === next.deviceScaleFactor) {
      const effective = await this.readVerifiedViewport(entry, next, this.attachedView === entry.view ? entry.presentationScale : 1);
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
    if (present && this.attachedView === entry.view) await this.attach(entry);
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
    if (this.isHosted(entry)) throw new Error('a hosted page has no separate capture surface');
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
    this.applyRadius(entry, 0);
    if (entry.initialDocumentReady) {
      await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
      this.assertCurrentEntry(entry);
    }
    entry.visible = preserveVisible === true;
  }

  // A cross-site load swaps the page's renderer, and for a moment it refuses
  // device emulation (or reports metrics that do not match yet). Showing the
  // pane never waits on that: the view goes on screen at its fitted bounds and
  // the viewport is applied again once the page stops loading. Before, the
  // throw left the view parked in its hidden host and the pane showed only its
  // white placeholder until something else happened to re-attach it.
  deferViewport(entry, viewport) {
    entry.deferredViewport = viewport || entry.deferredViewport || { ...entry.viewport };
  }

  retryDeferredViewport(entry) {
    if (!entry.deferredViewport || entry.closed) return;
    void this.serializePresentation(async () => {
      const viewport = entry.deferredViewport;
      if (!viewport || entry.closed || this.entries.get(entry.chatId) !== entry) return;
      entry.deferredViewport = null;
      try {
        await this.setViewportSerialized(entry, viewport, true);
        if (this.attachedView === entry.view) await this.attach(entry);
      } catch {
        if (!entry.closed) entry.deferredViewport = viewport;
      }
    }).catch(() => {});
  }

  async attach(entry) {
    this.assertCurrentEntry(entry);
    const epoch = this.presentationEpoch;
    if (this.isHosted(entry)) {
      // The renderer places and scales the page itself; the page keeps its
      // logical viewport at 1:1.
      entry.presentationScale = 1;
      this.pinGuestZoom(entry);
      if (!this.metricsApplied(entry, entry.viewport, 1)) {
        try {
          entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, 1);
        } catch {
          this.deferViewport(entry);
        }
        this.assertCurrentEntry(entry);
      }
      if (epoch !== this.presentationEpoch) return;
      for (const other of this.entries.values()) if (other !== entry) other.visible = false;
      entry.visible = true;
      return;
    }
    // The capture host keeps a background page at its logical viewport size.
    // Prepare the fitted bounds and metrics while it is still hidden: adding
    // that full-size view to the visible window first paints over the chat.
    const fitted = entry.presentationBounds ? presentationFor(entry.viewport, entry.presentationBounds) : null;
    if (fitted) {
      entry.presentationScale = fitted.scale || 1;
      if (!this.metricsApplied(entry, entry.viewport, entry.presentationScale)) {
        try {
          entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, entry.presentationScale);
        } catch {
          this.deferViewport(entry);
        }
        this.assertCurrentEntry(entry);
      }
    }
    if (epoch !== this.presentationEpoch) return;
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
            if (epoch !== this.presentationEpoch) return;
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
      if (fitted) entry.view.setBounds(presentationFor(entry.viewport, entry.presentationBounds).bounds);
      this.applyRadius(entry, entry.presentationRadius);
      this.win.contentView.addChildView(entry.view);
      this.attachedView = entry.view;
    } else if (fitted) {
      entry.view.setBounds(presentationFor(entry.viewport, entry.presentationBounds).bounds);
      this.applyRadius(entry, entry.presentationRadius);
    }
    entry.visible = true;
  }

  activate(options) {
    const epoch = this.presentationEpoch;
    return this.serializePresentation(() => this.activateSerialized({ ...(options || {}), epoch }));
  }

  async activateSerialized({ chatId, conversationId, bounds, viewport, url, radius, epoch }) {
    const id = safeChatId(chatId);
    if (epoch !== undefined && epoch !== this.presentationEpoch) {
      const stale = this.entries.get(id);
      return stale ? this.publish(stale) : null;
    }
    // Adopt any background entry the agent opened for this conversation (keyed by
    // its conversation id) so the user's visible view and the agent's browser are
    // ONE view, never a duplicate.
    if (conversationId) this.adoptBackground(id, String(conversationId));
    const entry = this.get(id);
    // Showing the page in the app takes it back from its separate window.
    this.releasePipWindow(entry);
    if (conversationId) entry.conversationId = String(conversationId);
    this.activeId = entry.chatId;
    entry.presentationRadius = safeRadius(radius);
    entry.presentationBounds = safeBounds(this.hostBounds(bounds), this.win.getContentSize());
    entry.bounds = entry.presentationBounds;
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    if (viewport) {
      validateViewport(viewport.width, viewport.height);
      try { await this.setViewportSerialized(entry, viewport, false); } catch { this.deferViewport(entry, viewport); }
    }
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

  resize(chatId, bounds, viewport, radius) {
    const epoch = this.presentationEpoch;
    return this.serializePresentation(() => this.resizeSerialized(chatId, bounds, viewport, radius, epoch));
  }

  async resizeSerialized(chatId, bounds, viewport, radius, epoch) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry || this.activeId !== id || (epoch !== undefined && epoch !== this.presentationEpoch)) return false;
    if (entry.rescaling) await entry.rescaling;
    if (radius !== undefined) entry.presentationRadius = safeRadius(radius);
    entry.presentationBounds = safeBounds(this.hostBounds(bounds), this.win.getContentSize());
    entry.bounds = entry.presentationBounds;
    if (viewport) await this.setViewportSerialized(entry, viewport, false);
    if (this.attachedView === entry.view || (this.isHosted(entry) && entry.visible)) await this.attach(entry);
    return true;
  }

  // A drag or a resize of the floating frame, one call per animation frame.
  // The page view itself follows the pointer, placed here and now rather than
  // through resize (an awaited round trip queued behind every other
  // presentation step, which left the page trailing its frame). A move only
  // sets the view's bounds; a new size sets them first and then the page's
  // scale, so the page never paints past its bounds. Scale changes are
  // coalesced: one is in flight at a time and the latest one wins.
  place(chatId, bounds) {
    const entry = this.entries.get(safeChatId(chatId));
    // A hosted page moves with its frame in the renderer itself.
    if (this.hostMode === 'webview' && (!entry || this.isHosted(entry))) return true;
    if (!entry || this.attachedView !== entry.view || this.activeId !== entry.chatId || !entry.presentationBounds) return false;
    const next = safeBounds(this.hostBounds(bounds), this.win.getContentSize());
    entry.presentationBounds = next;
    entry.bounds = next;
    const fitted = presentationFor(entry.viewport, next);
    entry.view.setBounds(fitted.bounds);
    if (fitted.scale && !this.metricsApplied(entry, entry.viewport, fitted.scale)) {
      entry.presentationScale = fitted.scale;
      this.rescale(entry);
    }
    return true;
  }

  rescale(entry) {
    if (entry.rescaling) {
      entry.rescaleOwed = true;
      return;
    }
    entry.rescaling = (async () => {
      try {
        do {
          entry.rescaleOwed = false;
          const scale = entry.presentationScale;
          try {
            await this.applyDeviceEmulation(entry, entry.viewport, scale);
          } catch {
            this.deferViewport(entry);
          }
        } while (entry.rescaleOwed && !entry.closed && this.isCurrentEntry(entry));
      } finally {
        entry.rescaling = null;
      }
    })();
  }

  // The page in its own small window that stays above every app (approved
  // mock browser-pip-v1 · C, 2026-10-13, after t3code's desktop pop-out). The
  // same view moves, so the agent keeps driving the same page; it keeps its
  // logical viewport and is fitted into the window. Closing the window brings
  // the page back to Workass (the renderer floats it again).
  popOut(chatId) {
    return this.serializePresentation(() => this.popOutSerialized(chatId));
  }

  async popOutSerialized(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry) throw new Error('this chat has no browser to pop out');
    this.assertCurrentEntry(entry);
    if (entry.pipWindow && !entry.pipWindow.isDestroyed()) {
      try { entry.pipWindow.focus(); } catch { /* window is closing */ }
      return this.publish(entry);
    }
    if (!this.BrowserWindow) throw new Error('a separate browser window is unavailable in this shell');
    if (this.isHosted(entry)) return this.popOutHosted(entry);
    if (this.attachedView === entry.view) {
      try { this.win.contentView.removeChildView(entry.view); } catch { /* window is already closing */ }
      this.attachedView = null;
      if (this.activeId === entry.chatId) this.activeId = null;
    }
    if (entry.captureHostAttached && entry.captureHost && !entry.captureHost.isDestroyed()) {
      entry.captureHost.contentView.removeChildView(entry.view);
      entry.captureHostAttached = false;
    }
    const aspect = entry.viewport.height / Math.max(1, entry.viewport.width);
    const width = 420;
    const height = Math.max(200, Math.min(640, Math.round(width * aspect)));
    let host = 'Workass';
    try { host = `Workass · ${new URL(entry.url || DEFAULT_URL).host || 'navegador'}`; } catch { /* about:blank */ }
    const win = new this.BrowserWindow({
      width, height, minWidth: 240, minHeight: 160, show: false, title: host,
      alwaysOnTop: true, fullscreenable: true, backgroundColor: '#1b1a18',
      webPreferences: { partition: this.partition, nodeIntegration: false, contextIsolation: true, sandbox: true },
    });
    try { win.setAlwaysOnTop(true, 'floating'); } catch { /* platform without levels */ }
    entry.pipWindow = win;
    const fit = async () => {
      if (entry.pipWindow !== win || win.isDestroyed() || !this.isCurrentEntry(entry)) return;
      const [contentWidth, contentHeight] = win.getContentSize();
      const fitted = presentationFor(entry.viewport, { x: 0, y: 0, width: contentWidth, height: contentHeight });
      entry.view.setBounds(fitted.bounds);
      entry.presentationScale = fitted.scale || 1;
      try {
        entry.effectiveViewport = await this.applyAndReadDeviceEmulation(entry, entry.viewport, entry.presentationScale);
      } catch {
        this.deferViewport(entry);
      }
    };
    win.on('resize', () => { void this.serializePresentation(fit); });
    // The view outlives this window: detach it before the window goes, then
    // hand it back to its hidden host so the page keeps running.
    win.on('close', () => {
      try { win.contentView.removeChildView(entry.view); } catch { /* already detached */ }
    });
    win.on('closed', () => {
      if (entry.pipWindow !== win) return;
      entry.pipWindow = null;
      entry.visible = false;
      void this.serializePresentation(async () => {
        if (this.isCurrentEntry(entry) && entry.captureHost && !entry.captureHost.isDestroyed()) {
          await this.attachToCaptureHost(entry, false);
        }
      }).finally(() => { if (this.isCurrentEntry(entry)) this.publish(entry); });
    });
    this.applyRadius(entry, 0);
    win.contentView.addChildView(entry.view);
    await fit();
    try { win.show(); } catch { /* headless fake */ }
    entry.visible = true;
    return this.publish(entry);
  }

  // The webview host: the page stays in the app (out of sight, still
  // painting) and the window shows it live (browser-pip.js).
  async popOutHosted(entry) {
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    const guest = this.liveContents(entry);
    if (!guest || guest.isDestroyed?.()) throw new Error('the browser page is not up yet; try again in a moment');
    let title = 'Workass';
    try { title = `Workass · ${new URL(entry.url || DEFAULT_URL).host || 'navegador'}`; } catch { /* about:blank */ }
    const pip = openGuestPip({
      BrowserWindow: this.BrowserWindow,
      guest,
      subscribe: (callback) => this.addFrameSubscriber(entry, callback),
      embedder: this.win && this.win.webContents,
      title,
      viewport: { ...entry.viewport },
      preload: this.pipPreload,
      sendInput: (event) => this.executeCDP({ tabId: this.tabIdOf(entry) }, 'Input.dispatchMouseEvent', event),
      onClosed: () => {
        if (entry.pip !== pip) return;
        entry.pip = null;
        entry.pipWindow = null;
        if (this.isCurrentEntry(entry)) { this.publish(entry); this.publishHosts(); }
      },
    });
    entry.pip = pip;
    entry.pipWindow = pip.window;
    entry.visible = false;
    if (this.activeId === entry.chatId) this.activeId = null;
    this.publishHosts();
    return this.publish(entry);
  }

  popIn(chatId) {
    const entry = this.entries.get(safeChatId(chatId));
    if (!entry || !entry.pipWindow || entry.pipWindow.isDestroyed()) return false;
    try { entry.pipWindow.close(); } catch { /* already closing */ }
    return true;
  }

  // Taking the page back for the app: forget the separate window first, so its
  // closing never re-homes the view the app is about to show.
  releasePipWindow(entry) {
    if (entry.pip) {
      const pip = entry.pip;
      entry.pip = null;
      entry.pipWindow = null;
      pip.close();
      this.publishHosts();
      return;
    }
    const win = entry.pipWindow;
    if (!win) return;
    entry.pipWindow = null;
    try { win.contentView.removeChildView(entry.view); } catch { /* already detached */ }
    try { if (!win.isDestroyed()) win.destroy(); } catch { /* already gone */ }
  }

  hide(chatId) {
    return this.serializePresentation(() => this.hideSerialized(chatId));
  }

  async hideSerialized(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (entry && this.isHosted(entry)) {
      entry.visible = false;
      if (this.activeId === id) this.activeId = null;
      return true;
    }
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
        await this.stopLoadingBeforeNavigation(entry);
        entry.loading = true;
        entry.url = target;
        entry.error = null;
        this.publish(entry);
        this.artifactNavigations.set(wc, target);
        this.lifecycle(entry, 'before-user-navigation');
        await this.loadNavigationTarget(entry, target);
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

  // A navigation can return while some frames are still loading (a hung ad
  // request never lets the page finish). Say so in the result itself.
  navigationResult(entry) {
    const tab = this.tabInfo(entry);
    if (tab.loading === true) {
      tab.loadingNote = entry.mainDocumentReady
        ? 'The page is still loading: its document is ready, but some frames have not finished. Snapshot or screenshot it now, or navigate again to retry the load.'
        : 'The page is still loading. Snapshot it in a moment, or navigate again to retry the load.';
    }
    return tab;
  }

  // The first snapshot of a page that is still attaching or loading frames
  // can see them change mid-observation. Retry once before reporting it.
  async browserSnapshotSettled(entry, extra = {}) {
    const changed = (error) => /changed during snapshot/.test(String(error && error.message));
    // Both attempts share one child-frame budget.
    const options = { ...extra, childFramesDeadline: Date.now() + (this.snapshotChildFramesBudgetMs ?? SNAPSHOT_CHILD_FRAMES_BUDGET_MS) };
    try {
      return await this.browserSnapshot(entry, options);
    } catch (error) {
      if (!changed(error)) throw error;
    }
    await new Promise((resolve) => setTimeout(resolve, this.snapshotRetryDelayMs ?? 250));
    this.assertCurrentEntry(entry);
    try {
      return await this.browserSnapshot(entry, options);
    } catch (error) {
      if (!changed(error)) throw error;
      throw new Error('browser page kept changing while it was observed (frames are still loading or navigating); take the snapshot again in a moment');
    }
  }

  // ---------------------------------------------------------------------------
  // The page outline (browser-outline.js) and its short refs.

  // Refs stay the same for the same element while its document lives, so an
  // agent can keep using e12 across actions; a new document starts over.
  // The top document's identity: its frame and loader. A navigation that
  // never commits (a download, a 204) leaves the same document.
  documentKey(entry) {
    const root = this.frameForSession(entry, null);
    return root && root.loaderId ? `${root.id}:${root.loaderId}` : `g${entry.documentGeneration}`;
  }

  refAllocator(entry) {
    const documentKey = this.documentKey(entry);
    if (!entry.refMap || entry.refMap.documentKey !== documentKey) {
      entry.refMap = { documentKey, byKey: new Map(), byRef: new Map(), next: 1 };
    }
    const map = entry.refMap;
    return (frameKey, backendNodeId, role, name) => {
      const key = `${frameKey}|${backendNodeId}`;
      let ref = map.byKey.get(key);
      if (!ref) {
        if (map.byRef.size >= OUTLINE_REF_LIMIT) return null;
        ref = `e${map.next++}`;
        map.byKey.set(key, ref);
        const separator = frameKey.lastIndexOf(':');
        map.byRef.set(ref, { frameId: frameKey.slice(0, separator), loaderId: frameKey.slice(separator + 1), backendNodeId: Number(backendNodeId), role, name: String(name || '').slice(0, 120) });
      }
      return ref;
    };
  }

  // A short ref (e12) as an action target, checked against the frame's
  // current document.
  shortRefTarget(entry, ref, snapshotId = null) {
    const map = entry.refMap;
    const info = map && map.documentKey === this.documentKey(entry) ? map.byRef.get(ref) : null;
    if (!info) return { status: 'stale', reason: `${ref} is not on the current page; take a new outline` };
    if (snapshotId && entry.outlines && !entry.outlines.has(snapshotId) && !entry.observations.has(snapshotId)) {
      return { status: 'stale', reason: 'that outline is from an earlier page; take a new outline' };
    }
    const frame = entry.frames.get(info.frameId);
    if (!frame || frame.detached || frame.loaderId !== info.loaderId) {
      return { status: 'stale', reason: `the frame of ${ref} was replaced; take a new outline` };
    }
    return {
      path: null, nodeKey: null, selector: '', backendDOMNodeId: info.backendNodeId,
      frameId: frame.id,
      frameGeneration: frame.generation, frameLoaderId: frame.loaderId, targetId: frame.targetId,
      sessionId: frame.sessionId || null, contextId: frame.contextId ?? null, snapshotId, shortRef: ref,
    };
  }

  // Every owned frame's accessibility tree, the top frame first; a child
  // frame knows the <iframe> element that holds it.
  async collectOutlineFrames(entry, budgetMs = OUTLINE_FRAMES_BUDGET_MS) {
    await this.ensureCDP(entry);
    await this.refreshFrameTree(entry);
    const deadline = Date.now() + budgetMs;
    const root = this.frameForSession(entry, null);
    const live = Array.from(entry.frames.values()).filter((frame) => !frame.detached);
    const order = (root ? [root, ...live.filter((frame) => frame !== root)] : live).slice(0, OUTLINE_FRAME_LIMIT);
    const remaining = () => Math.max(1, deadline - Date.now());
    const keyOf = (frame) => `${frame.id}:${frame.loaderId}`;
    const read = async (frame) => {
      const parent = frame.parentFrameId ? entry.frames.get(frame.parentFrameId) : null;
      const record = { frameKey: keyOf(frame), frameId: frame.id, url: frame.url || '', ownedBy: null, nodes: null, unavailable: null };
      if (parent && !parent.detached) {
        try {
          const owner = await this.ownedSessionCommand(entry, 'DOM.getFrameOwner', { frameId: frame.id }, parent.sessionId || null, remaining());
          if (Number(owner?.backendNodeId) > 0) record.ownedBy = { frameKey: keyOf(parent), backendNodeId: Number(owner.backendNodeId) };
        } catch { /* placed as an orphan below its page */ }
      }
      const reachable = frame === root || frame.sessionId || frame.contextId != null || frame.targetId === entry.rootTargetId;
      if (!reachable || (frame.unavailableReason && !frame.sessionId && frame.contextId == null && frame !== root)) {
        record.unavailable = String(frame.unavailableReason || 'this frame is not owned by the tab');
        return record;
      }
      if (Date.now() >= deadline) { record.unavailable = 'the page has more frames than one outline reads in time'; return record; }
      try {
        const tree = await this.ownedSessionCommand(entry, 'Accessibility.getFullAXTree', { frameId: frame.id }, frame.sessionId || null, remaining());
        record.nodes = Array.isArray(tree?.nodes) ? tree.nodes : [];
        record.secretBackendIds = await this.outlineSecretFields(entry, frame, record.nodes, remaining);
      } catch (error) {
        record.unavailable = /did not answer/u.test(String(error && error.message)) ? 'the frame did not answer in time' : 'the frame could not be read';
      }
      return record;
    };
    const frames = new Array(order.length);
    let next = 0;
    const worker = async () => {
      while (next < order.length) {
        const index = next++;
        frames[index] = await read(order[index]);
      }
    };
    await Promise.all(Array.from({ length: Math.min(OUTLINE_READ_CONCURRENCY, order.length) }, worker));
    return { frames: frames.filter(Boolean), framesTotal: live.length };
  }

  // Which valued fields of a frame are secret, by their element's type,
  // autocomplete, name and id (the rule json snapshots and read apply). A
  // field whose element cannot be described is treated as secret.
  async outlineSecretFields(entry, frame, nodes, remaining) {
    const secret = new Set();
    const candidates = [];
    for (const node of nodes) {
      const backend = Number(node && node.backendDOMNodeId);
      const value = node && node.value && node.value.value;
      if (!(backend > 0) || value == null || String(value) === '' || node.ignored === true) continue;
      candidates.push(backend);
      if (candidates.length >= OUTLINE_SECRET_CHECK_LIMIT) break;
    }
    let next = 0;
    const worker = async () => {
      while (next < candidates.length) {
        const backend = candidates[next++];
        try {
          const described = await this.ownedSessionCommand(entry, 'DOM.describeNode', { backendNodeId: backend, depth: 0 }, frame.sessionId || null, remaining());
          if (outlineSecretField(described && described.node && described.node.attributes)) secret.add(backend);
        } catch { secret.add(backend); }
      }
    };
    await Promise.all(Array.from({ length: Math.min(OUTLINE_READ_CONCURRENCY, candidates.length) }, worker));
    // Fields past the check limit are not shown with values at all.
    for (const node of nodes) {
      const backend = Number(node && node.backendDOMNodeId);
      if (backend > 0 && node.value && node.value.value != null && String(node.value.value) !== '' && !candidates.includes(backend)) secret.add(backend);
    }
    return secret;
  }

  async takeOutline(entry, { budgetMs, activity } = {}) {
    this.assertCurrentEntry(entry);
    if (entry.dialog) throw dialogBlockedError(entry);
    // The page's activity count before reading it: an action can reuse this
    // outline as its "before" only while the count has not moved.
    const seen = activity && activity.documentKey === this.documentKey(entry) ? activity : await readActivity(this, entry);
    const documentGeneration = entry.documentGeneration;
    const { frames, framesTotal } = await this.collectOutlineFrames(entry, budgetMs);
    const wc = this.liveContents(entry);
    const url = wc && !wc.isDestroyed?.() ? wc.getURL() : entry.url;
    const built = buildOutline(frames, { refFor: this.refAllocator(entry), title: entry.title || '', url: url || entry.url || DEFAULT_URL });
    const outline = {
      id: 'o' + crypto.randomBytes(6).toString('base64url'),
      documentKey: this.documentKey(entry),
      takenAt: Date.now(),
      lines: built.lines,
      truncated: built.truncated,
      documentGeneration,
      url: redactPageMessage(url || entry.url || DEFAULT_URL, 2048),
      title: redactPageMessage(entry.title || '', 1024),
      frames: { total: framesTotal, read: frames.filter((frame) => frame.nodes).length, unreadable: frames.filter((frame) => frame.unavailable).length },
      activityN: seen.n,
      activitySeq: seen.activitySeq,
    };
    entry.outlines = entry.outlines || new Map();
    entry.outlines.set(outline.id, outline);
    while (entry.outlines.size > OUTLINE_HISTORY) entry.outlines.delete(entry.outlines.keys().next().value);
    entry.lastOutline = outline;
    return outline;
  }

  outlinePage(outline, start, maxBytes) {
    const page = pageLines(outline.lines, start, maxBytes);
    return {
      outline: page.text,
      outline_id: outline.id,
      lines_total: outline.lines.length,
      lines_from: Math.max(0, start),
      lines_returned: page.text ? page.text.split('\n').length : 0,
      next_cursor: page.next == null ? null : `${outline.id}:${page.next}`,
      truncated: outline.truncated,
    };
  }

  async browserOutline(entry, params = {}) {
    const maxBytes = params.maxBytes == null ? OUTLINE_PAGE_BYTES : Number(params.maxBytes);
    if (!Number.isInteger(maxBytes) || maxBytes < 2048 || maxBytes > 60000) throw new Error('max_bytes must be an integer from 2048 to 60000');
    const tab = { id: this.tabIdOf(entry), url: redactPageMessage(entry.url || DEFAULT_URL, 2048), title: redactPageMessage(entry.title || '', 1024), loading: entry.loading === true };
    if (params.cursor != null) {
      const match = /^(o[\w-]+):(\d+)$/u.exec(String(params.cursor));
      const outline = match && entry.outlines ? entry.outlines.get(match[1]) : null;
      if (!outline) throw new Error('cursor is from an outline this tab no longer has; take a new outline');
      if (outline.documentKey !== this.documentKey(entry)) throw new Error('the page changed since that outline; take a new outline');
      return { format: 'outline', tab, ...this.outlinePage(outline, Number(match[2]), maxBytes), frames: outline.frames, ...this.pageNotes(entry) };
    }
    const since = params.sinceSnapshotId != null ? String(params.sinceSnapshotId) : null;
    const base = since && entry.outlines ? entry.outlines.get(since) : null;
    // An explicit snapshot always reads the page as it is now.
    const outline = await this.takeOutline(entry);
    if (since) {
      if (base && base.documentKey === outline.documentKey) {
        const changes = diffOutlines(base.lines, outline.lines, { maxBytes });
        return { format: 'outline', tab, outline_id: outline.id, since_snapshot_id: since, changes, lines_total: outline.lines.length, frames: outline.frames, ...this.pageNotes(entry) };
      }
      return { format: 'outline', tab, since_snapshot_id: since, since_applied: false, since_reason: base ? 'the page navigated since then' : 'that outline is unknown', ...this.outlinePage(outline, 0, maxBytes), frames: outline.frames, ...this.pageNotes(entry) };
    }
    return { format: 'outline', tab, ...this.outlinePage(outline, 0, maxBytes), frames: outline.frames, ...this.pageNotes(entry) };
  }

  // What every page result carries besides its content: an open dialog,
  // finished and running downloads, events since the last call.
  pageNotes(entry) {
    const notes = {};
    if (entry.dialog) notes.dialog = { ...entry.dialog.public };
    if (entry.loading) notes.loading = true;
    return notes;
  }

  tabInfo(entry) {
    const wc = this.liveContents(entry);
    const liveURL = wc && !wc.isDestroyed?.() ? wc.getURL() : '';
    return {
      id: this.tabIdOf(entry),
      ...this.publicState(entry),
      chatId: entry.chatId,
      conversationId: entry.conversationId || null,
      url: redactPageMessage(liveURL || entry.url || DEFAULT_URL),
      title: redactPageMessage(entry.title || ''),
      active: this.activeId === entry.chatId,
      viewport: { ...entry.viewport },
      effectiveViewport: entry.effectiveViewport ? { ...entry.effectiveViewport } : null,
      viewportGeneration: entry.viewportGeneration,
      documentGeneration: entry.documentGeneration,
      visible: entry.visible === true,
      supportedCaptureModes: ['viewport', 'full_page', 'clip'],
      ...(entry.extraTab ? { opened_by_tab_id: entry.openerTabId ?? null } : {}),
      ...(entry.dialog ? { dialog: { ...entry.dialog.public } } : {}),
      ...(entry.downloads && entry.downloads.length ? { downloads: entry.downloads.map((item) => ({ ...item })) } : {}),
      ...(entry.wedgedAt ? { stuck_since_ms: Date.now() - entry.wedgedAt } : {}),
    };
  }

  entryBelongsToChat(entry, chatId) {
    const owner = String(chatId || '').trim();
    return !owner || entry.chatId === owner || entry.conversationId === owner || (!!entry.ownerChatId && entry.ownerChatId === owner) ||
      (!!entry.ownerConversationId && entry.ownerConversationId === owner);
  }

  browserTabs(chatId = '') {
    return this.browserEntries()
      .filter((entry) => this.entryBelongsToChat(entry, chatId))
      .map((entry) => this.tabInfo(entry));
  }

  entryForTab(tabId) {
    const id = Number(tabId);
    if (!Number.isInteger(id) || id <= 0) throw new Error('invalid browser tab id');
    const entry = this.browserEntries().find((candidate) => this.tabIdOf(candidate) === id);
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
      if (key === tabId || key === '__workass-health__' || entry.extraTab) continue;
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
    const matched = this.primaryEntryFor(conversationId);
    if (matched) return matched;
    const entry = this.create(conversationId);
    entry.conversationId = conversationId;
    return entry;
  }

  // The chat's own page (not a tab one of its pages opened).
  primaryEntryFor(conversationId) {
    return this.browserEntries().find((entry) => !entry.extraTab && (entry.conversationId === conversationId || entry.chatId === conversationId)) || null;
  }

  // A link or popup an agent's page opened becomes a tab of the same chat,
  // in the background, that the agent lists and drives by its tab id.
  popupWebPreferences() {
    return { partition: this.partition, nodeIntegration: false, nodeIntegrationInSubFrames: false, contextIsolation: true, sandbox: true, navigateOnDragDrop: false };
  }

  popupTabAllowed(opener, url) {
    const owner = opener.extraTab ? (this.entries.get(opener.ownerChatId) || opener) : opener;
    const siblings = this.browserEntries().filter((entry) => entry.extraTab && entry.ownerChatId === owner.chatId);
    if (siblings.length < POPUP_TAB_LIMIT) return true;
    pushEvent(opener, { type: 'popup_blocked', url: redactPageMessage(url, 1024), reason: `this chat already has ${POPUP_TAB_LIMIT} extra tabs; close one with workass_browser_close` });
    return false;
  }

  // Electron's createWindow for a popup an agent-driven page opened: a native
  // view around the webContents Chromium made (opener and request intact),
  // registered as another tab of the chat. Chromium loads it.
  adoptPopup(opener, url, disposition, options = {}) {
    const owner = opener.extraTab ? (this.entries.get(opener.ownerChatId) || opener) : opener;
    const view = new this.WebContentsView({
      webPreferences: { ...(options.webPreferences || {}), ...this.popupWebPreferences() },
      ...(options.webContents ? { webContents: options.webContents } : {}),
    });
    this.extraTabSeq = (this.extraTabSeq || 0) + 1;
    const key = `${owner.chatId}#tab${this.extraTabSeq}`;
    const tab = this.newEntry(key, view);
    tab.tabId = Number(view.webContents.id);
    tab.url = url;
    tab.loading = true;
    tab.popup = true;
    tab.extraTab = true;
    tab.ownerChatId = owner.chatId;
    tab.ownerConversationId = owner.conversationId || owner.chatId;
    tab.openerTabId = this.tabIdOf(opener);
    tab.agentActiveAt = Date.now();
    try { view.setBackgroundColor('#ffffff'); } catch { /* older Electron */ }
    try { view.webContents.setUserAgent(this.userAgent); } catch { /* best effort */ }
    this.entries.set(key, tab);
    this.bind(tab);
    opener.pendingTabs = (opener.pendingTabs || 0) + 1;
    touchActivity(opener);
    let pending = true;
    const settled = () => { if (pending) { pending = false; opener.pendingTabs = Math.max(0, (opener.pendingTabs || 1) - 1); touchActivity(opener); } };
    const lifecycleToken = tab.lifecycleToken;
    tab.ready = this.initializePopup(tab, lifecycleToken).then(() => {
      if (!this.isCurrentEntry(tab, lifecycleToken)) { settled(); return false; }
      pushEvent(opener, {
        type: 'tab_opened', tab_id: this.tabIdOf(tab), url: redactPageMessage(url, 1024),
        how: disposition === 'new-window' ? 'window_open' : 'link_new_tab', opener: true,
      });
      settled();
      return true;
    }, (error) => {
      pushEvent(opener, { type: 'tab_failed', url: redactPageMessage(url, 1024), reason: redactPageMessage(String(error && error.message || error), 300) });
      settled();
      if (this.isCurrentEntry(tab, lifecycleToken)) { tab.error = redactPageMessage(error && error.message || error); this.publish(tab); }
      return false;
    });
    return view.webContents;
  }

  // A popup's first document is Chromium's to load (with its opener and
  // request); it gets a capture surface, CDP and the chat's viewport.
  async initializePopup(tab, lifecycleToken) {
    await this.ensureCaptureHost(tab);
    this.assertCurrentEntry(tab, lifecycleToken);
    await this.attachToCaptureHost(tab, false);
    this.assertCurrentEntry(tab, lifecycleToken);
    tab.initialDocumentReady = true;
    if (!await this.ensureCDP(tab)) throw new Error('browser page debugger is unavailable');
    this.assertCurrentEntry(tab, lifecycleToken);
    tab.effectiveViewport = await this.applyAndReadDeviceEmulation(tab, tab.viewport, 1);
    tab.viewportGeneration = 1;
    try { await tab.view.webContents.debugger.sendCommand('Runtime.enable'); } catch { /* basic control remains */ }
    return true;
  }

  // workass_browser_close: a tab a page opened goes away; the chat's own page
  // is reset to a fresh blank page in the same signed-in profile.
  async closeForAgent(entry) {
    const tabId = this.tabIdOf(entry);
    if (entry.extraTab) {
      this.close(entry.chatId);
      return { ok: true, status: 'closed', tab_id: tabId };
    }
    const chatId = entry.chatId;
    const conversationId = entry.conversationId;
    // The page a person is looking at is replaced in place: same pane, same
    // bounds and corners, so the pane is never left without a page.
    const shown = this.activeId === chatId && entry.visible === true && entry.presentationBounds
      ? { bounds: { ...entry.presentationBounds }, radius: entry.presentationRadius, viewport: { ...entry.viewport } } : null;
    this.close(chatId);
    const fresh = this.create(chatId);
    fresh.conversationId = conversationId;
    if (fresh.ready && await fresh.ready !== true) throw new Error(fresh.error || 'the new browser page did not come up');
    if (shown) await this.serializePresentation(() => this.presentAgain(fresh, shown));
    return { ok: true, status: 'reset', closed_tab_id: tabId, tab: this.tabInfo(fresh) };
  }

  // Puts a page back where another one was shown (bounds already in window
  // coordinates).
  async presentAgain(entry, { bounds, radius, viewport }) {
    if (!this.isCurrentEntry(entry)) return null;
    this.activeId = entry.chatId;
    entry.presentationRadius = radius;
    entry.presentationBounds = bounds;
    entry.bounds = bounds;
    if (viewport && (viewport.width !== entry.viewport.width || viewport.height !== entry.viewport.height)) {
      try { await this.setViewportSerialized(entry, viewport, false); } catch { this.deferViewport(entry, viewport); }
    }
    await this.attach(entry);
    if (this.isHosted(entry)) entry.visible = true;
    return this.publish(entry);
  }

  async controlEntry(params = {}) {
    const conversationId = String(params.chatId || '').trim();
    if (params.tabId != null) return this.ownedEntryForTab(params.tabId, conversationId);
    if (conversationId) {
      const matched = this.primaryEntryFor(conversationId);
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
    if (commandName.startsWith('Input.')) return this.boundedInput(entry, debug, commandName, commandParams || {}, sessionId);
    return debug.sendCommand(commandName, commandParams || {}, sessionId || undefined);
  }

  // Input commands never wait without bound: a page that does not take one
  // (a hung renderer, a drag handed elsewhere) fails the call, and a press it
  // left down is released by the operation that made it (serialize) or on
  // blur.
  async boundedInput(entry, debug, method, params, sessionId) {
    const pointer = method === 'Input.dispatchMouseEvent' ? String(params.type || '') : '';
    if (pointer === 'mousePressed') {
      entry.pointerDown = { sessionId: sessionId || null, x: Number(params.x) || 0, y: Number(params.y) || 0, button: params.button || 'left' };
    }
    let timer = null;
    const limitMs = this.inputCommandTimeoutMs ?? INPUT_COMMAND_TIMEOUT_MS;
    try {
      const result = await Promise.race([
        Promise.resolve(debug.sendCommand(method, params, sessionId || undefined)),
        new Promise((_resolve, reject) => {
          timer = setTimeout(() => reject(new Error(`the page did not take ${method.slice('Input.'.length)} within ${Math.round(limitMs / 1000)}s`)), limitMs);
        }),
      ]);
      if (pointer === 'mouseReleased') entry.pointerDown = null;
      return result;
    } catch (error) {
      if (/did not take/u.test(String(error && error.message))) {
        entry.inputTimedOut = true;
        this.lifecycle(entry, 'input-command-timeout', { method, type: pointer || null });
      }
      throw error;
    } finally {
      clearTimeout(timer);
    }
  }

  // Puts the page's input back to rest: a press left down is released where
  // it was made, and a drag in progress (an intercepted HTML drag) is
  // cancelled. Each step is bounded and its failure ignored.
  async releaseStuckInput(entry) {
    const down = entry.pointerDown;
    entry.pointerDown = null;
    entry.inputTimedOut = false;
    const wc = this.liveContents(entry);
    const debug = wc && wc.debugger;
    if (!debug || debug.isAttached?.() !== true) return;
    const send = (method, params, sessionId) => Promise.race([
      Promise.resolve().then(() => debug.sendCommand(method, params, sessionId || undefined)).catch(() => {}),
      new Promise((resolve) => setTimeout(resolve, INPUT_RELEASE_TIMEOUT_MS)),
    ]);
    if (down) await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: down.x, y: down.y, button: down.button, buttons: 0, clickCount: 1 }, down.sessionId);
    await send('Input.cancelDragging', {}, down ? down.sessionId : null);
    this.lifecycle(entry, 'input-released', { pressed: !!down });
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
        return this.evaluateAgentWorld(entry, expression, true);
      }
      throw new Error('browser frame execution context is unavailable');
    }
    const target = frame.sessionId ? entry.targetSessions.get(frame.sessionId) : null;
    if (frame.sessionId && (!target || target.targetId !== frame.targetId ||
        (target.frameId !== frame.id && !this.isDescendantFrame(entry, frame.id, target.frameId)))) {
      throw new Error('browser frame target is no longer owned');
    }
    // A frame that shares its owned target's renderer (an about:blank,
    // srcdoc or same-site child inside a cross-site frame) is evaluated in its
    // own execution context only: without one the command would run in the
    // target's own frame instead.
    if (target && target.frameId !== frame.id && frame.contextId == null) {
      throw new Error('browser frame execution context is unavailable');
    }
    const params = {
      expression: String(expression),
      returnByValue: options.returnByValue !== false,
      awaitPromise: options.awaitPromise !== false,
    };
    return this.withAgentContext(entry, frame, async (contextId) => unwrapCDPValue(
      await this.ownedSessionCommand(entry, 'Runtime.evaluate', { ...params, contextId }, frame.sessionId || null, options.timeoutMs)));
  }

  // Text input for a frame goes to that frame's own owned target, so it
  // reaches the frame's widget directly instead of depending on which widget
  // the browser has focused at that moment. The top page (and a frame
  // sharing its renderer) is addressed through the tab.
  inputTargetForFrame(entry, frameId) {
    const tabId = this.tabIdOf(entry);
    const frame = frameId != null ? entry.frames.get(String(frameId)) : null;
    const owned = frame && frame.sessionId ? entry.targetSessions.get(frame.sessionId) : null;
    if (!owned || owned.targetId !== frame.targetId) return { tabId };
    return { tabId, targetId: owned.targetId, sessionId: frame.sessionId };
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
    const resolved = await this.withAgentContext(entry, frame, (executionContextId) => this.ownedSessionCommand(entry, 'DOM.resolveNode', {
      backendNodeId: target.backendDOMNodeId, executionContextId,
    }, sessionId));
    const objectId = resolved?.object?.objectId;
    if (!objectId) throw new Error('observed node is stale; refresh the snapshot');
    try {
      const installed = await this.ownedSessionCommand(entry, 'Runtime.callFunctionOn', {
        objectId,
        functionDeclaration: "function(){ if(!this||!this.isConnected)return false; globalThis[Symbol.for('workass.browser.action-target')]=this; return true; }",
        returnByValue: true,
      }, sessionId);
      if (installed?.result?.value !== true) throw new Error('observed node is stale; refresh the snapshot');
    } finally {
      try { await this.ownedSessionCommand(entry, 'Runtime.releaseObject', { objectId }, sessionId); } catch { /* context owns the remote object */ }
    }
    return {
      clear: () => this.evaluateFrame(entry, target.frameId, "delete globalThis[Symbol.for('workass.browser.action-target')]"),
    };
  }

  async frameOwnerIsDeepActive(entry, parent, child) {
    if (!parent || !child || child.parentFrameId !== parent.id || parent.detached || child.detached) return false;
    if (child.sessionId && child.sessionId !== parent.sessionId) {
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
      const resolved = await this.withAgentContext(entry, parent, (executionContextId) => debug.sendCommand('DOM.resolveNode', {
        backendNodeId, executionContextId,
      }, sessionId));
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
    const editor = await this.editorPageCall(entry, frameId, ACTIVE_ELEMENT_EXPRESSION, 'value');
    if (editor && editor.editor && Number.isFinite(editor.length)) return { found: true, length: editor.length };
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
    const editor = await this.editorPageCall(entry, frameId, ACTIVE_ELEMENT_EXPRESSION, 'selectAll');
    if (editor && editor.editor && editor.done === true) return { found: true, selectionVerified: true, strategy: editor.strategy };
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
    const editor = await this.editorPageCall(entry, frameId, ACTIVE_ELEMENT_EXPRESSION, 'deleteSelection');
    if (editor && editor.editor && editor.done === true) return { found: true, deletionVerified: true, strategy: editor.strategy };
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
    // A code editor (Monaco, CodeMirror) is read, focused and selected through
    // its own API, which only the page world has.
    let pageEditor = await this.editorPageCall(entry, locator.frameId ?? null, selector, 'inspect');
    if (pageEditor && !pageEditor.editor) pageEditor = null;
    if (pageEditor && pageEditor.readOnly !== true) await this.editorPageCall(entry, locator.frameId ?? null, selector, 'focus');

    // Text fields are typed into as a person types: focus, select what they
    // hold, then the text arrives as trusted input (the page's own input
    // events, isTrusted true, in any frame). Fields that take no typed text
    // (date, time and the like) and any field whose value does not come out
    // exactly as typed (a length limit, an input mask) keep the deterministic
    // value-set path, through the native prototype setter so React/Vue value
    // trackers observe its input event. Hidden editor textareas and
    // contenteditable surfaces are driven like a user too: focus, select all,
    // then insert native text.
    const valueSetter = `
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
        }, 0));`;
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
      // The code editor's own state, read through its API in the page world
      // before this script (editorPageCall); null when there is none.
      const pageEditor = ${JSON.stringify(pageEditor)};
      const nativeEditor = !!editorRoot || el.isContentEditable || (!formControl && el.getAttribute('role') === 'textbox');
      el.scrollIntoView({ block: 'center', inline: 'center' });
      if (pageEditor) { /* focused through the editor's own API */ }
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
      const editorReadOnly = pageEditor?.readOnly === true;
      if (editorReadOnly || (!editorRoot && (el.disabled || el.readOnly))) {
        return { found: true, editable: false, focused: focused(), reason: 'disabled or read-only' };
      }
      if (!formControl && !nativeEditor) {
        return { found: true, editable: false, focused: focused(), reason: 'target is not editable' };
      }
      if (!nativeEditor) {
        const typeable = textarea || ['', 'text', 'search', 'email', 'url', 'tel', 'password', 'number'].includes(String(el.type || '').toLowerCase());
        if (typeable && focused()) {
          const before = String(el.value ?? '');
          try { el.select(); } catch { /* the typed text is verified below either way */ }
          return { found: true, editable: true, strategy: 'keyboard', focused: true, before };
        }
        ${valueSetter}
      }

      const readModel = () => (pageEditor && typeof pageEditor.value === 'string' ? pageEditor.value : null);
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
        if (!pageEditor && el.isContentEditable) {
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
    if (pageEditor && prepared.strategy === 'native' && prepared.selectionVerified !== true) {
      const selected = await this.editorPageCall(entry, locator.frameId ?? null, selector, 'selectAll');
      if (selected && selected.done === true) { prepared.selectionVerified = true; prepared.selectionStrategy = selected.strategy; }
    }

    if (prepared.strategy === 'keyboard') {
      const inputTarget = this.inputTargetForFrame(entry, locator.frameId ?? null);
      let typed = null;
      try {
        if (text) {
          await this.executeCDP(inputTarget, 'Input.insertText', { text });
        } else {
          const key = cdpBrowserKey(parseBrowserKey('Backspace', this.platform));
          await this.executeCDP(inputTarget, 'Input.dispatchKeyEvent', { type: 'rawKeyDown', ...key });
          await this.executeCDP(inputTarget, 'Input.dispatchKeyEvent', { type: 'keyUp', ...key });
        }
        typed = await evaluate(`(/* workass-browser-type-keyboard-verify */ () => {
          const el = ${selector};
          if (!el) return { found: false };
          let active = document.activeElement;
          while (active?.shadowRoot?.activeElement) active = active.shadowRoot.activeElement;
          return { found: true, exact: String(el.value ?? '') === ${value}, focused: active === el, valueLength: String(el.value ?? '').length };
        })()`);
      } catch { typed = null; }
      if (typed && typed.found === true && typed.exact === true) {
        let submitted = false;
        if (params.submit === true) submitted = (await this.submitBrowserType(entry, locator)).submitted === true;
        entry.interactionGeneration += 1;
        return {
          found: true, editable: true, strategy: 'keyboard', focused: typed.focused === true,
          changed: String(prepared.before) !== text, replacementVerified: true,
          valueLength: typed.valueLength, trusted: true, submitted,
        };
      }
      if (typed && typed.found !== true) return { found: false, status: locator.path ? 'stale' : 'missing', reason: locator.path ? 'observed element was replaced; refresh the snapshot' : 'selector target is missing' };
      // The field did not take the typed text exactly: set the value instead.
      const fallback = await evaluate(`(/* workass-browser-type-value-fallback */ () => {
        const el = ${selector};
        if (!el) return { found: false };
        const view = el.ownerDocument.defaultView;
        const textarea = el instanceof view.HTMLTextAreaElement;
        const focused = () => { let active = document.activeElement; while (active?.shadowRoot?.activeElement) active = active.shadowRoot.activeElement; return active === el; };
        if (!focused()) el.focus();
        ${valueSetter}
      })()`);
      if (!fallback || fallback.found !== true) return { found: false, status: locator.path ? 'stale' : 'missing', reason: locator.path ? 'observed element was replaced; refresh the snapshot' : 'selector target is missing' };
      if (fallback.editable !== true) throw new Error(`browser type target is not editable${fallback.reason ? `: ${fallback.reason}` : ''}`);
      if (fallback.replacementVerified !== true) throw new Error('browser type target did not retain the replacement text');
      let submitted = false;
      if (params.submit === true) submitted = (await this.submitBrowserType(entry, locator)).submitted === true;
      entry.interactionGeneration += 1;
      return { ...fallback, changed: String(prepared.before) !== String(text), submitted };
    }
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
        await this.executeCDP(this.inputTargetForFrame(entry, locator.frameId ?? null), 'Input.insertText', { text });
      } else {
        await this.dispatchBrowserKey(entry, parseBrowserKey('Backspace', this.platform));
        await evaluate('(/* workass-browser-delete-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
        await this.deleteSelectedBrowserText(entry, locator.frameId ?? null, 'Backspace');
      }

      const verifyScript = (pageModel) => `(/* workass-browser-type-verify */ () => {
        const expected = ${value};
        const pageModel = ${JSON.stringify(pageModel)};
        const normalize = (raw) => String(raw ?? '').replace(/\\r\\n?/gu, '\\n').replace(/\\u00a0/gu, ' ');
        const compact = (raw) => normalize(raw).replace(/\\s+/gu, ' ').trim();
        const visibleMatches = (visible) => {
          const observed = compact(visible);
          const wanted = compact(expected);
          if (!wanted) return !observed;
          return !!observed && wanted.includes(observed);
        };
        const probe = globalThis[${encodedProbeKey}];
        const el = ${selector};
        if (!probe || !el) return { found: false };
        const editorRoot = el.closest('.monaco-editor,.CodeMirror,.cm-editor');
        const model = typeof pageModel === 'string' ? pageModel : null;
        const root = editorRoot || el;
        const lines = root.querySelectorAll('.view-lines .view-line,.CodeMirror-code pre,.cm-content .cm-line');
        const visible = lines.length
          ? Array.from(lines).map((line) => String(line.textContent || '')).join('\\n')
          : (el.isContentEditable ? String(el.innerText || el.textContent || '') : '');
        const emptyContentEditable = el.isContentEditable && expected === ''
          && String(el.textContent || '').replace(/​/gu, '') === '';
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
      })()`;
      const readPageModel = async () => {
        if (!pageEditor) return null;
        const read = await this.editorPageCall(entry, locator.frameId ?? null, selector, 'value');
        return read && typeof read.value === 'string' ? read.value : null;
      };
      let state = await evaluate(verifyScript(await readPageModel()));
      for (let attempt = 0; attempt < 20 && state && state.found && !state.exact && !state.visibleMatch; attempt += 1) {
        await new Promise((resolve) => setTimeout(resolve, 25));
        state = await evaluate(verifyScript(await readPageModel()));
      }
      await cleanupProbe();
      verification = state;
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
    const sessionId = frame.sessionId || undefined;
    let resolved;
    try {
      resolved = await this.withAgentContext(entry, frame, (executionContextId) => this.ownedSessionCommand(entry, 'DOM.resolveNode', {
        backendNodeId: Number(backendDOMNodeId), executionContextId,
      }, sessionId));
    } catch { return []; }
    const objectId = resolved && resolved.object && resolved.object.objectId;
    if (!objectId) return [];
    try {
      const response = await this.ownedSessionCommand(entry, 'Runtime.callFunctionOn', {
        objectId,
        functionDeclaration: "function(){ const refs=globalThis[Symbol.for('workass.browser.observed-node-refs')]; return refs&&refs.get(this)?Array.from(refs.get(this)):[]; }",
        returnByValue: true,
      }, sessionId);
      return Array.isArray(response && response.result && response.result.value)
        ? response.result.value.map(String)
        : [];
    } catch { return []; }
    finally {
      try { await this.ownedSessionCommand(entry, 'Runtime.releaseObject', { objectId }, sessionId); } catch { /* resolved node released with its context */ }
    }
  }

  async browserSnapshot(entry, { childFramesDeadline: sharedDeadline = null, unbounded = false } = {}) {
    // Compact views budget their own output, so they read the full result.
    const snapshotByteLimit = unbounded ? Number.POSITIVE_INFINITY : MAX_BROWSER_SNAPSHOT_BYTES;
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

    // Child frames that stopped answering share one bounded budget, so a page
    // full of stuck ad frames cannot stretch one snapshot frame by frame.
    const childFramesDeadline = sharedDeadline ?? Date.now() + (this.snapshotChildFramesBudgetMs ?? SNAPSHOT_CHILD_FRAMES_BUDGET_MS);
    const observe = async (frame) => {
      const script = observationScript({ snapshotId, maxNodes: 500, maxText: 32 * 1024, rootOnly: true });
      if (frame && frame.sessionId) {
        const remainingMs = childFramesDeadline - Date.now();
        if (remainingMs <= 0) throw new Error('browser frame observation budget exhausted');
        return this.evaluateFrame(entry, frame.id, script, { timeoutMs: remainingMs });
      }
      if (frame) return this.evaluateFrame(entry, frame.id, script);
      return this.evaluateAgentWorld(entry, script, true);
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
        if (source.editors.length) await this.readEditorModels(entry, frame, source.editors, [source.semantic, source.interactive]);
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
      const remainingMs = sessionId ? childFramesDeadline - Date.now() : null;
      let response;
      try {
        if (remainingMs != null && remainingMs <= 0) throw new Error('browser frame observation budget exhausted');
        response = await this.ownedSessionCommand(entry,
          'Accessibility.getFullAXTree', { frameId: frame.id }, sessionId, remainingMs,
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
      // A secret field's own text (its value) must not come back as rows.
      const secretFieldAxIds = new Set();
      const axRowNodeIds = new Map();
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
        if (existing && existing.valueRedacted === true) secretFieldAxIds.add(String(axNode.nodeId));
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
          axRowNodeIds.set(refIndex, String(axNode.nodeId));
          if (actionableRoles.has(role)) interactive.push({
            refIndex, role: axFields.role, name: axFields.name, text: '', selector: '',
            ariaLabel: null, readOnly: values.readonly === true, editor: null,
            focused: axFields.focused, disabled: axFields.disabled,
          });
        }
      }
      nodeCount -= dropSecretFieldAxRows({ axNodes, secretFieldAxIds, axRowNodeIds, semantic, interactive });
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
          const owner = await this.ownedSessionCommand(entry,
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
    // Element facts (href, src, value...) join the rows after the legacy budget.
    const nodeFacts = detachNodeFacts(semantic, interactive);

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
    while (serializedBytes > snapshotByteLimit - 16) {
      let bytesToRemove = serializedBytes - (snapshotByteLimit - 16);
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
    while (byteSize() > snapshotByteLimit) {
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
    while (byteSize() > snapshotByteLimit && result.semantic.length) {
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
    attachNodeFacts(result, nodeFacts, snapshotByteLimit);
    if (byteSize() > snapshotByteLimit) throw new Error('browser snapshot metadata exceeds the 64 KiB response limit');

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
      frameRefs: new Map(Array.from(frameRefById, ([frameId, frameRef]) => [frameRef, frameId])),
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
    // An outline ref (e12) is used on its own, or with the outline_id it came
    // from; a json/compact snapshot's element_ref needs its snapshot_id.
    const outlineRef = hasRef && /^e\d+$/u.test(params.elementRef.trim()) &&
      (!snapshotPresent || !entry.observations.has(String(params.snapshotId)));
    if (hasRef !== snapshotPresent && !outlineRef) {
      throw new Error('element_ref and snapshot_id must be supplied together');
    }
    if (!hasRef && snapshotPresent) throw new Error('snapshot_id is only used with element_ref');
    if (hasCoordinates && !coordinates) throw new Error('this browser action does not accept screenshot coordinates');
    const forms = Number(hasSelector) + Number(hasRef) + Number(coordinates && hasCoordinates);
    if (forms !== 1) throw new Error('choose exactly one browser target: an element_ref from the outline (e12), a selector, or screenshot coordinates');
    if (outlineRef) return this.shortRefTarget(entry, params.elementRef.trim(), snapshotPresent ? String(params.snapshotId) : null);
    if (hasSelector) {
      const selector = params.selector.trim();
      let count;
      try { count = await this.evaluateAgentWorld(entry, 'document.querySelectorAll(' + JSON.stringify(selector) + ').length'); }
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

  // stopAtFrameId: map only up to that ancestor (a frame's own renderer root),
  // so input can be sent straight to the widget that draws it.
  async mapFramePointToRoot(entry, frameId, point, { stopAtFrameId = null } = {}) {
    let frame = entry.frames.get(String(frameId));
    if (!frame || frame.detached) return { status: 'stale', reason: 'observed frame was replaced; refresh the snapshot' };
    let mappedPoint = { x: Number(point.x), y: Number(point.y) };
    while (frame.parentFrameId && frame.id !== stopAtFrameId) {
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
        const resolvedOwner = await this.withAgentContext(entry, parent, (executionContextId) => entry.view.webContents.debugger.sendCommand('DOM.resolveNode', {
          backendNodeId: owner.backendNodeId, executionContextId,
        }, sessionId));
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
    const childFramesDeadline = Date.now() + (this.snapshotChildFramesBudgetMs ?? SNAPSHOT_CHILD_FRAMES_BUDGET_MS);
    for (const frame of Array.from(entry.frames.values()).filter((item) => !item.detached)) {
      let scroll = null;
      const remainingMs = frame.sessionId ? childFramesDeadline - Date.now() : null;
      try {
        if (remainingMs == null || remainingMs > 0) {
          scroll = await this.evaluateFrame(entry, frame.id, '({ x: scrollX, y: scrollY, width: innerWidth, height: innerHeight })',
            remainingMs == null ? {} : { timeoutMs: remainingMs });
        }
      } catch { /* unavailable frames remain represented by their identity */ }
      frames.push([frame.id, frame.generation, frame.loaderId, frame.targetId, frame.sessionId,
        finiteMetric(scroll?.x), finiteMetric(scroll?.y), finiteMetric(scroll?.width), finiteMetric(scroll?.height)]);
    }
    let top = null;
    try { top = await this.readVerifiedViewport(entry, entry.viewport, this.attachedView === entry.view ? entry.presentationScale : 1); }
    catch { /* caller reports an unavailable metric */ }
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
    const button = params.button == null ? 'left' : String(params.button);
    if (!['left', 'right', 'middle'].includes(button)) throw new Error('button must be left, right or middle');
    const clickCount = params.clickCount == null ? 1 : Number(params.clickCount);
    if (!Number.isInteger(clickCount) || clickCount < 1 || clickCount > 3) throw new Error('click_count must be 1, 2 or 3');
    const aimed = await this.pointerTarget(entry, params, { coordinates: true });
    if (aimed.failure) return aimed.failure;
    const { inputTarget, inputPoint, point, reportedTarget } = aimed;
    if (point.nativeMenu === true || point.target?.nativeMenu === true) return { ...NATIVE_MENU_REFUSAL };
    await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mouseMoved', x: inputPoint.x, y: inputPoint.y });
    for (let count = 1; count <= clickCount; count += 1) {
      await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mousePressed', x: inputPoint.x, y: inputPoint.y, button, clickCount: count });
      await this.executeCDP(inputTarget, 'Input.dispatchMouseEvent', { type: 'mouseReleased', x: inputPoint.x, y: inputPoint.y, button, clickCount: count });
    }
    entry.interactionGeneration += 1;
    return {
      ok: true, status: 'clicked', trusted: true, point: { x: point.x, y: point.y }, target: reportedTarget,
      ...(button !== 'left' ? { button } : {}), ...(clickCount !== 1 ? { click_count: clickCount } : {}),
    };
  }

  // Where trusted pointer input for a target goes: the widget that draws it
  // (the tab, or a cross-site frame's own renderer) and the point there.
  // Returns { failure } with a status when the target cannot take input.
  async pointerTarget(entry, params, { coordinates = false } = {}) {
    const target = await this.observationTarget(entry, params, { coordinates });
    if (target.status) return { failure: { ok: false, status: target.status, reason: target.reason } };
    let point;
    let frameLocalPoint = null;
    if (target.coordinates) {
      if (typeof params.x !== 'number' || !Number.isFinite(params.x) ||
          typeof params.y !== 'number' || !Number.isFinite(params.y) ||
          typeof params.screenshotId !== 'string' || !params.screenshotId) {
        throw new Error('screenshot-coordinate click requires finite numeric x, y, and screenshot_id');
      }
      const image = entry.screenshots.get(params.screenshotId);
      if (!image) return { failure: { ok: false, status: 'stale', reason: 'screenshot_id is unavailable; take a fresh screenshot' } };
      const currentStamp = await this.readInteractionStamp(entry);
      if (image.documentGeneration !== entry.documentGeneration ||
          image.viewportGeneration !== entry.viewportGeneration ||
          image.frameTreeGeneration !== currentStamp.frameTreeGeneration ||
          image.interactionGeneration !== currentStamp.interactionGeneration ||
          JSON.stringify(image.frameStamp) !== JSON.stringify(currentStamp.frames) ||
          JSON.stringify(image.metricsStamp) !== JSON.stringify(screenshotMetricStamp(currentStamp.metrics, image.mode))) {
        return { failure: { ok: false, status: 'stale', reason: 'screenshot observation changed; take a fresh screenshot' } };
      }
      if (params.x < 0 || params.y < 0 || params.x >= image.imageWidth || params.y >= image.imageHeight) {
        return { failure: { ok: false, status: 'missing', reason: 'coordinates are outside the screenshot image' } };
      }
      const docX = image.cssRect.x + params.x * image.pixelToCss.x;
      const docY = image.cssRect.y + params.y * image.pixelToCss.y;
      point = { x: docX - currentStamp.topScroll.x, y: docY - currentStamp.topScroll.y };
      if (point.x < 0 || point.y < 0 || point.x >= currentStamp.metrics.width || point.y >= currentStamp.metrics.height) {
        return { failure: { ok: false, status: 'blocked', reason: 'screenshot point is outside the current visible viewport' } };
      }
      const inspected = await this.evaluateAgentWorld(entry, '(() => {' +
        'const x=' + JSON.stringify(point.x) + ';const y=' + JSON.stringify(point.y) + ';' +
        'const deep=(doc,px,py)=>{let hit=doc.elementFromPoint(px,py);const seen=new Set();while(hit&&!seen.has(hit)){seen.add(hit);const nested=hit.shadowRoot?.elementFromPoint?.(px,py);if(!nested||nested===hit)break;hit=nested;}return hit;};' +
        'const target=deep(document,x,y);if(!target)return {status:"missing"};' +
        'for(let node=target;node&&node.nodeType===1;node=node.parentElement||node.getRootNode?.()?.host||null){if(node.disabled||node.getAttribute("aria-disabled")==="true"||node.hasAttribute("inert"))return {status:"disabled"};}' +
        'const pick=target.closest?.("select");' +
        'return {status:"ready",tag:String(target.tagName||"").toLowerCase(),role:target.getAttribute("role")||null,name:String(target.getAttribute("aria-label")||target.innerText||target.textContent||"").trim().slice(0,160),nativeMenu:!!pick&&!pick.multiple&&!(pick.size>1)};' +
        '})()');
      if (!inspected || inspected.status !== 'ready') {
        return { failure: { ok: false, status: inspected?.status || 'blocked', reason: inspected?.status === 'disabled' ? 'the screenshot target is disabled' : 'no target is present at those screenshot coordinates' } };
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
        return { failure: { ok: false, status, reason } };
      }
      point = inspected;
      if (target.frameId) {
        frameLocalPoint = inspected;
        const mapped = await this.mapFramePointToRoot(entry, target.frameId, point);
        if (mapped.status !== 'ready') return { failure: { ok: false, status: mapped.status, reason: mapped.reason } };
        point = { ...point, x: mapped.x, y: mapped.y };
        const currentFrame = entry.frames.get(target.frameId);
        if (!currentFrame || currentFrame.generation !== frameGeneration || currentFrame.detached) {
          return { failure: { ok: false, status: 'stale', reason: 'observed frame changed during target validation; refresh the snapshot' } };
        }
      }
      const effective = await this.readEffectiveViewport(entry);
      if (!Number.isFinite(point.x) || !Number.isFinite(point.y) ||
          point.x < 0 || point.y < 0 || point.x >= effective.width || point.y >= effective.height) {
        return { failure: { ok: false, status: 'blocked', reason: 'target is outside the visible page viewport' } };
      }
    }
    const tabId = this.tabIdOf(entry);
    const targetFrame = target.frameId ? entry.frames.get(target.frameId) : null;
    // A frame inside a cross-site frame's renderer (that frame itself, or a
    // same-site child of it) gets the click on that renderer's own widget, at
    // the point mapped up to the widget's root frame. Routing a click from the
    // top page down into another renderer depends on its hit-test data, which a
    // page that is not on screen may not have produced.
    const owned = targetFrame?.sessionId ? entry.targetSessions.get(targetFrame.sessionId) : null;
    let inputTarget = { tabId };
    let inputPoint = point;
    if (owned && frameLocalPoint) {
      let widgetPoint = frameLocalPoint;
      if (owned.frameId !== targetFrame.id) {
        const mapped = await this.mapFramePointToRoot(entry, targetFrame.id, frameLocalPoint, { stopAtFrameId: owned.frameId });
        widgetPoint = mapped.status === 'ready' ? mapped : null;
      }
      if (widgetPoint) {
        inputTarget = { tabId, targetId: owned.targetId, sessionId: targetFrame.sessionId };
        inputPoint = { x: widgetPoint.x, y: widgetPoint.y };
      }
    }
    const reportedTarget = point.target || { tag: point.tag, name: point.name };
    if (reportedTarget && typeof reportedTarget.name === 'string') reportedTarget.name = redactPageMessage(reportedTarget.name, 160);
    return { inputTarget, inputPoint, point, reportedTarget, target };
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

    if (this.isHosted(entry)) return this.captureHostedPng(entry, captureParams, decode);
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

  // One capture at a time per page, bounded. Chromium below 157 can bring the
  // whole app down when a guest's renderer exits with a screenshot copy in
  // flight (crbug.com/567321049; this shell pins Chromium 150), and a capture
  // given up on stays pending in Chromium: a later one waits for it to settle
  // rather than overlapping it.
  async captureHostedPng(entry, captureParams, decode) {
    const wc = this.liveContents(entry);
    if (!wc || wc.isDestroyed?.()) throw new Error('the browser page is not up; try again in a moment');
    if (entry.captureInFlight) {
      await this.boundedCapture(entry.captureInFlight, 'an earlier screenshot of this page is still being taken; try again in a moment');
    }
    this.assertCurrentEntry(entry);
    let release;
    const turn = new Promise((resolve) => { release = resolve; });
    entry.captureInFlight = turn;
    try {
      const png = await this.capturePageRegion(entry, wc, captureParams.clip);
      return { ...decode({ data: png.toString('base64') }), surface: 'webview' };
    } finally {
      if (entry.captureInFlight === turn) entry.captureInFlight = null;
      release();
    }
  }

  boundedCapture(promise, message) {
    const limitMs = this.captureTimeoutMs ?? CAPTURE_TIMEOUT_MS;
    let timer;
    return Promise.race([
      promise,
      new Promise((_resolve, reject) => { timer = setTimeout(() => reject(new Error(message)), limitMs); }),
    ]).finally(() => clearTimeout(timer));
  }

  // A webview guest is captured from its own surface (webContents
  // capturePage): sharp whatever size the page is shown at, and whatever
  // covers it. (Page.captureScreenshot on a guest copies the window's
  // composited output instead: blurry when the page is shown small, and
  // waiting forever while the app covers it.) The guest's view is its
  // viewport at 1:1, so view coordinates are the viewport's CSS pixels. A
  // region beyond the viewport is taken one viewport at a time, scrolling the
  // page there and back.
  async capturePageRegion(entry, wc, clip) {
    if (!this.nativeImage) throw new Error('image encoding is unavailable in this shell');
    const at = await this.readEffectiveViewport(entry);
    const vw = at.width;
    const vh = at.height;
    const outW = Math.max(1, Math.round(clip.width));
    const outH = Math.max(1, Math.round(clip.height));
    const snap = async (rect, width, height) => {
      const capture = Promise.resolve(wc.capturePage({
        x: Math.round(rect.x), y: Math.round(rect.y), width: Math.max(1, Math.round(rect.width)), height: Math.max(1, Math.round(rect.height)),
      }));
      const image = await this.boundedCapture(capture, `the page did not give a frame within ${Math.round((this.captureTimeoutMs ?? CAPTURE_TIMEOUT_MS) / 1000)}s; try again, or reload it if it stays stuck`);
      if (!image || image.isEmpty()) throw new Error('browser capture returned an empty image');
      const size = image.getSize();
      return size.width === width && size.height === height ? image : image.resize({ width, height, quality: 'best' });
    };
    const inside = clip.x >= at.scrollX - 0.5 && clip.y >= at.scrollY - 0.5 &&
      clip.x + clip.width <= at.scrollX + vw + 0.5 && clip.y + clip.height <= at.scrollY + vh + 0.5;
    if (inside) {
      return (await snap({ x: clip.x - at.scrollX, y: clip.y - at.scrollY, width: clip.width, height: clip.height }, outW, outH)).toPNG();
    }
    if (clip.width > vw + 0.5) {
      throw new Error(`a capture wider than the page's viewport (${vw} CSS pixels) is not available here; capture a narrower clip or set a wider viewport`);
    }
    const pixels = Buffer.alloc(outW * outH * 4);
    const scrollTo = (x, y) => this.evaluateAgentWorld(entry, `(async () => { scrollTo({ left: ${JSON.stringify(x)}, top: ${JSON.stringify(y)}, behavior: 'instant' }); await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))); return { x: scrollX, y: scrollY }; })()`);
    let row = 0;
    try {
      while (row < outH) {
        const want = clip.y + row;
        const scrolled = await this.boundedCapture(Promise.resolve(scrollTo(clip.x, want)), 'the page did not scroll for the capture');
        const top = want - Number(scrolled?.y || 0);
        const left = clip.x - Number(scrolled?.x || 0);
        if (top < 0 || top >= vh || left < 0 || left + clip.width > vw + 0.5) throw new Error('the page could not be scrolled to the requested region');
        const rows = Math.min(outH - row, Math.floor(vh - top));
        if (rows <= 0) throw new Error('the page could not be scrolled to the requested region');
        const bitmap = (await snap({ x: left, y: top, width: clip.width, height: rows }, outW, rows)).toBitmap();
        bitmap.copy(pixels, row * outW * 4, 0, Math.min(bitmap.length, rows * outW * 4));
        row += rows;
      }
    } finally {
      try { await scrollTo(at.scrollX, at.scrollY); } catch { /* the page navigated; nothing to restore */ }
    }
    return this.nativeImage.createFromBitmap(pixels, { width: outW, height: outH }).toPNG();
  }

  browserScreenshot(entry, params = {}) {
    return this.serializePresentation(() => this.withPainting(entry, () => this.browserScreenshotSerialized(entry, params)));
  }

  async browserScreenshotSerialized(entry, params = {}) {
    this.assertCurrentEntry(entry);
    const wc = entry.view.webContents;
    const mode = String(params.mode || 'viewport');
    if (!['viewport', 'full_page', 'clip'].includes(mode)) throw new Error('mode must be viewport, full_page, or clip');
    if (mode === 'clip' && (!params.clip || typeof params.clip !== 'object')) throw new Error('clip mode requires a CSS clip rectangle');
    if (mode !== 'clip' && params.clip != null) throw new Error('clip is only accepted with mode=clip');
    if (entry.loading && !entry.mainDocumentReady) throw new Error('browser page is navigating; wait for navigation before taking a screenshot');
    await this.ensureCDP(entry);
    const beforeStamp = await this.readInteractionStamp(entry);
    if (entry.loading && !entry.mainDocumentReady) throw new Error('browser page is navigating; wait for navigation before taking a screenshot');
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
    } else if (mode === 'full_page' && this.isHosted(entry)) {
      // A hosted page is captured from its own surface, one viewport at a
      // time (captureHostedPng); the page's width is the viewport's.
      const metrics = await wc.debugger.sendCommand('Page.getLayoutMetrics');
      const content = metrics && (metrics.cssContentSize || metrics.contentSize);
      cssRect = { x: 0, y: 0, width: Math.min(Number(content?.width), entry.viewport.width), height: Number(content?.height) };
      fullPageRasterExtent = { width: Math.ceil(cssRect.width), height: Math.ceil(cssRect.height) };
      captureParams.clip = { ...cssRect, ...fullPageRasterExtent, scale: 1 };
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
    const captureSurfaceOptions = mode === 'full_page' && !this.isHosted(entry)
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
        (entry.loading && !entry.mainDocumentReady)) {
      const changed = [];
      for (const key of ['documentGeneration', 'viewportGeneration', 'frameTreeGeneration', 'interactionGeneration']) {
        if (beforeStamp[key] !== afterStamp[key]) changed.push(key);
      }
      if (JSON.stringify(beforeMetricsStamp) !== JSON.stringify(afterMetricsStamp)) changed.push('metrics');
      if (entry.loading && !entry.mainDocumentReady) changed.push('navigation_in_progress');
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
        catch { tab = { id: this.tabIdOf(entry), chatId: redactPageMessage(entry.chatId, 256), closed: true }; }
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
      click: new Set(['action', 'selector', 'elementRef', 'snapshotId', 'x', 'y', 'screenshotId', 'button', 'clickCount']),
      type: new Set(['action', 'selector', 'elementRef', 'snapshotId', 'text', 'submit']),
      scroll: new Set(['action', 'x', 'y', 'elementRef', 'snapshotId']),
      key: new Set(['action', 'key']),
      snapshot: new Set(['action']),
      select: new Set(['action', 'selector', 'elementRef', 'snapshotId', 'values', 'labels', 'indexes']),
      hover: new Set(['action', 'selector', 'elementRef', 'snapshotId']),
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
      const outlineRef = refPresent && typeof action.elementRef === 'string' && /^e\d+$/u.test(action.elementRef.trim());
      const validateTargetPair = () => {
        if (refPresent !== snapshotPresent && !outlineRef) throw new Error('browser batch ' + name + ' ' + index + ' element_ref and snapshot_id must be supplied together');
        if (snapshotPresent && !refPresent) throw new Error('browser batch ' + name + ' ' + index + ' snapshot_id is only used with element_ref');
        if (refPresent && (typeof action.elementRef !== 'string' || !action.elementRef.trim() ||
            (snapshotPresent && (typeof action.snapshotId !== 'string' || !action.snapshotId.trim())))) {
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
      } else if (name === 'select' || name === 'hover') {
        validateTargetPair();
        if (Number(selectorPresent) + Number(refPresent) !== 1) throw new Error('browser batch ' + name + ' ' + index + ' requires exactly one target form');
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
      markAgentActive(entry);
      if (params.url) {
        const navigate = async () => {
          await this.command(entry.chatId, 'navigate', params.url);
          return this.navigationResult(entry);
        };
        return this.serialize(entry, () => (internal.report ? runAction(this, entry, navigate, { before: null, method }) : navigate()),
          { deadlineMs: LONG_OPERATION_DEADLINE_MS });
      }
      return this.tabInfo(entry);
    }
    const entry = await this.controlEntry(params);
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || 'browser page initialization failed');
    if (!internal.serialized && !READ_ONLY_METHODS.has(method)) markAgentActive(entry);
    if (method === 'browser.close') return this.closeForAgent(entry);
    // A JavaScript dialog pauses the page: only answering it (or reading what
    // does not need the page) goes through.
    if (entry.dialog && !['browser.dialog', 'browser.screenshot', 'browser.diagnostics'].includes(method)) throw dialogBlockedError(entry);
    const serializedMethods = new Set([
      'browser.navigate', 'browser.back', 'browser.forward', 'browser.reload', 'browser.click',
      'browser.type', 'browser.scroll', 'browser.key', 'browser.setViewport', 'browser.resetViewport',
      'browser.batch', 'browser.snapshot', 'browser.screenshot', 'browser.read',
      'browser.select', 'browser.hover', 'browser.drag', 'browser.upload', 'browser.evaluate',
    ]);
      if (!internal.serialized && serializedMethods.has(method)) {
      const long = method === 'browser.navigate' || method === 'browser.batch' || method === 'browser.reload';
      return this.serialize(entry, () => this.browserControl(method, params, { serialized: true, report: internal.report === true }),
        long ? { deadlineMs: LONG_OPERATION_DEADLINE_MS } : {});
    }
    // Every action reports what it changed (browser-actions.js); a batch
    // reports once, for all of its actions.
    const actionMethods = new Set([
      'browser.navigate', 'browser.back', 'browser.forward', 'browser.reload', 'browser.click', 'browser.type',
      'browser.scroll', 'browser.key', 'browser.batch', 'browser.select', 'browser.hover', 'browser.drag',
      'browser.upload', 'browser.evaluate',
    ]);
    if (actionMethods.has(method) && internal.report && !internal.bare) {
      // A navigation replaces the document: no outline before it.
      const navigation = ['browser.navigate', 'browser.back', 'browser.forward', 'browser.reload'].includes(method);
      return runAction(this, entry, () => this.browserControl(method, params, { ...internal, bare: true }),
        navigation ? { before: null, method } : { method });
    }
    if (method === 'browser.dialog') return browserDialog(this, entry, params);
    if (method === 'browser.select') return browserSelect(this, entry, params);
    if (method === 'browser.hover') return browserHover(this, entry, params);
    if (method === 'browser.drag') return browserDrag(this, entry, params);
    if (method === 'browser.upload') return browserUpload(this, entry, params);
    if (method === 'browser.evaluate') return browserEvaluate(this, entry, params);
    if (method === 'browser.setViewport') return this.setViewport(entry, params);
    if (method === 'browser.resetViewport') return this.resetViewport(entry);
    if (method === 'browser.navigate') {
      await this.command(entry.chatId, 'navigate', params.url);
      return this.navigationResult(entry);
    }
    if (method === 'browser.back') { await this.command(entry.chatId, 'back'); return this.tabInfo(entry); }
    if (method === 'browser.forward') { await this.command(entry.chatId, 'forward'); return this.tabInfo(entry); }
    if (method === 'browser.reload') { await this.command(entry.chatId, 'reload'); return this.tabInfo(entry); }
    if (method === 'browser.snapshot') return browserSnapshotForParams(this, entry, params);
    if (method === 'browser.read') return browserRead(this, entry, params);
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
      const result = await this.evaluateAgentWorld(entry, '(() => { const before = { x: scrollX, y: scrollY }; window.scrollBy(' + JSON.stringify(x) + ', ' + JSON.stringify(y) + '); const after = { x: scrollX, y: scrollY }; return { changed: before.x !== after.x || before.y !== after.y, before, after }; })()');
      if (result?.changed) entry.interactionGeneration += 1;
      return result;
    }
    if (method === 'browser.key') {
      const parsed = internal.parsedKey || parseBrowserKey(params.key, this.platform);
      const focusedFrame = await this.focusedFrame(entry);
      const focusedFrameId = focusedFrame?.id ?? null;
      if (MENU_OPENING_KEYS.has(String(parsed.keyCode))) {
        let onSelect = false;
        try {
          onSelect = await this.evaluateFrame(entry, focusedFrameId, '(() => { let el = document.activeElement; while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement; return !!el && el.tagName === "SELECT" && !el.multiple && !(el.size > 1); })()') === true;
        } catch { /* the key goes ahead */ }
        if (onSelect) return { ...NATIVE_MENU_REFUSAL };
      }
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
      // The action run this batch is part of (browser-actions.js): a dialog
      // that opens stops it there, and the report says how far it got.
      const control = internal.bare ? entry.actionControl : null;
      for (let index = 0; index < actions.length; index += 1) {
        const action = actions[index];
        const name = action.action;
        if (control && control.stopped) break;
        if (control) {
          control.partial = { results: results.slice(), completed_indexes: completedIndexes.slice(), dialog_index: index,
            unexecuted_indexes: actions.slice(index + 1).map((_item, offset) => index + offset + 1) };
        }
        try {
          const result = await this.browserControl(`browser.${name}`, { ...action, tabId: this.tabInfo(entry).id }, { serialized: true, bare: true, parsedKey: action.parsedKey });
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
        try { observed = await this.browserControl('browser.snapshot', { tabId: this.tabInfo(entry).id }, { serialized: true, bare: true }); }
        catch (error) { observeAfterError = redactPageMessage(String(error && error.message || error), 1024); }
      }
      return { results, completed_indexes: completedIndexes, failed_index: null, unexecuted_indexes: [], observe_after: observed, observe_after_error: observeAfterError };
    }
    if (method === 'browser.screenshot') {
      return this.browserScreenshot(entry, params);
    }
    if (method === 'browser.wait') return this.withPainting(entry, () => this.browserWait(entry, params));
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
    this.cancelOwnedTargetTasks(entry, () => true, 'entry_closed');
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
    const tabId = this.tabIdOf(entry);
    for (const [sessionId, target] of Array.from(entry.targetSessions.entries())) {
      this.childSessions.delete(`${tabId}:${target.targetId}`);
      try {
        const debug = this.liveContents(entry)?.debugger;
        if (debug) void Promise.resolve(debug.sendCommand('Target.detachFromTarget', { sessionId }, target.parentSessionId || undefined)).catch(() => {});
      } catch { /* debugger may already be gone */ }
      entry.targetSessions.delete(sessionId);
      this.releaseSessionCommands(entry, sessionId);
    }
    entry.executionContexts.clear();
    entry.frames.clear();
    this.releasePipWindow(entry);
    if (entry.guestWaiter) entry.guestWaiter.settle(false);
    // The tabs this page opened go with it (they hold renderers and count
    // against the chat's tab limit).
    if (!entry.extraTab) {
      for (const [otherId, other] of Array.from(this.entries.entries())) {
        if (other !== entry && other.extraTab && other.ownerChatId === entry.chatId) this.close(otherId);
      }
    }
    if (this.isHosted(entry)) {
      // The renderer drops the page's <webview> (its guest goes with it); a
      // renderer that is gone or busy does not keep the page alive.
      entry.visible = false;
      if (this.activeId === id) this.activeId = null;
      this.publishHosts();
      const guest = this.liveContents(entry);
      if (guest) {
        const timer = setTimeout(() => { try { if (!guest.isDestroyed()) guest.close(); } catch { /* already gone */ } }, 1500);
        if (typeof timer?.unref === 'function') timer.unref();
      }
      return true;
    }
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
