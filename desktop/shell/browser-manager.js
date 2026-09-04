'use strict';

const DEFAULT_PARTITION = 'persist:workass-browser';
const DEFAULT_URL = 'about:blank';

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
  constructor({ win, WebContentsView, session, partition = DEFAULT_PARTITION, chromeVersion, platform, onState, requestOpen }) {
    if (!win || !WebContentsView || !session) throw new Error('browser manager dependencies missing');
    this.win = win;
    this.WebContentsView = WebContentsView;
    this.partition = partition;
    this.platform = platform || process.platform;
    this.profile = session.fromPartition(partition, { cache: true });
    this.userAgent = cleanUserAgent(chromeVersion, platform);
    this.onState = typeof onState === 'function' ? onState : () => {};
    this.requestOpen = typeof requestOpen === 'function' ? requestOpen : () => {};
    this.entries = new Map();
    this.activeId = null;
    this.attachedView = null;
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
      url: entry.url || DEFAULT_URL,
      title: entry.title || '',
      loading: !!entry.loading,
      error: entry.error || null,
      canGoBack: !!(history && history.canGoBack && history.canGoBack()),
      canGoForward: !!(history && history.canGoForward && history.canGoForward()),
      cdpAttached: !!(wc.debugger && wc.debugger.isAttached && wc.debugger.isAttached()),
      persistent: !!(wc.session && wc.session.isPersistent && wc.session.isPersistent()),
      agentControl: this.agentControl,
    };
  }

  setAgentControlReady(ready) {
    this.agentControl = ready === true;
    for (const entry of this.entries.values()) this.publish(entry);
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

  bind(entry) {
    const wc = entry.view.webContents;
    const syncURL = (_event, url) => {
      entry.url = String(url || wc.getURL() || DEFAULT_URL);
      entry.error = null;
      this.publish(entry);
    };
    wc.on('did-start-loading', () => { entry.loading = true; entry.error = null; this.publish(entry); });
    wc.on('did-stop-loading', () => {
      entry.loading = false;
      entry.url = wc.getURL() || entry.url || DEFAULT_URL;
      this.publish(entry);
    });
    wc.on('did-navigate', syncURL);
    wc.on('did-navigate-in-page', syncURL);
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
        this.emitCDP({ tabId: Number(wc.id), method, params: params || {}, sessionId: sessionId || null });
      });
      wc.debugger.on('detach', (_event, reason) => {
        this.emitCDP({ tabId: Number(wc.id), detach: true, reason: String(reason || 'detached') });
      });
    }
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
    const entry = { chatId, view, url: DEFAULT_URL, title: '', loading: false, error: null };
    try { view.setBackgroundColor('#ffffff'); } catch { /* older Electron */ }
    try { view.webContents.setUserAgent(this.userAgent); } catch { /* best effort */ }
    this.entries.set(chatId, entry);
    this.bind(entry);
    return entry;
  }

  get(chatId) {
    const id = safeChatId(chatId);
    return this.entries.get(id) || this.create(id);
  }

  async ensureCDP(entry) {
    const debug = entry.view.webContents.debugger;
    if (!debug || typeof debug.attach !== 'function') return false;
    try {
      if (!debug.isAttached()) debug.attach('1.3');
      await debug.sendCommand('Target.getTargetInfo');
      return true;
    } catch {
      return false;
    }
  }

  // Build-health probe: initialize the persistent Chromium profile and attach
  // CDP without showing a view. Electron may start with the browser rail closed,
  // but rebuild health still needs to prove the browser process is usable.
  async probe() {
    const entry = this.get('__workass-health__');
    await this.ensureCDP(entry);
    return this.publish(entry);
  }

  attach(entry) {
    if (this.attachedView !== entry.view) {
      if (this.attachedView) this.win.contentView.removeChildView(this.attachedView);
      this.win.contentView.addChildView(entry.view);
      this.attachedView = entry.view;
    }
    if (entry.bounds) entry.view.setBounds(entry.bounds);
  }

  async activate({ chatId, conversationId, bounds, url }) {
    const id = safeChatId(chatId);
    // Adopt any background entry the agent opened for this conversation (keyed by
    // its conversation id) so the user's visible view and the agent's browser are
    // ONE view, never a duplicate.
    if (conversationId) this.adoptBackground(id, String(conversationId));
    const entry = this.get(id);
    if (conversationId) entry.conversationId = String(conversationId);
    this.activeId = entry.chatId;
    entry.bounds = safeBounds(bounds, this.win.getContentSize());
    // Opening the pane must always leave the user with a browser, so an
    // unsupported URL becomes a visible pane error instead of a failed open.
    const resolved = resolveBrowserURL(url);
    const target = resolved.error ? DEFAULT_URL : resolved.url;
    if (resolved.error) entry.error = resolved.error;
    this.attach(entry);
    if (target !== DEFAULT_URL && (!entry.view.webContents.getURL() || entry.view.webContents.getURL() === DEFAULT_URL)) {
      entry.loading = true;
      entry.url = target;
      void entry.view.webContents.loadURL(target).catch((err) => {
        entry.loading = false;
        entry.error = String(err && err.message || err);
        this.publish(entry);
      });
    }
    await this.ensureCDP(entry);
    return this.publish(entry);
  }

  resize(chatId, bounds) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry || this.activeId !== id) return false;
    entry.bounds = safeBounds(bounds, this.win.getContentSize());
    if (this.attachedView === entry.view) entry.view.setBounds(entry.bounds);
    return true;
  }

  hide(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry || this.attachedView !== entry.view) return false;
    // BrowserWindow can destroy its native contentView before emitting
    // `closed`. Teardown still has to forget the attachment and close the child
    // WebContentsView; touching the dead native owner must never escape as an
    // uncaught "Object has been destroyed" dialog that strands the shell.
    try { this.win.contentView.removeChildView(entry.view); } catch { /* owning window already gone */ }
    this.attachedView = null;
    this.activeId = null;
    return true;
  }

  async command(chatId, command, value) {
    const entry = this.get(chatId);
    const wc = entry.view.webContents;
    switch (command) {
      case 'navigate': {
        const target = normalizeBrowserURL(value);
        // Only (re)attach a view that is ALREADY the visible one — an agent
        // navigating a background browser (a chat the user isn't viewing) must
        // never yank that view into the window and steal the screen.
        if (this.attachedView === entry.view) this.attach(entry);
        entry.loading = true;
        entry.url = target;
        entry.error = null;
        this.publish(entry);
        await wc.loadURL(target);
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
      chatId: entry.chatId,
      conversationId: entry.conversationId || null,
      url: wc.getURL() || entry.url || DEFAULT_URL,
      title: entry.title || '',
      active: this.activeId === entry.chatId,
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
    this.requestOpen(conversationId);
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
    const debug = entry.view.webContents.debugger;
    let sessionId = target && target.sessionId;
    if (!sessionId && target && target.targetId) {
      sessionId = this.childSessions.get(`${target.tabId}:${target.targetId}`);
      if (!sessionId) throw new Error(`browser child target is not attached: ${target.targetId}`);
    }
    return debug.sendCommand(String(method || ''), commandParams || {}, sessionId || undefined);
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

  async selectAllBrowserTarget(webContents) {
    return webContents.executeJavaScript(`(/* workass-browser-select-all */ () => {
      const el = document.activeElement;
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
          const selection = globalThis.getSelection();
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
        if (!editorRoot && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement)) {
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

  async deleteSelectedBrowserText(webContents, keyCode) {
    const command = JSON.stringify(keyCode === 'Delete' ? 'forwardDelete' : 'delete');
    return webContents.executeJavaScript(`(/* workass-browser-delete-selection */ () => {
      const el = document.activeElement;
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
          const selection = globalThis.getSelection();
          if (selection?.rangeCount && !selection.getRangeAt(0).collapsed) {
            const before = String(el.innerText || el.textContent || '').length;
            document.execCommand(${command}, false, null);
            const after = String(el.innerText || el.textContent || '').length;
            return { found: true, deletionVerified: after < before, strategy: 'contenteditable-command' };
          }
        }
        if (!editorRoot && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement)) {
          const start = Number(el.selectionStart);
          const end = Number(el.selectionEnd);
          if (Number.isInteger(start) && Number.isInteger(end) && end > start) {
            const before = String(el.value || '');
            const next = before.slice(0, start) + before.slice(end);
            const prototype = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
            const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
            if (typeof setter === 'function') setter.call(el, next);
            else el.value = next;
            el.setSelectionRange(start, start);
            let event;
            try { event = new InputEvent('input', { bubbles: true, inputType: ${command} === 'forwardDelete' ? 'deleteContentForward' : 'deleteContentBackward' }); }
            catch { event = new Event('input', { bubbles: true }); }
            el.dispatchEvent(event);
            return { found: true, deletionVerified: String(el.value || '') === next, strategy: 'form-control-selection' };
          }
        }
      } catch { /* the CDP key event remains the primary attempt */ }
      return { found: false, deletionVerified: false, strategy: 'keyboard-only' };
    })()`);
  }

  async attachTarget(tabId, targetId) {
    const result = await this.executeCDP({ tabId }, 'Target.attachToTarget', { targetId: String(targetId), flatten: true });
    if (!result || !result.sessionId) throw new Error('browser child target attach returned no session id');
    this.childSessions.set(`${Number(tabId)}:${String(targetId)}`, result.sessionId);
    return {};
  }

  async detachTarget(tabId, targetId) {
    const key = `${Number(tabId)}:${String(targetId)}`;
    const sessionId = this.childSessions.get(key);
    if (sessionId) {
      await this.executeCDP({ tabId }, 'Target.detachFromTarget', { sessionId });
      this.childSessions.delete(key);
    }
    return {};
  }

  // Why a capture came back empty, said in terms the caller can act on rather
  // than as a missing field.
  captureFailure(entry) {
    const info = this.tabInfo(entry);
    if (this.attachedView === entry.view) {
      return `browser tab ${info.id} captured nothing: the Workass window is hidden or minimized. Restore it and retry.`;
    }
    const active = this.browserEntries().find((candidate) => candidate.chatId === this.activeId);
    const visible = active ? ` The visible tab is ${this.tabInfo(active).id}.` : ' No browser tab is visible right now.';
    return `browser tab ${info.id} is not the visible tab, so it has no surface to capture.`
      + `${visible} Only the visible tab can be screenshotted — open the browser on chat ${info.chatId} and retry.`;
  }

  async submitBrowserType(entry, selector) {
    const webContents = entry.view.webContents;
    const target = JSON.stringify(String(selector || ''));
    const submit = await webContents.executeJavaScript(`(() => {
      const el = document.querySelector(${target});
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

  async browserType(entry, params = {}) {
    const wc = entry.view.webContents;
    const selectorText = String(params.selector || '');
    const text = String(params.text ?? '');
    const selector = JSON.stringify(selectorText);
    const value = JSON.stringify(text);
    const probeKey = `__workassBrowserTypeProbe${++this.typeProbeSeq}`;
    const encodedProbeKey = JSON.stringify(probeKey);

    // Ordinary form controls keep the deterministic value-set path, but use
    // the native prototype setter so React/Vue value trackers observe the
    // input event. Hidden editor textareas and contenteditable surfaces must be
    // driven like a user: focus, select all, then insert native text.
    const prepared = await wc.executeJavaScript(`(/* workass-browser-type-prepare */ () => {
      const el = document.querySelector(${selector});
      if (!el) return { found: false };
      const input = el instanceof HTMLInputElement;
      const textarea = el instanceof HTMLTextAreaElement;
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
      const focused = () => document.activeElement === el || (!!editorRoot && editorRoot.contains(document.activeElement));
      const editorReadOnly = monacoEditor?.getRawOptions?.()?.readOnly === true;
      if (editorReadOnly || (!editorRoot && (el.disabled || el.readOnly))) {
        return { found: true, editable: false, focused: focused(), reason: 'disabled or read-only' };
      }
      if (!formControl && !nativeEditor) {
        return { found: true, editable: false, focused: focused(), reason: 'target is not editable' };
      }
      if (!nativeEditor) {
        const before = String(el.value ?? '');
        const prototype = textarea ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
        if (typeof setter !== 'function') {
          return { found: true, editable: false, focused: focused(), reason: 'value setter is unavailable' };
        }
        setter.call(el, ${value});
        let inputEvent;
        try {
          inputEvent = new InputEvent('input', { bubbles: true, inputType: 'insertReplacementText', data: ${value} });
        } catch {
          inputEvent = new Event('input', { bubbles: true });
        }
        el.dispatchEvent(inputEvent);
        el.dispatchEvent(new Event('change', { bubbles: true }));
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
          const selection = globalThis.getSelection();
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

    if (!prepared || prepared.found !== true) return { found: false };
    if (prepared.editable !== true) {
      throw new Error(`browser type target is not editable${prepared.reason ? `: ${prepared.reason}` : ''}`);
    }
    if (prepared.focused !== true) throw new Error('browser type target could not be focused');

    if (prepared.strategy === 'value') {
      if (prepared.replacementVerified !== true) throw new Error('browser type target did not retain the replacement text');
      let submitted = false;
      if (params.submit === true) submitted = (await this.submitBrowserType(entry, selectorText)).submitted === true;
      return { ...prepared, submitted };
    }
    if (prepared.strategy !== 'native') throw new Error('browser type selected an unknown edit strategy');

    const cleanupProbe = async () => {
      try {
        await wc.executeJavaScript(`(() => {
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
      await wc.executeJavaScript('(/* workass-browser-input-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
      if (text) {
        await this.executeCDP({ tabId: this.tabInfo(entry).id }, 'Input.insertText', { text });
      } else {
        await this.dispatchBrowserKey(entry, parseBrowserKey('Backspace', this.platform));
        await wc.executeJavaScript('(/* workass-browser-delete-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
        await this.deleteSelectedBrowserText(wc, 'Backspace');
      }

      verification = await wc.executeJavaScript(`(/* workass-browser-type-verify */ async () => {
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
          const el = document.querySelector(${selector});
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
          const exact = model !== null
            ? normalize(model) === normalize(expected)
            : (el.isContentEditable ? normalize(visible) === normalize(expected) : false);
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
    if (params.submit === true) submitted = (await this.submitBrowserType(entry, selectorText)).submitted === true;
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

  async browserControl(method, params = {}) {
    if (method === 'browser.list') return { tabs: this.browserTabs(params.chatId) };
    if (method === 'browser.open') {
      const entry = this.openConversation(params);
      if (params.url) await this.command(entry.chatId, 'navigate', params.url);
      return this.tabInfo(entry);
    }
    const entry = await this.controlEntry(params);
    const wc = entry.view.webContents;
    if (method === 'browser.navigate') {
      await this.command(entry.chatId, 'navigate', params.url);
      return this.tabInfo(entry);
    }
    if (method === 'browser.back') { await this.command(entry.chatId, 'back'); return this.tabInfo(entry); }
    if (method === 'browser.forward') { await this.command(entry.chatId, 'forward'); return this.tabInfo(entry); }
    if (method === 'browser.reload') { await this.command(entry.chatId, 'reload'); return this.tabInfo(entry); }
    if (method === 'browser.snapshot') {
      return wc.executeJavaScript(`(() => {
        const visible = (el) => { const r = el.getBoundingClientRect(); const s = getComputedStyle(el); return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'; };
        const esc = (v) => globalThis.CSS && CSS.escape ? CSS.escape(v) : String(v).replace(/[^a-zA-Z0-9_-]/g, '\\\\$&');
        const selector = (el) => {
          if (el.id) return '#' + esc(el.id);
          const parts = [];
          for (let n = el; n && n.nodeType === 1 && n !== document.documentElement; n = n.parentElement) {
            if (n.id) {
              parts.unshift('#' + esc(n.id));
              break;
            }
            let p = n.tagName.toLowerCase();
            const same = n.parentElement ? Array.from(n.parentElement.children).filter((x) => x.tagName === n.tagName) : [];
            if (same.length > 1) p += ':nth-of-type(' + (same.indexOf(n) + 1) + ')';
            parts.unshift(p);
          }
          return parts.join(' > ');
        };
        const readEditor = (root) => {
          let value = '';
          let valueRead = false;
          let readOnly = root.getAttribute('aria-readonly') === 'true';
          let kind = root.matches('.monaco-editor') ? 'monaco' : 'code-editor';
          try {
            if (kind === 'monaco') {
              const api = globalThis.monaco?.editor;
              const candidates = typeof api?.getEditors === 'function' ? api.getEditors() : [];
              const editor = candidates.find((candidate) => candidate?.getDomNode?.() === root);
              const model = editor?.getModel?.();
              if (model) {
                value = String(model.getValue());
                valueRead = true;
              }
              readOnly = readOnly || editor?.getRawOptions?.()?.readOnly === true;
            } else if (root.CodeMirror?.getValue) {
              value = String(root.CodeMirror.getValue());
              valueRead = true;
              readOnly = readOnly || root.CodeMirror.getOption?.('readOnly') === true;
            }
          } catch { /* visible lines remain available below */ }
          if (!valueRead) {
            const lines = root.querySelectorAll('.view-lines .view-line,.CodeMirror-code pre,.cm-content .cm-line');
            if (lines.length) value = Array.from(lines).map((line) => String(line.textContent || '')).join('\\n');
          }
          return {
            selector: selector(root), kind,
            text: value.slice(0, 12000), valueLength: value.length, truncated: value.length > 12000,
            focused: root.contains(document.activeElement), readOnly,
          };
        };
        const editorRoots = Array.from(document.querySelectorAll('.monaco-editor,.CodeMirror,.cm-editor')).filter(visible);
        const editorStates = new Map(editorRoots.map((root) => [root, readEditor(root)]));
        const nodes = Array.from(document.querySelectorAll('a,button,input,textarea,select,[role="button"],[role="textbox"],[contenteditable]:not([contenteditable="false"]),[tabindex],.monaco-editor,.CodeMirror,.cm-editor')).filter(visible).slice(0, 200);
        return {
          url: location.href,
          title: document.title,
          text: String(document.body?.innerText || '').slice(0, 12000),
          editors: editorRoots.map((root) => editorStates.get(root)),
          interactive: nodes.map((el) => {
            const bounds = el.getBoundingClientRect();
            const editorState = editorStates.get(el);
            return {
              selector: selector(el), tag: el.tagName.toLowerCase(),
              text: String(editorState?.text || el.innerText || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').trim().slice(0, 240),
              type: el.getAttribute('type') || null,
              role: el.getAttribute('role') || null,
              ariaLabel: el.getAttribute('aria-label') || null,
              focused: document.activeElement === el || el.contains(document.activeElement),
              disabled: !!el.disabled || el.getAttribute('aria-disabled') === 'true',
              readOnly: !!el.readOnly || el.getAttribute('aria-readonly') === 'true' || editorState?.readOnly === true,
              editable: !!el.isContentEditable || el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el.matches('.monaco-editor,.CodeMirror,.cm-editor'),
              editor: el.matches('.monaco-editor') ? 'monaco' : (el.matches('.CodeMirror,.cm-editor') ? 'code-editor' : null),
              valueLength: el instanceof HTMLInputElement && el.type === 'password'
                ? null
                : (typeof el.value === 'string' ? el.value.length : null),
              bounds: { x: Math.round(bounds.x), y: Math.round(bounds.y), width: Math.round(bounds.width), height: Math.round(bounds.height) },
            };
          }),
        };
      })()`);
    }
    if (method === 'browser.click') {
      const selector = JSON.stringify(String(params.selector || ''));
      return wc.executeJavaScript(`(() => { const el = document.querySelector(${selector}); if (!el) return { found:false }; el.scrollIntoView({block:'center',inline:'center'}); el.click(); return { found:true }; })()`);
    }
    if (method === 'browser.type') {
      return this.browserType(entry, params);
    }
    if (method === 'browser.scroll') {
      const x = Number.isFinite(Number(params.x)) ? Number(params.x) : 0;
      const y = Number.isFinite(Number(params.y)) ? Number(params.y) : 0;
      return wc.executeJavaScript(`(() => { window.scrollBy(${JSON.stringify(x)}, ${JSON.stringify(y)}); return { x: window.scrollX, y: window.scrollY }; })()`);
    }
    if (method === 'browser.key') {
      const parsed = parseBrowserKey(params.key, this.platform);
      await this.dispatchBrowserKey(entry, parsed);
      const selectAll = parsed.keyCode === 'A'
        && (parsed.modifiers.includes('meta') || parsed.modifiers.includes('control'))
        && !parsed.modifiers.includes('alt');
      const selection = selectAll ? await this.selectAllBrowserTarget(wc) : null;
      let deletion = null;
      if (parsed.keyCode === 'Backspace' || parsed.keyCode === 'Delete') {
        await wc.executeJavaScript('(/* workass-browser-key-barrier */ () => new Promise((resolve) => setTimeout(resolve, 0)))()');
        deletion = await this.deleteSelectedBrowserText(wc, parsed.keyCode);
      }
      return {
        sent: true, key: parsed.keyCode, modifiers: parsed.modifiers,
        ...(selection ? { selectionVerified: selection.selectionVerified === true, selectionStrategy: selection.strategy } : {}),
        ...(deletion?.found ? { deletionVerified: deletion.deletionVerified === true, deletionStrategy: deletion.strategy } : {}),
      };
    }
    if (method === 'browser.batch') {
      const actions = Array.isArray(params.actions) ? params.actions : [];
      if (!actions.length || actions.length > 20) throw new Error('browser batch requires 1-20 actions');
      const results = [];
      for (const action of actions) {
        const name = String(action && action.action || '');
        if (!['click', 'type', 'scroll', 'key', 'snapshot'].includes(name)) throw new Error(`unsupported browser batch action: ${name}`);
        results.push(await this.browserControl(`browser.${name}`, { ...action, tabId: this.tabInfo(entry).id }));
      }
      return { results };
    }
    if (method === 'browser.screenshot') {
      const image = await wc.capturePage();
      const png = image && typeof image.toPNG === 'function' ? image.toPNG() : null;
      // A tab is a child view of one window, so only the visible one has a
      // surface to copy pixels from. capturePage() on any other tab resolves
      // with an EMPTY image rather than rejecting, which reached the caller as
      // "returned no image" with nothing in it to act on.
      if (!png || png.length === 0) throw new Error(this.captureFailure(entry));
      return { mimeType: 'image/png', base64: png.toString('base64'), tab: this.tabInfo(entry) };
    }
    throw new Error(`unknown browser control method: ${String(method)}`);
  }

  close(chatId) {
    const id = safeChatId(chatId);
    const entry = this.entries.get(id);
    if (!entry) return false;
    if (this.attachedView === entry.view) this.hide(id);
    this.entries.delete(id);
    try { entry.view.webContents.close(); } catch { /* already gone */ }
    return true;
  }

  destroy() {
    for (const id of Array.from(this.entries.keys())) this.close(id);
    this.cdpListeners.clear();
    this.childSessions.clear();
  }
}

module.exports = { BrowserManager, DEFAULT_PARTITION, cleanUserAgent, normalizeBrowserURL, parseBrowserKey, resolveBrowserURL, safeBounds };
