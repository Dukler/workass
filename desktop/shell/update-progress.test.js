'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  PROGRESS_ARGUMENT,
  boundedUpdateError,
  expectedProgressExecutable,
  installedProgressExecutable,
  phaseView,
  progressHTML,
  progressExecutableIsAllowed,
  progressOwnerReceiptPath,
  progressProcessOwnership,
  progressReceiptIsLive,
  progressReceiptProcessIsRunning,
  progressTransactionArgument,
  runUpdateProgressProcess,
  spawnVisibleUpdateProgress,
  terminateUpdateProgress,
  validateProgressTransaction,
} = require('./update-progress');

function progressFixture() {
  const dataRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-progress-'));
  const updateId = 'update-progress-1234';
  const transactionRoot = path.join(dataRoot, 'updates', 'transactions', updateId);
  const installTarget = path.join(dataRoot, 'Applications', 'Workass.app');
  const incomingTarget = path.join(path.dirname(installTarget), `.Workass.app.incoming-${updateId}`);
  const transaction = {
    schemaVersion: 4,
    updateId,
    platform: 'darwin',
    currentVersion: '1.1.0',
    targetVersion: '1.2.0',
    workerId: `worker-${'2'.repeat(32)}`,
    progressId: `progress-${'3'.repeat(32)}`,
    installationId: `install-${'1'.repeat(32)}`,
    transactionRoot,
    installTarget,
    incomingTarget,
    backupTarget: path.join(path.dirname(installTarget), `.Workass.app.previous-${updateId}`),
    receiptPath: path.join(dataRoot, 'updates', 'receipt.json'),
    journalPath: path.join(transactionRoot, 'journal.json'),
    progressReceiptPath: path.join(transactionRoot, 'progress-receipt.json'),
    progressExecutable: path.join(incomingTarget, 'Contents', 'MacOS', 'Workass'),
  };
  fs.mkdirSync(transactionRoot, { recursive: true });
  fs.mkdirSync(path.dirname(transaction.progressExecutable), { recursive: true });
  fs.writeFileSync(transaction.progressExecutable, 'app');
  const transactionPath = path.join(transactionRoot, 'transaction.json');
  fs.writeFileSync(transactionPath, `${JSON.stringify(transaction)}\n`);
  writeJournal(transaction, { phase: 'preparing' });
  return { dataRoot, transaction, transactionPath };
}

function writeJournal(transaction, overrides = {}) {
  const journal = {
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    phase: 'preparing',
    terminal: false,
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
  fs.mkdirSync(path.dirname(transaction.journalPath), { recursive: true });
  fs.writeFileSync(transaction.journalPath, `${JSON.stringify(journal)}\n`);
  return journal;
}

function exactProgressReceipt(transaction, overrides = {}) {
  return {
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    workerId: transaction.workerId,
    progressId: transaction.progressId,
    pid: 4321,
    executablePath: transaction.progressExecutable,
    transactionPath: path.join(transaction.transactionRoot, 'transaction.json'),
    phase: 'visible',
    displayedPhase: 'preparing',
    windowVisible: true,
    updatedAt: new Date(1000).toISOString(),
    ...overrides,
  };
}

function fakeTimers() {
  const handles = [];
  const create = (kind, fn, delay) => {
    const handle = { kind, fn, delay, active: true, unref() {} };
    handles.push(handle);
    return handle;
  };
  return {
    handles,
    schedule: (fn, delay) => create('timeout', fn, delay),
    cancelSchedule: (handle) => { handle.active = false; },
    repeat: (fn, delay) => create('interval', fn, delay),
    cancelRepeat: (handle) => { handle.active = false; },
    fire(handle) {
      if (!handle.active) return;
      if (handle.kind === 'timeout') handle.active = false;
      handle.fn();
    },
  };
}

function fakeElectron({ executeJavaScript = null, loadURL = null } = {}) {
  const windows = [];
  class FakeWindow extends EventEmitter {
    constructor(options) {
      super();
      this.options = options;
      this.visible = false;
      this.destroyed = false;
      this.rendered = [];
      this.webContents = {
        executeJavaScript: (script) => {
          this.rendered.push(script);
          return executeJavaScript ? executeJavaScript(script) : Promise.resolve();
        },
        setWindowOpenHandler: (handler) => { this.windowOpenHandler = handler; },
      };
      windows.push(this);
    }

    async loadURL(url) {
      this.url = url;
      if (loadURL) return loadURL(url);
    }
    show() { this.visible = true; }
    focus() { this.focused = true; }
    isVisible() { return this.visible; }
    isDestroyed() { return this.destroyed; }
    close() {
      const event = { prevented: false, preventDefault() { this.prevented = true; } };
      this.emit('close', event);
      if (event.prevented) return;
      this.visible = false;
      this.destroyed = true;
      this.emit('closed');
    }
    destroy() {
      this.visible = false;
      this.destroyed = true;
      this.emit('closed');
    }
  }
  class FakeApp extends EventEmitter {
    constructor() {
      super();
      this.paths = new Map();
      this.quitCalls = 0;
    }
    setPath(name, value) { this.paths.set(name, value); }
    setName(value) { this.name = value; }
    async whenReady() {}
    quit() { this.quitCalls += 1; this.emit('before-quit'); }
  }
  return { app: new FakeApp(), BrowserWindow: FakeWindow, windows };
}

test('the progress mode accepts one exact absolute transaction argument', () => {
  const transactionPath = path.resolve('/tmp/workass-update/transaction.json');
  assert.equal(progressTransactionArgument(['Workass', PROGRESS_ARGUMENT, transactionPath]), transactionPath);
  assert.equal(progressTransactionArgument(['Workass']), '');
  assert.throws(() => progressTransactionArgument(['Workass', PROGRESS_ARGUMENT, 'relative.json']), /argument is invalid/);
  assert.throws(() => progressTransactionArgument([
    'Workass', PROGRESS_ARGUMENT, transactionPath, PROGRESS_ARGUMENT, transactionPath,
  ]), /argument is invalid/);
});

test('the staged executable is mandatory until an explicit macOS recovery attempt', () => {
  const { transaction, transactionPath } = progressFixture();
  assert.equal(expectedProgressExecutable(transaction), transaction.progressExecutable);
  assert.equal(progressExecutableIsAllowed(transaction, transaction.progressExecutable), true);
  const installed = installedProgressExecutable(transaction);
  assert.equal(progressExecutableIsAllowed(transaction, installed), false);
  assert.throws(() => validateProgressTransaction(transactionPath, {
    ...transaction, progressExecutable: installed,
  }, {
    platform: 'darwin', executablePath: installed, progressId: transaction.progressId,
  }), /paths are invalid/);
  const recovery = { ...transaction, progressExecutable: installed, recoveryAttempt: 1 };
  assert.equal(validateProgressTransaction(transactionPath, recovery, {
    platform: 'darwin', executablePath: installed, progressId: transaction.progressId,
  }), recovery);
});

test('progress readiness requires the exact visible live identity and a fresh heartbeat', () => {
  const { transaction } = progressFixture();
  const receipt = exactProgressReceipt(transaction);
  assert.equal(progressReceiptIsLive(receipt, transaction, { now: () => 4000, alive: (pid) => pid === 4321 }), true);
  assert.equal(progressReceiptIsLive({ ...receipt, updatedAt: new Date(0).toISOString() }, transaction, {
    now: () => 6000, alive: () => true,
  }), false);
  assert.equal(progressReceiptIsLive({ ...receipt, windowVisible: false }, transaction, {
    now: () => 1000, alive: () => true,
  }), false);
  assert.equal(progressReceiptIsLive({ ...receipt, progressId: `progress-${'9'.repeat(32)}` }, transaction, {
    now: () => 1000, alive: () => true,
  }), false);
  assert.equal(progressReceiptProcessIsRunning({ ...receipt, phase: 'terminal' }, transaction, {
    alive: (pid) => pid === 4321,
  }), true);
  assert.equal(progressReceiptProcessIsRunning({ ...receipt, phase: 'starting', windowVisible: false }, transaction, {
    alive: (pid) => pid === 4321,
  }), true);
});

test('progress ownership requires the exact executable and transaction command', () => {
  const { transaction } = progressFixture();
  const receipt = exactProgressReceipt(transaction);
  const exactCommand = `${transaction.progressExecutable} ${PROGRESS_ARGUMENT} ${path.join(transaction.transactionRoot, 'transaction.json')}`;
  assert.deepEqual(progressProcessOwnership(receipt, transaction, {
    platform: 'darwin', alive: () => true, run: () => ({ status: 0, stdout: exactCommand }),
  }), { running: true, exact: true, ambiguous: false, pid: receipt.pid });
  assert.deepEqual(progressProcessOwnership(receipt, transaction, {
    platform: 'darwin', alive: () => true, run: () => ({ status: 0, stdout: '/Applications/Another.app/Contents/MacOS/Another' }),
  }), { running: true, exact: false, ambiguous: true, pid: receipt.pid });
});

test('progress termination owns the complete detached process tree', async () => {
  let mainAlive = true;
  let groupAlive = true;
  const signals = [];
  assert.equal(await terminateUpdateProgress({ exact: true, pid: 5432 }, {
    platform: 'darwin',
    alive: () => mainAlive,
    groupAlive: () => groupAlive,
    kill: (pid, signal) => {
      signals.push([pid, signal]);
      mainAlive = false;
      groupAlive = false;
    },
    pause: async () => {},
  }), true);
  assert.deepEqual(signals, [[-5432, 'SIGTERM']]);

  let stubbornMainAlive = true;
  let stubbornGroupAlive = true;
  let pauses = 0;
  const stubbornSignals = [];
  assert.equal(await terminateUpdateProgress({ exact: true, pid: 5433 }, {
    platform: 'darwin',
    alive: () => stubbornMainAlive,
    groupAlive: () => stubbornGroupAlive,
    kill: (pid, signal) => {
      stubbornSignals.push([pid, signal]);
      if (signal === 'SIGKILL') {
        stubbornMainAlive = false;
        stubbornGroupAlive = false;
      }
    },
    pause: async () => { pauses += 1; },
  }), true);
  assert.deepEqual(stubbornSignals, [[-5433, 'SIGTERM'], [-5433, 'SIGKILL']]);
  assert.equal(pauses, 30);

  let taskkill = null;
  let windowsAlive = true;
  assert.equal(await terminateUpdateProgress({ exact: true, pid: 6543 }, {
    platform: 'win32',
    alive: () => windowsAlive,
    run: (command, args) => {
      taskkill = { command, args };
      windowsAlive = false;
      return { status: 0 };
    },
    pause: async () => {},
  }), true);
  assert.deepEqual(taskkill, {
    command: 'taskkill.exe', args: ['/PID', '6543', '/T', '/F'],
  });
  assert.equal(await terminateUpdateProgress({ exact: false, pid: 6543 }, {
    platform: 'win32', alive: () => true,
  }), false);
});

test('the parent durably owns a starting progress PID before waiting for visible readiness', async () => {
  const { transaction } = progressFixture();
  const child = new EventEmitter();
  child.pid = 7654;
  child.exitCode = null;
  child.signalCode = null;
  child.unref = () => {};
  const timers = fakeTimers();
  const starting = spawnVisibleUpdateProgress({
    command: transaction.progressExecutable,
    args: [PROGRESS_ARGUMENT, path.join(transaction.transactionRoot, 'transaction.json')],
    options: { detached: true },
    transaction,
  }, {
    spawnProcess: () => child,
    now: () => 1000,
    terminateChild: async () => true,
    ...timers,
  });
  const owner = JSON.parse(fs.readFileSync(progressOwnerReceiptPath(transaction), 'utf8'));
  assert.equal(owner.phase, 'starting');
  assert.equal(owner.pid, child.pid);
  assert.equal(owner.progressId, transaction.progressId);
  assert.equal(owner.executablePath, transaction.progressExecutable);
  assert.equal(owner.transactionPath, path.join(transaction.transactionRoot, 'transaction.json'));
  assert.equal(fs.existsSync(transaction.progressReceiptPath), false);

  fs.writeFileSync(transaction.progressReceiptPath, `${JSON.stringify(exactProgressReceipt(transaction, {
    pid: child.pid,
    updatedAt: new Date(1000).toISOString(),
  }))}\n`);
  child.emit('spawn');
  assert.equal(await starting, child);
  assert.equal(timers.handles.some((handle) => handle.active), false);
});

test('progress text shows honest rollback and failure states without leaking credentials', () => {
  const { transaction } = progressFixture();
  const rollback = phaseView(transaction, writeJournal(transaction, {
    phase: 'rollback_healthy', terminal: true, error: 'token=supersecret rollback completed',
  }), null);
  assert.equal(rollback.terminal, true);
  assert.equal(rollback.tone, 'rollback');
  assert.match(rollback.detail, /token=\[redacted\]/);
  assert.doesNotMatch(rollback.detail, /supersecret/);
  const failed = phaseView(transaction, null, {
    phase: 'failed', rollbackError: 'password=hunter2 recovery failed',
  });
  assert.equal(failed.tone, 'failed');
  assert.equal(failed.action, 'Cerrar');
  assert.equal(boundedUpdateError('password=hunter2 recovery failed'), 'password=[redacted] recovery failed');
});

test('the updater uses the native window as its only visual surface', () => {
  const html = progressHTML();
  assert.match(html, /<main id="surface" class="active">/);
  assert.match(html, /aria-live="polite"/);
  assert.match(html, /Actualizador independiente/);
  assert.doesNotMatch(html, /id="card"/);
  assert.doesNotMatch(html, /box-shadow:0 24px 70px/);
});

test('the progress process becomes ready only after its isolated window is visible', async () => {
  const { transaction, transactionPath } = progressFixture();
  const timers = fakeTimers();
  const electron = fakeElectron();
  const controller = await runUpdateProgressProcess({
    ...electron,
    transactionPath,
    platform: 'darwin',
    executablePath: transaction.progressExecutable,
    progressId: transaction.progressId,
    now: () => 1000,
    ...timers,
  });
  const receipt = JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8'));
  assert.equal(electron.app.paths.get('userData'), path.join(transaction.transactionRoot, 'progress-profile'));
  assert.equal(electron.app.name, 'Workass Update');
  assert.equal(electron.windows.length, 1);
  assert.equal(electron.windows[0].options.show, false);
  assert.equal(electron.windows[0].options.width, 560);
  assert.equal(electron.windows[0].options.height, 300);
  assert.equal(electron.windows[0].options.resizable, false);
  assert.equal(electron.windows[0].visible, true);
  assert.equal(receipt.phase, 'visible');
  assert.equal(receipt.windowVisible, true);
  assert.equal(receipt.executablePath, transaction.progressExecutable);
  assert.equal(timers.handles.filter((handle) => handle.kind === 'interval' && handle.active).length, 1);

  electron.windows[0].close();
  assert.equal(electron.windows[0].isDestroyed(), false);
  assert.equal(electron.app.quitCalls, 0);

  writeJournal(transaction, {
    phase: 'rollback_healthy', terminal: true, error: 'the target did not become healthy',
  });
  const view = controller.render();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(view.tone, 'rollback');
  assert.equal(electron.windows[0].isDestroyed(), false);
  assert.equal(timers.handles.some((handle) => handle.active), false);
  assert.equal(JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8')).phase, 'terminal');

  electron.windows[0].close();
  assert.equal(electron.windows[0].isDestroyed(), true);
  assert.equal(electron.app.quitCalls, 1);
  assert.equal(timers.handles.some((handle) => handle.active), false);
  assert.equal(JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8')).phase, 'closed');
});

test('pre-visible renderer failure destroys the hidden window and records a lost owner', async () => {
  const { transaction, transactionPath } = progressFixture();
  const timers = fakeTimers();
  const electron = fakeElectron({ loadURL: async () => { throw new Error('renderer did not load'); } });
  await assert.rejects(runUpdateProgressProcess({
    ...electron,
    transactionPath,
    platform: 'darwin',
    executablePath: transaction.progressExecutable,
    progressId: transaction.progressId,
    now: () => 1000,
    ...timers,
  }), /renderer did not load/);
  assert.equal(electron.windows.length, 1);
  assert.equal(electron.windows[0].isDestroyed(), true);
  assert.equal(electron.app.quitCalls, 1);
  assert.equal(timers.handles.some((handle) => handle.active), false);
  const receipt = JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8'));
  assert.equal(receipt.phase, 'lost');
  assert.equal(receipt.windowVisible, false);
});

test('successful progress closes on one bounded timer and releases every handle', async () => {
  const { transaction, transactionPath } = progressFixture();
  const timers = fakeTimers();
  const electron = fakeElectron();
  const controller = await runUpdateProgressProcess({
    ...electron,
    transactionPath,
    platform: 'darwin',
    executablePath: transaction.progressExecutable,
    progressId: transaction.progressId,
    now: () => 1000,
    ...timers,
  });
  fs.writeFileSync(transaction.receiptPath, `${JSON.stringify({
    schemaVersion: 2,
    updateId: transaction.updateId,
    phase: 'healthy',
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    workerId: transaction.workerId,
  })}\n`);
  controller.render();
  await new Promise((resolve) => setImmediate(resolve));
  const active = timers.handles.filter((handle) => handle.active);
  assert.equal(active.length, 1);
  assert.equal(active[0].kind, 'timeout');
  assert.equal(active[0].delay, 1400);
  timers.fire(active[0]);
  assert.equal(electron.windows[0].isDestroyed(), true);
  assert.equal(electron.app.quitCalls, 1);
  assert.equal(timers.handles.some((handle) => handle.active), false);
});

test('render polling is single-flight and coalesces a blocked UI into the newest phase', async () => {
  const { transaction, transactionPath } = progressFixture();
  const timers = fakeTimers();
  let releaseFirst;
  let activeExecutions = 0;
  let maximumExecutions = 0;
  let executions = 0;
  const first = new Promise((resolve) => { releaseFirst = resolve; });
  const electron = fakeElectron({
    executeJavaScript: () => {
      executions += 1;
      activeExecutions += 1;
      maximumExecutions = Math.max(maximumExecutions, activeExecutions);
      const pending = executions === 1 ? first : Promise.resolve();
      return pending.finally(() => { activeExecutions -= 1; });
    },
  });
  const starting = runUpdateProgressProcess({
    ...electron,
    transactionPath,
    platform: 'darwin',
    executablePath: transaction.progressExecutable,
    progressId: transaction.progressId,
    now: () => 1000,
    ...timers,
  });
  while (electron.windows.length === 0 || timers.handles.length === 0) {
    await new Promise((resolve) => setImmediate(resolve));
  }
  const poll = timers.handles.find((handle) => handle.kind === 'interval');
  for (const phase of ['incoming_verified', 'snapshotting_state', 'activated']) {
    writeJournal(transaction, { phase });
    timers.fire(poll);
  }
  assert.equal(executions, 1);
  assert.equal(maximumExecutions, 1);
  assert.equal(fs.existsSync(transaction.progressReceiptPath), false);

  releaseFirst();
  await starting;
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(executions, 2);
  assert.equal(maximumExecutions, 1);
  assert.equal(electron.windows[0].rendered.filter((script) => script.includes('"phase":"activated"')).length, 1);
  const receipt = JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8'));
  assert.equal(receipt.phase, 'watching');
  assert.equal(receipt.displayedPhase, 'activated');
  electron.windows[0].destroy();
  assert.equal(timers.handles.some((handle) => handle.active), false);
});

test('a hung terminal UI acknowledgement is bounded and releases the progress process', async () => {
  const { transaction, transactionPath } = progressFixture();
  const timers = fakeTimers();
  let executions = 0;
  const never = new Promise(() => {});
  const electron = fakeElectron({
    executeJavaScript: () => {
      executions += 1;
      return executions === 1 ? Promise.resolve() : never;
    },
  });
  const controller = await runUpdateProgressProcess({
    ...electron,
    transactionPath,
    platform: 'darwin',
    executablePath: transaction.progressExecutable,
    progressId: transaction.progressId,
    terminalAckTimeoutMs: 5000,
    now: () => 1000,
    ...timers,
  });
  writeJournal(transaction, { phase: 'failed', terminal: true, error: 'target recovery failed' });
  controller.render();
  await new Promise((resolve) => setImmediate(resolve));
  const watchdog = timers.handles.find((handle) => handle.kind === 'timeout' && handle.active);
  assert.equal(executions, 2);
  assert.equal(watchdog.delay, 5000);
  timers.fire(watchdog);
  assert.equal(electron.windows[0].isDestroyed(), true);
  assert.equal(electron.app.quitCalls, 1);
  assert.equal(timers.handles.some((handle) => handle.active), false);
  assert.equal(JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8')).phase, 'lost');
});

test('the primary shell branch is never entered by a progress-mode invocation', () => {
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  const branch = source.indexOf('if (updateProgressTransaction)');
  const primaryDefinition = source.indexOf('function startPrimaryShell()');
  assert.ok(branch >= 0 && primaryDefinition > branch);
  const dispatch = source.slice(branch, primaryDefinition);
  assert.match(dispatch, /runUpdateProgressProcess/);
  assert.match(dispatch, /else if \(!updateProgressRequested\) \{\s*startPrimaryShell\(\);\s*\}/);
});
