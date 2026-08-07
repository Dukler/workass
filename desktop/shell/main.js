// workass shell — minimal Electron window over the always-on Go daemon.
// The daemon owns state/agents; the shell-owned loopback view server owns only
// renderer bytes so Electron can rebuild/restart without touching ACP turns.
// Legacy desktop/main.js is the old full Electron app and stays untouched.
const { app, BrowserWindow, WebContentsView, session, ipcMain, shell, nativeImage, dialog, Menu } = require('electron');
const fs = require('node:fs');
const net = require('node:net');
const path = require('node:path');
const { createViewServer } = require('./view-server');
const { BrowserManager } = require('./browser-manager');
const { BrowserControlServer } = require('./browser-control-server');
const { resolveRuntimeProfile } = require('./runtime-profile');
const { applyMacDockIcon } = require('./app-icon');
const { ensurePackagedDaemon, ensurePortableDaemon, restartDaemonAndRecover, restartPackagedDaemonAndRecover } = require('./runtime-bootstrap');
const { UpdateManager, resolveUpdateFeed } = require('./update-manager');
const { copyImageAt, installImageCopyMenu, openImageExternally } = require('./image-copy');
const { acquireProfileSingleton } = require('./profile-singleton');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const RUNTIME = resolveRuntimeProfile({
  env: process.env,
  isPackaged: app.isPackaged,
  resourcesPath: process.resourcesPath,
  repoRoot: REPO_ROOT,
});
fs.mkdirSync(RUNTIME.userDataDir, { recursive: true });
fs.mkdirSync(RUNTIME.runDir, { recursive: true });
app.setName(RUNTIME.appName);
app.setPath('userData', RUNTIME.userDataDir);
const ownsProfileInstance = acquireProfileSingleton({
  app,
  profile: RUNTIME.profile,
  dataRoot: RUNTIME.dataRoot,
  getWindows: () => BrowserWindow.getAllWindows(),
});
if (!ownsProfileInstance) {
  console.error(`[shell] profile=${RUNTIME.profile} already has a primary instance; duplicate exiting`);
  app.quit();
}

const DAEMON_URL = RUNTIME.daemonURL;
const RECOVER_CONTROLLER = process.env.WORKASS_CONTROLLER_RECOVERY === '1';
const RENDERER_DIR = process.env.WORKASS_RENDERER_DIR || (app.isPackaged
  ? path.join(process.resourcesPath, 'renderer')
  : path.join(__dirname, '..', 'renderer2', 'dist'));
const VIEW_PORT = RUNTIME.viewPort;
const APP_VERSION = app.isPackaged ? app.getVersion() : 'development';
let viewServer = null;
let browserManager = null;
let browserControlServer = null;
let updateManager = null;
const externalImageTempDirs = new Set();

function sourceDaemonExecutable() {
  if (app.isPackaged) return '';
  const goArch = process.arch === 'x64' ? 'amd64' : process.arch;
  const suffix = process.platform === 'win32' ? 'windows-amd64.exe' : `${process.platform}-${goArch}`;
  const candidate = path.join(REPO_ROOT, 'dist-bin', `workass-${suffix}`);
  return fs.existsSync(candidate) ? candidate : '';
}

async function recoverLocalDaemon() {
  if (app.isPackaged && process.platform === 'darwin') {
    return restartPackagedDaemonAndRecover({
      runtime: RUNTIME,
      resourcesPath: process.resourcesPath,
    });
  }
  return restartDaemonAndRecover({
    runtime: RUNTIME,
    resourcesPath: process.resourcesPath,
    executablePath: process.execPath,
    daemonExecutable: sourceDaemonExecutable(),
  });
}

function privateLANHost(hostname) {
  const host = String(hostname || '').replace(/^\[|\]$/g, '');
  if (host === 'localhost' || host === '::1') return true;
  if (net.isIP(host) !== 4) return false;
  const [a, b] = host.split('.').map(Number);
  return a === 10 || a === 127 || (a === 192 && b === 168) || (a === 172 && b >= 16 && b <= 31);
}

// Workass daemons use persistent self-signed certificates rather than a public
// CA: a LAN daemon is reached by its discovery IP, not a public DNS name. Keep
// Chromium's normal verification everywhere except private/loopback Workass
// endpoints, where the remote pairing flow pins the daemon identity. This never
// permits an arbitrary public site's invalid certificate.
function allowPrivateWorkassCertificates() {
  try {
    session.defaultSession.setCertificateVerifyProc((request, callback) => {
      const invalidAuthority = request.verificationResult === 'net::ERR_CERT_AUTHORITY_INVALID';
      callback(privateLANHost(request.hostname) && invalidAuthority ? 0 : -3);
    });
  } catch (err) {
    console.error(`[shell] certificate verifier unavailable: ${err.message}`);
  }
}

// Dictation needs the microphone, and nothing in this shell needs any other
// permission. Electron grants every request when no handler is installed, so
// installing one narrows rather than widens: audio is allowed, the rest — the
// camera, geolocation, notifications from a page — is refused.
//
// macOS gates the device again underneath this against the bundle's
// NSMicrophoneUsageDescription. Granting here does not bypass the system
// prompt; it only decides whether Chromium asks for the device at all.
function grantMicrophoneOnly() {
  try {
    session.defaultSession.setPermissionRequestHandler((_contents, permission, callback) => {
      callback(permission === 'media' || permission === 'audioCapture');
    });
    session.defaultSession.setPermissionCheckHandler((_contents, permission) =>
      permission === 'media' || permission === 'audioCapture');
  } catch (err) {
    console.error(`[shell] permission handler unavailable: ${err.message}`);
  }
}

function createWindow(url, browserReporter, isController) {
  const win = new BrowserWindow({
    width: 1440,
    height: 900,
    minWidth: 960,
    minHeight: 600,
    ...(process.platform === 'darwin'
      ? { titleBarStyle: 'hiddenInset', trafficLightPosition: { x: 14, y: 14 } }
      : { frame: false }),
    backgroundColor: '#151413',
    title: RUNTIME.appName,
    webPreferences: {
      contextIsolation: true,
      sandbox: true,
      preload: path.join(__dirname, 'preload.js'),
    },
  });
  browserManager = new BrowserManager({
    win,
    WebContentsView,
    session,
    chromeVersion: process.versions.chrome,
    requestOpen: (chatId) => {
      if (!win.isDestroyed()) win.webContents.send('workass-browser:open-request', chatId || null);
    },
    onState: (state) => {
      if (browserReporter) browserReporter(state);
      if (!win.isDestroyed()) win.webContents.send('workass-browser:state', state);
    },
  });
  browserControlServer = new BrowserControlServer({
    manager: browserManager,
    isController,
    controlFile: RUNTIME.browserControlFile,
  });
  void browserControlServer.start().then((control) => {
    browserManager.setAgentControlReady(true);
    console.error(`[shell] provider-neutral browser control ready (${control.controlFile})`);
  }).catch((err) => {
    browserManager.setAgentControlReady(false);
    console.error(`[shell] provider-neutral browser control unavailable: ${err.message}`);
  });
  const own = (event) => event.sender === win.webContents;
  const removeImageCopyMenu = installImageCopyMenu({ win, Menu });
  ipcMain.handle('workass-browser:activate', (event, payload) => own(event) ? browserManager.activate(payload || {}) : null);
  ipcMain.handle('workass-browser:resize', (event, payload) => own(event) ? browserManager.resize(payload && payload.chatId, payload && payload.bounds) : false);
  ipcMain.handle('workass-browser:hide', (event, chatId) => own(event) ? browserManager.hide(chatId) : false);
  ipcMain.handle('workass-browser:close', (event, chatId) => own(event) ? browserManager.close(chatId) : false);
  ipcMain.handle('workass-browser:command', (event, payload) => own(event) ? browserManager.command(payload && payload.chatId, payload && payload.command, payload && payload.value) : null);
  ipcMain.handle('workass-clipboard:copy-image-at', (event, payload) => own(event) ? copyImageAt(win, payload) : false);
  ipcMain.handle('workass-image:open-external', async (event, payload) => {
    if (!own(event)) return false;
    const result = await openImageExternally(payload, { fs, path, tempRoot: app.getPath('temp'), shell });
    if (result.opened && result.cleanupPath) externalImageTempDirs.add(result.cleanupPath);
    return result.opened;
  });
  ipcMain.handle('workass-window:control', (event, action) => {
    if (!own(event)) return false;
    if (action === 'minimize') { win.minimize(); return true; }
    if (action === 'toggle-maximize') {
      if (win.isMaximized()) win.unmaximize(); else win.maximize();
      return true;
    }
    if (action === 'close') { win.close(); return true; }
    return false;
  });
	ipcMain.handle('workass-recovery:restart-daemon', async (event) => {
		if (!own(event)) return { ok: false, error: 'untrusted renderer' };
		try { return { ok: true, ...(await recoverLocalDaemon()) }; }
		catch (err) { return { ok: false, error: String(err && err.message || err) }; }
	});
  ipcMain.handle('workass-updater:get-state', (event) => own(event) ? updateManager?.snapshot() || null : null);
  ipcMain.handle('workass-updater:check', async (event) => own(event) ? updateManager?.check() || null : null);
  ipcMain.handle('workass-updater:download', async (event) => own(event) ? updateManager?.download() || null : null);
  ipcMain.handle('workass-updater:install', async (event) => own(event) ? updateManager?.install() || null : null);
  win.on('closed', () => {
    removeImageCopyMenu();
    if (browserControlServer) void browserControlServer.close();
    browserControlServer = null;
    if (browserManager) browserManager.destroy();
    browserManager = null;
    for (const channel of ['activate', 'resize', 'hide', 'close', 'command']) {
      try { ipcMain.removeHandler(`workass-browser:${channel}`); } catch { /* ignore */ }
    }
    try { ipcMain.removeHandler('workass-clipboard:copy-image-at'); } catch { /* ignore */ }
    try { ipcMain.removeHandler('workass-image:open-external'); } catch { /* ignore */ }
    try { ipcMain.removeHandler('workass-window:control'); } catch { /* ignore */ }
		try { ipcMain.removeHandler('workass-recovery:restart-daemon'); } catch { /* ignore */ }
    for (const channel of ['get-state', 'check', 'download', 'install']) {
      try { ipcMain.removeHandler(`workass-updater:${channel}`); } catch { /* ignore */ }
    }
  });
  win.loadURL(url);
  win.webContents.on('did-finish-load', async () => {
    if (updateManager) win.webContents.send('workass-updater:state', updateManager.snapshot());
    try {
      await browserManager.probe();
      const receipt = await win.webContents.executeJavaScript(`({
        browserBridge: !!(window.workassBrowser && window.workassBrowser.supported),
        browserCard: !!document.querySelector('.live-browser'),
        rootClass: document.getElementById('root')?.className || ''
      })`);
      console.error(`[shell] browser bridge receipt ${JSON.stringify(receipt)}`);
    } catch (err) {
      console.error(`[shell] browser bridge receipt failed: ${err.message}`);
    }
  });
  win.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url);
    return { action: 'deny' };
  });
  // Dev screenshot: hand the view server a capture fn bound to THIS window so
  // GET /__workass-shell/screenshot returns a PNG of the user's real view.
  // Captures at the window's current content size (i.e. maximized if they are).
  if (viewServer && typeof viewServer.setCapture === 'function') {
    viewServer.setCapture(async (opts) => {
      if (win.isDestroyed()) throw new Error('window destroyed');
      // Optional pre-capture click (a CSS selector) to drive the UI into a state
      // worth reviewing (e.g. the rail toggle). Best-effort; a missing element
      // is a no-op. Settle briefly so React re-render + CSS transitions finish.
      const click = opts && typeof opts.click === 'string' ? opts.click : null;
      const event = opts && typeof opts.event === 'string' ? opts.event : 'click';
      const target = opts && typeof opts.target === 'string' ? opts.target : null;
      const value = opts && typeof opts.value === 'string' ? opts.value : '';
      if (click) {
        await win.webContents.executeJavaScript(
          `(async () => {
            const el = document.querySelector(${JSON.stringify(click)}); if (!el) return false;
            const ev = ${JSON.stringify(event)};
            const pause = (ms) => new Promise((r) => setTimeout(r, ms));
            if (ev === 'dblclick') el.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, cancelable: true, view: window }));
            else if (ev === 'contextmenu') { const r = el.getBoundingClientRect(); el.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, view: window, clientX: Math.round(r.left + 20), clientY: Math.round(r.top + 8) })); }
            else if (ev === 'input') {
              el.focus();
              const value = ${JSON.stringify(value)};
              const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
              const setter = Object.getOwnPropertyDescriptor(proto, 'value')?.set;
              if (!setter) return false;
              setter.call(el, value);
              el.dispatchEvent(new Event('input', { bubbles: true, cancelable: true }));
            }
            else if (ev === 'key') {
              // "cmd+," / "ctrl+shift+k" — without modifiers a shortcut like ⌘,
              // cannot be driven at all, so its surface can never be screenshotted.
              const raw = ${JSON.stringify(value)};
              const parts = raw.split('+');
              const key = parts.pop() || raw;
              const mod = (n) => parts.some((p) => p.toLowerCase() === n);
              el.focus();
              el.dispatchEvent(new KeyboardEvent('keydown', {
                key, bubbles: true, cancelable: true,
                metaKey: mod('cmd') || mod('meta'), ctrlKey: mod('ctrl'),
                shiftKey: mod('shift'), altKey: mod('alt'),
              }));
            }
            else if (ev === 'dragto') {
              // Full HTML5 drag sequence with pauses so React re-renders between
              // dragstart (sets drag state) and drop (reads it).
              const tgt = document.querySelector(${JSON.stringify(target)}); if (!tgt) return false;
              const dt = new DataTransfer();
              const mk = (type, node) => { const r = node.getBoundingClientRect(); return new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt, view: window, clientX: Math.round(r.left + 10), clientY: Math.round(r.top + r.height / 2) }); };
              el.dispatchEvent(mk('dragstart', el)); await pause(70);
              tgt.dispatchEvent(mk('dragenter', tgt)); tgt.dispatchEvent(mk('dragover', tgt)); await pause(70);
              tgt.dispatchEvent(mk('drop', tgt)); el.dispatchEvent(mk('dragend', el));
            }
            else el.click();
            return true;
          })()`,
        ).catch(() => {});
        await new Promise((resolve) => setTimeout(resolve, 420));
      }
      const image = await win.webContents.capturePage();
      return image.toPNG();
    });
  }
  if (viewServer && typeof viewServer.setProbe === 'function') {
    viewServer.setProbe(async (selector) => {
      if (win.isDestroyed()) throw new Error('window destroyed');
      return win.webContents.executeJavaScript(`(() => {
        const q = ${JSON.stringify(selector)};
        const els = Array.from(document.querySelectorAll(q)).slice(0, 40).map((el) => {
          const r = el.getBoundingClientRect(); const s = getComputedStyle(el);
          return { cls: String(el.className || ''), title: el.getAttribute('title'),
            x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height),
            display: s.display, visibility: s.visibility, opacity: s.opacity };
        });
        const root = document.getElementById('root'); const main = document.querySelector('main');
        return { innerWidth: window.innerWidth, rootClass: root ? root.className : null,
          mainClientW: main ? main.clientWidth : null, mainScrollW: main ? main.scrollWidth : null,
          count: els.length, els };
      })()`);
    });
  }
  if (viewServer && typeof viewServer.setReload === 'function') {
    viewServer.setReload(async ({ recoverController } = {}) => {
      if (win.isDestroyed()) throw new Error('window destroyed');
      // Clearing the controller-migration marker is what lets the next load
      // re-take a lease stranded on a dead device identity — the same step the
      // renderer's ⌘, "Recargar" performs. Without it this is only an F5.
      if (recoverController) {
        await win.webContents.executeJavaScript(
          `(() => { try { localStorage.removeItem('workass.shell.controllerMigration.v1'); return true; } catch (_) { return false; } })()`,
        ).catch(() => {});
      }
      const url = win.webContents.getURL();
      // reloadIgnoringCache: a promoted index.html points at a new hashed bundle,
      // but a cached index.html would keep loading the old one forever.
      win.webContents.reloadIgnoringCache();
      return { reloaded: true, url, recoverController: recoverController === true };
    });
  }
	if (viewServer && typeof viewServer.setRecovery === 'function') {
		viewServer.setRecovery(recoverLocalDaemon);
	}
  if (viewServer && typeof viewServer.setPerf === 'function') {
    viewServer.setPerf(async (requestedAction) => {
      if (win.isDestroyed()) throw new Error('window destroyed');
      const action = ['start', 'read', 'stop'].includes(requestedAction) ? requestedAction : 'read';
      return win.webContents.executeJavaScript(`(() => {
        const action = ${JSON.stringify(action)};
        const summarize = (state) => {
          if (!state) return { running: false, durationMs: 0, frameCount: 0, p95FrameGapMs: 0, maxFrameGapMs: 0, over50ms: 0, longTaskCount: 0, maxLongTaskMs: 0, assistantCount: document.querySelectorAll('.amsg').length, toolRowCount: document.querySelectorAll('.evt').length };
          const gaps = state.frameGaps.slice().sort((a, b) => a - b);
          const at = (ratio) => gaps.length ? gaps[Math.min(gaps.length - 1, Math.floor(gaps.length * ratio))] : 0;
          return {
            running: state.running,
            durationMs: Math.round((performance.now() - state.startedAt) * 1000) / 1000,
            frameCount: state.frameGaps.length,
            p95FrameGapMs: Math.round(at(.95) * 1000) / 1000,
            maxFrameGapMs: Math.round((gaps[gaps.length - 1] || 0) * 1000) / 1000,
            over50ms: gaps.filter((gap) => gap > 50).length,
            longTaskCount: state.longTasks.length,
            maxLongTaskMs: Math.round((Math.max(0, ...state.longTasks)) * 1000) / 1000,
            assistantCount: document.querySelectorAll('.amsg').length,
            toolRowCount: document.querySelectorAll('.evt').length,
          };
        };
        if (action === 'start') {
          const prior = window.__workassPerf;
          if (prior?.frame) cancelAnimationFrame(prior.frame);
          try { prior?.observer?.disconnect(); } catch (_) {}
          const state = { running: true, startedAt: performance.now(), lastFrame: 0, frameGaps: [], longTasks: [], frame: 0, observer: null };
          const tick = (now) => {
            if (!state.running) return;
            if (state.lastFrame) {
              state.frameGaps.push(now - state.lastFrame);
              if (state.frameGaps.length > 20000) state.frameGaps.shift();
            }
            state.lastFrame = now;
            state.frame = requestAnimationFrame(tick);
          };
          try {
            state.observer = new PerformanceObserver((list) => {
              for (const entry of list.getEntries()) state.longTasks.push(entry.duration);
              if (state.longTasks.length > 2000) state.longTasks.splice(0, state.longTasks.length - 2000);
            });
            state.observer.observe({ entryTypes: ['longtask'] });
          } catch (_) {}
          state.frame = requestAnimationFrame(tick);
          window.__workassPerf = state;
          return summarize(state);
        }
        const state = window.__workassPerf;
        if (action === 'stop' && state) {
          state.running = false;
          if (state.frame) cancelAnimationFrame(state.frame);
          try { state.observer?.disconnect(); } catch (_) {}
        }
        return summarize(state);
      })()`);
    });
  }
  return win;
}

if (ownsProfileInstance) app.on('will-quit', () => {
  updateManager?.dispose();
  for (const dir of externalImageTempDirs) {
    try { fs.rmSync(dir, { recursive: true, force: true }); } catch { /* best effort */ }
  }
  externalImageTempDirs.clear();
});

if (ownsProfileInstance) app.whenReady().then(async () => {
  allowPrivateWorkassCertificates();
  grantMicrophoneOnly();
  if (app.isPackaged) {
    try {
      const daemonReceipt = process.platform === 'darwin'
        ? await ensurePackagedDaemon({ runtime: RUNTIME, resourcesPath: process.resourcesPath })
        : await ensurePortableDaemon({ runtime: RUNTIME, resourcesPath: process.resourcesPath, executablePath: process.execPath });
      console.error(`[shell] packaged daemon receipt ${JSON.stringify(daemonReceipt)}`);
      if (daemonReceipt.status === 'move-to-applications') {
        await dialog.showMessageBox({
          type: 'info',
          title: 'Move Workass to Applications',
          message: 'Move Workass to the Applications folder before opening it.',
          detail: 'The Workass background service needs a stable application path so it keeps working after logout and restart.',
        });
      }
    } catch (err) {
      console.error(`[shell] packaged daemon bootstrap failed: ${err.message}`);
    }
  }
  const iconReceipt = applyMacDockIcon({
    app,
    nativeImage,
    isPackaged: app.isPackaged,
    resourcesPath: process.resourcesPath,
    repoRoot: REPO_ROOT,
  });
  console.error(`[shell] dock icon receipt ${JSON.stringify(iconReceipt)}`);
  let viewURL = DAEMON_URL;
  try {
    viewServer = await createViewServer({
      daemonURL: DAEMON_URL,
      daemonCAPath: path.join(RUNTIME.stateDir, 'daemon-cert.pem'),
      rendererDir: RENDERER_DIR,
      port: VIEW_PORT,
      runtimeVersion: process.versions.electron,
      appVersion: APP_VERSION,
      recoverController: RECOVER_CONTROLLER,
    });
    viewURL = viewServer.url;
    console.error(`[shell] profile=${RUNTIME.profile} renderer ${RENDERER_DIR} -> ${viewURL}; daemon ${DAEMON_URL}`);
  } catch (err) {
    if (err && err.code === 'EADDRINUSE') {
      console.error(`[shell] profile=${RUNTIME.profile} view port ${VIEW_PORT} already has an owner; duplicate exiting`);
      app.quit();
      return;
    }
    console.error(`[shell] local renderer unavailable; falling back to daemon UI: ${err.message}`);
  }
  const browserReporter = viewServer && viewServer.reportBrowserState;
  const isController = viewServer && viewServer.isController;
  updateManager = new UpdateManager({
    app,
    runtime: RUNTIME,
    resourcesPath: process.resourcesPath,
    executablePath: process.execPath,
    currentVersion: APP_VERSION,
    platform: process.platform,
    arch: process.arch,
    isPackaged: app.isPackaged,
    feedURL: resolveUpdateFeed({
      channel: RUNTIME.updateChannel,
      dataRoot: RUNTIME.dataRoot,
      platform: process.platform,
      arch: process.arch,
    }),
    onState: (state) => {
      for (const window of BrowserWindow.getAllWindows()) {
        if (!window.isDestroyed()) window.webContents.send('workass-updater:state', state);
      }
    },
  });
  const updateState = updateManager.init();
  createWindow(viewURL, browserReporter, isController);
  // A rolled-back/failed transaction stays visible until the user explicitly
  // retries. An automatic check here would immediately re-offer the exact
  // release that just failed its health gates and hide the recovery receipt.
  if (updateState.supported && !['rollback_healthy', 'failed'].includes(updateState.phase)) {
    updateManager.startAutoChecks({
      // The private feed is one bounded local-file read, so dogfood releases
      // become visible promptly without restarting Electron. Public builds
      // retain a quiet hourly HTTPS cadence.
      intervalMs: RUNTIME.updateChannel === 'local' ? 30_000 : 60 * 60 * 1000,
    });
  }
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow(viewURL, browserReporter, isController);
  });
});

if (ownsProfileInstance) app.on('window-all-closed', () => app.quit());
if (ownsProfileInstance) app.on('before-quit', () => {
  if (browserControlServer) void browserControlServer.close();
  if (browserManager) browserManager.destroy();
  if (viewServer) void viewServer.close();
});
