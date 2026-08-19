'use strict';

// Windows can keep Chromium's profile singleton alive briefly after the old
// Electron main PID has begun shutting down. An incoming update launched in
// that interval must not just exit and leave Workass closed. This bounded
// bootstrap uses an isolated Chromium profile, keeps a responsive window on
// screen, and retries the real profile until exactly one primary owns it.

const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const path = require('node:path');
const { spawn } = require('node:child_process');

const DEFAULT_STATUS_URL = 'http://127.0.0.1:8798/__workass-shell/status';

function shouldStartUpdateLockRecovery({
  platform = process.platform,
  ownsProfileInstance = false,
  recoverController = false,
  retryChild = false,
} = {}) {
  return platform === 'win32' && ownsProfileInstance && recoverController && !retryChild;
}

function updateLockRecoveryUserData(dataRoot) {
  if (!path.isAbsolute(String(dataRoot || ''))) throw new Error('update recovery data root must be absolute');
  return path.join(dataRoot, 'run', 'update-lock-recovery');
}

function bootstrapHTML({ failed = false } = {}) {
  const title = failed ? 'Workass needs another launch' : 'Finishing the Workass update…';
  const detail = failed
    ? 'The update could not acquire the previous window lock. Close this window and open Workass again.'
    : 'Your projects and chats are safe. Workass will reopen automatically.';
  return `<!doctype html><meta charset="utf-8"><title>${title}</title><style>
    :root{color-scheme:dark}body{margin:0;background:#151413;color:#f3efe8;font:15px system-ui,-apple-system,"Segoe UI",sans-serif;display:grid;place-items:center;height:100vh}
    main{width:min(460px,calc(100vw - 64px));padding:36px;border:1px solid #393532;border-radius:18px;background:#1d1b19;box-shadow:0 24px 70px #0008}
    h1{font-size:22px;margin:0 0 12px}p{color:#bdb5ac;line-height:1.55;margin:0}.dot{display:inline-block;width:9px;height:9px;border-radius:99px;background:#e9a85d;margin-right:10px;box-shadow:0 0 0 5px #e9a85d22}
  </style><main><h1><span class="dot"></span>${title}</h1><p>${detail}</p></main>`;
}

function createUpdateBootstrapWindow({ BrowserWindow, windowIcon = '', failed = false } = {}) {
  if (typeof BrowserWindow !== 'function') throw new Error('BrowserWindow is required');
  const win = new BrowserWindow({
    width: 720,
    height: 420,
    minWidth: 620,
    minHeight: 360,
    backgroundColor: '#151413',
    title: 'Workass',
    show: true,
    frame: true,
    autoHideMenuBar: true,
    ...(windowIcon ? { icon: windowIcon } : {}),
    webPreferences: { contextIsolation: true, sandbox: true },
  });
  void win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(bootstrapHTML({ failed }))}`);
  win.show?.();
  win.focus?.();
  return win;
}

function requestShellStatus(url = DEFAULT_STATUS_URL, timeoutMs = 700) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL(url); } catch { resolve(null); return; }
    const transport = parsed.protocol === 'https:' ? https : http;
    let request;
    try {
      request = transport.get(parsed, {
        timeout: timeoutMs,
        rejectUnauthorized: !['127.0.0.1', 'localhost', '::1'].includes(parsed.hostname),
        headers: { 'cache-control': 'no-store' },
      }, (response) => {
        let body = '';
        response.setEncoding('utf8');
        response.on('data', (chunk) => { if (body.length < 65536) body += chunk; });
        response.on('end', () => {
          if ((response.statusCode || 500) >= 400) { resolve(null); return; }
          try { resolve(JSON.parse(body)); } catch { resolve(null); }
        });
      });
    } catch { resolve(null); return; }
    request.on('timeout', () => request.destroy());
    request.on('error', () => resolve(null));
  });
}

async function startWindowsUpdateLockRecovery({
  app,
  BrowserWindow,
  executablePath = process.execPath,
  dataRoot,
  appVersion,
  env = process.env,
  windowIcon = '',
  statusURL = DEFAULT_STATUS_URL,
  daemonHealthURL = '',
  readStatus = requestShellStatus,
  readDaemon = requestShellStatus,
  spawnProcess = spawn,
  schedule = setTimeout,
  cancelSchedule = clearTimeout,
  now = Date.now,
  retryIntervalMs = 2000,
  timeoutMs = 110000,
} = {}) {
  if (!app || !path.isAbsolute(String(dataRoot || '')) || !String(appVersion || '').trim() ||
      !String(daemonHealthURL || '').trim()) {
    throw new Error('Windows update lock recovery requires an app, data root, version, and daemon health URL');
  }
  const recoveryRoot = updateLockRecoveryUserData(dataRoot);
  fs.mkdirSync(recoveryRoot, { recursive: true, mode: 0o700 });
  app.setPath('userData', recoveryRoot);
  await app.whenReady();

  const win = createUpdateBootstrapWindow({ BrowserWindow, windowIcon });
  const startedAt = now();
  let retry = null;
  let timer = null;
  let stopped = false;
  let timedOut = false;
  let quitting = false;

  const quit = () => {
    if (quitting) return;
    quitting = true;
    app.quit();
  };

  const stop = () => {
    stopped = true;
    if (timer) cancelSchedule(timer);
    timer = null;
  };

  const poll = async () => {
    if (stopped) return false;
    const [status, daemon] = await Promise.all([
      readStatus(statusURL),
      readDaemon(daemonHealthURL),
    ]);
    if (daemon?.app === 'workass' && daemon?.version === appVersion &&
        status?.appVersion === appVersion && status?.controller === true && status?.windowVisible === true) {
      stop();
      try { win.close?.(); } catch { /* updater may already be stopping us */ }
      quit();
      return true;
    }

    if (now() - startedAt >= timeoutMs) {
      timedOut = true;
      stop();
      try { void win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(bootstrapHTML({ failed: true }))}`); } catch { /* keep existing bootstrap */ }
      return false;
    }

    if (!retry) {
      try {
        retry = spawnProcess(executablePath, [], {
          cwd: path.dirname(executablePath),
          // The helper exits as soon as this real-profile child is healthy.
          // Keep the child in an independent process group so Windows cannot
          // tear the recovered app down with the temporary helper/job tree.
          detached: true,
          windowsHide: false,
          stdio: 'ignore',
          env: {
            ...env,
            WORKASS_CONTROLLER_RECOVERY: '1',
            WORKASS_UPDATE_RELAUNCH: '1',
            WORKASS_LOCK_RECOVERY_CHILD: '1',
          },
        });
        retry.unref?.();
        const clearRetry = () => { retry = null; };
        retry.once?.('error', clearRetry);
        retry.once?.('exit', clearRetry);
      } catch { retry = null; }
    }
    timer = schedule(() => { void poll(); }, retryIntervalMs);
    timer?.unref?.();
    return false;
  };

  win.on?.('closed', () => {
    stop();
    quit();
  });
  await poll();
  return {
    recoveryRoot,
    window: win,
    poll,
    stop,
    get retryRunning() { return Boolean(retry); },
    get timedOut() { return timedOut; },
  };
}

module.exports = {
  DEFAULT_STATUS_URL,
  bootstrapHTML,
  createUpdateBootstrapWindow,
  requestShellStatus,
  shouldStartUpdateLockRecovery,
  startWindowsUpdateLockRecovery,
  updateLockRecoveryUserData,
};
