// node vrig.mjs BIN LABEL [--shell DIR] [--renderer DIR] [--file MP4]
// Real daemon (scratch state), real shell + renderer in the pinned Electron, the real
// `workass tools call workass_host_artifact`, the real Markdown link clicked in the real chat.
// Nothing touches the installed app, its state or its daemon. Results land in the evidence dir.
import { spawn, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';
const REPO = '/Users/dev/Workspace/workass';
const HERE = path.dirname(new URL(import.meta.url).pathname);
const args = process.argv.slice(2);
const opt = (name, dflt) => { const i = args.indexOf(name); return i >= 0 ? args[i + 1] : dflt; };
const [BIN, LABEL = 'run'] = args;
const SHELL_DIR = opt('--shell', `${REPO}/desktop/shell`);
const RENDERER = opt('--renderer', `${REPO}/desktop/renderer2/dist`);
const SOURCE = opt('--file', '/Users/dev/Workspace/myproject/tmp/test-set-look1-20261101/worst-clip-accepted.mp4');
const EVIDENCE = opt('--out', `${HERE}/../${LABEL}`);
const ROOT = `${REPO}/.dev/vrig/${LABEL}-${Date.now()}`;
const STATE = `${ROOT}/data/state`;
const MEDIA = `${ROOT}/media`;
for (const d of [`${ROOT}/logs`, EVIDENCE, MEDIA]) fs.mkdirSync(d, { recursive: true });
const file = path.join(MEDIA, path.basename(SOURCE));
fs.copyFileSync(SOURCE, file);
const base = 29400 + Math.floor(Math.random() * 300) * 5;
const DAEMON = base, VIEW = base + 1, PROBE = base + 2;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const log = (...a) => console.log(new Date().toISOString().slice(11, 23), ...a);
const ENV = {
  HOME: process.env.HOME, USER: process.env.USER, LOGNAME: process.env.USER, TMPDIR: process.env.TMPDIR, LANG: 'en_US.UTF-8',
  PATH: '/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin', WORKASS_PROFILE: 'dev', WORKASS_DATA_ROOT: `${ROOT}/data`,
  WORKASS_LOG_ROOT: `${ROOT}/logs`, WORKASS_BROWSER_CONTROL_FILE: `${ROOT}/browser-control.json`, WORKASS_MOCK_ACP_DELAY_MS: '5',
};
const REMOTE = args.includes('--remote');
const results = { label: LABEL, source: SOURCE };
const note = (name, value) => { results[name] = value; log(name, JSON.stringify(value)?.slice(0, 700)); };
const probe = (p, body) => new Promise((resolve, reject) => {
  const req = http.request({ host: '127.0.0.1', port: PROBE, path: p, method: body ? 'POST' : 'GET' }, (res) => {
    let data = ''; res.on('data', (c) => { data += c; }); res.on('end', () => { try { resolve(JSON.parse(data)); } catch { reject(new Error(data)); } });
  });
  req.on('error', reject); if (body) req.write(body); req.end();
});
const rEval = async (js) => { const r = await probe('/eval', js); if (r.error) throw new Error(r.error); return r.value; };
const pageEval = async (id, js) => { const r = await probe(`/pageeval?id=${id}`, js); if (r.error) throw new Error(r.error); return r.value; };
// play() on an element with no usable source stays pending forever: that hang is itself the failure.
const daemonOut = fs.openSync(`${ROOT}/logs/daemon.out`, 'a');
const daemon = spawn(BIN, ['--headless', '--state-dir', STATE, '--port', String(DAEMON), '--bind', 'localhost', '--beacon=false', '--renderer-dir', RENDERER], { cwd: REPO, env: ENV, stdio: ['ignore', daemonOut, daemonOut] });
let shellPid = 0;
let remoteDaemon = null;
const cleanup = () => { try { process.kill(shellPid, 'SIGTERM'); } catch { /* gone */ } try { daemon.kill('SIGTERM'); } catch { /* gone */ } try { remoteDaemon?.kill('SIGTERM'); } catch { /* gone */ } };
process.on('exit', cleanup);
for (let i = 0; i < 200; i += 1) { try { const r = await fetch(`https://127.0.0.1:${DAEMON}/workass/health`); if (r.ok) break; } catch { /* starting */ } await sleep(100); }
const wireTo = (port, channel, ...a) => new Promise((resolve, reject) => {
  const ws = new WebSocket(`wss://127.0.0.1:${port}/ws`);
  const timer = setTimeout(() => { try { ws.close(); } catch { /* */ } reject(new Error(`timeout ${channel}`)); }, 20000);
  ws.onopen = () => ws.send(JSON.stringify({ t: 'invoke', id: 'x', channel, args: a }));
  ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.t === 'reply' && m.id === 'x') { clearTimeout(timer); ws.close(); m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result); } };
  ws.onerror = () => { clearTimeout(timer); reject(new Error(`ws ${channel}`)); };
});
const wire = (channel, ...a) => wireTo(DAEMON, channel, ...a);
execFileSync('/usr/bin/open', ['-n', '-g', `${REPO}/.dev/runtime/electron/darwin-arm64/Electron.app`, '--stdout', `${ROOT}/logs/shell.log`, '--stderr', `${ROOT}/logs/shell.log`,
  ...Object.entries({ ...ENV, WORKASS_URL: `https://127.0.0.1:${DAEMON}`, WORKASS_VIEW_PORT: String(VIEW), WORKASS_CONTROLLER_RECOVERY: '1', WORKASS_RENDERER_DIR: RENDERER, RIG_PROBE_PORT: String(PROBE), RIG_SHELL_DIR: SHELL_DIR }).flatMap(([k, v]) => ['--env', `${k}=${v}`]),
  '--args', `${HERE}/wrap.cjs`]);
for (let i = 0; i < 150; i += 1) { try { shellPid = (await probe('/windows')).pid; if (shellPid) break; } catch { /* starting */ } await sleep(200); }
if (!shellPid) throw new Error('shell never came up');
for (let i = 0; i < 100; i += 1) { try { if (await rEval('!!document.querySelector("#root") && !!(window.api && window.api.lanTakeControl)')) break; } catch { /* loading */ } await sleep(200); }
const isController = async () => { try { return (await (await fetch(`http://127.0.0.1:${VIEW}/__workass-shell/status`)).json()).controller === true; } catch { return false; } };
const holdControl = async () => {
  let stable = 0;
  for (let i = 0; i < 60 && stable < 6; i += 1) {
    if (await isController()) stable += 1;
    else { stable = 0; await rEval('window.api.lanTakeControl()').catch(() => {}); }
    await sleep(300);
  }
  return stable >= 6;
};
note('controller', await holdControl());
let seq = 0; const newId = (p) => `${p}-${Date.now().toString(36)}-${++seq}`;
const tab = newId('tab'), chat = newId('conv');
await wire('chat:create', { tabId: tab, chatId: chat, operationId: newId('create'), title: 'video rig', cwd: MEDIA, providerId: 'mock', currentModelId: 'mock-model', focus: false });
const send = async (prompt) => { const op = newId('op'); await wire('job:start', { tabId: tab, chatId: chat, operationId: op, userMessageId: `u-${op}`, assistantMessageId: `a-${op}`, prompt, cwd: MEDIA, providerId: 'mock', modelId: 'mock-model', kind: 'app-chat', title: 'rig' }); };
await send('hello');
let ctx = '';
for (let i = 0; i < 100 && !ctx; i += 1) {
  const dir = `${STATE}/tool-bootstrap`;
  if (fs.existsSync(dir)) for (const d of fs.readdirSync(dir)) { try { if (JSON.parse(fs.readFileSync(`${dir}/${d}/context.json`)).chat_id === chat) { ctx = `${ROOT}/ctx.json`; fs.copyFileSync(`${dir}/${d}/context.json`, ctx); } } catch { /* */ } }
  if (!ctx) await sleep(100);
}
if (!ctx) throw new Error('no tool context');
const tool = (name, input = {}, context = ctx, env = ENV) => new Promise((resolve, reject) => {
  const child = spawn(BIN, ['tools', '--context', context, 'call', name], { env, cwd: MEDIA, stdio: ['pipe', 'pipe', 'pipe'] });
  let out = ''; let err = '';
  child.stdout.on('data', (c) => { out += c; }); child.stderr.on('data', (c) => { err += c; });
  child.on('close', (code) => { if (code !== 0) return reject(new Error(`${name}: ${(err || out).slice(0, 500)}`)); try { resolve(JSON.parse(out)); } catch { resolve({ raw: out }); } });
  child.stdin.end(JSON.stringify(input));
});
const VIDEO_STATE = `(()=>{const v=document.querySelector('video');if(!v)return {video:false,inner:{w:innerWidth,h:innerHeight},body:document.body?document.body.innerText.slice(0,200):null};const r=v.getBoundingClientRect();
  const b=[];for(let i=0;i<v.buffered.length;i++)b.push([v.buffered.start(i),v.buffered.end(i)]);
  return {video:true,src:v.currentSrc?new URL(v.currentSrc).pathname:'',readyState:v.readyState,networkState:v.networkState,error:v.error?{code:v.error.code,message:v.error.message}:null,
  muted:v.muted,videoWidth:v.videoWidth,videoHeight:v.videoHeight,duration:v.duration,currentTime:v.currentTime,paused:v.paused,controls:v.controls,seeking:v.seeking,buffered:b,
  rect:{w:Math.round(r.width),h:Math.round(r.height),x:Math.round(r.x),y:Math.round(r.y)},inner:{w:innerWidth,h:innerHeight},hidden:document.hidden}})()`;
try {
  note('controller_after_chat', await holdControl());
  let hosted;
  if (REMOTE) {
    // A second real daemon, another machine in every respect but the hardware: its own state, its
    // own identity, a LAN listener, reached only through the renderer's paired MachineSocket.
    const lan = execFileSync('/usr/sbin/ipconfig', ['getifaddr', 'en0'], { encoding: 'utf8' }).trim();
    const REMOTE_PORT = base + 3;
    const R = `${ROOT}/remote`;
    fs.mkdirSync(`${R}/logs`, { recursive: true });
    const RENV = { ...ENV, WORKASS_DATA_ROOT: `${R}/data`, WORKASS_LOG_ROOT: `${R}/logs`, WORKASS_BROWSER_CONTROL_FILE: `${R}/browser-control.json` };
    const key = execFileSync(BIN, ['fleet', 'key', '--state-dir', `${R}/data/state`], { env: RENV, encoding: 'utf8' }).split('\n').map((l) => l.trim()).find((l) => /^[A-Za-z0-9_+/=-]{20,}$/.test(l)); // never logged
    if (!key) throw new Error('no fleet key in the output');
    const rlog = fs.openSync(`${R}/logs/daemon.out`, 'a');
    remoteDaemon = spawn(BIN, ['--headless', '--state-dir', `${R}/data/state`, '--port', String(REMOTE_PORT), '--bind', 'lan', '--beacon=false', '--renderer-dir', RENDERER], { cwd: REPO, env: RENV, stdio: ['ignore', rlog, rlog] });
    for (let i = 0; i < 200; i += 1) { try { const r = await fetch(`https://127.0.0.1:${REMOTE_PORT}/workass/health`); if (r.ok) break; } catch { /* starting */ } await sleep(100); }
    const rTab = newId('tab'), rChat = newId('conv');
    await wireTo(REMOTE_PORT, 'chat:create', { tabId: rTab, chatId: rChat, operationId: newId('create'), title: 'owner of the video', cwd: MEDIA, providerId: 'mock', currentModelId: 'mock-model', focus: false });
    { const op = newId('op'); await wireTo(REMOTE_PORT, 'job:start', { tabId: rTab, chatId: rChat, operationId: op, userMessageId: `u-${op}`, assistantMessageId: `a-${op}`, prompt: 'hello', cwd: MEDIA, providerId: 'mock', modelId: 'mock-model', kind: 'app-chat', title: 'rig' }); }
    let rctx = '';
    for (let i = 0; i < 100 && !rctx; i += 1) {
      const dir = `${R}/data/state/tool-bootstrap`;
      if (fs.existsSync(dir)) for (const d of fs.readdirSync(dir)) { try { if (JSON.parse(fs.readFileSync(`${dir}/${d}/context.json`)).chat_id === rChat) { rctx = `${R}/ctx.json`; fs.copyFileSync(`${dir}/${d}/context.json`, rctx); } } catch { /* */ } }
      if (!rctx) await sleep(100);
    }
    if (!rctx) throw new Error('no tool context on the owner');
    hosted = await tool('workass_host_artifact', { source_path: file, operation_id: newId('host') }, rctx, RENV);
    note('owner_machine', { selfId: (await (await fetch(`https://127.0.0.1:${DAEMON}/workass/health`)).json()).machineId ?? null, ownerLink: String(hosted.urlPath).split('/')[3] });
    // Direct HTTP to the owner from off loopback must stay refused: the socket is the only path.
    note('direct_lan_http_status', await fetch(`https://${lan}:${REMOTE_PORT}${hosted.urlPath}`).then((r) => r.status, (e) => String(e.cause?.code || e.message)));
    // Pair the way a person does: the renderer holds the key, the book holds the address.
    await rEval(`localStorage.setItem('workass.fleetKey', ${JSON.stringify(key)}); location.reload(); 1`).catch(() => {});
    await sleep(1500);
    for (let i = 0; i < 100; i += 1) { try { if (await rEval('!!document.querySelector("#root") && !!(window.api && window.api.lanTakeControl)')) break; } catch { /* loading */ } await sleep(200); }
    note('controller_after_reload', await holdControl());
    note('machine_added', await rEval(`window.api.machinesAdd(${JSON.stringify(`${lan}:${REMOTE_PORT}`)})`).then((r) => ({ ok: r?.ok, error: r?.error })));
    note('renderer_machine_keys', await rEval(`Object.keys(localStorage).filter(k=>k.includes('machine')).map(k=>k.replace(/m-[0-9a-f]{8}.*?\\./,'m-.'))`));
    await rEval('location.reload(); 1').catch(() => {});
    await sleep(1500);
    for (let i = 0; i < 100; i += 1) { try { if (await rEval('!!document.querySelector("#root") && !!(window.api && window.api.lanTakeControl)')) break; } catch { /* loading */ } await sleep(200); }
    note('controller_after_add', await holdControl());
    await sleep(3000);
  } else {
    hosted = await tool('workass_host_artifact', { source_path: file, operation_id: newId('host') });
  }
  note('hosted', { id: hosted.id ?? hosted.artifact?.id, urlPath: hosted.url_path ?? hosted.urlPath, markdown: hosted.markdown, raw: JSON.stringify(hosted).slice(0, 400) });
  const url = hosted.url_path ?? hosted.urlPath ?? hosted.url ?? String(hosted.markdown).match(/\((.*)\)/)?.[1];
  await send(`[ver video](${url})`);
  await sleep(800);
  let selected = false;
  for (let i = 0; i < 30 && !selected; i += 1) {
    selected = await rEval(`(()=>{const leaf=[...document.querySelectorAll('*')].find(e=>e.children.length===0&&e.textContent.trim()==='video rig'); if(leaf) leaf.click(); return !!leaf})()`).catch(() => false);
    if (!selected) await sleep(300);
  }
  note('chat_selected', selected);
  let linkFound = false;
  for (let i = 0; i < 40 && !linkFound; i += 1) { linkFound = await rEval(`document.querySelectorAll('a[href*="/workass/artifacts/"]').length>=2`).catch(() => false); if (!linkFound) await sleep(250); }
  note('link', await rEval(`(()=>{const all=document.querySelectorAll('a[href*="/workass/artifacts/"]');const a=all[all.length-1];return a?{text:a.textContent,href:a.getAttribute('href')}:null})()`));
  await rEval(`[...document.querySelectorAll('a[href*="/workass/artifacts/"]')].pop().click()`);
  await sleep(150);
  note('after_click', await rEval(`({toasts:[...document.querySelectorAll('.toast')].map(t=>t.innerText),pane:!!document.querySelector('.brw2view'),parked:Object.keys(localStorage).filter(k=>k.includes('auto-connect')),hasKey:!!localStorage.getItem('workass.fleetKey')})`));
  let guest = null;
  for (let i = 0; i < 50 && !guest; i += 1) {
    const { contents } = await probe('/contents');
    guest = contents.find((c) => c.url.includes('/workass/artifacts/') && c.type !== 'remote');
    if (!guest) await sleep(200);
  }
  note('guest', guest);
  if (!guest) note('debug', { contents: (await probe('/contents')).contents, windows: (await probe('/windows')).windows, panel: await rEval(`(()=>{const b=document.querySelector('.live-browser');return {panel:!!b,text:b?b.innerText.slice(0,300):null,toasts:[...document.querySelectorAll('[class*=toast]')].map(t=>t.innerText).slice(0,5),url:(document.querySelector('.brw2url')||{}).value}})()`) });
  if (!guest) throw new Error('the artifact page never opened');
  const panel = await rEval(`(()=>{const v=document.querySelector('.brw2view');if(!v)return null;const r=v.getBoundingClientRect();return {w:Math.round(r.width),h:Math.round(r.height)}})()`);
  note('panel_frame', panel);
  await pageEval(guest.id, `(()=>{const v=document.querySelector('video');if(v)v.muted=true;return true})()`).catch(() => {});
  note('muted', { all: (await probe('/contents')).contents.map((c) => ({ id: c.id, audioMuted: c.audioMuted })), });
  // The first 12 s: does the pane stay open and the page stay on screen?
  const alerts = () => rEval(`({toasts:[...document.querySelectorAll('.toast')].map(t=>t.innerText),paneError:(document.querySelector('.brw2viewport-error')||{}).innerText||null})`);
  await sleep(600);
  note('alerts_after_open', await alerts());
  const timeline = [];
  const t0 = Date.now();
  while (Date.now() - t0 < 12000) {
    const pane = await rEval(`(()=>{const v=document.querySelector('.brw2view');const b=v&&v.getBoundingClientRect();return {pane:!!v,w:b?Math.round(b.width):0,float:!!document.querySelector('.fbrw')}})()`).catch((e) => ({ err: String(e) }));
    const page = await pageEval(guest.id, `(()=>{const v=document.querySelector('video');return {hidden:document.hidden,focus:document.hasFocus(),t:v&&+v.currentTime.toFixed(2),paused:v&&v.paused,rs:v&&v.readyState,iw:innerWidth,ih:innerHeight}})()`).catch((e) => ({ err: String(e) }));
    timeline.push({ ms: Date.now() - t0, ...pane, ...page });
    await sleep(400);
  }
  results.timeline = timeline;
  log('timeline', timeline.map((x) => `${x.ms}:${x.pane ? 'P' + x.w : '-'}${x.float ? 'F' : ''} ${x.iw}x${x.ih} ${x.hidden ? 'hid' : 'vis'} t=${x.t}${x.paused ? ' paused' : ''}`).join(' | '));
  note('alerts_after_12s', await alerts());
  results.browser = await probe('/browser');
  log('calls', JSON.stringify(results.browser.calls));
  log('states', JSON.stringify(results.browser.states.slice(-8)));
  const state0 = await pageEval(guest.id, VIDEO_STATE);
  note('video_after_6s', state0);
  note('viewport', await pageEval(guest.id, `({href:location.href,origin:self.origin,w:innerWidth,h:innerHeight,dpr:devicePixelRatio,html:document.documentElement.outerHTML.slice(0,500),csp:document.contentType})`));
  // Playback: a real play() and a real seek on the page the link opened.
  const played = await pageEval(guest.id, `(()=>{const v=document.querySelector('video');if(!v)return 'no video';return Promise.race([v.play().then(()=>'playing',e=>String(e)),new Promise(r=>setTimeout(()=>r('play() still pending after 5s'),5000))])})()`).catch((e) => String(e));
  note('play', played);
  await sleep(1500);
  const t1 = await pageEval(guest.id, VIDEO_STATE);
  await sleep(3000);
  const t2 = await pageEval(guest.id, VIDEO_STATE);
  note('playback', { t1: t1.currentTime, t2: t2.currentTime, advanced: t2.currentTime > t1.currentTime, paused: t2.paused, readyState: t2.readyState, error: t2.error });
  const size = await probe(`/pagecapture?id=${guest.id}&file=${EVIDENCE}/page-playing.png`).catch((e) => String(e));
  note('page_capture', size);
  try {
    const py = "import sys;from PIL import Image;i=Image.open(sys.argv[1]).convert('L');h=i.histogram();t=sum(h);print(sum(h[32:])/t)";
    const share = Number(execFileSync('/usr/bin/python3', ['-c', py, `${EVIDENCE}/page-playing.png`], { encoding: 'utf8' }).trim());
    note('page_nonblack_share', share);
  } catch (error) { note('page_nonblack_share', String(error.message).slice(0, 160)); }
  // The window as the window server paints it, native page included.
  try {
    const cg = JSON.parse(execFileSync(`${process.env.TMPDIR || '/tmp/'}vrig-cgwin`, [String(shellPid)], { encoding: 'utf8' })).filter((w) => w.layer === 0 && w.w > 200);
    execFileSync('/usr/sbin/screencapture', ['-x', '-o', `-l${cg[0].id}`, `${EVIDENCE}/window.png`]);
    note('window_capture', `${EVIDENCE}/window.png`);
  } catch (error) { note('window_capture', String(error.message).slice(0, 200)); }
  const seek = await pageEval(guest.id, `new Promise((res)=>{const v=document.querySelector('video');if(!v)return res('no video');const to=setTimeout(()=>res({seeked:false,currentTime:v.currentTime,readyState:v.readyState}),8000);v.addEventListener('seeked',()=>{clearTimeout(to);res({seeked:true,currentTime:v.currentTime,readyState:v.readyState})},{once:true});v.currentTime=14;})`).catch((e) => String(e));
  note('seek', seek);
  await sleep(1500);
  note('after_seek', await pageEval(guest.id, VIDEO_STATE));
  if (args.includes('--stress')) {
    // A big moov-at-end file: far seeks past what is buffered, and a pause longer than every
    // bridge and transfer timeout (15 s write stall, 30 s idle) on a stream held open.
    const seekTo = (t) => pageEval(guest.id, `new Promise((res)=>{const v=document.querySelector('video');const s=performance.now();const to=setTimeout(()=>res({seeked:false,target:${t},currentTime:v.currentTime,readyState:v.readyState,error:v.error&&v.error.message}),15000);const done=()=>{if(v.readyState<3)return;clearTimeout(to);v.removeEventListener('canplay',done);v.removeEventListener('seeked',done);res({seeked:true,target:${t},ms:Math.round(performance.now()-s),currentTime:v.currentTime,readyState:v.readyState})};v.addEventListener('seeked',done);v.addEventListener('canplay',done);v.currentTime=${t};})`);
    note('stress_seek_far', await seekTo(190));
    note('stress_seek_back', await seekTo(30));
    note('stress_seek_mid', await seekTo(120));
    await pageEval(guest.id, `document.querySelector('video').pause()`);
    await sleep(40000);
    note('stress_after_40s_pause', await pageEval(guest.id, VIDEO_STATE));
    await pageEval(guest.id, `document.querySelector('video').play().catch(()=>{})`);
    await sleep(1500);
    const a = await pageEval(guest.id, VIDEO_STATE);
    await sleep(4000);
    const b = await pageEval(guest.id, VIDEO_STATE);
    note('stress_resume', { t1: a.currentTime, t2: b.currentTime, advanced: b.currentTime > a.currentTime, readyState: b.readyState, error: b.error });
    note('stress_seek_after_pause', await seekTo(60));
    await sleep(3000);
    const c = await pageEval(guest.id, VIDEO_STATE);
    note('stress_final', { t: c.currentTime, paused: c.paused, readyState: c.readyState, error: c.error });
  }
  // Paused: the native controls stay on screen and the capture shows them.
  await pageEval(guest.id, `document.querySelector('video').pause()`);
  await sleep(800);
  note('paused_capture', await probe(`/pagecapture?id=${guest.id}&file=${EVIDENCE}/page-paused.png`).catch((e) => String(e)));
  note('muted_at_end', (await probe('/contents')).contents.map((c) => ({ id: c.id, audioMuted: c.audioMuted, audible: c.audible })));
  const netlog = await probe('/net');
  note('net', netlog.net);
  note('guest_console', netlog.console);
} catch (error) {
  note('FAILED', String(error && error.stack || error));
}
fs.writeFileSync(`${EVIDENCE}/results.json`, JSON.stringify(results, null, 2));
try { fs.copyFileSync(`${ROOT}/logs/shell.log`, `${EVIDENCE}/shell.log`); } catch { /* */ }
console.log('RESULTS', `${EVIDENCE}/results.json`);
cleanup();
process.exit(0);
