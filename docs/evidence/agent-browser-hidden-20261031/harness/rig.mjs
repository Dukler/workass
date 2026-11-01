// node rig.mjs BIN LABEL   Real daemon (scratch state), real shell + renderer in the pinned
// Electron, the real `workass tools call` CLI. Every path is under .dev/brig; nothing touches
// the installed app, its state or its daemon. The person's foreground app is sampled all along.
import { spawn, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const REPO = '/Users/dev/Workspace/workass';
const [BIN, LABEL = 'run'] = process.argv.slice(2);
const ROOT = `${REPO}/.dev/brig/${LABEL}-${Date.now()}`;
const STATE = `${ROOT}/data/state`;
const OUT = `${ROOT}/out`;
fs.mkdirSync(`${ROOT}/logs`, { recursive: true });
fs.mkdirSync(OUT, { recursive: true });
const base = 28900 + Math.floor(Math.random() * 400) * 5;
const DAEMON = base, VIEW = base + 1, PROBE = base + 2, SITE = base + 3;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const log = (...args) => console.log(new Date().toISOString().slice(11, 23), ...args);
const ENV = {
  HOME: process.env.HOME, USER: process.env.USER, LOGNAME: process.env.USER, TMPDIR: process.env.TMPDIR, LANG: 'en_US.UTF-8',
  PATH: '/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin', WORKASS_PROFILE: 'dev', WORKASS_DATA_ROOT: `${ROOT}/data`,
  WORKASS_LOG_ROOT: `${ROOT}/logs`, WORKASS_BROWSER_CONTROL_FILE: `${ROOT}/browser-control.json`, WORKASS_MOCK_ACP_DELAY_MS: '5',
};
const RENDERER = process.env.RIG_RENDERER || `${REPO}/desktop/renderer2/dist`;
const results = {};
const note = (name, value) => { results[name] = value; log(name, JSON.stringify(value).slice(0, 400)); };

// ---- The site the agent browses.
const page = (title, script = '', body = '') => `<!doctype html><title>${title}</title><body><h1>${title}</h1>${body}<script>${script}</script>`;
const VIS = `window.__ticks=0;window.__raf=0;window.__hidden=[];setInterval(()=>__ticks++,100);(function r(){__raf++;requestAnimationFrame(r)})();
  document.addEventListener('visibilitychange',()=>__hidden.push(document.visibilityState));`;
const SITE_PAGES = {
  '/a': page('Page A', VIS),
  '/b': page('Page B', VIS),
  '/popup': page('Popup page', `${VIS} if(window.opener) window.opener.postMessage('hello from popup','*');`,
    '<button id="approve" onclick="document.getElementById(\'r\').textContent=\'approved\'" style="width:300px;height:80px">Approve</button><p id="r">pending</p>'),
  '/opener': page('Opener page', `${VIS} window.__msg=[];addEventListener('message',e=>__msg.push(e.data));
    window.__openNow=()=>{window.__w=window.open('/popup','signin','width=600,height=500');return !!__w};
    setTimeout(()=>{window.__auto=window.open('/popup?auto','auto','width=500,height=400');},300);`,
  `<button id="open" onclick="__openNow()" style="width:300px;height:60px">Sign in popup</button>
   <a id="link" target="_blank" href="/b?fromlink" style="display:block;padding:20px">new tab link</a>
   <button id="fs" onclick="document.documentElement.requestFullscreen().then(()=>window.__fs='in',e=>window.__fs=String(e))" style="width:300px;height:60px">Fullscreen</button>
   <button id="focus" onclick="window.focus();window.__w&&__w.focus();window.__focused=true" style="width:300px;height:60px">Focus me</button>
   <button id="target" onclick="this.textContent='clicked'" style="width:300px;height:60px">Target</button>`),
  // A site that only draws while it believes it is on screen.
  '/gated': page('Gated', `${VIS} function draw(){ if(document.visibilityState==='visible'){ requestAnimationFrame(()=>{document.getElementById('c').textContent='DRAWN'}) } }
    document.addEventListener('visibilitychange',draw);draw();`, '<p id="c">not drawn</p>'),
};
const site = http.createServer((req, res) => {
  const html = SITE_PAGES[new URL(req.url, 'http://x').pathname];
  res.setHeader('content-type', 'text/html');
  res.end(html || 'nope');
}).listen(SITE, '127.0.0.1');
const S = (p) => `http://127.0.0.1:${SITE}${p}`;

// ---- What the person's machine shows.
const frontApp = () => { try { return execFileSync('lsappinfo', ['info', '-only', 'name,pid', execFileSync('lsappinfo', ['front'], { encoding: 'utf8' }).trim()], { encoding: 'utf8' }).replace(/\s+/g, ' ').trim(); } catch { return 'unknown'; } };
const probe = (p, body) => new Promise((resolve, reject) => {
  const req = http.request({ host: '127.0.0.1', port: PROBE, path: p, method: body ? 'POST' : 'GET' }, (res) => {
    let data = ''; res.on('data', (c) => { data += c; }); res.on('end', () => { try { resolve(JSON.parse(data)); } catch (e) { reject(new Error(data)); } });
  });
  req.on('error', reject); if (body) req.write(body); req.end();
});
const rEval = async (js) => { const r = await probe('/eval', js); if (r.error) throw new Error(r.error); return r.value; };
let shellPid = 0;
const onScreen = () => { try { return JSON.parse(execFileSync('/tmp/brig-cgwin', [String(shellPid)], { encoding: 'utf8' })); } catch { return []; } };
const samples = { fronts: new Set(), electronFront: 0, focusedWindows: 0, maxOnScreen: 0, maxVisibleWindows: 0, n: 0, offenders: [] };
let sampling = false;
async function sampler() {
  while (sampling) {
    const front = frontApp();
    samples.fronts.add(front);
    if (shellPid && front.includes(`"pid"=${shellPid}`)) samples.electronFront += 1;
    try {
      const { windows } = await probe('/windows');
      const visible = windows.filter((w) => w.visible);
      samples.maxVisibleWindows = Math.max(samples.maxVisibleWindows, visible.length);
      if (windows.some((w) => w.focused)) samples.focusedWindows += 1;
      for (const w of visible) if (!w.main) samples.offenders.push({ at: samples.step, title: w.title, bounds: w.bounds });
      const cg = onScreen().filter((w) => w.layer === 0 && w.w > 50);
      samples.maxOnScreen = Math.max(samples.maxOnScreen, cg.length);
    } catch { /* shell still starting */ }
    samples.n += 1;
    await sleep(120);
  }
}
const step = (name) => { samples.step = name; log('--', name); };

// ---- Up.
const daemonOut = fs.openSync(`${ROOT}/logs/daemon.out`, 'a');
const daemon = spawn(BIN, ['--headless', '--state-dir', STATE, '--port', String(DAEMON), '--bind', 'localhost', '--renderer-dir', RENDERER], { cwd: REPO, env: ENV, stdio: ['ignore', daemonOut, daemonOut] });
const cleanup = () => {
  try { process.kill(shellPid, 'SIGTERM'); } catch { /* gone */ }
  try { daemon.kill('SIGTERM'); } catch { /* gone */ }
  site.close();
};
process.on('exit', cleanup);
for (let i = 0; i < 200; i += 1) { try { const r = await fetch(`https://127.0.0.1:${DAEMON}/workass/health`); if (r.ok) break; } catch { /* starting */ } await sleep(100); }
const wire = (channel, ...args) => new Promise((resolve, reject) => {
  const ws = new WebSocket(`wss://127.0.0.1:${DAEMON}/ws`);
  const timer = setTimeout(() => { try { ws.close(); } catch { /* */ } reject(new Error(`timeout ${channel}`)); }, 20000);
  ws.onopen = () => ws.send(JSON.stringify({ t: 'invoke', id: 'x', channel, args }));
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.t === 'reply' && m.id === 'x') { clearTimeout(timer); ws.close(); m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result); } };
  ws.onerror = () => { clearTimeout(timer); reject(new Error(`ws ${channel}`)); };
});
const front0 = frontApp();
note('front_before_launch', front0);
const electronApp = `${REPO}/.dev/runtime/electron/darwin-arm64/Electron.app`;
execFileSync('/usr/bin/open', ['-n', '-g', electronApp, '--stdout', `${ROOT}/logs/shell.log`, '--stderr', `${ROOT}/logs/shell.log`,
  ...Object.entries({ ...ENV, WORKASS_URL: `https://127.0.0.1:${DAEMON}`, WORKASS_VIEW_PORT: String(VIEW), WORKASS_CONTROLLER_RECOVERY: '1', WORKASS_RENDERER_DIR: RENDERER, RIG_PROBE_PORT: String(PROBE), RIG_REPO: REPO }).flatMap(([k, v]) => ['--env', `${k}=${v}`]),
  '--args', `${REPO}/docs/evidence/agent-browser-hidden-20261031/harness/wrap.cjs`]);
for (let i = 0; i < 150; i += 1) { try { shellPid = (await probe('/windows')).pid; if (shellPid) break; } catch { /* starting */ } await sleep(200); }
if (!shellPid) throw new Error('shell never came up');
for (let i = 0; i < 100; i += 1) { try { if (await rEval('!!document.querySelector("#root") && !!(window.api && window.api.lanTakeControl)')) break; } catch { /* loading */ } await sleep(200); }
const isController = async () => { try { return (await (await fetch(`http://127.0.0.1:${VIEW}/__workass-shell/status`)).json()).controller === true; } catch { return false; } };
// Holds only once it has stayed ours for a while (the renderer's own start-up also reports).
const holdControl = async () => {
  let stable = 0;
  for (let i = 0; i < 60 && stable < 6; i += 1) {
    if (await isController()) stable += 1;
    else { stable = 0; await rEval('window.api.lanTakeControl()').catch(() => {}); }
    await sleep(300);
  }
  return stable >= 6;
};
const controller = await holdControl();
note('controller', controller);
note('windows_at_start', (await probe('/windows')).windows);
sampling = true;
const samplerDone = sampler();

// ---- A chat with a real tool context.
let seq = 0; const newId = (p) => `${p}-${Date.now().toString(36)}-${++seq}`;
const tab = newId('tab'), chat = newId('conv');
await wire('chat:create', { tabId: tab, chatId: chat, operationId: newId('create'), title: 'agent browses', cwd: '/tmp', providerId: 'mock', currentModelId: 'mock-model', focus: false });
{
  const op = newId('op');
  await wire('job:start', { tabId: tab, chatId: chat, operationId: op, userMessageId: `u-${op}`, assistantMessageId: `a-${op}`, prompt: 'hello', cwd: '/tmp', providerId: 'mock', modelId: 'mock-model', kind: 'app-chat', title: 'rig' });
}
let ctx = '';
for (let i = 0; i < 100 && !ctx; i += 1) {
  const dir = `${STATE}/tool-bootstrap`;
  if (fs.existsSync(dir)) for (const d of fs.readdirSync(dir)) { try { if (JSON.parse(fs.readFileSync(`${dir}/${d}/context.json`)).chat_id === chat) { ctx = `${ROOT}/ctx.json`; fs.copyFileSync(`${dir}/${d}/context.json`, ctx); } } catch { /* */ } }
  if (!ctx) await sleep(100);
}
if (!ctx) throw new Error('no tool context');
let opSeq = 0;
// Asynchronous: the site and the sampler live in this process and must keep running during a call.
const tool = (name, input = {}) => new Promise((resolve, reject) => {
  const payload = { ...input };
  if (!['workass_browser_list', 'workass_browser_snapshot', 'workass_browser_screenshot', 'workass_browser_wait'].includes(name)) payload.operation_id = `rig-${++opSeq}-${Date.now()}`;
  const child = spawn(BIN, ['tools', '--context', ctx, 'call', name], { env: ENV, cwd: '/tmp', stdio: ['pipe', 'pipe', 'pipe'] });
  let out = ''; let err = '';
  child.stdout.on('data', (c) => { out += c; }); child.stderr.on('data', (c) => { err += c; });
  child.on('close', (code) => {
    if (code !== 0) return reject(new Error(`${name}: ${(err || out).slice(0, 500)}`));
    try { resolve(JSON.parse(out)); } catch { resolve({ raw: out }); }
  });
  child.stdin.end(JSON.stringify(payload));
});
const toolErr = async (name, input) => { try { return { ok: await tool(name, input) }; } catch (error) { return { error: String(error.message).slice(0, 400) }; } };
const pageEval = async (id, js) => { const r = await probe(`/pageeval?id=${id}`, js); if (r.error) throw new Error(r.error); return r.value; };
const tabs = async () => { const r = await tool('workass_browser_list'); return r.tabs || r.result?.tabs || []; };

try {
  note('controller_after_chat', await holdControl());
  // Select the chat in the real renderer, so a pane request would really be shown.
  await sleep(600);
  let selected = false;
  for (let i = 0; i < 30 && !selected; i += 1) {
    selected = await rEval(`(()=>{const leaf=[...document.querySelectorAll('*')].find(e=>e.children.length===0&&e.textContent.trim()==='agent browses'); if(leaf) leaf.click(); return !!leaf})()`).catch(() => false);
    if (!selected) await sleep(300);
  }
  note('chat_selected', selected);
  await sleep(500);
  await probe(`/shot?file=${OUT}/00-before.png`);

  step('open A, omitted visible');
  await tool('workass_browser_open', { url: S('/a') });
  await sleep(1200);
  note('after_open_a', { windows: (await probe('/windows')).windows.filter((w) => w.visible).length, floatInRenderer: await rEval(`!!document.querySelector('.fbrw, .fbrw-full, .live-browser')`) });
  await probe(`/shot?file=${OUT}/01-after-open-a.png`);

  step('navigate B');
  await tool('workass_browser_navigate', { url: S('/b') });
  await sleep(500);

  step('opener page: window.open on a timer and on an agent click');
  await tool('workass_browser_navigate', { url: S('/opener') });
  await sleep(1000);
  await tool('workass_browser_click', { selector: '#open' });
  await sleep(1500);
  await tool('workass_browser_click', { selector: '#link' });
  await sleep(1000);
  const list = await tabs();
  note('tabs', list.map((t) => ({ id: t.id, url: t.url, visible: t.visible, opened_by: t.opened_by_tab_id ?? null })));
  note('windows_after_popups', (await probe('/windows')).windows.filter((w) => w.visible).map((w) => ({ title: w.title, main: w.main, bounds: w.bounds })));
  note('on_screen_after_popups', onScreen());
  await probe(`/shot?file=${OUT}/02-after-popups.png`);

  const openerTab = list.find((t) => t.url.includes('/opener'));
  const popupTab = list.find((t) => t.url.includes('/popup') && !t.url.includes('auto'));
  note('opener_got_message', await pageEval(openerTab.id, 'window.__msg'));

  step('screenshot + click inside the hidden popup');
  const shot = await tool('workass_browser_screenshot', { tab_id: popupTab.id });
  note('popup_screenshot', { keys: Object.keys(shot).slice(0, 12), width: shot.image_width ?? shot.width, height: shot.image_height ?? shot.height, file: shot.image_path || shot.file || shot.path || null, raw: JSON.stringify(shot).slice(0, 300) });
  const clicked = await tool('workass_browser_click', { tab_id: popupTab.id, selector: '#approve' });
  note('popup_click', { status: clicked.status, text: await pageEval(popupTab.id, `document.getElementById('r').textContent`) });
  const openerShot = await tool('workass_browser_screenshot', { tab_id: openerTab.id });
  note('opener_screenshot', JSON.stringify(openerShot).slice(0, 260));
  await tool('workass_browser_click', { tab_id: openerTab.id, selector: '#target' });
  note('opener_click', await pageEval(openerTab.id, `document.getElementById('target').textContent`));

  step('page tries to take the screen: window.focus, popup.focus, requestFullscreen on a trusted click');
  await tool('workass_browser_click', { tab_id: openerTab.id, selector: '#focus' });
  await sleep(800);
  await tool('workass_browser_click', { tab_id: openerTab.id, selector: '#fs' });
  await sleep(1500);
  note('after_focus_and_fullscreen', { page: await pageEval(openerTab.id, '({fs:window.__fs,focused:window.__focused})'), windows: (await probe('/windows')).windows.filter((w) => w.visible || w.fullscreen).map((w) => ({ title: w.title, main: w.main, fs: w.fullscreen })), onScreen: onScreen() });

  step('a hidden page left alone for 8 s (sites that throttle hidden pages)');
  const idlePopup = list.find((t) => t.url.includes('auto')) || popupTab;
  await sleep(4500); // past the agent's paint hold
  const before = await pageEval(idlePopup.id, '({t:__ticks,r:__raf,v:document.visibilityState,h:document.hidden,f:document.hasFocus()})');
  await sleep(8000);
  const after = await pageEval(idlePopup.id, '({t:__ticks,r:__raf,v:document.visibilityState,h:document.hidden,f:document.hasFocus()})');
  note('idle_hidden_popup', { timerHz: (after.t - before.t) / 8, rafHz: (after.r - before.r) / 8, visibility: after.v, hidden: after.h, hasFocus: after.f });
  await sleep(1000); // the 4 s hold from its last call has gone
  const drivenBefore = await pageEval(openerTab.id, '({t:__ticks,r:__raf})');
  await sleep(4000);
  const drivenAfter = await pageEval(openerTab.id, '({t:__ticks,r:__raf,v:document.visibilityState})');
  note('idle_after_agent_drove_it', { timerHz: (drivenAfter.t - drivenBefore.t) / 4, rafHz: (drivenAfter.r - drivenBefore.r) / 4, visibility: drivenAfter.v });
  await tool('workass_browser_navigate', { tab_id: openerTab.id, url: S('/gated') });
  await sleep(1500);
  note('gated_site', await pageEval(openerTab.id, `document.getElementById('c').textContent`));

  step('a crashed hidden page comes back hidden');
  const frontBeforeCrash = frontApp();
  await probe(`/crash?id=${idlePopup.id}`);
  await sleep(2500);
  note('after_crash', { front: frontApp() === frontBeforeCrash, windows: (await probe('/windows')).windows.filter((w) => w.visible).length, tabAlive: (await tabs()).some((t) => t.id === idlePopup.id) });

  if (process.env.RIG_PANEL) await (await import(process.env.RIG_PANEL)).default({ tool, toolErr, tabs, probe, rEval, pageEval, sleep, note, step, S, OUT, popupTab, openerTab, frontApp, onScreen, tab, chat });
} catch (error) {
  note('FAILED', String(error && error.stack || error));
  try { note('status_at_failure', await (await fetch(`http://127.0.0.1:${VIEW}/__workass-shell/status`)).json()); } catch { /* */ }
}
sampling = false;
await samplerDone;
note('samples', { n: samples.n, fronts: [...samples.fronts], electronFront: samples.electronFront, focusedWindows: samples.focusedWindows, maxVisibleWindows: samples.maxVisibleWindows, maxOnScreen: samples.maxOnScreen, offenders: samples.offenders.slice(0, 5), offenderCount: samples.offenders.length });
note('front_after', frontApp());
fs.writeFileSync(`${ROOT}/results.json`, JSON.stringify(results, null, 2));
console.log('RESULTS', `${ROOT}/results.json`);
cleanup();
process.exit(0);
