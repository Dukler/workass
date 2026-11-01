'use strict';

// Which surface hosts the Workass browser page, and what a <webview> may be.
//
// 'view' (the default everywhere): every page is its own WebContentsView,
// stacked over the window when shown and otherwise kept in its own hidden,
// non-focusable window. A page then has a focus of its own: an agent clicking
// and typing in it never touches the app's focus or the person's composer.
// 'webview': the page is the guest of a <webview> in the app window, so it
// moves, resizes and stacks with the HTML around it. A guest shares the app
// window's focus: Chromium hands protocol keyboard input on its top frame to
// whatever has focus in the window (the person's composer), and any press in
// it moves the app's focus onto the page, whatever the page does with the
// event (2026-10-28, an agent's typing landed in the composer of the chat the
// person was in). Opt-in only: WORKASS_BROWSER_HOST=webview|view, or
// {"browserHost": "webview"|"view"} in shell-settings.json in the profile's
// data directory.

const path = require('node:path');

const HOSTS = new Set(['webview', 'view']);
const SETTINGS_FILE = 'shell-settings.json';

function resolveBrowserHost({ env = process.env, dataRoot = '', fs = require('node:fs') } = {}) {
  const fromEnv = String(env.WORKASS_BROWSER_HOST || '').trim().toLowerCase();
  if (HOSTS.has(fromEnv)) return { host: fromEnv, source: 'env' };
  if (dataRoot) {
    try {
      const raw = fs.readFileSync(path.join(dataRoot, SETTINGS_FILE), 'utf8');
      const value = String(JSON.parse(raw)?.browserHost || '').trim().toLowerCase();
      if (HOSTS.has(value)) return { host: value, source: 'settings' };
    } catch { /* absent or unreadable: the default applies */ }
  }
  return { host: 'view', source: 'default' };
}

// will-attach-webview: the only <webview> the app window may create is a
// blank Workass browser page in the browser's persistent profile, with no
// preload, no Node, sandboxed and isolated. Main loads every URL itself.
function hardenWebviewAttach(webPreferences, params, { partition }) {
  const requestedPartition = String(params && params.partition || webPreferences && webPreferences.partition || '');
  if (requestedPartition !== partition) return { allow: false, reason: 'partition' };
  const src = String(params && params.src || 'about:blank');
  if (src !== 'about:blank') return { allow: false, reason: 'src' };
  for (const key of ['preload', 'preloadURL']) delete webPreferences[key];
  if (params) {
    delete params.preload;
    params.src = 'about:blank';
  }
  Object.assign(webPreferences, {
    partition,
    nodeIntegration: false,
    nodeIntegrationInSubFrames: false,
    nodeIntegrationInWorker: false,
    contextIsolation: true,
    sandbox: true,
    webSecurity: true,
    allowRunningInsecureContent: false,
    experimentalFeatures: false,
    navigateOnDragDrop: false,
    webviewTag: false,
    plugins: false,
  });
  delete webPreferences.enableBlinkFeatures;
  delete webPreferences.disableBlinkFeatures;
  return { allow: true };
}

// What may leave the app for the OS. A link in a reply or a hosted page is one
// click from whatever handler its scheme names, and file:, smb: and search-ms:
// each launch something, so this is an allowlist. The URL parser removes what a
// browser would ignore ("fi\tle:"), so the check sees what the OS would.
const EXTERNAL_PROTOCOLS = new Set(['http:', 'https:', 'mailto:']);

function isSafeExternalURL(url) {
  if (typeof url !== 'string') return false;
  try { return EXTERNAL_PROTOCOLS.has(new URL(url).protocol); } catch { return false; }
}

// Returns whether the URL was handed to the OS. The launch itself is not awaited
// by any caller, so its failure is reported here rather than left unhandled.
function openExternalIfSafe(nativeShell, url, log = console.error) {
  if (!isSafeExternalURL(url)) {
    log(`[shell] refused to open a link with a disallowed scheme: ${String(url).slice(0, 200)}`);
    return Promise.resolve(false);
  }
  return Promise.resolve(nativeShell.openExternal(url)).then(() => true, (err) => {
    log(`[shell] could not open a link: ${err && err.message || err}`);
    return true;
  });
}

module.exports = { hardenWebviewAttach, isSafeExternalURL, openExternalIfSafe, resolveBrowserHost, SETTINGS_FILE };
