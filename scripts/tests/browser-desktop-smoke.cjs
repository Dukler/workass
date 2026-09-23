'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { execFileSync, spawn } = require('node:child_process');

const ROOT = path.resolve(__dirname, '../..');
const runDirArg = process.argv.find((value) => value.startsWith('--workass-run-dir='));
const RUN_DIR = runDirArg ? path.resolve(runDirArg.slice('--workass-run-dir='.length)) : '';
const EVIDENCE = RUN_DIR ? path.join(RUN_DIR, 'evidence') : '';
const CHILD = process.argv.includes('--child');
const SUPERVISION_TESTS = process.argv.includes('--supervision-tests');
const CHILD_STAGE = process.argv.find((value) => value.startsWith('--workass-smoke-stage='))?.slice('--workass-smoke-stage='.length) || 'full';
let HTML = '';
const NATIVE_CRASH_SIGNALS = new Set(['SIGABRT', 'SIGBUS', 'SIGFPE', 'SIGILL', 'SIGSEGV', 'SIGTRAP']);
let app;
let BrowserWindow;
let WebContentsView;
let nativeImage;
let session;
let BrowserManager;
let finishChildRun = null;
let CHILD_PROCESS_IDENTITY = null;
let holdApplicationUntilSmokeReceipt = null;

function observeDarwinProcess(pid) {
  try {
    const options = { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'], timeout: 2500 };
    const startIdentity = execFileSync('/bin/ps', ['-p', String(pid), '-o', 'lstart='], options).trim();
    const commandLine = execFileSync('/bin/ps', ['-p', String(pid), '-o', 'command='], options).trim();
    if (!startIdentity || !commandLine) return { status: 'unknown', error: 'ps returned an empty identity' };
    return { status: 'alive', identity: { pid: Number(pid), startIdentity, commandLine } };
  } catch (error) {
    if (error && error.status === 1 && !error.signal && error.code !== 'ETIMEDOUT') return { status: 'gone' };
    return { status: 'unknown', error: String(error && (error.code || error.message) || 'ps observation failed') };
  }
}

function readDarwinProcessIdentity(pid, runNonce) {
  const observed = observeDarwinProcess(pid);
  if (observed.status !== 'alive') return observed;
  const identity = { ...observed.identity, runNonce };
  if (!identity.startIdentity || !identity.commandLine.includes(`--workass-smoke-run-nonce=${runNonce}`)) {
    return { status: 'other', identity };
  }
  return { status: 'target', identity };
}

function sameProcessIdentity(expected, observed) {
  return !!expected && !!observed && Number(expected.pid) === Number(observed.pid) &&
    expected.startIdentity === observed.startIdentity && expected.runNonce === observed.runNonce &&
    expected.commandLine === observed.commandLine;
}

function targetObservationState(expected, observed) {
  if (!observed || observed.status === 'unknown') return 'unknown';
  return observed.status === 'target' && sameProcessIdentity(expected, observed.identity) ? 'alive' : 'gone';
}

function signalVerifiedProcess(expected, signal, observe = readDarwinProcessIdentity, send = process.kill.bind(process)) {
  const observed = observe(expected.pid, expected.runNonce);
  if (observed?.status !== 'target' || !sameProcessIdentity(expected, observed.identity)) return false;
  try { send(expected.pid, signal); return true; }
  catch (error) { if (error && error.code === 'ESRCH') return false; throw error; }
}

function atomicPrivateJSON(filePath, value) {
  const temporary = `${filePath}.${process.pid}.tmp`;
  fs.writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, filePath);
  try { fs.chmodSync(filePath, 0o600); } catch { /* inherited private run directory */ }
}

function publishChildProcessIdentity() {
  const runDir = String(process.env.WORKASS_BROWSER_SMOKE_RUN_DIR || '').trim();
  const runNonce = String(process.env.WORKASS_BROWSER_SMOKE_RUN_NONCE || '').trim();
  const receiptPath = String(process.env.WORKASS_BROWSER_SMOKE_PROCESS_RECEIPT || '').trim();
  if (!runDir || !runNonce || !receiptPath || !path.resolve(receiptPath).startsWith(`${path.resolve(runDir)}${path.sep}`)) {
    throw new Error('private Electron process receipt configuration is missing');
  }
  const observed = readDarwinProcessIdentity(process.pid, runNonce);
  if (observed.status !== 'target') throw new Error(`Electron could not publish its exact macOS PID/start identity (${observed.status})`);
  const identity = observed.identity;
  atomicPrivateJSON(receiptPath, { ...identity, state: 'running', publishedAt: new Date().toISOString() });
  return identity;
}

if (SUPERVISION_TESTS) {
  const { test } = require('node:test');
  test('timeout signaling requires the exact PID, start identity, command, and run nonce', () => {
    const expected = { pid: 41, startIdentity: 'Tue Sep 22 22:00:00 2026', commandLine: 'Electron --workass-smoke-run-nonce=nonce-a', runNonce: 'nonce-a' };
    let signaled = null;
    const observe = (pid, runNonce) => ({ status: 'target', identity: { ...expected, pid, runNonce } });
    assert.equal(signalVerifiedProcess(expected, 'SIGTERM', observe, (pid, signal) => { signaled = { pid, signal }; }), true);
    assert.deepEqual(signaled, { pid: 41, signal: 'SIGTERM' });
    signaled = null;
    assert.equal(signalVerifiedProcess(expected, 'SIGKILL', () => ({ status: 'target', identity: { ...expected, startIdentity: 'reused pid' } }), (pid) => { signaled = pid; }), false);
    assert.equal(signaled, null);
    assert.equal(signalVerifiedProcess(expected, 'SIGKILL', () => ({ status: 'other', identity: { ...expected, commandLine: 'different process' } }), (pid) => { signaled = pid; }), false);
    assert.equal(signalVerifiedProcess(expected, 'SIGKILL', () => ({ status: 'unknown', error: 'timed out' }), (pid) => { signaled = pid; }), false);
    assert.equal(signaled, null);
    assert.equal(targetObservationState(expected, { status: 'unknown', error: 'timed out' }), 'unknown', 'ps timeout/error must never prove exit');
    assert.equal(targetObservationState(expected, { status: 'gone' }), 'gone');
  });
} else if (!CHILD) {
  require('node:test')('pinned Electron proves browser viewport and capture pixels', async () => {
    assert.equal(process.platform, 'darwin', 'this fixture uses the pinned macOS Electron runtime');
    const electronApp = path.resolve(ROOT, '.dev/runtime/electron/darwin-arm64/Electron.app');
    assert.ok(fs.existsSync(path.join(electronApp, 'Contents/MacOS/Electron')), 'canonical pinned dev Electron runtime is required');
    const rebuildDir = path.join(ROOT, '.dev/rebuild');
    fs.mkdirSync(rebuildDir, { recursive: true });
    const runDir = fs.mkdtempSync(path.join(rebuildDir, 'browser-desktop-smoke-run-'));
    fs.chmodSync(runDir, 0o700);
    const lockPath = path.join(rebuildDir, 'browser-desktop-smoke.lock');
    let lockFd;
    try {
      lockFd = fs.openSync(lockPath, 'wx', 0o600);
    } catch (error) {
      fs.rmSync(runDir, { recursive: true, force: true });
      if (error && error.code === 'EEXIST') throw new Error(`browser smoke is already active or has a stale lock: ${lockPath}`);
      throw error;
    }
    const stdoutPath = path.join(runDir, 'electron.stdout.log');
    const stderrPath = path.join(runDir, 'electron.stderr.log');
    const launcherStdoutPath = path.join(runDir, 'launch-services.stdout.log');
    const launcherStderrPath = path.join(runDir, 'launch-services.stderr.log');
    const receiptPath = path.join(runDir, 'supervision.json');
    const processReceiptPath = path.join(runDir, 'electron-process.json');
    const finalReceiptPath = path.join(runDir, 'electron-result.json');
    const userData = path.join(runDir, 'profile');
    const runId = path.basename(runDir);
    const runNonce = `${runId}-${process.pid}`;
    const requestedStage = process.env.WORKASS_BROWSER_SMOKE_STAGE;
    const stage = ['bootstrap', 'rendering'].includes(requestedStage) ? requestedStage : 'full';
    const owner = { pid: process.pid, parentPid: process.ppid, runId, startedAt: new Date().toISOString() };

    let launcher;
    let launcherClosed = false;
    let launcherResult = null;
    let targetIdentity = null;
    let targetExited = false;
    let safeToUnlock = true;
    let cleanupPromise = null;
    const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
    const persistReceipt = (value) => atomicPrivateJSON(receiptPath, value);
    const readProcessReceipt = () => {
      try {
        const value = JSON.parse(fs.readFileSync(processReceiptPath, 'utf8'));
        if (value.runNonce !== runNonce || !Number.isInteger(Number(value.pid)) || !value.startIdentity || !value.commandLine) return null;
        return value;
      } catch { return null; }
    };
    const readFinalReceipt = () => {
      try {
        const value = JSON.parse(fs.readFileSync(finalReceiptPath, 'utf8'));
        return value.runNonce === runNonce && value.pid === targetIdentity?.pid ? value : null;
      } catch { return null; }
    };
    const observeTarget = () => targetIdentity && readDarwinProcessIdentity(targetIdentity.pid, runNonce);
    const targetState = () => targetObservationState(targetIdentity, observeTarget());
    const waitForTargetExit = async (ms) => {
      const deadline = Date.now() + ms;
      while (Date.now() < deadline) {
        if (targetState() === 'gone') { targetExited = true; return true; }
        await wait(100);
      }
      if (targetState() === 'gone') { targetExited = true; return true; }
      return false;
    };
    const awaitTargetReceipt = async (ms) => {
      const deadline = Date.now() + ms;
      while (Date.now() < deadline) {
        const processReceipt = readProcessReceipt();
        if (processReceipt) {
          const current = readDarwinProcessIdentity(processReceipt.pid, runNonce);
          if (current.status === 'unknown') { await wait(50); continue; }
          if (current.status !== 'target' || !sameProcessIdentity(processReceipt, current.identity)) throw new Error('Electron process receipt does not match the live target identity');
          return processReceipt;
        }
        if (launcherClosed) throw new Error(`LaunchServices exited before the Electron process published identity (exit=${launcherResult?.code ?? 'unknown'})`);
        await wait(50);
      }
      throw new Error(`LaunchServices did not publish the exact Electron process identity; launcherPid=${launcher?.pid || 'unknown'}; run=${runDir}`);
    };
    const cleanupTarget = () => {
      if (cleanupPromise) return cleanupPromise;
      cleanupPromise = (async () => {
        const cleanup = [];
        if (!targetIdentity || targetExited) return { cleanup, targetExited };
        const state = targetState();
        if (state === 'gone') { targetExited = true; return { cleanup, targetExited: true, result: readFinalReceipt() }; }
        if (state === 'unknown') {
          safeToUnlock = false;
          cleanup.push({ signal: 'unconfirmed', sent: false, observation: 'unknown' });
          return { cleanup, targetExited: false, result: readFinalReceipt() };
        }
        cleanup.push({ signal: 'SIGTERM', sent: signalVerifiedProcess(targetIdentity, 'SIGTERM') });
        let exited = await waitForTargetExit(3000);
        if (!exited) {
          cleanup.push({ signal: 'SIGKILL', sent: signalVerifiedProcess(targetIdentity, 'SIGKILL') });
          exited = await waitForTargetExit(5000);
        }
        if (!exited) {
          cleanup.push({ signal: 'unconfirmed', sent: false });
          safeToUnlock = false;
        }
        return { cleanup, targetExited: exited, result: readFinalReceipt() };
      })();
      return cleanupPromise;
    };
    const observeUntilExit = async (ms) => {
      const deadline = Date.now() + ms;
      while (Date.now() < deadline) {
        const state = targetState();
        if (state === 'gone') {
          targetExited = true;
          return { timedOut: false, result: readFinalReceipt(), observedExitAt: new Date().toISOString() };
        }
        await wait(100);
      }
      return { timedOut: true, result: null };
    };

    try {
      fs.writeSync(lockFd, JSON.stringify(owner));
      fs.fsyncSync(lockFd);
      fs.mkdirSync(userData, { recursive: true, mode: 0o700 });
      const launcherStdoutFd = fs.openSync(launcherStdoutPath, 'wx', 0o600);
      const launcherStderrFd = fs.openSync(launcherStderrPath, 'wx', 0o600);
      const launchArgs = [
        '-n', '-g', '-W', '-a', electronApp,
        '-i', '/dev/null', '-o', stdoutPath, '--stderr', stderrPath,
        '--env', `WORKASS_BROWSER_SMOKE_RUN_DIR=${runDir}`,
        '--env', `WORKASS_BROWSER_SMOKE_RUN_NONCE=${runNonce}`,
        '--env', `WORKASS_BROWSER_SMOKE_USER_DATA=${userData}`,
        '--env', `WORKASS_BROWSER_SMOKE_PROCESS_RECEIPT=${processReceiptPath}`,
        '--env', `WORKASS_BROWSER_SMOKE_FINAL_RECEIPT=${finalReceiptPath}`,
        '--env', `WORKASS_BROWSER_SMOKE_STAGE=${stage}`,
        '--env', 'ELECTRON_ENABLE_LOGGING=1',
        '--env', 'ELECTRON_ENABLE_STACK_DUMPING=1',
        '--args', __filename, '--child', `--workass-run-dir=${runDir}`,
        `--workass-smoke-run-nonce=${runNonce}`, `--workass-smoke-stage=${stage}`,
        '--enable-logging=stderr', '--v=1',
      ];
      launcher = spawn('/usr/bin/open', launchArgs, {
        cwd: ROOT, env: process.env, stdio: ['ignore', launcherStdoutFd, launcherStderrFd],
      });
      fs.closeSync(launcherStdoutFd);
      fs.closeSync(launcherStderrFd);
      launcher.once('error', (error) => {
        launcherClosed = true;
        launcherResult = { code: null, signal: null, error: String(error.message || error) };
      });
      launcher.once('close', (code, signal) => {
        launcherClosed = true;
        launcherResult = { code, signal, error: '' };
      });
      persistReceipt({
        ...owner, state: 'launching', launchCount: 1, supervisorPid: process.pid,
        launchMethod: 'LaunchServices /usr/bin/open -n -W', launcherPid: launcher.pid || null,
        electronApp, launchArgs, processReceiptPath, finalReceiptPath, stdoutPath, stderrPath,
        launcherStdoutPath, launcherStderrPath, runDir, userData, stage, runNonce,
      });
      targetIdentity = await awaitTargetReceipt(15000);
      persistReceipt({
        ...JSON.parse(fs.readFileSync(receiptPath, 'utf8')), state: 'running',
        electronPid: targetIdentity.pid, electronStartIdentity: targetIdentity.startIdentity,
        electronCommandLine: targetIdentity.commandLine, processIdentityObservedAt: new Date().toISOString(),
      });
      const observed = await observeUntilExit(120000);
      const timedOut = { timedOut: observed.timedOut };
      let result = observed.result;
      let cleanup = [];
      if (timedOut.timedOut) {
        const terminated = await cleanupTarget();
        cleanup = terminated.cleanup;
        result = terminated.result;
      }
      const stdout = fs.readFileSync(stdoutPath, 'utf8');
      const stderr = fs.readFileSync(stderrPath, 'utf8');
      const nativeCrashDetail = stderr.split(/\r?\n/u).find((line) => line.includes('WORKASS_BROWSER_SMOKE_NATIVE_CHILD_CRASH')) || '';
      const final = {
        ...JSON.parse(fs.readFileSync(receiptPath, 'utf8')), state: 'finished', supervisorPid: process.pid,
        timeout: timedOut.timedOut, cleanup, electronExited: targetExited,
        electronResult: result, launcherResult, observedExitAt: observed.observedExitAt || null,
        exitCode: result?.exitCode ?? null, signal: result?.signal || null, spawnError: launcherResult?.error || '',
        nativeCrash: NATIVE_CRASH_SIGNALS.has(result?.signal) || !!nativeCrashDetail || (!!targetExited && !result),
        nativeCrashDetail,
        stderrBytes: Buffer.byteLength(stderr),
        outcomeAt: new Date().toISOString(),
      };
      persistReceipt(final);
      assert.equal(timedOut.timedOut, false, `Electron smoke timed out; exact target pid=${targetIdentity.pid}; stdout=${stdoutPath}; stderr=${stderrPath}`);
      assert.equal(final.spawnError, '', `LaunchServices could not supervise the Electron target pid=${targetIdentity.pid}; stderr=${launcherStderrPath}`);
      assert.equal(final.nativeCrash, false, `Electron target pid=${targetIdentity.pid} exited without a clean result; signal=${final.signal || 'unknown'}; detail=${nativeCrashDetail || 'no final Electron receipt'}; stderr=${stderrPath}`);
      assert.equal(final.signal, null, `Electron target pid=${targetIdentity.pid} reported signal ${final.signal}; stderr=${stderrPath}`);
      assert.equal(final.exitCode, 0, `Electron target pid=${targetIdentity.pid} exited ${final.exitCode}; stderr=${stderrPath}`);
      const passMarker = stage === 'bootstrap' ? /browser-desktop-bootstrap-pass/ : stage === 'rendering' ? /browser-desktop-rendering-pass/ : /browser-desktop-smoke-pass/;
      assert.match(stdout, passMarker, `Electron target pid=${targetIdentity.pid} did not finish the ${stage} fixture; stdout=${stdoutPath}; stderr=${stderrPath}`);
    } finally {
      if (targetIdentity && !targetExited && !cleanupPromise) {
        try { await cleanupTarget(); } catch { safeToUnlock = false; }
      }
      if (launcher && !launcherClosed) {
        await Promise.race([new Promise((resolve) => launcher.once('close', resolve)), wait(3000)]);
        if (!launcherClosed && !targetExited) safeToUnlock = false;
      }
      try { if (lockFd !== undefined) fs.closeSync(lockFd); } catch { /* already closed */ }
      if (safeToUnlock) {
        try {
          const current = JSON.parse(fs.readFileSync(lockPath, 'utf8'));
          if (current.pid === owner.pid && current.runId === owner.runId) fs.unlinkSync(lockPath);
        } catch { /* retain an unreadable lock for human review */ }
      }
    }
  });
} else {
  const childIdentity = publishChildProcessIdentity();
  CHILD_PROCESS_IDENTITY = childIdentity;
  HTML = fs.readFileSync(path.join(__dirname, 'fixtures/browser-desktop.html'), 'utf8');
  let requestedExitCode = null;
  let requestedExitReason = '';
  let finalReceiptWritten = false;
  const finalReceiptPath = String(process.env.WORKASS_BROWSER_SMOKE_FINAL_RECEIPT || '').trim();
  const safeFailureText = (error) => String(error && (error.stack || error.message) || error || 'unknown smoke failure')
    .replace(/\b(api[_-]?key|token|secret|password|credential|bearer)\b([:=\s]+)[^\s,;]+/giu, '$1$2[redacted]')
    .slice(0, 1200);
  const writeChildFinalReceipt = (exitCode, reason = '') => {
    if (finalReceiptWritten || !finalReceiptPath) return;
    requestedExitCode = Number(exitCode);
    requestedExitReason = String(reason || '');
    atomicPrivateJSON(finalReceiptPath, {
      pid: childIdentity.pid, startIdentity: childIdentity.startIdentity,
      runNonce: childIdentity.runNonce, state: 'finished', exitCode,
      reason: String(reason || ''), completedAt: new Date().toISOString(),
    });
    finalReceiptWritten = true;
  };
  finishChildRun = writeChildFinalReceipt;
  process.on('exit', () => {
    const exitCode = requestedExitCode == null
      ? (typeof process.exitCode === 'number' ? process.exitCode : 1)
      : requestedExitCode;
    writeChildFinalReceipt(exitCode, requestedExitReason || (exitCode === 0 ? 'missing_final_outcome' : 'process_exit'));
  });
  process.on('uncaughtException', (error) => {
    const detail = safeFailureText(error);
    process.stderr.write(`${detail}\n`);
    writeChildFinalReceipt(1, `uncaught_exception: ${detail}`);
    if (app?.exit) app.exit(1);
  });
  process.on('unhandledRejection', (error) => {
    const detail = safeFailureText(error);
    process.stderr.write(`${detail}\n`);
    writeChildFinalReceipt(1, `unhandled_rejection: ${detail}`);
    if (app?.exit) app.exit(1);
  });
  ({ app, BrowserWindow, WebContentsView, nativeImage, session } = require('electron'));
  holdApplicationUntilSmokeReceipt = (event) => event.preventDefault();
  app.on('before-quit', holdApplicationUntilSmokeReceipt);
  ({ BrowserManager } = require(path.join(ROOT, 'desktop/shell/browser-manager.js')));
}

function pngSize(data) {
  const png = Buffer.isBuffer(data) ? data : Buffer.from(data, 'base64');
  assert.equal(png.toString('hex', 0, 8), '89504e470d0a1a0a', 'capture must be a PNG');
  return { width: png.readUInt32BE(16), height: png.readUInt32BE(20), png };
}

function assertPainted(data, label) {
  const { width, height, png } = pngSize(data);
  const bitmap = nativeImage.createFromBuffer(png).toBitmap();
  const seen = new Set();
  for (let offset = 0; offset < bitmap.length; offset += Math.max(4, Math.floor(bitmap.length / 4096 / 4) * 4)) {
    seen.add(`${bitmap[offset]},${bitmap[offset + 1]},${bitmap[offset + 2]}`);
  }
  assert.ok(seen.size > 8, `${label} capture must contain painted page pixels`);
  return { width, height, distinctSampleColors: seen.size };
}

function bitmapFor(data) {
  const { width, height, png } = pngSize(data);
  return { width, height, pixels: nativeImage.createFromBuffer(png).toBitmap() };
}

function bitmapPixel(bitmap, x, y) {
  const offset = (y * bitmap.width + x) * 4;
  assert.ok(x >= 0 && y >= 0 && x < bitmap.width && y < bitmap.height, `pixel ${x},${y} is outside ${bitmap.width}x${bitmap.height}`);
  return [bitmap.pixels[offset + 2], bitmap.pixels[offset + 1], bitmap.pixels[offset], bitmap.pixels[offset + 3]];
}

function assertPixelNear(bitmap, x, y, expected, tolerance, label) {
  const actual = bitmapPixel(bitmap, x, y).slice(0, 3);
  assert.ok(expected.every((channel, index) => Math.abs(actual[index] - channel) <= tolerance), `${label}: expected RGB near ${expected.join(',')}, observed ${actual.join(',')}`);
  return actual;
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject);
      resolve(server.address().port);
    });
  });
}

async function delay(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }

async function main() {
  const milestone = (name, details = {}) => {
    fs.writeSync(1, `${JSON.stringify({ milestone: name, at: new Date().toISOString(), ...details })}\n`);
  };
  if (!RUN_DIR || !EVIDENCE) throw new Error('supervised run directory is required');
  milestone('start', { runDir: RUN_DIR, pid: process.pid });
  let fatalChildCrash = false;
  app.on('child-process-gone', (_event, details) => {
    if (fatalChildCrash || !['crashed', 'oom', 'launch-failed'].includes(details.reason)) return;
    fatalChildCrash = true;
    const detail = `renderer_${String(details.reason || 'unknown')}`;
    finishChildRun?.(86, detail);
    process.stderr.write(`WORKASS_BROWSER_SMOKE_NATIVE_CHILD_CRASH ${JSON.stringify({ type: details.type, reason: details.reason, exitCode: details.exitCode })}\n`);
    app.exit(86);
  });
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const userData = String(process.env.WORKASS_BROWSER_SMOKE_USER_DATA || '').trim();
  if (!userData || !path.resolve(userData).startsWith(`${RUN_DIR}${path.sep}`)) {
    throw new Error('isolated smoke user-data path is missing or outside the run directory');
  }
  app.setPath('userData', userData);
  milestone('user-data-set');
  await app.whenReady();
  milestone('electron-ready', { electronVersion: process.versions.electron, chromeVersion: process.versions.chrome });

  const cross = http.createServer((_req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end('<!doctype html><title>Cross frame</title><label for="cross-input">Cross input</label><input id="cross-input"><button id="cross-action">Cross frame action</button><script>window.crossProof=null;document.querySelector("#cross-action").addEventListener("click",event=>{window.crossProof={trusted:event.isTrusted};parent.postMessage({kind:"workass-cross-frame-click",trusted:event.isTrusted},"*")})</script>');
  });
  const crossPort = await listen(cross);
  milestone('fixture-cross-ready');
  const local = http.createServer((req, res) => {
    if (req.url === '/frame-same') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end('<!doctype html><title>Same frame</title><label for="same-input">Same input</label><input id="same-input"><button id="same-frame-button">Same frame action</button><script>window.sameProof=null;document.querySelector("#same-frame-button").addEventListener("click",event=>{window.sameProof={trusted:event.isTrusted}})</script>');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(HTML.replaceAll('PORT', String(crossPort)));
  });
  const localPort = await listen(local);
  milestone('fixture-local-ready');

  let mainWindow;
  let manager;
  const lifecycleEvents = [];
  const runtimeEventCounts = Object.create(null);
  const rawRuntimeEventCounts = Object.create(null);
  const requestedPaneOpens = [];
  const observePage = (entry, expression) => entry.view.webContents.executeJavaScript(expression);
  const capture = (tabId, mode, clip) => manager.browserControl('browser.screenshot', {
    tabId, ...(mode ? { mode } : {}), ...(clip ? { clip } : {}),
  });
  const assertShot = (shot, label) => assertPainted(Buffer.from(shot.base64, 'base64'), label);
  const saveShot = (name, shot) => fs.writeFileSync(path.join(EVIDENCE, name), Buffer.from(shot.base64, 'base64'));
  const watchRenderer = (entry) => {
    entry.view.webContents.on('render-process-gone', (_event, details) => {
      if (fatalChildCrash) return;
      fatalChildCrash = true;
      finishChildRun?.(86, `renderer_${String(details.reason || 'unknown')}`);
      process.stderr.write(`WORKASS_BROWSER_SMOKE_NATIVE_CHILD_CRASH ${JSON.stringify({ type: 'renderer', reason: details.reason, exitCode: details.exitCode })}\n`);
      app.exit(86);
    });
  };
  const openBackground = async (chatId, url) => {
    const entry = manager.openConversation({ chatId, visible: false });
    watchRenderer(entry);
    if (entry.ready && await entry.ready !== true) throw new Error(entry.error || `BrowserManager initialization failed for ${chatId}`);
    entry.view.webContents.debugger.on('message', (_event, method, params, sessionId) => {
      if (method === 'Runtime.consoleAPICalled' || method === 'Runtime.exceptionThrown') {
        const type = method === 'Runtime.consoleAPICalled' ? String(params?.type || 'unknown') : 'exception';
        const key = `${method}:${type}:session=${sessionId == null || sessionId === '' ? 'root' : 'child'}`;
        rawRuntimeEventCounts[key] = (rawRuntimeEventCounts[key] || 0) + 1;
      } else if (method === 'Target.attachedToTarget') {
        const type = String(params?.targetInfo?.type || 'unknown');
        const key = `${method}:${type}:parent=${sessionId == null || sessionId === '' ? 'root' : 'child'}`;
        rawRuntimeEventCounts[key] = (rawRuntimeEventCounts[key] || 0) + 1;
      } else if (['Page.frameAttached', 'Page.frameNavigated', 'Page.frameDetached'].includes(method)) {
        const key = `${method}:session=${sessionId == null || sessionId === '' ? 'root' : 'child'}`;
        rawRuntimeEventCounts[key] = (rawRuntimeEventCounts[key] || 0) + 1;
      }
    });
    const tab = manager.tabInfo(entry);
    await manager.browserControl('browser.navigate', { tabId: tab.id, url });
    return { entry, tab: manager.tabInfo(entry) };
  };
  try {
    mainWindow = new BrowserWindow({ width: 1440, height: 900, show: false, focusable: true, webPreferences: { sandbox: true, contextIsolation: true } });
    milestone('browser-window-created');
    await mainWindow.loadURL('about:blank');
    milestone('browser-window-loaded');
    manager = new BrowserManager({
      win: mainWindow, WebContentsView, BrowserWindow, nativeImage, session,
      chromeVersion: process.versions.chrome,
      requestOpen: (chatId) => requestedPaneOpens.push(chatId),
      onLifecycle: ({ stage, ...details }) => {
        lifecycleEvents.push({ stage, ...details });
        milestone(stage, details);
      },
    });
    manager.onCDP((event) => {
      if (event.method === 'Runtime.consoleAPICalled') {
        const key = `${event.method}:${String(event.params?.type || 'unknown')}`;
        runtimeEventCounts[key] = (runtimeEventCounts[key] || 0) + 1;
      } else if (event.method === 'Runtime.exceptionThrown') {
        runtimeEventCounts[event.method] = (runtimeEventCounts[event.method] || 0) + 1;
      }
    });
    milestone('browser-manager-created');

    // This is the same product manager used by the shell and agent tools. The
    // detached background page is initialized in BrowserManager before its first
    // fixture navigation; there is no separate CDP-only page in this runner.
    const { entry: entryA, tab: tabA } = await openBackground('smoke-chat-a', `http://127.0.0.1:${localPort}/fixture`);
    const wc = entryA.view.webContents;
    const requiredStartupStages = [
      'before-initial-blank-navigation', 'after-initial-document-readiness', 'before-page-debugger-attach',
      'after-page-debugger-attach', 'before-metrics', 'after-metrics', 'before-user-navigation',
    ];
    const observedStartupStages = lifecycleEvents.map((event) => event.stage);
    const startupStageIndexes = requiredStartupStages.map((stageName) => observedStartupStages.indexOf(stageName));
    assert.ok(startupStageIndexes.every((index) => index >= 0), `BrowserManager lifecycle omitted a startup proof stage: ${JSON.stringify(observedStartupStages)}`);
    assert.ok(startupStageIndexes.every((index, stageIndex) => stageIndex === 0 || startupStageIndexes[stageIndex - 1] < index), 'same-view about:blank readiness, CDP attach, metrics, and first user URL must occur in order');
    const initialReadyEvent = lifecycleEvents.find((event) => event.stage === 'after-initial-document-readiness');
    const beforeBlankEvent = lifecycleEvents.find((event) => event.stage === 'before-initial-blank-navigation');
    const beforeMetricsEvent = lifecycleEvents.find((event) => event.stage === 'before-metrics');
    assert.equal(initialReadyEvent.tabId, tabA.id, 'initial about:blank readiness must belong to the first BrowserManager view');
    assert.equal(initialReadyEvent.url, 'about:blank', 'the BrowserManager view must report its initial document before metrics');
    assert.equal(initialReadyEvent.readyState === 'loading', false, 'the initial BrowserManager document must be ready before metrics');
    assert.equal(beforeBlankEvent.initialDocumentReady, false, 'fresh WebContentsView host attachment must not apply emulation before a document exists');
    assert.equal(initialReadyEvent.initialDocumentReady, true, 'the first emulation is gated on initial document readiness');
    assert.equal(beforeMetricsEvent.initialDocumentReady, true, 'metrics must be applied only after the initial blank readiness proof');
    assert.ok(lifecycleEvents.filter((event) => requiredStartupStages.includes(event.stage)).every((event) => event.tabId === tabA.id), 'one native view must own every startup stage');
    milestone('first-tab-opened-in-background', {
      tabId: tabA.id, active: tabA.active, visible: tabA.visible,
      captureHostAttached: entryA.captureHostAttached,
    });
    assert.deepEqual(requestedPaneOpens, [], 'visible=false must not request the browser pane');
    assert.equal(tabA.visible, false);
    assert.equal(tabA.active, false);
    assert.equal(entryA.captureHostAttached, true, 'the same owned page is initialized on the hidden non-focusing surface');
    assert.deepEqual(entryA.viewport, { width: 1440, height: 900, deviceScaleFactor: 1 });
    assert.deepEqual({
      width: entryA.effectiveViewport.width,
      height: entryA.effectiveViewport.height,
      deviceScaleFactor: entryA.effectiveViewport.deviceScaleFactor,
    }, { width: 1440, height: 900, deviceScaleFactor: 1 });

    let initial;
    for (let attempt = 0; attempt < 60; attempt += 1) {
      initial = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, desktop: matchMedia('(min-width: 1000px)').matches, title: document.title, scrollY })`);
      if (initial.desktop && initial.title === 'Workass browser desktop fixture') break;
      await delay(50);
    }
    assert.deepEqual(initial, { width: 1440, height: 900, dpr: 1, desktop: true, title: 'Workass browser desktop fixture', scrollY: 0 });
    const initialStableWait = await manager.browserControl('browser.wait', {
      tabId: tabA.id, condition: 'element_visible', selector: '#delayed', timeoutMs: 2000,
    });
    assert.equal(initialStableWait.success, true, 'the initial capture waits for the fixture layout to settle');
    const neverShownScreenshot = await capture(tabA.id);
    const neverShownPixels = assertShot(neverShownScreenshot, 'never-shown BrowserManager viewport');
    assert.deepEqual({ width: neverShownPixels.width, height: neverShownPixels.height }, { width: 1440, height: 900 });
    saveShot('browser-desktop-never-shown.png', neverShownScreenshot);

    if (CHILD_STAGE === 'bootstrap') {
      const evidence = {
        mechanism: 'LaunchServices starts the pinned Electron app; its main process publishes its PID/start identity before app readiness; BrowserManager verifies the same owned WebContentsView has a ready about:blank document before CDP metrics and the first fixture URL',
        electronVersion: process.versions.electron,
        processIdentity: {
          pid: CHILD_PROCESS_IDENTITY.pid,
          startIdentity: CHILD_PROCESS_IDENTITY.startIdentity,
        },
        startupStages: lifecycleEvents.filter((event) => requiredStartupStages.includes(event.stage)),
        page: initial,
        capture: { ...neverShownPixels, ...neverShownScreenshot.metadata },
        userDataIsolated: userData.startsWith(`${RUN_DIR}${path.sep}`),
        sameNativePage: wc.id === tabA.id,
      };
      fs.writeFileSync(path.join(EVIDENCE, 'browser-desktop-bootstrap.json'), `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 });
      process.stdout.write(`browser-desktop-bootstrap-pass ${JSON.stringify(evidence)}\n`);
      return;
    }

    await observePage(entryA, `(() => { const input = document.querySelector('#first-name'); input.value = 'A retained state'; input.focus(); window.scrollTo(0, 220); return true; })()`);
    const preservedA = await observePage(entryA, `({ input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY })`);
    assert.deepEqual(preservedA, { input: 'A retained state', focused: 'first-name', scrollY: 220 });
    const beforeInvalidViewport = { viewport: { ...entryA.viewport }, generation: entryA.viewportGeneration, page: await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, scrollY })`) };
    await assert.rejects(manager.browserControl('browser.setViewport', { tabId: tabA.id, width: 319, height: 844 }), /width must be an integer/);
    assert.deepEqual({ viewport: entryA.viewport, generation: entryA.viewportGeneration, page: await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, scrollY })`) }, beforeInvalidViewport, 'invalid viewport requests must leave logical metrics and state unchanged');

    const narrowReport = await manager.browserControl('browser.setViewport', { tabId: tabA.id, width: 390, height: 844 });
    assert.equal(narrowReport.chatId, 'smoke-chat-a');
    assert.deepEqual(narrowReport.viewport, { width: 390, height: 844, deviceScaleFactor: 1 });
    assert.deepEqual(narrowReport.effectiveViewport && { width: narrowReport.effectiveViewport.width, height: narrowReport.effectiveViewport.height, deviceScaleFactor: narrowReport.effectiveViewport.deviceScaleFactor }, { width: 390, height: 844, deviceScaleFactor: 1 });
    const narrow = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, mobile: matchMedia('(max-width: 999px)').matches, y: scrollY })`);
    assert.deepEqual(narrow, { width: 390, height: 844, dpr: 1, mobile: true, y: preservedA.scrollY });
    const resetReport = await manager.browserControl('browser.resetViewport', { tabId: tabA.id });
    const reset = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY })`);
    assert.deepEqual(reset, { width: 1440, height: 900, dpr: 1, input: preservedA.input, focused: preservedA.focused, scrollY: preservedA.scrollY });
    assert.equal(resetReport.chatId, 'smoke-chat-a');
    assert.deepEqual(resetReport.viewport, { width: 1440, height: 900, deviceScaleFactor: 1 });
    assert.equal(wc.id, tabA.id, 'viewport changes must keep the same native page');

    const laptopReport = await manager.browserControl('browser.setViewport', { tabId: tabA.id, width: 1280, height: 800 });
    assert.deepEqual(laptopReport.viewport, { width: 1280, height: 800, deviceScaleFactor: 1 });
    const laptop = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, scrollY })`);
    assert.deepEqual(laptop, { width: 1280, height: 800, dpr: 1, scrollY: preservedA.scrollY });
    await manager.browserControl('browser.resetViewport', { tabId: tabA.id });

    const largeReport = await manager.browserControl('browser.setViewport', { tabId: tabA.id, width: 1920, height: 1080 });
    assert.deepEqual(largeReport.viewport, { width: 1920, height: 1080, deviceScaleFactor: 1 });
    const largePage = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio, scrollY })`);
    assert.deepEqual(largePage, { width: 1920, height: 1080, dpr: 1, scrollY: preservedA.scrollY });
    const largeScreenshot = await capture(tabA.id);
    const largePixels = assertShot(largeScreenshot, '1920x1080 custom BrowserManager viewport');
    assert.deepEqual({ width: largePixels.width, height: largePixels.height }, { width: 1920, height: 1080 });
    saveShot('browser-desktop-1920x1080.png', largeScreenshot);
    await manager.browserControl('browser.resetViewport', { tabId: tabA.id });

    const media = await observePage(entryA, `(() => { const marker = document.querySelector('#bottom-marker').getBoundingClientRect(); return { max: document.documentElement.scrollHeight, scrollX, scrollY, marker: { x: marker.x + scrollX, y: marker.y + scrollY, width: marker.width, height: marker.height } }; })()`);
    const priorCaptureHostSize = entryA.captureHost.getContentSize();
    const priorViewBounds = entryA.view.getBounds();
    let fullPageSurfaceAtRequest = null;
    const fullPageSendCommand = entryA.view.webContents.debugger.sendCommand;
    entryA.view.webContents.debugger.sendCommand = function (method, params, ...rest) {
      if (method === 'Page.captureScreenshot' && params?.captureBeyondViewport === true) {
        fullPageSurfaceAtRequest = {
          requestedClip: params.clip,
          hostContentSize: entryA.captureHost?.getContentSize(),
          viewBounds: entryA.view.getBounds(),
          attachedToHiddenHost: entryA.captureHostAttached,
        };
      }
      return fullPageSendCommand.call(this, method, params, ...rest);
    };
    let full;
    try {
      full = await capture(tabA.id, 'full_page');
    } finally {
      entryA.view.webContents.debugger.sendCommand = fullPageSendCommand;
    }
    const expectedFullSurface = {
      width: Math.max(entryA.viewport.width, Math.ceil(fullPageSurfaceAtRequest.requestedClip.x + fullPageSurfaceAtRequest.requestedClip.width)),
      height: Math.max(entryA.viewport.height, Math.ceil(fullPageSurfaceAtRequest.requestedClip.y + fullPageSurfaceAtRequest.requestedClip.height)),
    };
    assert.deepEqual(fullPageSurfaceAtRequest.hostContentSize, [expectedFullSurface.width, expectedFullSurface.height]);
    assert.deepEqual(fullPageSurfaceAtRequest.viewBounds, { x: 0, y: 0, ...expectedFullSurface });
    assert.equal(fullPageSurfaceAtRequest.attachedToHiddenHost, true, 'the full-page request must use the same page on the hidden host');
    assert.ok(fullPageSurfaceAtRequest.hostContentSize[1] >= media.max, 'the full-page native host must reach the fixture document bottom');
    assert.deepEqual(entryA.captureHost.getContentSize(), priorCaptureHostSize, 'the hidden host content extent must be restored after capture');
    assert.deepEqual(entryA.view.getBounds(), priorViewBounds, 'the same native view bounds must be restored after capture');
    const fullPixels = assertShot(full, 'BrowserManager full-page capture');
    assert.ok(fullPixels.height >= media.max - 2, 'full-page capture includes the page extent');
    const afterFull = await observePage(entryA, `(() => { const marker = document.querySelector('#bottom-marker').getBoundingClientRect(); return { max: document.documentElement.scrollHeight, scrollX, scrollY, marker: { x: marker.x + scrollX, y: marker.y + scrollY, width: marker.width, height: marker.height } }; })()`);
    assert.deepEqual(afterFull, media, 'full-page capture must preserve the pre-capture scroll and document geometry');
    const clip = await capture(tabA.id, 'clip', { x: 0, y: 0, width: 320, height: 200 });
    const clipPixels = assertShot(clip, 'BrowserManager clip capture');
    assert.deepEqual({ width: clipPixels.width, height: clipPixels.height }, { width: 320, height: 200 });
    assert.deepEqual(clip.metadata.css_capture_rect, { x: 0, y: 0, width: 320, height: 200 });
    saveShot('browser-desktop-full-page.png', full);
    saveShot('browser-desktop-clip.png', clip);
    const fullBitmap = bitmapFor(Buffer.from(full.base64, 'base64'));
    const topClipBitmap = bitmapFor(Buffer.from(clip.base64, 'base64'));
    for (const [x, y] of [[15, 15], [100, 50], [50, 130], [200, 190]]) {
      const actual = bitmapPixel(topClipBitmap, x, y).slice(0, 3);
      const expected = bitmapPixel(fullBitmap, x, y).slice(0, 3);
      assert.ok(expected.every((channel, index) => Math.abs(actual[index] - channel) <= 32), `clip origin pixels must match the full-page document origin at ${x},${y}: ${actual.join(',')} vs ${expected.join(',')}`);
    }
    assertPixelNear(fullBitmap, 300, 50, [23, 105, 255], 32, 'full-page document origin header marker');
    assertPixelNear(topClipBitmap, 300, 50, [23, 105, 255], 32, 'clip document origin header marker');
    const bottomClipRect = { x: media.marker.x, y: media.marker.y, width: 320, height: media.marker.height };
    const bottomClip = await capture(tabA.id, 'clip', bottomClipRect);
    const bottomClipPng = pngSize(Buffer.from(bottomClip.base64, 'base64'));
    const bottomClipPixels = { width: bottomClipPng.width, height: bottomClipPng.height };
    assert.equal(bottomClipPixels.width, 320);
    assert.ok([Math.floor(media.marker.height), Math.ceil(media.marker.height)].includes(bottomClipPixels.height), 'fractional CSS clip height must map to one adjacent integer raster extent');
    assert.deepEqual(bottomClip.metadata.css_capture_rect, bottomClipRect);
    saveShot('browser-desktop-bottom-marker-clip.png', bottomClip);
    const fullBottomBitmap = bitmapFor(Buffer.from(full.base64, 'base64'));
    const clipBottomBitmap = bitmapFor(Buffer.from(bottomClip.base64, 'base64'));
    const greenPoint = { x: Math.floor(media.marker.x + 160), y: Math.floor(media.marker.y + media.marker.height / 2) };
    const cssGreen = [18, 160, 92];
    const clipGreen = bitmapPixel(clipBottomBitmap, 160, Math.floor(media.marker.height / 2)).slice(0, 3);
    assert.ok(clipGreen.every((channel, index) => Math.abs(channel - cssGreen[index]) <= 64), `bottom clip marker must retain its fixed CSS green under the pinned macOS capture color transform: ${clipGreen.join(',')}`);
    assert.ok(clipGreen[1] >= clipGreen[0] * 1.5 && clipGreen[1] >= clipGreen[2] * 1.3, 'bottom clip reference must remain green, not a repeated white or blue tile');
    const fullGreen = assertPixelNear(fullBottomBitmap, greenPoint.x, greenPoint.y, clipGreen, 12, 'full-page bottom marker must match the independently captured clip');

    let shellZoomPage;
    let shellZoomScreenshot;
    let shellZoomPixels;
    try {
      mainWindow.webContents.setZoomFactor(1.25);
      shellZoomPage = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`);
      assert.deepEqual(shellZoomPage, { width: 1440, height: 900, dpr: 1 }, 'shell zoom must not alter the independent browser metrics');
      shellZoomScreenshot = await capture(tabA.id);
      shellZoomPixels = assertShot(shellZoomScreenshot, 'browser capture while shell webContents zoom is 1.25');
      assert.deepEqual({ width: shellZoomPixels.width, height: shellZoomPixels.height }, { width: 1440, height: 900 });
      saveShot('browser-desktop-shell-zoom.png', shellZoomScreenshot);
    } finally {
      mainWindow.webContents.setZoomFactor(1);
    }

    const delayed = await manager.browserControl('browser.wait', { tabId: tabA.id, condition: 'element_visible', selector: '#delayed', timeoutMs: 2000 });
    assert.equal(delayed.success, true, 'BrowserManager wait observes the delayed fixture element');
    const missingWait = await manager.browserControl('browser.wait', {
      tabId: tabA.id, condition: 'element_visible', selector: '#never-arrives', timeoutMs: 100,
    });
    assert.equal(missingWait.success, false);
    assert.equal(missingWait.timedOut, true, 'a missing element completes at the host-owned deadline');
    assert.equal(entryA.waits.size, 0, 'a completed host wait releases its timers and waiter registration');
    const diagnosticsReady = await manager.browserControl('browser.wait', {
      tabId: tabA.id, condition: 'element_visible', selector: '#diagnostics-ready', timeoutMs: 6000,
    });
    assert.equal(diagnosticsReady.success, true, 'the fixture reports warning and exception only after its frame targets settle');
    const initialDiagnostics = await manager.browserControl('browser.diagnostics', { tabId: tabA.id, limit: 100 });
    milestone('initial-diagnostics-observed', {
      kinds: initialDiagnostics.entries.map((item) => item.kind), count: initialDiagnostics.entries.length,
      runtimeEvents: { ...runtimeEventCounts },
      rawRuntimeEvents: { ...rawRuntimeEventCounts },
      ownedFrameSessions: entryA.targetSessions.size,
      frameAccess: Array.from(entryA.frames.values()).map((frame) => ({
        access: frame.access, root: frame.parentFrameId == null, contextAvailable: frame.contextId != null, unavailable: !!frame.unavailableReason,
      })),
    });
    assert.ok(initialDiagnostics.entries.some((item) => item.kind === 'console.warning'));
    assert.ok(initialDiagnostics.entries.some((item) => item.kind === 'exception'));
    const frameHitLayout = await observePage(entryA, `({ frames: Array.from(document.querySelectorAll('iframe')).map((frame) => { const rect=frame.getBoundingClientRect(); return { x:rect.x,y:rect.y,width:rect.width,height:rect.height }; }), overlay: (() => { const rect=document.querySelector('#overlay').getBoundingClientRect(); return { x:rect.x,y:rect.y,width:rect.width,height:rect.height }; })() })`);
    milestone('frame-hit-layout', frameHitLayout);

    const originalSendCommand = wc.debugger.sendCommand;
    let invalidBatchMouseDispatches = 0;
    wc.debugger.sendCommand = function (method, ...args) {
      if (method === 'Input.dispatchMouseEvent') invalidBatchMouseDispatches += 1;
      return originalSendCommand.call(this, method, ...args);
    };
    try {
      await assert.rejects(manager.browserControl('browser.batch', {
        tabId: tabA.id,
        actions: [{ action: 'click', selector: '#label-target' }, { action: 'key', key: 'BogusModifier+A' }],
      }), /unsupported browser modifier/);
      assert.equal(await observePage(entryA, 'window.labelActionProof'), null, 'batch schema and every key are checked before index zero');
      assert.equal(invalidBatchMouseDispatches, 0, 'full batch prevalidation dispatches no first pointer action');
    } finally {
      wc.debugger.sendCommand = originalSendCommand;
    }

    const interactionSnapshot = await manager.browserControl('browser.snapshot', { tabId: tabA.id });
    assert.ok(Buffer.byteLength(JSON.stringify(interactionSnapshot), 'utf8') <= 64 * 1024);
    assert.equal(interactionSnapshot.truncation.total_bytes, Buffer.byteLength(JSON.stringify(interactionSnapshot), 'utf8'));
    assert.ok(interactionSnapshot.accessibility.axTreeAvailable, 'the owned page AX tree must be read from CDP');
    assert.doesNotMatch(JSON.stringify(interactionSnapshot), /never expose/);
    const labelledFirstName = interactionSnapshot.semantic.find((node) => node.name === 'Name' && node.selector === '#first-name');
    assert.ok(labelledFirstName?.element_ref, 'a labelled field has an opaque observed ref');
    const labelledClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: labelledFirstName.element_ref, snapshotId: interactionSnapshot.snapshot_id,
    });
    assert.equal(labelledClick.ok, true, 'an observed accessible-name ref dispatches a trusted click');
    assert.equal(await observePage(entryA, 'document.activeElement.id'), 'first-name');

    const ambiguous = await manager.browserControl('browser.click', { tabId: tabA.id, selector: 'button' });
    assert.equal(ambiguous.ok, false);
    assert.equal(ambiguous.status, 'ambiguous', 'a repeated selector never picks its first match');
    const disabled = await manager.browserControl('browser.click', { tabId: tabA.id, selector: '#disabled-button' });
    assert.equal(disabled.status, 'disabled');
    const blocked = await manager.browserControl('browser.click', { tabId: tabA.id, selector: '#covered-target' });
    assert.equal(blocked.status, 'blocked', 'the overlay hit test rejects a covered target');

    const labelNode = interactionSnapshot.semantic.find((node) => node.name === 'Label action');
    assert.ok(labelNode?.element_ref);
    const labelClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: labelNode.element_ref, snapshotId: interactionSnapshot.snapshot_id,
    });
    assert.equal(labelClick.ok, true);
    assert.equal((await observePage(entryA, 'window.labelActionProof'))?.trusted, true);

    const nestedNode = interactionSnapshot.semantic.find((node) => node.name === 'Nested scroll region');
    assert.ok(nestedNode?.element_ref);
    const pageScrollBeforeNested = await observePage(entryA, 'scrollY');
    const nestedScroll = await manager.browserControl('browser.scroll', {
      tabId: tabA.id, elementRef: nestedNode.element_ref, snapshotId: interactionSnapshot.snapshot_id, y: 90,
    });
    assert.equal(nestedScroll.nested, true);
    assert.equal(nestedScroll.changed, true);
    assert.equal(await observePage(entryA, 'scrollY'), pageScrollBeforeNested, 'a ref-targeted nested scroll leaves the page scroll unchanged');
    assert.ok(nestedScroll.after.elementY > nestedScroll.before.elementY);

    const shadowNode = interactionSnapshot.semantic.find((node) => node.name === 'Shadow action');
    assert.ok(shadowNode?.element_ref, 'the open-shadow control receives an observed ref');
    const shadowClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: shadowNode.element_ref, snapshotId: interactionSnapshot.snapshot_id,
    });
    assert.equal(shadowClick.ok, true);
    assert.equal((await observePage(entryA, 'window.shadowActionProof'))?.trusted, true);
    const canvasClick = await manager.browserControl('browser.click', { tabId: tabA.id, selector: '#canvas' });
    assert.equal(canvasClick.ok, true, 'canvas controls accept a geometry-verified native pointer');
    assert.equal((await observePage(entryA, 'window.canvasActionProof'))?.trusted, true);

    const sameFrameButton = interactionSnapshot.semantic.find((node) => node.name === 'Same frame action');
    const crossFrameButton = interactionSnapshot.semantic.find((node) => node.name === 'Cross frame action');
    const sameFrameInput = interactionSnapshot.semantic.find((node) => node.name === 'Same input');
    const crossFrameInput = interactionSnapshot.semantic.find((node) => node.name === 'Cross input');
    assert.ok(sameFrameButton?.element_ref, 'same-origin owned frame DOM/AX nodes are observable');
    assert.ok(crossFrameButton?.element_ref, 'cross-origin owned frame DOM/AX nodes are observable');
    assert.ok(sameFrameInput?.element_ref);
    assert.ok(crossFrameInput?.element_ref);
    assert.ok(interactionSnapshot.frames.some((frame) => frame.access === 'same_target_context' && frame.accessible));
    assert.ok(interactionSnapshot.frames.some((frame) => frame.access === 'owned_oopif' && frame.accessible));
    const sameFrameTyped = await manager.browserControl('browser.type', {
      tabId: tabA.id, elementRef: sameFrameInput.element_ref, snapshotId: interactionSnapshot.snapshot_id, text: 'same-frame-value',
    });
    assert.equal(sameFrameTyped.replacementVerified, true);
    const sameFrameSelectAll = await manager.browserControl('browser.key', { tabId: tabA.id, key: 'Meta+A' });
    assert.equal(sameFrameSelectAll.selectionVerified, true, 'a standalone key follows focus into the same-origin frame');
    const sameFrameDelete = await manager.browserControl('browser.key', { tabId: tabA.id, key: 'Backspace' });
    assert.equal(sameFrameDelete.deletionVerified, true);
    assert.equal(await observePage(entryA, 'document.querySelector(\'iframe[title="Same origin frame"]\').contentDocument.querySelector("#same-input").value'), '');
    const sameFrameClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: sameFrameButton.element_ref, snapshotId: interactionSnapshot.snapshot_id,
    });
    assert.equal(sameFrameClick.ok, true, JSON.stringify(sameFrameClick));
    assert.equal(await observePage(entryA, 'document.querySelector(\'iframe[title="Same origin frame"]\').contentWindow.sameProof?.trusted'), true);
    const crossFrameTyped = await manager.browserControl('browser.type', {
      tabId: tabA.id, elementRef: crossFrameInput.element_ref, snapshotId: interactionSnapshot.snapshot_id, text: 'cross-frame-value',
    });
    assert.equal(crossFrameTyped.replacementVerified, true);
    const crossFrameSelectAll = await manager.browserControl('browser.key', { tabId: tabA.id, key: 'Meta+A' });
    assert.equal(crossFrameSelectAll.selectionVerified, true, 'a standalone key follows focus into the owned OOPIF');
    const crossFrameDelete = await manager.browserControl('browser.key', { tabId: tabA.id, key: 'Backspace' });
    assert.equal(crossFrameDelete.deletionVerified, true);
    const crossOwnedFrame = Array.from(entryA.frames.values()).find((frame) => frame.access === 'owned_oopif' && /^http:\/\/localhost:\d+\//u.test(frame.url));
    assert.ok(crossOwnedFrame, 'the cross-origin fixture has a live owned OOPIF');
    assert.equal(await manager.evaluateFrame(entryA, crossOwnedFrame.id, 'document.querySelector("#cross-input").value'), '');
    const crossFrameClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: crossFrameButton.element_ref, snapshotId: interactionSnapshot.snapshot_id,
    });
    assert.equal(crossFrameClick.ok, true);
    const crossMessageWait = await manager.browserControl('browser.wait', {
      tabId: tabA.id, condition: 'element_visible', selector: '#cross-message-proof', timeoutMs: 2000,
    });
    const crossFrameProof = crossOwnedFrame ? await manager.evaluateFrame(entryA, crossOwnedFrame.id, 'window.crossProof') : null;
    milestone('cross-frame-click-result', {
      click: crossFrameClick, wait: crossMessageWait, childHandlerTrusted: crossFrameProof?.trusted === true,
      sameFrameKey: sameFrameSelectAll.selectionVerified && sameFrameDelete.deletionVerified,
      crossFrameKey: crossFrameSelectAll.selectionVerified && crossFrameDelete.deletionVerified,
    });
    assert.equal(crossMessageWait.success, true);
    assert.equal((await observePage(entryA, 'window.crossFrameActionProof'))?.trusted, true);

    const editorNode = interactionSnapshot.semantic.find((node) => node.name === 'Code editor');
    assert.ok(editorNode?.element_ref);
    const editorText = 'const firstLine = true;\nconst secondLine = "fixture";';
    const editorTyped = await manager.browserControl('browser.type', {
      tabId: tabA.id, elementRef: editorNode.element_ref, snapshotId: interactionSnapshot.snapshot_id, text: editorText,
    });
    assert.equal(editorTyped.replacementVerified, true, 'multiline editor input is verified in its target realm');
    const editorCleared = await manager.browserControl('browser.type', {
      tabId: tabA.id, elementRef: editorNode.element_ref, snapshotId: interactionSnapshot.snapshot_id, text: '',
    });
    assert.equal(editorCleared.replacementVerified, true, 'empty editor replacement is verified: ' + JSON.stringify(editorCleared));
    assert.equal(await observePage(entryA, '(() => { const editor=document.querySelector("#editor"); return editor.textContent === "" && editor.innerText.trim() === ""; })()'), true);

    const replaceSnapshot = await manager.browserControl('browser.snapshot', { tabId: tabA.id });
    const replaceNode = replaceSnapshot.semantic.find((node) => node.selector === '#replace-target');
    assert.ok(replaceNode?.element_ref);
    await observePage(entryA, '(() => { const old = document.querySelector("#replace-target"); const replacement = document.createElement("button"); replacement.id = "replace-target"; replacement.textContent = "Replacement action"; old.replaceWith(replacement); return true; })()');
    const staleReplace = await manager.browserControl('browser.click', {
      tabId: tabA.id, elementRef: replaceNode.element_ref, snapshotId: replaceSnapshot.snapshot_id,
    });
    assert.equal(staleReplace.status, 'stale', 'same-selector node replacement does not rebind an old ref');
    const labelCountBeforeStoppedBatch = (await observePage(entryA, 'window.labelActionProof')).count;
    const stoppedBatch = await manager.browserControl('browser.batch', {
      tabId: tabA.id,
      actions: [
        { action: 'click', elementRef: replaceNode.element_ref, snapshotId: replaceSnapshot.snapshot_id },
        { action: 'click', selector: '#label-target' },
      ],
    });
    assert.equal(stoppedBatch.failed_index, 0);
    assert.deepEqual(stoppedBatch.unexecuted_indexes, [1]);
    assert.equal((await observePage(entryA, 'window.labelActionProof')).count, labelCountBeforeStoppedBatch);

    await manager.browserControl('browser.scroll', { tabId: tabA.id, x: 0, y: 35 });
    const scrolledScreenshot = await capture(tabA.id);
    saveShot('browser-desktop-scrolled-viewport.png', scrolledScreenshot);
    const observedScrollY = await observePage(entryA, 'scrollY');
    assert.equal(scrolledScreenshot.metadata.scroll_origin.y, observedScrollY);
    const scrolledOriginClip = await capture(tabA.id, 'clip', { x: 0, y: observedScrollY, width: 320, height: 200 });
    saveShot('browser-desktop-scrolled-origin-clip.png', scrolledOriginClip);
    const viewportOriginPixels = bitmapFor(Buffer.from(scrolledScreenshot.base64, 'base64'));
    const documentOriginPixels = bitmapFor(Buffer.from(scrolledOriginClip.base64, 'base64'));
    for (const [x, y] of [[15, 15], [100, 50], [300, 130]]) {
      const viewportPixel = bitmapPixel(viewportOriginPixels, x, y).slice(0, 3);
      const documentPixel = bitmapPixel(documentOriginPixels, x, y).slice(0, 3);
      assert.ok(viewportPixel.every((channel, index) => Math.abs(channel - documentPixel[index]) <= 8),
        `nonzero-scroll viewport pixel ${x},${y} must match the document clip at scroll origin ${observedScrollY}: ${viewportPixel.join(',')} vs ${documentPixel.join(',')}`);
    }
    await manager.browserControl('browser.scroll', { tabId: tabA.id, x: 0, y: 1 });
    const staleScrollScreenshot = await manager.browserControl('browser.click', {
      tabId: tabA.id, screenshotId: scrolledScreenshot.metadata.screenshot_id, x: 10, y: 10,
    });
    assert.equal(staleScrollScreenshot.status, 'stale', 'a page scroll invalidates screenshot-coordinate input');
    const coordinateScreenshot = await capture(tabA.id);
    const coordinateRect = await observePage(entryA, 'document.querySelector("#coordinate-target").getBoundingClientRect().toJSON()');
    const coordinateClick = await manager.browserControl('browser.click', {
      tabId: tabA.id, screenshotId: coordinateScreenshot.metadata.screenshot_id,
      x: coordinateRect.x + coordinateRect.width / 2, y: coordinateRect.y + coordinateRect.height / 2,
    });
    assert.equal(coordinateClick.ok, true);
    const coordinateProof = await observePage(entryA, 'window.coordinateActionProof');
    assert.equal(coordinateProof?.trusted, true);

    const metricsScreenshot = await capture(tabA.id);
    await manager.browserControl('browser.setViewport', { tabId: tabA.id, width: 1280, height: 800 });
    const staleMetricsScreenshot = await manager.browserControl('browser.click', {
      tabId: tabA.id, screenshotId: metricsScreenshot.metadata.screenshot_id, x: 10, y: 10,
    });
    assert.equal(staleMetricsScreenshot.status, 'stale', 'viewport generation changes invalidate screenshot-coordinate input');
    await manager.browserControl('browser.resetViewport', { tabId: tabA.id });

    await observePage(entryA, '(() => { const host = document.createElement("section"); host.id = "snapshot-stress"; for (let index = 0; index < 450; index += 1) { const button = document.createElement("button"); button.setAttribute("aria-label", "bounded-node-" + index + "-" + "x".repeat(180)); button.textContent = "bounded content " + "y".repeat(160); host.append(button); } document.body.append(host); return true; })()');
    const boundedSnapshot = await manager.browserControl('browser.snapshot', { tabId: tabA.id });
    assert.ok(Buffer.byteLength(JSON.stringify(boundedSnapshot), 'utf8') <= 64 * 1024);
    assert.equal(boundedSnapshot.truncation.total_bytes, Buffer.byteLength(JSON.stringify(boundedSnapshot), 'utf8'));
    assert.ok(boundedSnapshot.text_bytes_scanned >= boundedSnapshot.text_bytes_returned);
    await observePage(entryA, 'document.querySelector("#snapshot-stress").remove()');

    await observePage(entryA, '(() => { console.warn("token=SMOKE_SECRET_REDACTED"); for (let index = 0; index < 220; index += 1) console.warn("retention-marker-" + index); return true; })()');
    await delay(200);
    const diagnostics = await manager.browserControl('browser.diagnostics', {
      tabId: tabA.id, afterSequence: Math.max(0, entryA.diagnosticSequence - 100), limit: 100,
    });
    const diagnosticsFromOrigin = await manager.browserControl('browser.diagnostics', { tabId: tabA.id, afterSequence: 0, limit: 100 });
    assert.ok(diagnostics.entries.some((item) => item.message.includes('retention-marker-219')));
    assert.ok(diagnosticsFromOrigin.truncated, 'the diagnostic cursor reports loss when the 200-entry ring rolls over');
    assert.doesNotMatch(JSON.stringify(diagnostics), /SMOKE_SECRET_REDACTED/);

    const interactionEvidence = {
      snapshotBytes: Buffer.byteLength(JSON.stringify(boundedSnapshot), 'utf8'),
      snapshotTruncation: boundedSnapshot.truncation,
      accessibility: interactionSnapshot.accessibility,
      frames: interactionSnapshot.frames.map((frame) => ({ access: frame.access, accessible: frame.accessible, limitation: frame.limitation })),
      labels: { refClickTrusted: labelledClick.trusted, repeatedSelectorStatus: ambiguous.status },
      targetFailures: { covered: blocked.status, disabled: disabled.status, staleReplacement: staleReplace.status },
      shadowClickTrusted: (await observePage(entryA, 'window.shadowActionProof'))?.trusted,
      canvasClickTrusted: (await observePage(entryA, 'window.canvasActionProof'))?.trusted,
      frameActions: { sameOrigin: sameFrameClick.trusted, crossOrigin: crossFrameClick.trusted },
      editor: { multilineVerified: editorTyped.replacementVerified, emptyVerified: editorCleared.replacementVerified },
      nestedScroll,
      coordinate: { screenshotOriginY: coordinateScreenshot.metadata.scroll_origin.y, scrolledViewportMatchesDocumentClip: true, trusted: coordinateProof?.trusted },
      staleScreenshot: { afterScroll: staleScrollScreenshot.status, afterMetrics: staleMetricsScreenshot.status },
      waits: { delayed: delayed.success, timedOut: missingWait.timedOut, frameMessage: crossMessageWait.success },
      batch: { prevalidatedBeforeMutation: true, stoppedAt: stoppedBatch.failed_index },
      diagnostics: { retainedLatest: true, truncated: diagnosticsFromOrigin.truncated, secretRedacted: true },
      navigationStaleRef: null,
    };
    milestone('interaction-matrix-complete', {
      snapshots: { bytes: interactionEvidence.snapshotBytes, ax: interactionEvidence.accessibility.axTreeAvailable },
      frameCount: interactionEvidence.frames.length,
    });
    await delay(100);

    // Keep page B's DOM state live while A is changed and captured in the background.
    const { entry: entryB, tab: tabB } = await openBackground('smoke-chat-b', `http://127.0.0.1:${localPort}/fixture?chat=b`);
    assert.notEqual(tabB.id, tabA.id);
    const navigationSnapshot = await manager.browserControl('browser.snapshot', { tabId: tabB.id });
    const navigationTarget = navigationSnapshot.semantic.find((node) => node.selector === '#first-name');
    assert.ok(navigationTarget?.element_ref);
    const navigationScreenshot = await capture(tabB.id);
    await manager.browserControl('browser.navigate', {
      tabId: tabB.id, url: `http://127.0.0.1:${localPort}/fixture?chat=b-navigation-replacement`,
    });
    const navigationStaleRef = await manager.browserControl('browser.click', {
      tabId: tabB.id, elementRef: navigationTarget.element_ref, snapshotId: navigationSnapshot.snapshot_id,
    });
    assert.equal(navigationStaleRef.status, 'stale', 'a navigation invalidates an observed ref without rebinding it');
    const navigationStaleScreenshot = await manager.browserControl('browser.click', {
      tabId: tabB.id, screenshotId: navigationScreenshot.metadata.screenshot_id, x: 10, y: 10,
    });
    assert.equal(navigationStaleScreenshot.status, 'stale', 'a navigation invalidates earlier screenshot coordinates');
    interactionEvidence.navigationStaleRef = navigationStaleRef.status;
    interactionEvidence.staleScreenshot.afterNavigation = navigationStaleScreenshot.status;
    milestone('navigation-stale-ref-verified', { tabId: tabB.id });
    await manager.browserControl('browser.navigate', { tabId: tabB.id, url: `http://127.0.0.1:${localPort}/fixture?chat=b` });
    await observePage(entryB, `(() => { const input = document.querySelector('#first-name'); input.value = 'B retained state'; input.focus(); window.scrollTo(0, 410); return true; })()`);
    await manager.activate({ chatId: 'smoke-chat-b', bounds: { x: 0, y: 0, width: 312, height: 195 } });
    assert.equal(manager.activeId, 'smoke-chat-b');
    assert.equal(entryA.visible, false);
    assert.equal(entryB.visible, true);
    assert.deepEqual({ width: entryA.viewport.width, height: entryA.viewport.height }, { width: 1440, height: 900 });
    assert.deepEqual({ width: entryB.viewport.width, height: entryB.viewport.height }, { width: 1440, height: 900 });
    const bBeforeBackgroundCapture = await observePage(entryB, `({ url: location.href, input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`);
    assert.equal(bBeforeBackgroundCapture.input, 'B retained state');
    assert.equal(bBeforeBackgroundCapture.focused, 'first-name');
    assert.equal(bBeforeBackgroundCapture.scrollY, 410);
    const aBeforeBackgroundCapture = await observePage(entryA, `({ url: location.href, input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`);
    const otherChatScreenshot = await capture(tabA.id);
    const otherChatPixels = assertShot(otherChatScreenshot, 'BrowserManager capture while another chat is selected');
    assert.deepEqual({ width: otherChatPixels.width, height: otherChatPixels.height }, { width: 1440, height: 900 });
    saveShot('browser-desktop-other-chat.png', otherChatScreenshot);
    assert.equal(manager.activeId, 'smoke-chat-b', 'background capture must preserve the selected chat');
    assert.equal(wc.id, tabA.id);
    assert.deepEqual(await observePage(entryB, `({ url: location.href, input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`), bBeforeBackgroundCapture, 'capturing A must preserve B URL, form, focus, scroll, and viewport');
    assert.deepEqual(await observePage(entryA, `({ url: location.href, input: document.querySelector('#first-name').value, focused: document.activeElement.id, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`), aBeforeBackgroundCapture, 'background capture must preserve A document state');
    const selectedChatDuringBackgroundCapture = manager.activeId;
    milestone('background-capture-state-verified', { selectedChat: selectedChatDuringBackgroundCapture });

    // Minimize with B still attached to the shell, then exercise both fallback
    // and throwing capture restoration on the same selected native view.
    mainWindow.showInactive();
    mainWindow.minimize();
    await delay(100);
    assert.equal(mainWindow.isMinimized(), true, 'the isolated shell window must be minimized for this capture');
    assert.equal(manager.attachedView, entryB.view, 'minimizing must not detach B from its BrowserWindow');
    assert.equal(entryB.visible, true, 'B remains logically presented while minimized');
    const bOriginalSendCommand = entryB.view.webContents.debugger.sendCommand;
    const originalApplyEmulation = manager.applyAndReadDeviceEmulation;
    manager.applyAndReadDeviceEmulation = async function (entry, ...args) {
      const effective = await originalApplyEmulation.call(this, entry, ...args);
      if (entry === entryB) {
        milestone('minimized-capture-emulation-applied', {
          scale: args[1],
          effective,
          page: await observePage(entryB, '({ scrollX, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio, documentHeight: document.documentElement.scrollHeight })'),
          attachedToShell: manager.attachedView === entryB.view,
          hostAttached: entryB.captureHostAttached,
        });
      }
      return effective;
    };
    let injectedCaptureFailures = 0;
    entryB.view.webContents.debugger.sendCommand = function (method, ...args) {
      if (method === 'Page.captureScreenshot' && injectedCaptureFailures++ === 0) return Promise.reject(new Error('injected first capture request failure'));
      return bOriginalSendCommand.call(this, method, ...args);
    };
    let minimizedScreenshot;
    try {
      milestone('before-minimized-attached-capture', {
        page: await observePage(entryB, '({ scrollX, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })'),
        effective: await manager.readEffectiveViewport(entryB),
      });
      try {
        minimizedScreenshot = await capture(tabB.id);
      } catch (error) {
        milestone('minimized-attached-capture-failed', {
          error: String(error && error.message || error).slice(0, 256),
          page: await observePage(entryB, '({ scrollX, scrollY, width: innerWidth, height: innerHeight, dpr: devicePixelRatio })'),
          effective: await manager.readEffectiveViewport(entryB),
          cachedEffective: entryB.effectiveViewport,
          attachedToShell: manager.attachedView === entryB.view,
          hostAttached: entryB.captureHostAttached,
        });
        throw error;
      }
    } finally {
      entryB.view.webContents.debugger.sendCommand = bOriginalSendCommand;
      manager.applyAndReadDeviceEmulation = originalApplyEmulation;
    }
    milestone('minimized-attached-capture-complete', { image: minimizedScreenshot.metadata.image, captureSurface: minimizedScreenshot.metadata.capture_surface });
    assert.equal(injectedCaptureFailures, 2, 'a thrown first request must retry against the same page on its hidden host');
    assert.equal(minimizedScreenshot.metadata.capture_surface, 'hidden_host');
    assert.equal(manager.attachedView, entryB.view, 'successful fallback must restore B to the owning shell');
    assert.equal(entryB.visible, true);
    const minimizedPixels = assertShot(minimizedScreenshot, 'BrowserManager minimized attached-view fallback capture');
    assert.deepEqual({ width: minimizedPixels.width, height: minimizedPixels.height }, { width: 1440, height: 900 });
    saveShot('browser-desktop-minimized-attached.png', minimizedScreenshot);

    const bFailingSendCommand = entryB.view.webContents.debugger.sendCommand;
    let forcedCaptureFailures = 0;
    entryB.view.webContents.debugger.sendCommand = function (method, ...args) {
      if (method === 'Page.captureScreenshot') { forcedCaptureFailures += 1; return Promise.reject(new Error('injected persistent capture failure')); }
      return bFailingSendCommand.call(this, method, ...args);
    };
    try {
      await assert.rejects(capture(tabB.id), /first surface.*same-page hidden host/);
    } finally {
      entryB.view.webContents.debugger.sendCommand = bFailingSendCommand;
    }
    assert.equal(forcedCaptureFailures, 2, 'both capture surfaces must be attempted after a request throw');
    assert.equal(manager.attachedView, entryB.view, 'failed fallback must still restore the original attachment');
    assert.equal(entryB.visible, true);
    const expectedBScale = Math.min(entryB.presentationBounds.width / entryB.viewport.width, entryB.presentationBounds.height / entryB.viewport.height, 1);
    assert.equal(entryB.presentationScale, expectedBScale, 'failed fallback must restore the presentation scale');

    const attachedMinimizedScreenshot = await capture(tabA.id);
    const attachedMinimizedPixels = assertShot(attachedMinimizedScreenshot, 'background A capture while minimized B stays attached');
    assert.deepEqual({ width: attachedMinimizedPixels.width, height: attachedMinimizedPixels.height }, { width: 1440, height: 900 });
    saveShot('browser-desktop-minimized-background.png', attachedMinimizedScreenshot);
    assert.equal(manager.attachedView, entryB.view, 'A background capture must leave minimized B attached');

    assert.equal(await manager.hide('smoke-chat-b'), true);
    assert.equal(manager.attachedView, null);
    const paneClosedScreenshotFinal = await capture(tabB.id);
    const paneClosedPixels = assertShot(paneClosedScreenshotFinal, 'BrowserManager pane-closed minimized capture');
    assert.deepEqual({ width: paneClosedPixels.width, height: paneClosedPixels.height }, { width: 1440, height: 900 });
    saveShot('browser-desktop-pane-closed-minimized.png', paneClosedScreenshotFinal);
    milestone('pane-closed-capture-complete', { image: paneClosedPixels });

    milestone('rendering-matrix-complete-before-native-input', {
      fullPage: full.metadata.image,
      fullPageSurface: fullPageSurfaceAtRequest,
      paneClosedMinimized: { width: paneClosedPixels.width, height: paneClosedPixels.height },
    });
    if (CHILD_STAGE === 'rendering') {
      const shellWindowState = { visible: mainWindow.isVisible(), minimized: mainWindow.isMinimized() };
      const windowsBeforeShellClose = BrowserWindow.getAllWindows().filter((window) => !window.isDestroyed()).length;
      assert.ok(windowsBeforeShellClose >= 3, 'the isolated shell plus hidden capture hosts must be live before close');
      const shellClosed = new Promise((resolve) => mainWindow.once('closed', resolve));
      mainWindow.once('closed', () => manager.destroy());
      mainWindow.destroy();
      await shellClosed;
      const windowsAfterShellClose = BrowserWindow.getAllWindows().filter((window) => !window.isDestroyed()).length;
      assert.equal(windowsAfterShellClose, 0, 'shell teardown must destroy hidden capture hosts before window-all-closed/activate checks');
      let reactivatedWindow = null;
      app.once('activate', () => {
        if (BrowserWindow.getAllWindows().length === 0) reactivatedWindow = new BrowserWindow({ width: 800, height: 600, show: false });
      });
      app.emit('activate');
      assert.ok(reactivatedWindow && !reactivatedWindow.isDestroyed(), 'activate with no remaining hidden hosts must create a fresh shell window');
      // Keep the replacement alive until main() resolves so Electron cannot
      // auto-quit before the child writes its successful final receipt.
      const renderingEvidence = {
        stage: 'rendering',
        electronVersion: process.versions.electron,
        chromeVersion: process.versions.chrome,
        userDataIsolated: userData.startsWith(`${RUN_DIR}${path.sep}`),
        defaultViewport: initial,
        defaultBackgroundCapture: { ...neverShownPixels, ...neverShownScreenshot.metadata },
        narrowViewport: { page: narrow, reported: narrowReport.viewport },
        resetViewport: { page: reset, reported: resetReport.viewport },
        laptopViewport: { page: laptop, reported: laptopReport.viewport },
        customViewport1920: { page: largePage, reported: largeReport.viewport, image: { width: largePixels.width, height: largePixels.height }, metadata: largeScreenshot.metadata },
        invalidViewportNoMutation: true,
        fullPage: { ...fullPixels, metadata: full.metadata },
        clip: { ...clipPixels, metadata: clip.metadata },
        fullPageSurfaceExpansion: { atRequest: fullPageSurfaceAtRequest, restoredHostContentSize: priorCaptureHostSize, restoredViewBounds: priorViewBounds },
        fullPageStateAfterCapture: afterFull,
        deterministicPixels: { topCropEqual: true, bottomMarkerCssRgb: cssGreen, bottomMarkerRasterRgb: fullGreen, bottomMarkerClipRasterRgb: clipGreen, bottomMarkerPoint: greenPoint, clipOrigin: bottomClipRect },
        shellZoom: { factor: 1.25, logical: shellZoomPage, image: { width: shellZoomPixels.width, height: shellZoomPixels.height } },
        otherChatCapture: { ...otherChatPixels, ...otherChatScreenshot.metadata },
        minimizedAttachedFallbackCapture: { ...minimizedPixels, ...minimizedScreenshot.metadata },
        minimizedBackgroundCapture: { ...attachedMinimizedPixels, ...attachedMinimizedScreenshot.metadata },
        paneClosedMinimizedCapture: { ...paneClosedPixels, ...paneClosedScreenshotFinal.metadata },
        fallbackRestoration: { initialFailureRetried: injectedCaptureFailures === 2, persistentFailureRequests: forcedCaptureFailures, attachedAfterFailure: true, presentationScaleAfterFailure: entryB.presentationScale },
        sameNativePage: wc.id === tabA.id,
        windowStateBeforeClose: shellWindowState,
        shellLifecycle: { windowsBeforeClose: windowsBeforeShellClose, windowsAfterClose: windowsAfterShellClose, reactivatedWindowCreated: true },
      };
      fs.writeFileSync(path.join(EVIDENCE, 'browser-desktop-rendering.json'), `${JSON.stringify(renderingEvidence, null, 2)}\n`);
      process.stdout.write(`browser-desktop-rendering-pass ${JSON.stringify(renderingEvidence)}\n`);
      return;
    }

    // Re-present A in a narrow rail and deliver pointer input at its actual
    // scaled WebContentsView coordinates while the isolated window is focused.
    milestone('before-native-input-representation');
    await manager.activate({ chatId: 'smoke-chat-a', bounds: { x: 0, y: 0, width: 312, height: 195 } });
    milestone('after-native-input-representation', { activeId: manager.activeId, attached: manager.attachedView === entryA.view });
    const presentationScale = entryA.presentationScale;
    assert.ok(presentationScale > 0 && presentationScale < 1);
    const scaledViewport = await observePage(entryA, `({ width: innerWidth, height: innerHeight, dpr: devicePixelRatio })`);
    assert.deepEqual(scaledViewport, { width: 1440, height: 900, dpr: 1 }, 'fitting must not change logical page metrics');
    const pointerRect = await observePage(entryA, `document.querySelector('#pointer-target').getBoundingClientRect().toJSON()`);
    const viewBounds = entryA.view.getBounds();
    assert.deepEqual({ width: viewBounds.width, height: viewBounds.height }, { width: 312, height: 195 });
    const nativePoint = {
      x: Math.round((pointerRect.x + pointerRect.width / 2) * presentationScale),
      y: Math.round((pointerRect.y + pointerRect.height / 2) * presentationScale),
    };
    assert.ok(nativePoint.x >= 0 && nativePoint.x < viewBounds.width && nativePoint.y >= 0 && nativePoint.y < viewBounds.height);
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.show();
    mainWindow.focus();
    await delay(100);
    assert.equal(mainWindow.isFocused(), true, 'Electron native-view input requires the isolated containing window to be focused');
    milestone('native-input-window-focused', { focused: mainWindow.isFocused(), visible: mainWindow.isVisible(), minimized: mainWindow.isMinimized() });
    milestone('before-native-input-capture', { nativePoint, viewBounds });
    const scaledScreenshot = await capture(tabA.id);
    milestone('after-native-input-capture', { image: scaledScreenshot.metadata.image });
    entryA.view.webContents.sendInputEvent({ type: 'mouseMove', x: nativePoint.x, y: nativePoint.y });
    entryA.view.webContents.sendInputEvent({ type: 'mouseDown', x: nativePoint.x, y: nativePoint.y, button: 'left', clickCount: 1 });
    entryA.view.webContents.sendInputEvent({ type: 'mouseUp', x: nativePoint.x, y: nativePoint.y, button: 'left', clickCount: 1 });
    milestone('native-input-dispatched', { focused: mainWindow.isFocused(), nativePoint, viewBounds });
    await delay(50);
    milestone('before-native-input-proof-read');
    const pointerProof = await observePage(entryA, 'window.pointerProof');
    milestone('after-native-input-proof-read', { trusted: pointerProof?.trusted === true });
    assert.equal(pointerProof?.trusted, true, 'native scaled-view input must dispatch a trusted click to the pointer fixture');
    assert.ok(Math.abs(pointerProof.x - (pointerRect.x + pointerRect.width / 2)) <= 3);
    assert.ok(Math.abs(pointerProof.y - (pointerRect.y + pointerRect.height / 2)) <= 3);
    saveShot('browser-desktop-scaled-input.png', scaledScreenshot);

    const finalWindowState = { visible: mainWindow.isVisible(), minimized: mainWindow.isMinimized() };
    const captureHostStateBeforeClose = {
      used: !!entryA.captureHost,
      attached: entryA.captureHostAttached,
      sameNativePage: wc.id === tabA.id,
      restoredHostContentSize: priorCaptureHostSize,
      restoredViewBounds: priorViewBounds,
    };
    const windowsBeforeShellClose = BrowserWindow.getAllWindows().filter((window) => !window.isDestroyed()).length;
    assert.ok(windowsBeforeShellClose >= 3, 'the isolated shell plus hidden capture hosts must be live before close');
    const shellClosed = new Promise((resolve) => mainWindow.once('closed', resolve));
    mainWindow.once('closed', () => manager.destroy());
    mainWindow.destroy();
    await shellClosed;
    const windowsAfterShellClose = BrowserWindow.getAllWindows().filter((window) => !window.isDestroyed()).length;
    assert.equal(windowsAfterShellClose, 0, 'shell teardown must destroy hidden capture hosts before window-all-closed/activate checks');
    let reactivatedWindow = null;
    app.once('activate', () => {
      if (BrowserWindow.getAllWindows().length === 0) reactivatedWindow = new BrowserWindow({ width: 800, height: 600, show: false });
    });
    app.emit('activate');
    assert.ok(reactivatedWindow && !reactivatedWindow.isDestroyed(), 'activate with no remaining hidden hosts must create a fresh shell window');
    reactivatedWindow.destroy();

    const evidence = {
      mechanism: 'BrowserManager initialized and retained the same owned WebContentsView on a hidden non-focusing host, verified logical metrics, and captured through BrowserManager across chat, pane, and window visibility changes',
      electronVersion: process.versions.electron,
      chromeVersion: process.versions.chrome,
      userDataIsolated: userData.startsWith(`${RUN_DIR}${path.sep}`),
      selectedChatDuringBackgroundCapture,
      windowVisible: finalWindowState.visible,
      windowMinimized: finalWindowState.minimized,
      defaultViewport: initial,
      defaultBackgroundCapture: { ...neverShownPixels, ...neverShownScreenshot.metadata },
      otherChatCapture: { ...otherChatPixels, ...otherChatScreenshot.metadata },
      minimizedAttachedFallbackCapture: { ...minimizedPixels, ...minimizedScreenshot.metadata },
      minimizedBackgroundCapture: { ...attachedMinimizedPixels, ...attachedMinimizedScreenshot.metadata },
      paneClosedMinimizedCapture: { ...paneClosedPixels, ...paneClosedScreenshotFinal.metadata },
      minimizedCapture: { ...minimizedPixels, ...minimizedScreenshot.metadata },
      narrowViewport: { page: narrow, reported: narrowReport.viewport },
      resetViewport: { page: reset, reported: resetReport.viewport },
      laptopViewport: { page: laptop, reported: laptopReport.viewport },
      customViewport1920: { page: largePage, reported: largeReport.viewport, image: { width: largePixels.width, height: largePixels.height }, metadata: largeScreenshot.metadata },
      fullPage: { ...fullPixels, metadata: full.metadata },
      clip: { ...clipPixels, metadata: clip.metadata },
      fullPageSurfaceExpansion: { atRequest: fullPageSurfaceAtRequest, restoredHostContentSize: captureHostStateBeforeClose.restoredHostContentSize, restoredViewBounds: captureHostStateBeforeClose.restoredViewBounds },
      fullPageStateAfterCapture: afterFull,
      deterministicPixels: { topCropEqual: true, bottomMarkerCssRgb: cssGreen, bottomMarkerRasterRgb: fullGreen, bottomMarkerClipRasterRgb: clipGreen, bottomMarkerPoint: greenPoint, clipOrigin: bottomClipRect },
      shellZoom: { factor: 1.25, logical: shellZoomPage, image: { width: shellZoomPixels.width, height: shellZoomPixels.height } },
      invalidViewportNoMutation: true,
      delayedWait: delayed,
      diagnosticKinds: diagnostics.entries.map((item) => item.kind),
      interaction: interactionEvidence,
      scaledPresentation: { scale: presentationScale, logical: scaledViewport, nativeViewBounds: viewBounds, inputPoint: nativePoint, focusedTestWindow: true, trustedPointer: pointerProof },
      fallbackRestoration: { initialFailureRetried: injectedCaptureFailures === 2, persistentFailureRequests: forcedCaptureFailures, attachedAfterFailure: true, presentationScaleAfterFailure: entryB.presentationScale },
      shellLifecycle: { windowsBeforeClose: windowsBeforeShellClose, windowsAfterClose: windowsAfterShellClose, reactivatedWindowCreated: true },
      sameNativePage: captureHostStateBeforeClose.sameNativePage,
      captureHostUsed: captureHostStateBeforeClose.used,
      captureHostAttached: captureHostStateBeforeClose.attached,
      tabIds: [tabA.id, tabB.id],
    };
    fs.writeFileSync(path.join(EVIDENCE, 'browser-desktop-smoke.json'), `${JSON.stringify(evidence, null, 2)}\n`);
    process.stdout.write(`browser-desktop-smoke-pass ${JSON.stringify(evidence)}\n`);
  } catch (error) {
    const detail = String(error && error.message || error)
      .replace(/\b(api[_-]?key|token|secret|password|credential|bearer)\b([:=\s]+)[^\s,;]+/giu, '$1$2[redacted]')
      .slice(0, 1000);
    finishChildRun?.(1, `smoke_failed: ${detail}`);
    throw error;
  } finally {
    try { if (manager) manager.destroy(); } catch { /* smoke teardown */ }
    try { if (mainWindow && !mainWindow.isDestroyed()) mainWindow.destroy(); } catch { /* smoke teardown */ }
    await new Promise((resolve) => local.close(resolve));
    await new Promise((resolve) => cross.close(resolve));
  }
}
if (CHILD) {
  main().then(() => {
    finishChildRun?.(0);
    app.removeListener('before-quit', holdApplicationUntilSmokeReceipt);
    app.quit();
  }, (error) => {
    const detail = String(error && (error.stack || error.message) || error)
      .replace(/\b(api[_-]?key|token|secret|password|credential|bearer)\b([:=\s]+)[^\s,;]+/giu, '$1$2[redacted]')
      .slice(0, 1200);
    process.stderr.write(`${detail}\n`);
    finishChildRun?.(1, `smoke_failed: ${String(error && error.message || error).slice(0, 1000)}`);
    app.removeListener('before-quit', holdApplicationUntilSmokeReceipt);
    app.exit(1);
  });
}
