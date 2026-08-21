'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

const PROGRESS_ARGUMENT = '--workass-update-progress';
const PROGRESS_RECEIPT_SCHEMA_VERSION = 1;
const TRANSACTION_SCHEMA_VERSION = 4;
const ACTIVE_PROGRESS_PHASES = new Set(['visible', 'watching']);
const TERMINAL_UPDATE_PHASES = new Set(['healthy', 'rollback_healthy', 'failed']);

function progressTransactionArgument(argv = process.argv) {
  const positions = [];
  for (let index = 0; index < argv.length; index += 1) {
    if (argv[index] === PROGRESS_ARGUMENT) positions.push(index);
  }
  if (positions.length === 0) return '';
  if (positions.length !== 1 || !path.isAbsolute(String(argv[positions[0] + 1] || ''))) {
    throw new Error('the Workass update progress transaction argument is invalid');
  }
  return path.resolve(argv[positions[0] + 1]);
}

function expectedProgressExecutable(transaction) {
  if (transaction.platform === 'darwin') {
    return path.join(transaction.incomingTarget, 'Contents', 'MacOS', 'Workass');
  }
  if (transaction.platform === 'win32') return path.join(transaction.incomingTarget, 'Workass.exe');
  throw new Error('the Workass update progress platform is unsupported');
}

function installedProgressExecutable(transaction) {
  if (transaction.platform === 'darwin') return path.join(transaction.installTarget, 'Contents', 'MacOS', 'Workass');
  if (transaction.platform === 'win32') return path.join(transaction.installTarget, 'Workass.exe');
  throw new Error('the Workass update progress platform is unsupported');
}

function progressExecutableIsAllowed(transaction, executable) {
  const candidate = path.resolve(String(executable || ''));
  if (candidate === path.resolve(expectedProgressExecutable(transaction))) return true;
  return transaction.platform === 'darwin' && Number.isInteger(transaction.recoveryAttempt) &&
    transaction.recoveryAttempt > 0 && candidate === path.resolve(installedProgressExecutable(transaction));
}

function progressOwnerReceiptPath(transaction) {
  return path.join(transaction.transactionRoot, 'progress-owner.json');
}

function validateProgressTransaction(transactionPath, transaction, {
  platform = process.platform,
  executablePath = process.execPath,
  progressId = process.env.WORKASS_UPDATE_PROGRESS_ID,
} = {}) {
  if (!transaction || transaction.schemaVersion !== TRANSACTION_SCHEMA_VERSION ||
      transaction.platform !== platform ||
      !/^[A-Za-z0-9_-]{8,96}$/.test(String(transaction.updateId || '')) ||
      !/^install-[a-f0-9]{32}$/.test(String(transaction.installationId || '')) ||
      !/^worker-[a-f0-9]{32}$/.test(String(transaction.workerId || '')) ||
      !/^progress-[a-f0-9]{32}$/.test(String(transaction.progressId || '')) ||
      transaction.progressId !== progressId) {
    throw new Error('the Workass update progress identity is invalid');
  }
  const root = path.resolve(String(transaction.transactionRoot || ''));
  const exactTransactionPath = path.join(root, 'transaction.json');
  if (path.resolve(transactionPath) !== exactTransactionPath ||
      transaction.transactionRoot !== root ||
      transaction.progressReceiptPath !== path.join(root, 'progress-receipt.json') ||
      transaction.journalPath !== path.join(root, 'journal.json') ||
      transaction.receiptPath !== path.join(path.dirname(path.dirname(root)), 'receipt.json') ||
      !progressExecutableIsAllowed(transaction, transaction.progressExecutable) ||
      path.resolve(executablePath) !== path.resolve(transaction.progressExecutable)) {
    throw new Error('the Workass update progress paths are invalid');
  }
  return transaction;
}

function atomicJSON(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const incoming = `${file}.incoming-${process.pid}`;
  const descriptor = fs.openSync(incoming, 'w', 0o600);
  try {
    fs.writeFileSync(descriptor, `${JSON.stringify(value, null, 2)}\n`, 'utf8');
    fs.fsyncSync(descriptor);
  } finally {
    fs.closeSync(descriptor);
  }
  fs.renameSync(incoming, file);
}

function exactJournal(transaction, value) {
  if (!value || value.schemaVersion !== 1 || value.updateId !== transaction.updateId ||
      value.installationId !== transaction.installationId ||
      path.resolve(String(value.installTarget || '')) !== path.resolve(transaction.installTarget) ||
      value.previousVersion !== transaction.currentVersion || value.targetVersion !== transaction.targetVersion) return null;
  return value;
}

function exactUpdateReceipt(transaction, value) {
  if (!value || value.schemaVersion !== 2 || value.updateId !== transaction.updateId ||
      value.installationId !== transaction.installationId || value.workerId !== transaction.workerId ||
      path.resolve(String(value.installTarget || '')) !== path.resolve(transaction.installTarget) ||
      value.previousVersion !== transaction.currentVersion || value.targetVersion !== transaction.targetVersion) return null;
  return value;
}

function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch { return null; }
}

function boundedUpdateError(value, maximum = 360) {
  const flattened = String(value || '').replace(/[\u0000-\u001f\u007f]+/g, ' ').replace(/\s+/g, ' ').trim();
  const redacted = flattened.replace(/\b(api[_-]?key|token|secret|password|credential|bearer)\b\s*[:=]\s*[^\s,;]+/gi, '$1=[redacted]');
  if (redacted.length <= maximum) return redacted;
  return `${redacted.slice(0, maximum - 1)}…`;
}

function phaseView(transaction, journal, receipt) {
  const terminal = receipt && TERMINAL_UPDATE_PHASES.has(receipt.phase) ? receipt :
    journal && TERMINAL_UPDATE_PHASES.has(journal.phase) ? journal : null;
  const state = terminal || journal || receipt || { phase: 'preparing' };
  const phase = String(state.phase || 'preparing');
  const common = { phase, terminal: Boolean(terminal), tone: 'active', action: '' };
  if (phase === 'healthy') return {
    ...common,
    tone: 'success',
    title: `Workass ${transaction.targetVersion} está listo`,
    detail: 'La actualización terminó correctamente. Workass ya volvió a abrirse.',
  };
  if (phase === 'rollback_healthy') return {
    ...common,
    tone: 'rollback',
    action: 'Cerrar',
    title: 'La actualización no pudo completarse',
    detail: `Workass ${transaction.currentVersion} fue restaurado y volvió a abrirse.${boundedUpdateError(state.error) ? ` ${boundedUpdateError(state.error)}` : ''}`,
  };
  if (phase === 'failed') return {
    ...common,
    tone: 'failed',
    action: 'Cerrar',
    title: 'La actualización falló',
    detail: boundedUpdateError(state.rollbackError || state.error) || 'Workass no pudo completar la actualización ni confirmar una recuperación saludable.',
  };
  const details = {
    preparing: 'Preparando una transacción verificable fuera de la aplicación.',
    armed: 'El actualizador independiente está listo.',
    shell_stopped: 'La ventana anterior se cerró. Tus chats permanecen guardados.',
    daemon_stopped: 'El servicio se detuvo de forma segura.',
    incoming_verified: 'La nueva versión fue verificada.',
    snapshotting_state: 'Protegiendo chats y configuración antes del cambio.',
    state_snapshotted: 'La copia de recuperación está lista.',
    activating: `Instalando Workass ${transaction.targetVersion}.`,
    activated: 'La nueva versión está instalada. Verificando que responda.',
    health_verified: 'Workass respondió correctamente. Terminando la actualización.',
    rollback_started: 'La nueva versión no pasó la verificación. Restaurando la versión anterior.',
    rolled_back: 'La versión anterior fue restaurada. Recuperando tus datos.',
    restoring_state: 'Restaurando chats y configuración.',
    state_restored: 'Los datos fueron restaurados. Reabriendo Workass.',
    admission_fence_pending: 'Esperando que el servicio confirme un cierre seguro.',
    daemon_fence_cleared: 'El cierre seguro fue cancelado. Reabriendo Workass.',
  };
  const rollingBack = phase.includes('rollback') || phase === 'restoring_state' || phase === 'state_restored';
  return {
    ...common,
    tone: rollingBack ? 'rollback' : 'active',
    title: rollingBack ? 'Restaurando Workass…' : 'Actualizando Workass…',
    detail: details[phase] || 'La actualización sigue en curso. Esta ventana se cerrará cuando Workass vuelva a estar listo.',
  };
}

function progressHTML() {
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'"><title>Workass Update</title><style>
    :root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#12110f;color:#f2efe9;font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;display:grid;place-items:center;height:100vh}
    main{width:min(520px,calc(100vw - 56px));padding:34px;border:1px solid #3b3732;border-radius:18px;background:#1c1a18;box-shadow:0 24px 70px #0009}header{display:flex;align-items:center;gap:12px}.mark{width:14px;height:14px;border-radius:99px;background:#4fa583;box-shadow:0 0 0 6px #4fa58324}.rollback .mark{background:#d8a05a;box-shadow:0 0 0 6px #d8a05a24}.failed .mark{background:#d96b5f;box-shadow:0 0 0 6px #d96b5f24}.success .mark{background:#4cae7f;box-shadow:0 0 0 6px #4cae7f24}h1{font-size:22px;line-height:1.2;margin:0}p{margin:18px 0 0;color:#beb8af;min-height:46px}.track{height:3px;margin-top:24px;border-radius:99px;background:#302d29;overflow:hidden}.bar{width:34%;height:100%;border-radius:inherit;background:#4fa583;animation:move 1.25s ease-in-out infinite}.rollback .bar{background:#d8a05a}.failed .bar{background:#d96b5f;animation:none;width:100%}.success .bar{background:#4cae7f;animation:none;width:100%}button{display:none;margin:24px 0 0 auto;border:1px solid #5b554e;border-radius:9px;padding:8px 16px;background:#292622;color:#f2efe9;font:inherit;cursor:pointer}button.visible{display:block}@keyframes move{0%{transform:translateX(-110%)}50%{transform:translateX(190%)}100%{transform:translateX(-110%)}}</style></head><body><main id="card"><header><span class="mark"></span><h1 id="title">Actualizando Workass…</h1></header><p id="detail">Preparando el actualizador independiente.</p><div class="track"><div class="bar"></div></div><button id="close" type="button">Cerrar</button></main><script>
    window.__workassRender=(view)=>{const card=document.getElementById('card');card.className=view.tone||'active';document.getElementById('title').textContent=view.title;document.getElementById('detail').textContent=view.detail;const button=document.getElementById('close');button.textContent=view.action||'Cerrar';button.className=view.action?'visible':'';button.onclick=()=>window.close()};
  </script></body></html>`;
}

async function runUpdateProgressProcess({
  app,
  BrowserWindow,
  transactionPath,
  platform = process.platform,
  executablePath = process.execPath,
  progressId = process.env.WORKASS_UPDATE_PROGRESS_ID,
  windowIcon = '',
  schedule = setTimeout,
  cancelSchedule = clearTimeout,
  repeat = setInterval,
  cancelRepeat = clearInterval,
  now = Date.now,
  terminalAckTimeoutMs = 5000,
} = {}) {
  const transaction = validateProgressTransaction(
    transactionPath,
    JSON.parse(fs.readFileSync(transactionPath, 'utf8')),
    { platform, executablePath, progressId },
  );
  const progressProfile = path.join(transaction.transactionRoot, 'progress-profile');
  fs.mkdirSync(progressProfile, { recursive: true, mode: 0o700 });
  app.setPath('userData', progressProfile);
  app.setName('Workass Update');
  await app.whenReady();

  const win = new BrowserWindow({
    width: 680,
    height: 400,
    minWidth: 560,
    minHeight: 340,
    backgroundColor: '#12110f',
    title: 'Workass Update',
    show: false,
    frame: true,
    autoHideMenuBar: true,
    resizable: true,
    ...(windowIcon ? { icon: windowIcon } : {}),
    webPreferences: { contextIsolation: true, sandbox: true, nodeIntegration: false },
  });

  let terminal = false;
  let terminalObserved = false;
  let disposed = false;
  let pollTimer = null;
  let closeTimer = null;
  let terminalAckTimer = null;
  let displayedPhase = 'preparing';
  let desiredView = null;
  let desiredRevision = 0;
  let renderInFlight = false;
  let readySettled = false;
  let resolveReady;
  let rejectReady;
  const ready = new Promise((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });
  const writeProgress = (phase, patch = {}) => atomicJSON(transaction.progressReceiptPath, {
    schemaVersion: PROGRESS_RECEIPT_SCHEMA_VERSION,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    workerId: transaction.workerId,
    progressId: transaction.progressId,
    pid: process.pid,
    executablePath: path.resolve(executablePath),
    transactionPath,
    phase,
    displayedPhase,
    windowVisible: !win.isDestroyed() && win.isVisible(),
    updatedAt: new Date(now()).toISOString(),
    ...patch,
  });
  const stopWatching = () => {
    if (pollTimer) cancelRepeat(pollTimer);
    pollTimer = null;
  };
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    stopWatching();
    if (closeTimer) cancelSchedule(closeTimer);
    if (terminalAckTimer) cancelSchedule(terminalAckTimer);
    closeTimer = null;
    terminalAckTimer = null;
  };
  const finishTerminal = (view) => {
    if (terminal) return;
    stopWatching();
    if (terminalAckTimer) cancelSchedule(terminalAckTimer);
    terminalAckTimer = null;
    writeProgress('terminal', { result: view.phase, windowVisible: !win.isDestroyed() && win.isVisible() });
    terminal = true;
    if (view.phase === 'healthy') {
      closeTimer = schedule(() => {
        closeTimer = null;
        if (!win.isDestroyed()) win.close();
        else app.quit();
      }, 1400);
      closeTimer?.unref?.();
    }
  };
  const failUI = (error) => {
    if (disposed) return;
    stopWatching();
    if (!readySettled) {
      readySettled = true;
      rejectReady(error instanceof Error ? error : new Error(String(error || 'update progress UI failed')));
    }
    try { writeProgress('lost', { windowVisible: false }); } catch {}
    if (!win.isDestroyed()) win.destroy();
    else app.quit();
  };
  const pump = () => {
    if (disposed || renderInFlight || !desiredView) return;
    const view = desiredView;
    const revision = desiredRevision;
    renderInFlight = true;
    let execution;
    try {
      execution = win.webContents.executeJavaScript(`window.__workassRender(${JSON.stringify(view)})`);
    } catch (err) {
      renderInFlight = false;
      failUI(err);
      return;
    }
    Promise.resolve(execution).then(() => {
      if (disposed) return;
      displayedPhase = view.phase;
      if (!readySettled) {
        writeProgress('visible');
        readySettled = true;
        resolveReady();
      } else if (!terminal) {
        writeProgress('watching');
      }
      if (view.terminal) finishTerminal(view);
    }).catch(failUI).finally(() => {
      renderInFlight = false;
      if (!disposed && desiredRevision > revision) pump();
    });
  };
  const render = () => {
    const journal = exactJournal(transaction, readJSON(transaction.journalPath));
    const receipt = exactUpdateReceipt(transaction, readJSON(transaction.receiptPath));
    const view = phaseView(transaction, journal, receipt);
    desiredView = view;
    desiredRevision += 1;
    if (view.terminal && !terminalObserved) {
      terminalObserved = true;
      stopWatching();
      terminalAckTimer = schedule(() => {
        terminalAckTimer = null;
        failUI(new Error('the terminal update result could not be rendered'));
      }, terminalAckTimeoutMs);
      terminalAckTimer?.unref?.();
    }
    pump();
    return view;
  };

  win.on('close', (event) => {
    if (!terminal) event.preventDefault();
  });
  win.on('closed', () => {
    dispose();
    try { writeProgress(terminal ? 'closed' : 'lost', { windowVisible: false }); } catch {}
    app.quit();
  });
  win.webContents.setWindowOpenHandler?.(() => ({ action: 'deny' }));
  try {
    await win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(progressHTML())}`);
    win.show();
    win.focus();
    if (!win.isVisible()) throw new Error('the Workass update progress window did not become visible');
  } catch (err) {
    dispose();
    try { writeProgress('lost', { windowVisible: false }); } catch {}
    if (!win.isDestroyed()) win.destroy();
    else app.quit();
    throw err;
  }
  pollTimer = repeat(() => {
    if (terminalObserved || terminal || win.isDestroyed() || !win.isVisible()) return;
    render();
  }, 250);
  pollTimer?.unref?.();
  app.on('before-quit', dispose);
  render();
  await ready;
  return { transaction, window: win, dispose, render };
}

function progressReceiptMatches(receipt, transaction) {
  if (!receipt || receipt.schemaVersion !== PROGRESS_RECEIPT_SCHEMA_VERSION ||
      receipt.updateId !== transaction?.updateId || receipt.installationId !== transaction?.installationId ||
      receipt.workerId !== transaction?.workerId || receipt.progressId !== transaction?.progressId ||
      receipt.transactionPath !== path.join(transaction.transactionRoot, 'transaction.json') ||
      !progressExecutableIsAllowed(transaction, transaction.progressExecutable) ||
      path.resolve(String(receipt.executablePath || '')) !== path.resolve(transaction.progressExecutable) ||
      !Number.isInteger(receipt.pid) || receipt.pid <= 1) return false;
  return true;
}

function progressReceiptProcessIsRunning(receipt, transaction, {
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
} = {}) {
  return progressReceiptMatches(receipt, transaction) &&
    ['starting', 'visible', 'watching', 'terminal'].includes(receipt.phase) && alive(receipt.pid);
}

function progressReceiptIsLive(receipt, transaction, {
  now = Date.now,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
  maximumHeartbeatAgeMs = 5000,
} = {}) {
  if (!progressReceiptMatches(receipt, transaction) || receipt.windowVisible !== true ||
      !ACTIVE_PROGRESS_PHASES.has(receipt.phase)) return false;
  const updatedAt = Date.parse(String(receipt.updatedAt || ''));
  return Number.isFinite(updatedAt) && now() - updatedAt >= 0 && now() - updatedAt <= maximumHeartbeatAgeMs && alive(receipt.pid);
}

function progressProcessOwnership(receipt, transaction, {
  platform = transaction?.platform || process.platform,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
  groupAlive = (pid) => {
    try { process.kill(-pid, 0); return true; } catch { return false; }
  },
  run = spawnSync,
} = {}) {
  const pid = Number(receipt?.pid);
  if (!Number.isInteger(pid) || pid <= 1) {
    return { running: false, exact: false, ambiguous: false };
  }
  const mainRunning = alive(pid);
  const orphanedGroup = platform !== 'win32' && !mainRunning && groupAlive(pid);
  if (!mainRunning && !orphanedGroup) return { running: false, exact: false, ambiguous: false };
  if (!progressReceiptMatches(receipt, transaction)) {
    return { running: true, exact: false, ambiguous: true, pid };
  }
  if (orphanedGroup) return { running: true, exact: true, ambiguous: false, orphanedGroup: true, pid };
  let result;
  if (platform === 'win32') {
    result = run('powershell.exe', [
      '-NoProfile', '-NonInteractive', '-Command',
      '$p = Get-CimInstance Win32_Process -Filter (\'ProcessId = \' + $env:WORKASS_PROGRESS_PID); if ($null -eq $p) { exit 3 }; [Console]::Out.Write([string]$p.CommandLine)',
    ], {
      windowsHide: true,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      env: { ...process.env, WORKASS_PROGRESS_PID: String(pid) },
    });
  } else {
    result = run('/bin/ps', ['-p', String(pid), '-o', 'command='], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'],
    });
  }
  if (result.error || result.status !== 0) {
    return { running: true, exact: false, ambiguous: true, inspectionFailed: true, pid };
  }
  const command = platform === 'win32'
    ? String(result.stdout || '').toLocaleLowerCase('en-US')
    : String(result.stdout || '');
  const normalize = platform === 'win32'
    ? (value) => String(value || '').toLocaleLowerCase('en-US')
    : (value) => String(value || '');
  const exact = command.includes(normalize(transaction.progressExecutable)) &&
    command.includes(normalize(PROGRESS_ARGUMENT)) &&
    command.includes(normalize(path.join(transaction.transactionRoot, 'transaction.json')));
  return { running: true, exact, ambiguous: !exact, pid };
}

async function terminateUpdateProgress(owner, {
  platform = process.platform,
  run = spawnSync,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
  kill = process.kill,
  groupAlive = (pid) => {
    try { process.kill(-pid, 0); return true; } catch { return false; }
  },
  pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
} = {}) {
  if (!owner || owner.exact !== true || !Number.isInteger(owner.pid) || owner.pid <= 1) return false;
  if (!alive(owner.pid) && (platform === 'win32' || !groupAlive(owner.pid))) return true;
  if (platform === 'win32') {
    const result = run('taskkill.exe', ['/PID', String(owner.pid), '/T', '/F'], {
      windowsHide: true,
      stdio: 'ignore',
    });
    if ((result.error || result.status !== 0) && alive(owner.pid)) return false;
  } else {
    try { kill(-owner.pid, 'SIGTERM'); } catch {}
  }
  const gracefulAttempts = platform === 'win32' ? 50 : 30;
  for (let attempt = 0; attempt < gracefulAttempts; attempt += 1) {
    if (!alive(owner.pid) && (platform === 'win32' || !groupAlive(owner.pid))) return true;
    await pause(100);
  }
  if (platform !== 'win32') {
    try { kill(-owner.pid, 'SIGKILL'); } catch {}
    for (let attempt = 0; attempt < 20; attempt += 1) {
      if (!alive(owner.pid) && !groupAlive(owner.pid)) return true;
      await pause(100);
    }
  }
  return !alive(owner.pid) && (platform === 'win32' || !groupAlive(owner.pid));
}

function spawnVisibleUpdateProgress({
  command,
  args = [],
  options = {},
  transaction,
  timeoutMs = 45_000,
  pollIntervalMs = 50,
}, {
  spawnProcess = spawn,
  readReceipt = readJSON,
  writeReceipt = atomicJSON,
  schedule = setTimeout,
  cancelSchedule = clearTimeout,
  repeat = setInterval,
  cancelRepeat = clearInterval,
  now = Date.now,
  terminateChild = (child) => terminateUpdateProgress({ exact: true, pid: child?.pid }, {
    platform: transaction?.platform || process.platform,
  }),
} = {}) {
  if (!transaction || transaction.schemaVersion !== TRANSACTION_SCHEMA_VERSION ||
      !path.isAbsolute(String(command || '')) || command !== transaction.progressExecutable ||
      !progressExecutableIsAllowed(transaction, command) ||
      !path.isAbsolute(String(transaction.progressReceiptPath || '')) ||
      !Number.isFinite(timeoutMs) || timeoutMs <= 0 || !Number.isFinite(pollIntervalMs) || pollIntervalMs <= 0) {
    return Promise.reject(new Error('invalid visible update progress handoff'));
  }
  return new Promise((resolve, reject) => {
    let child = null;
    let timeout = null;
    let poller = null;
    let settled = false;
    let stopping = false;
    const clear = () => {
      if (timeout) cancelSchedule(timeout);
      if (poller) cancelRepeat(poller);
      timeout = null;
      poller = null;
    };
    const fail = async (reason) => {
      if (settled || stopping) return;
      stopping = true;
      clear();
      const error = reason instanceof Error ? reason : new Error(String(reason || 'the update progress window did not open'));
      let fenced = false;
      try { fenced = await terminateChild(child); } catch { fenced = false; }
      settled = true;
      if (!fenced) {
        const fenceError = new Error('the rejected update progress process tree could not be fenced');
        fenceError.code = 'WORKASS_UPDATE_PROGRESS_FENCE_FAILED';
        reject(fenceError);
        return;
      }
      reject(error);
    };
    const inspect = () => {
      if (settled || !child) return;
      const receipt = readReceipt(transaction.progressReceiptPath);
      if (!progressReceiptIsLive(receipt, transaction, {
        now,
        alive: (pid) => pid === child.pid && child.exitCode == null && child.signalCode == null,
      })) return;
      settled = true;
      clear();
      child.workassUpdateProgressReceipt = receipt;
      child.unref?.();
      resolve(child);
    };
    try { child = spawnProcess(command, args, options); }
    catch (err) { void fail(err); return; }
    if (!child || typeof child.once !== 'function' || !Number.isInteger(child.pid) || child.pid <= 1) {
      void fail(new Error('the update progress launch returned no process handle'));
      return;
    }
    child.once('error', (err) => { void fail(err); });
    child.once('exit', (code, signal) => {
      inspect();
      if (!settled) void fail(new Error(`the update progress process exited before becoming visible (${signal || code || 0})`));
    });
    try {
      writeReceipt(progressOwnerReceiptPath(transaction), {
        schemaVersion: PROGRESS_RECEIPT_SCHEMA_VERSION,
        updateId: transaction.updateId,
        installationId: transaction.installationId,
        workerId: transaction.workerId,
        progressId: transaction.progressId,
        pid: child.pid,
        executablePath: path.resolve(command),
        transactionPath: path.join(transaction.transactionRoot, 'transaction.json'),
        phase: 'starting',
        displayedPhase: '',
        windowVisible: false,
        updatedAt: new Date(now()).toISOString(),
      });
    } catch (err) {
      void fail(err);
      return;
    }
    child.once('spawn', () => {
      inspect();
      if (settled) return;
      poller = repeat(inspect, pollIntervalMs);
      timeout = schedule(() => { void fail(new Error('the update progress window did not become visibly ready')); }, timeoutMs);
    });
  });
}

module.exports = {
  PROGRESS_ARGUMENT,
  PROGRESS_RECEIPT_SCHEMA_VERSION,
  TRANSACTION_SCHEMA_VERSION,
  boundedUpdateError,
  expectedProgressExecutable,
  installedProgressExecutable,
  phaseView,
  progressHTML,
  progressReceiptIsLive,
  progressReceiptMatches,
  progressReceiptProcessIsRunning,
  progressProcessOwnership,
  progressExecutableIsAllowed,
  progressOwnerReceiptPath,
  progressTransactionArgument,
  runUpdateProgressProcess,
  spawnVisibleUpdateProgress,
  terminateUpdateProgress,
  validateProgressTransaction,
};
