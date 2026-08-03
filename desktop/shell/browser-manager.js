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

class BrowserManager {
  constructor({ win, WebContentsView, session, partition = DEFAULT_PARTITION, chromeVersion, platform, onState, requestOpen }) {
    if (!win || !WebContentsView || !session) throw new Error('browser manager dependencies missing');
    this.win = win;
    this.WebContentsView = WebContentsView;
    this.partition = partition;
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
            let p = n.tagName.toLowerCase();
            const same = n.parentElement ? Array.from(n.parentElement.children).filter((x) => x.tagName === n.tagName) : [];
            if (same.length > 1) p += ':nth-of-type(' + (same.indexOf(n) + 1) + ')';
            parts.unshift(p);
          }
          return parts.join(' > ');
        };
        const nodes = Array.from(document.querySelectorAll('a,button,input,textarea,select,[role="button"],[tabindex]')).filter(visible).slice(0, 200);
        return {
          url: location.href,
          title: document.title,
          text: String(document.body?.innerText || '').slice(0, 12000),
          interactive: nodes.map((el) => ({
            selector: selector(el), tag: el.tagName.toLowerCase(),
            text: String(el.innerText || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').trim().slice(0, 240),
            type: el.getAttribute('type') || null,
          })),
        };
      })()`);
    }
    if (method === 'browser.click') {
      const selector = JSON.stringify(String(params.selector || ''));
      return wc.executeJavaScript(`(() => { const el = document.querySelector(${selector}); if (!el) return { found:false }; el.scrollIntoView({block:'center',inline:'center'}); el.click(); return { found:true }; })()`);
    }
    if (method === 'browser.type') {
      const selector = JSON.stringify(String(params.selector || ''));
      const value = JSON.stringify(String(params.text || ''));
      const submit = params.submit === true ? 'true' : 'false';
      return wc.executeJavaScript(`(() => { const el = document.querySelector(${selector}); if (!el) return { found:false }; el.focus(); el.value = ${value}; el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true})); if (${submit}) { const form = el.form; if (form?.requestSubmit) form.requestSubmit(); else el.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',code:'Enter',bubbles:true})); } return { found:true }; })()`);
    }
    if (method === 'browser.scroll') {
      const x = Number.isFinite(Number(params.x)) ? Number(params.x) : 0;
      const y = Number.isFinite(Number(params.y)) ? Number(params.y) : 0;
      return wc.executeJavaScript(`(() => { window.scrollBy(${JSON.stringify(x)}, ${JSON.stringify(y)}); return { x: window.scrollX, y: window.scrollY }; })()`);
    }
    if (method === 'browser.key') {
      const key = String(params.key || '').trim();
      if (!key) throw new Error('browser key is required');
      wc.sendInputEvent({ type: 'keyDown', keyCode: key });
      wc.sendInputEvent({ type: 'keyUp', keyCode: key });
      return { sent: true };
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

module.exports = { BrowserManager, cleanUserAgent, normalizeBrowserURL, resolveBrowserURL, safeBounds };
