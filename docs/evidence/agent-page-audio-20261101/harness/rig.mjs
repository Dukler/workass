// node rig.mjs BIN SHELL_DIR RENDERER_DIR ELECTRON_APP REPO LABEL
// Real daemon (scratch --state-dir, mock provider), real shell and renderer in the pinned Electron,
// real `workass tools call`: an agent opens a hosted video with sound, out of sight, then asks to
// show it. Nothing touches the installed app or its state, and nothing reaches the speakers (wrap.cjs).
import { spawn, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const HERE = path.dirname(new URL(import.meta.url).pathname);
const [BIN, SHELL_DIR, RENDERER, ELECTRON, REPO, LABEL = 'run'] = process.argv.slice(2);
const ROOT = fs.mkdtempSync(path.join(os.tmpdir(), `audio-rig-${LABEL}-`));
const STATE = `${ROOT}/data/state`;
const MEDIA = `${ROOT}/media`;
for (const d of [`${ROOT}/logs`, MEDIA]) fs.mkdirSync(d, { recursive: true });
const file = `${MEDIA}/tone.mp4`;
execFileSync('ffmpeg', ['-loglevel', 'error', '-f', 'lavfi', '-i', 'sine=frequency=440:duration=30', '-f', 'lavfi', '-i', 'color=black:size=320x240:duration=30:rate=10', '-shortest', '-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-movflags', '+faststart', file]);
const base = 29700 + Math.floor(Math.random() * 200) * 5;
const DAEMON = base, VIEW = base + 1, PROBE = base + 2;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const results = { label: LABEL };
const note = (name, value) => { results[name] = value; console.log(name, JSON.stringify(value)?.slice(0, 500)); };
const ENV = {
  HOME: process.env.HOME, USER: process.env.USER, LOGNAME: process.env.USER, TMPDIR: process.env.TMPDIR, LANG: 'en_US.UTF-8',
  PATH: '/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin', WORKASS_PROFILE: 'dev', WORKASS_DATA_ROOT: `${ROOT}/data`,
  WORKASS_LOG_ROOT: `${ROOT}/logs`, WORKASS_BROWSER_CONTROL_FILE: `${ROOT}/browser-control.json`, WORKASS_MOCK_ACP_DELAY_MS: '5',
};
const probe = (p, body) => new Promise((resolve, reject) => {
  const req = http.request({ host: '127.0.0.1', port: PROBE, path: p, method: body ? 'POST' : 'GET' }, (res) => {
    let data = ''; res.on('data', (c) => { data += c; }); res.on('end', () => { try { resolve(JSON.parse(data)); } catch { reject(new Error(data)); } });
  });
  req.on('error', reject); if (body) req.write(body); req.end();
});
const rEval = async (js) => { const r = await probe('/eval', js); if (r.error) throw new Error(r.error); return r.value; };
const pageEval = async (id, js) => { const r = await probe(`/pageeval?id=${id}`, js); if (r.error) throw new Error(r.error); return r.value; };
const out = fs.openSync(`${ROOT}/logs/daemon.out`, 'a');
const daemon = spawn(BIN, ['--headless', '--state-dir', STATE, '--port', String(DAEMON), '--bind', 'localhost', '--beacon=false', '--renderer-dir', RENDERER], { cwd: REPO, env: ENV, stdio: ['ignore', out, out] });
let shellPid = 0;
const cleanup = () => { try { process.kill(shellPid, 'SIGTERM'); } catch { /* gone */ } try { daemon.kill('SIGTERM'); } catch { /* gone */ } };
process.on('exit', cleanup);
for (let i = 0; i < 200; i += 1) { try { const r = await fetch(`https://127.0.0.1:${DAEMON}/workass/health`); if (r.ok) break; } catch { /* starting */ } await sleep(100); }
const wire = (channel, ...a) => new Promise((resolve, reject) => {
  const ws = new WebSocket(`wss://127.0.0.1:${DAEMON}/ws`);
  const timer = setTimeout(() => { try { ws.close(); } catch { /* */ } reject(new Error(`timeout ${channel}`)); }, 20000);
  ws.onopen = () => ws.send(JSON.stringify({ t: 'invoke', id: 'x', channel, args: a }));
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.t === 'reply' && m.id === 'x') { clearTimeout(timer); ws.close(); m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result); } };
  ws.onerror = () => { clearTimeout(timer); reject(new Error(`ws ${channel}`)); };
});
execFileSync('/usr/bin/open', ['-n', '-g', ELECTRON, '--stdout', `${ROOT}/logs/shell.log`, '--stderr', `${ROOT}/logs/shell.log`,
  ...Object.entries({ ...ENV, WORKASS_URL: `https://127.0.0.1:${DAEMON}`, WORKASS_VIEW_PORT: String(VIEW), WORKASS_CONTROLLER_RECOVERY: '1', WORKASS_RENDERER_DIR: RENDERER, RIG_PROBE_PORT: String(PROBE), RIG_SHELL_DIR: SHELL_DIR }).flatMap(([k, v]) => ['--env', `${k}=${v}`]),
  '--args', `${HERE}/wrap.cjs`]);
for (let i = 0; i < 150; i += 1) { try { shellPid = (await probe('/windows')).pid; if (shellPid) break; } catch { /* starting */ } await sleep(200); }
if (!shellPid) throw new Error('shell never came up');
for (let i = 0; i < 100; i += 1) { try { if (await rEval('!!document.querySelector("#root") && !!(window.api && window.api.lanTakeControl)')) break; } catch { /* loading */ } await sleep(200); }
const isController = async () => { try { return (await (await fetch(`http://127.0.0.1:${VIEW}/__workass-shell/status`)).json()).controller === true; } catch { return false; } };
const holdControl = async () => { for (let i = 0, stable = 0; i < 60 && stable < 6; i += 1) { if (await isController()) stable += 1; else { stable = 0; await rEval('window.api.lanTakeControl()').catch(() => {}); } await sleep(300); } };
await holdControl();
let seq = 0; const newId = (p) => `${p}-${Date.now().toString(36)}-${++seq}`;
const tab = newId('tab'), chat = newId('conv');
await wire('chat:create', { tabId: tab, chatId: chat, operationId: newId('create'), title: 'audio rig', cwd: MEDIA, providerId: 'mock', currentModelId: 'mock-model', focus: false });
const op = newId('op');
await wire('job:start', { tabId: tab, chatId: chat, operationId: op, userMessageId: `u-${op}`, assistantMessageId: `a-${op}`, prompt: 'hello', cwd: MEDIA, providerId: 'mock', modelId: 'mock-model', kind: 'app-chat', title: 'rig' });
let ctx = '';
for (let i = 0; i < 100 && !ctx; i += 1) {
  const dir = `${STATE}/tool-bootstrap`;
  if (fs.existsSync(dir)) for (const d of fs.readdirSync(dir)) { try { if (JSON.parse(fs.readFileSync(`${dir}/${d}/context.json`)).chat_id === chat) { ctx = `${ROOT}/ctx.json`; fs.copyFileSync(`${dir}/${d}/context.json`, ctx); } } catch { /* */ } }
  if (!ctx) await sleep(100);
}
if (!ctx) throw new Error('no tool context');
const tool = (name, input = {}) => new Promise((resolve, reject) => {
  const child = spawn(BIN, ['tools', '--context', ctx, 'call', name], { env: ENV, cwd: MEDIA, stdio: ['pipe', 'pipe', 'pipe'] });
  let o = ''; let err = '';
  child.stdout.on('data', (c) => { o += c; }); child.stderr.on('data', (c) => { err += c; });
  child.on('close', (code) => { if (code !== 0) return reject(new Error(`${name}: ${(err || o).slice(0, 500)}`)); try { resolve(JSON.parse(o)); } catch { resolve({ raw: o }); } });
  child.stdin.end(JSON.stringify(input));
});
const STATE_JS = `(()=>{const v=document.querySelector('video');return v?{t:+v.currentTime.toFixed(2),paused:v.paused,pageMuted:v.muted,volume:v.volume,hidden:document.hidden}:{noVideo:true}})()`;
try {
  await holdControl();
  note('controller', await isController());
  const hosted = await tool('workass_host_artifact', { source_path: file, operation_id: newId('host') });
  const url = new URL(hosted.url_path ?? hosted.urlPath ?? String(hosted.markdown).match(/\((.*)\)/)?.[1], `http://127.0.0.1:${VIEW}`).href;
  note('hosted', url.replace(/:\d+/, ':PORT'));
  // The shell takes the tool link a moment after its window is up.
  let opened = null;
  for (let i = 0; i < 100 && !opened; i += 1) {
    try { opened = await tool('workass_browser_open', { url, operation_id: newId('open') }); } catch (error) { if (!/not ready|not running/.test(error.message) || i === 99) throw error; await sleep(300); }
  }
  const tabId = opened.tab?.id ?? opened.tab_id ?? opened.result?.tab?.id;
  note('opened', { tabId, visible: opened.tab?.visible ?? opened.visible });
  let guest = null;
  for (let i = 0; i < 50 && !guest; i += 1) { guest = (await probe('/contents')).contents.find((c) => c.url.includes('/workass/artifacts/')); if (!guest) await sleep(200); }
  if (!guest) throw new Error(`the artifact page never opened: ${JSON.stringify((await probe('/contents')).contents)}`);
  const read = async () => { const row = (await probe('/contents')).contents.find((c) => c.id === guest.id); return { askedMute: row.asked, lastAsked: row.asked.at(-1) ?? 'never asked', audibleOutput: row.audible, ...(await pageEval(guest.id, STATE_JS)) }; };
  await sleep(2500);
  note('hidden_after_2_5s', await read());
  await sleep(1500);
  note('hidden_after_4s', await read());
  const shown = await tool('workass_browser_open', { visible: true, reason: 'audio rig: the person watches this video', tab_id: tabId, operation_id: newId('show') });
  note('shown_call', { ok: shown.ok ?? true });
  let selected = false;
  for (let i = 0; i < 30 && !selected; i += 1) { selected = await rEval(`(()=>{const leaf=[...document.querySelectorAll('*')].find(e=>e.children.length===0&&e.textContent.trim()==='audio rig'); if(leaf) leaf.click(); return !!leaf})()`).catch(() => false); if (!selected) await sleep(300); }
  note('chat_selected', selected);
  await sleep(3000);
  note('on_screen', await read());
} catch (error) { note('error', String(error.stack || error).slice(0, 800)); }
fs.writeFileSync(`${HERE}/../${LABEL}.json`, `${JSON.stringify(results, null, 2)}\n`);
cleanup();
process.exit(0);
