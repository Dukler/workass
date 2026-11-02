// RIG_MODEL=<cheap id> node rig.mjs BIN REPO LABEL
// Real daemon (scratch --state-dir and HOME, short idle TTL), real Codex through the real host. A
// top-level chat leaves a command running past its turn; the rig watches its heartbeat file and the
// engine through the idle TTL. CODEX_HOME is scratch: only auth.json is linked from the real one.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const HERE = path.dirname(new URL(import.meta.url).pathname);
const [BIN, REPO, LABEL = 'run'] = process.argv.slice(2);
const REAL_HOME = os.homedir();
const ROOT = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), `codex-bg-${LABEL}-`)));
const HOME = `${ROOT}/home`, CODEX = `${HOME}/.codex`, STATE = `${ROOT}/data/state`, CWD = `${ROOT}/work`;
for (const d of [`${ROOT}/logs`, CODEX, CWD]) fs.mkdirSync(d, { recursive: true });
fs.symlinkSync(`${REAL_HOME}/.codex/auth.json`, `${CODEX}/auth.json`);
const TTL_S = Number(process.env.RIG_TTL_S || 20);
const HOLD_S = Number(process.env.RIG_HOLD_S || 75);
const PORT = 30800 + Math.floor(Math.random() * 400);
const ENV = { HOME, CODEX_HOME: CODEX, USER: process.env.USER, TMPDIR: process.env.TMPDIR, LANG: 'en_US.UTF-8',
  PATH: `${REAL_HOME}/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin`, WORKASS_PROFILE: 'dev', WORKASS_DATA_ROOT: `${ROOT}/data`, WORKASS_LOG_ROOT: `${ROOT}/logs` };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const results = { label: LABEL, ttlSeconds: TTL_S };
const note = (k, v) => { results[k] = v; console.log(k, JSON.stringify(v)?.slice(0, 600)); };
const out = fs.openSync(`${ROOT}/logs/daemon.out`, 'a');
const daemon = spawn(BIN, ['--headless', '--state-dir', STATE, '--port', String(PORT), '--bind', 'localhost', '--beacon=false', '--hibernate-ttl', `${TTL_S}s`], { cwd: REPO, env: ENV, stdio: ['ignore', out, out] });
const cleanup = () => { try { daemon.kill('SIGTERM'); } catch { /* gone */ } };
process.on('exit', cleanup);
for (let i = 0; i < 200; i += 1) { try { if ((await fetch(`https://127.0.0.1:${PORT}/workass/health`)).ok) break; } catch { /* starting */ } await sleep(100); }
const wire = (channel, ...a) => new Promise((resolve, reject) => {
  const ws = new WebSocket(`wss://127.0.0.1:${PORT}/ws`);
  const timer = setTimeout(() => { try { ws.close(); } catch { /* */ } reject(new Error(`timeout ${channel}`)); }, 60000);
  ws.onopen = () => ws.send(JSON.stringify({ t: 'invoke', id: 'x', channel, args: a }));
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.t === 'reply' && m.id === 'x') { clearTimeout(timer); ws.close(); m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result); } };
  ws.onerror = () => { clearTimeout(timer); reject(new Error(`ws ${channel}`)); };
});
let seq = 0; const newId = (p) => `${p}-${Date.now().toString(36)}-${++seq}`;
const logFiles = () => { const found = []; const walk = (d) => { for (const n of fs.readdirSync(d, { withFileTypes: true })) { const p = path.join(d, n.name); if (n.isDirectory()) walk(p); else found.push(p); } }; walk(`${ROOT}/logs`); return found; };
const grepLogs = (text) => logFiles().flatMap((f) => fs.readFileSync(f, 'utf8').split('\n').filter((l) => l.includes(text)).map((l) => l.slice(0, 220)));
try {
  // A fresh state dir starts with providers off; detection takes a moment.
  for (let i = 0; i < 60; i += 1) {
    const list = await wire('providers:list');
    const codex = (list.list || list.providers || list).find((p) => p.id === 'codex');
    if (codex?.status === 'ready' && codex.enabled) break;
    if (codex?.status === 'ready') await wire('providers:toggle', 'codex', true).catch(() => wire('providers:toggle', { id: 'codex', enabled: true }));
    await sleep(1000);
  }
  const beat = `${CWD}/beat.txt`;
  const tab = newId('tab'), chat = newId('conv');
  await wire('chat:create', { tabId: tab, chatId: chat, operationId: newId('create'), title: 'background rig', cwd: CWD, providerId: 'codex', currentModelId: process.env.RIG_MODEL, focus: false });
  const op = newId('op');
  const prompt = `Run exactly this one shell command as a single foreground command, with no wrapper, no nohup, no ampersand: for i in $(seq 1 ${HOLD_S}); do date +%s >> beat.txt; sleep 1; done\nIt runs for a long time, on purpose. As soon as the tool answers that the process is still running, reply with the single word STARTED and end your turn. Do not wait, poll or read its output.`;
  await wire('job:start', { tabId: tab, chatId: chat, operationId: op, userMessageId: `u-${op}`, assistantMessageId: `a-${op}`, prompt, cwd: CWD, providerId: 'codex', modelId: process.env.RIG_MODEL, kind: 'app-chat', title: 'rig' });
  const t0 = Date.now();
  const timeline = [];
  let lastLines = -1, stoppedAt = null;
  while (Date.now() - t0 < (HOLD_S + TTL_S + 45) * 1000) {
    const lines = fs.existsSync(beat) ? fs.readFileSync(beat, 'utf8').trim().split('\n').length : 0;
    const hib = grepLogs('acp hibernat').length;
    const aborted = grepLogs('hibernate aborted').length;
    timeline.push({ s: Math.round((Date.now() - t0) / 1000), lines, hib, aborted });
    if (lines === lastLines && lines > 0 && stoppedAt == null) stoppedAt = Math.round((Date.now() - t0) / 1000);
    if (lines !== lastLines) stoppedAt = null;
    lastLines = lines;
    if (stoppedAt != null && Date.now() - t0 > stoppedAt * 1000 + 10000 && lines < HOLD_S && hib) break;
    if (lines >= HOLD_S && hib && Date.now() - t0 > (HOLD_S + TTL_S + 15) * 1000) break;
    await sleep(3000);
  }
  note('timeline', timeline);
  note('heartbeats', { wanted: HOLD_S, got: lastLines, stoppedAtSecond: stoppedAt });
  note('survived', lastLines >= HOLD_S);
  note('hibernate_log', grepLogs('acp hibernat').slice(0, 6));
  note('hibernate_held', grepLogs('hibernate aborted').length);
} catch (error) { note('error', String(error.stack || error).slice(0, 800)); }
fs.writeFileSync(`${HERE}/../${LABEL}.json`, `${JSON.stringify(results, null, 2)}\n`);
cleanup();
await sleep(500);
process.exit(0);
