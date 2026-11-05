import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { after, before, beforeEach, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';
import type { ChangesState, SpawnedWorkItem } from '../src/wire/types.ts';

// The activity pane (approved mock rail-v7, 2026-11-05): «Actividad», three
// text tabs, and a Trabajos tab of totals, machines, subagents, background
// jobs, the queue and what finished today. The live turn — intent, plan,
// current call, calls — is the chat's to show and never comes back here.

const NOW = Date.parse('2026-11-05T15:00:00Z');
const ago = (ms: number) => new Date(NOW - ms).toISOString();
const MIN = 60_000;
const HOUR = 60 * MIN;

let server: ViteDevServer;
let RightRail: React.ComponentType<{ chat: unknown }>;
let TareasCard: React.ComponentType;
let store: { state: Record<string, any>; clearToastTimers(): void };
let railView: { resetRailViews(): void; setRailTab(key: string, tab: string): void; setRailQueueOpen(key: string, open: boolean): void };
let setMachineRouter: (router: unknown) => void;
let changesStore: { resetChangesStore(state?: ChangesState | null): void };
const realNow = Date.now;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ RightRail } = await server.ssrLoadModule('/src/components/RightRail.tsx') as { RightRail: typeof RightRail });
  ({ TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx') as { TareasCard: typeof TareasCard });
  ({ store } = await server.ssrLoadModule('/src/store/store.ts') as { store: typeof store });
  railView = await server.ssrLoadModule('/src/rail-view.ts') as typeof railView;
  ({ setMachineRouter } = await server.ssrLoadModule('/src/wire/api.ts') as { setMachineRouter: typeof setMachineRouter });
  changesStore = await server.ssrLoadModule('/src/changes-store.ts') as typeof changesStore;
});

after(async () => { Date.now = realNow; setMachineRouter(undefined); store.clearToastTimers(); await server.close(); });

beforeEach(() => {
  Date.now = () => NOW;
  railView.resetRailViews();
  setMachineRouter(undefined);
  changesStore.resetChangesStore(null);
});

const tool = (over: Partial<ToolEvent>): ToolEvent => ({
  key: over.id ?? 'tool', at: 0, kind: 'tool', id: 'tool', toolKind: 'execute', title: 'Bash', status: 'completed',
  command: null, terminalId: null, input: null, output: null, location: null, ...over,
});
const job = (id: string, extra: Partial<SpawnedWorkItem> = {}): SpawnedWorkItem => ({
  id, taskId: id, tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'external', label: id,
  status: 'running', startedAt: ago(HOUR), updatedAt: ago(0), ...extra,
});
const runningJob = (id: string, ranMs: number, extra: Partial<SpawnedWorkItem> = {}) =>
  job(id, { queuedAt: ago(ranMs + MIN), execStartedAt: ago(ranMs), summary: `python train.py --run ${id}`, ...extra });
const waitingJob = (id: string, waitedMs: number, extra: Partial<SpawnedWorkItem> = {}) =>
  job(id, { queuedAt: ago(waitedMs), startedAt: ago(waitedMs), resource: 'gpu0', ...extra });
const far = { hostMachine: 'm-far', hostMachineName: 'buildbox' };

function seed({ messages = [], work = [], changes, pane }: { messages?: unknown[]; work?: SpawnedWorkItem[]; changes?: ChangesState; pane?: string } = {}) {
  const chat: Record<string, unknown> = { id: 'tab-1', chatId: 'chat-1', cwd: '/work/project', messages };
  if (pane) chat.pane = pane;
  store.state.meta = { profile: 'dev' };
  store.state.activeId = 'tab-1';
  store.state.chats = [chat, { id: 'tab-2', chatId: 'chat-2', cwd: null, messages: [] }];
  store.state.hasSpawnedWorkChannels = false;
  store.state.spawnedWorkByChat = { 'tab-1\u0000chat-1': work };
  // This machine's name as the daemon reports it (its hostname unless renamed).
  Object.assign(store, { selfMachineName: 'workbench' });
  changesStore.resetChangesStore(changes ?? null);
  return chat;
}
const rail = (chat: unknown) => renderToStaticMarkup(React.createElement(RightRail, { chat }));
const work = () => renderToStaticMarkup(React.createElement(TareasCard));
const count = (html: string, pattern: RegExp) => (html.match(pattern) ?? []).length;

// Three changed files in one repo, as changes:state reports them.
const changesPayload = (): ChangesState => ({
  seq: 1, windowDays: 7, chats: [{ chatId: 'chat-1', tabId: 'tab-1', title: 'Panel', providerId: 'claude' }],
  repos: [{ root: '/work/project', name: 'project', branch: 'feature/rail', adds: 52, dels: 5, checked: true, files: [
    { path: 'internal/wire/wire.go', adds: 12, dels: 3, chatId: 'chat-1', tabId: 'tab-1', at: NOW - MIN },
    { path: 'desktop/renderer2/src/components/RightRail.tsx', adds: 40, dels: 0, chatId: 'chat-1', tabId: 'tab-1', at: NOW - MIN },
    { path: 'README.md', adds: 0, dels: 2 },
  ] }],
});

test('the pane leaves the live turn to the chat: no hero, plan, current call or Llamadas', () => {
  const entries = [{ status: 'completed', content: 'Leer el panel' }, { status: 'in_progress', content: 'Rediseñar el panel' }];
  const chat = seed({ messages: [{ id: 'a-1', role: 'assistant', content: '', status: 'running', at: null, events: [
    tool({ id: 'bash-1', command: 'go test ./...', startedAt: NOW - 5000, endedAt: NOW - 2000 }),
    tool({ id: 'bash-2', status: 'in_progress', command: 'go vet ./...', startedAt: NOW - 1000 }),
  ] }] });
  (chat as { planLatest?: unknown }).planLatest = entries;
  const html = work();
  assert.doesNotMatch(html, /Rediseñar el panel|Leer el panel/, 'the plan is the chat\'s');
  assert.doesNotMatch(html, /r-say|r-pdots|r-live-call|r-meta|Ahora|Último turno|Sin turno activo|Sin plan del agente|Llamadas/);
  assert.equal(html, '<section class="rp-work"><div class="rp-empty">Nada corriendo</div></section>', 'one short muted line, no icon, no paragraph');
});

test('the header says Actividad and keeps its expand and close buttons; three text tabs with counts', () => {
  const chat = seed({
    messages: [{ id: 'a-1', role: 'assistant', content: 'Listo: [informe.pdf](/workass/artifacts/informe-1/informe.pdf)', status: 'done', at: null, events: [] }],
    work: [runningJob('r1', 10 * MIN), runningJob('r2', 5 * MIN), waitingJob('w1', HOUR)],
    changes: changesPayload(),
  });
  const html = rail(chat);
  assert.match(html, /<div class="railhead"><span class="rp-title">Actividad<\/span><button class="tico" title="Expandir panel">/);
  assert.match(html, /<button class="tico" title="Cerrar panel">/);
  const tabs = [...html.matchAll(/<button type="button" role="tab" id="rp-tab-(\w+)" aria-selected="(true|false)" class="rp-tab( on)?">(\w+)(?:<span class="rp-tab-c">(\d+)<\/span>)?<\/button>/g)]
    .map((m) => [m[4], m[2], m[5] ?? '']);
  assert.deepEqual(tabs, [['Trabajos', 'true', '3'], ['Cambios', 'false', '3'], ['Salidas', 'false', '1']]);
  assert.match(html, /<div class="rp-body" role="tabpanel" aria-labelledby="rp-tab-trabajos" data-tab="trabajos">/);
  // No pill, no filled tab: the active tab is its underline (CSS below).
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const on = css.match(/\.rp-tab\.on\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(on, /border-bottom-color:\s*var\(--acc\)/);
  assert.doesNotMatch(on, /background(?!:\s*none)/);
  assert.match(css.match(/\.rp-tab\s*\{([^}]*)\}/)?.[1] ?? '', /border-bottom:\s*2px solid transparent/);
});

test('zero counts are left out and the chosen tab is kept per chat', () => {
  const chat = seed({ changes: changesPayload() });
  let html = rail(chat);
  assert.match(html, /class="rp-tab on">Trabajos<\/button>/, 'Trabajos is the default and shows no 0');
  assert.match(html, /class="rp-tab">Salidas<\/button>/);
  railView.setRailTab('tab-1', 'cambios');
  html = rail(chat);
  assert.match(html, /aria-selected="true" class="rp-tab on">Cambios<span class="rp-tab-c">3<\/span>/);
  assert.match(html, /data-tab="cambios"/);
  // A window that remembered the old «Archivos» tab lands on «Cambios».
  railView.setRailTab('tab-1', 'archivos');
  assert.match(rail(chat), /data-tab="cambios"/);
  // Another chat keeps its own choice.
  store.state.activeId = 'tab-2';
  assert.match(rail(store.state.chats[1]), /data-tab="trabajos"/);
});

test('totals omit zero parts; one row per machine with a segment per job and running + waiting', () => {
  seed({ work: [
    runningJob('here-1', 2 * HOUR + 2 * MIN), runningJob('here-2', 2 * HOUR + 4 * MIN), runningJob('far-1', 41 * MIN, far),
    waitingJob('here-w1', 2 * HOUR), waitingJob('here-w2', 2 * HOUR - MIN), waitingJob('here-w3', 2 * HOUR - 2 * MIN),
    waitingJob('far-w1', 44 * MIN, far), waitingJob('far-w2', 43 * MIN, far),
  ] });
  const html = work();
  assert.match(html, /<div class="wtotals"><span class="wtotal"><b>3<\/b> corriendo<\/span><span class="wtotal"><span class="wtotal-sep" aria-hidden="true">·<\/span><b>5<\/b> en espera<\/span><\/div>/);
  assert.doesNotMatch(html, /subagente/);
  const rows = [...html.matchAll(/<div class="wload-r"[^>]*>([\s\S]*?)<span class="wload-n"[^>]*>(\d+ \+ \d+)<\/span><\/div>/g)];
  assert.equal(rows.length, 2);
  assert.match(rows[0][1], /data-local=""|wload-nm">workbench</);
  assert.match(rows[0][1], /<span class="wload-nm">workbench<\/span>/);
  assert.equal(count(rows[0][1], /<i class="r"><\/i>/g), 2);
  assert.equal(count(rows[0][1], /<i><\/i>/g), 3);
  assert.equal(rows[0][2], '2 + 3');
  assert.match(rows[1][1], /<span class="wload-nm">buildbox<\/span>/);
  assert.equal(rows[1][2], '1 + 2');
  // a screen for this machine, a stacked server for the other
  assert.match(rows[0][1], /<rect x="2.3" y="3.3" width="11.4" height="8"/);
  assert.match(rows[1][1], /<rect x="2.3" y="2.7" width="11.4" height="4.7"/);
});

test('running jobs are flat rows capped at four; the rest fold into «+N más corriendo»', () => {
  setMachineRouter({ spawnedWorkStop: async () => ({ ok: true }) });
  seed({ work: [
    runningJob('mem-reset01-s5-mac with a name long enough to clamp', 2 * HOUR + 2 * MIN, { outputFile: '/fixture/a.output', pid: 11 }),
    runningJob('j2', 30 * MIN), runningJob('j3', 20 * MIN), runningJob('j4', 10 * MIN, far), runningJob('j5', 5 * MIN), runningJob('j6', MIN),
  ] });
  const html = work();
  assert.match(html, /<div class="wsec">Trabajos en segundo plano<span class="wsec-h">6 corriendo<\/span><\/div>/);
  assert.equal(count(html, /<div class="wjob"/g), 4);
  assert.match(html, /<button type="button" class="wmore" aria-expanded="false">\+2 más corriendo<\/button>/);
  assert.doesNotMatch(html, /data-work-id="j5"|data-work-id="j6"/);
  const first = html.slice(html.indexOf('<div class="wjob"'), html.indexOf('<div class="wjob"', html.indexOf('<div class="wjob"') + 1));
  assert.match(first, /<span class="wdot" aria-label="En curso"><\/span>/);
  assert.match(first, /<span class="wjob-n" title="mem-reset01-s5-mac with a name long enough to clamp">mem-reset01-s5-mac with a name long enough to clamp<\/span>/);
  assert.match(first, /<span class="wjob-t">2 h 2 min<\/span>/);
  assert.match(first, /<span class="wjob-mt">workbench · python train\.py --run mem-reset01/);
  // hover actions: the output tail and the stop, as text
  assert.match(first, /<button type="button" class="wbtn" aria-expanded="false">Ver salida<\/button>/);
  assert.match(first, /<button type="button" class="wbtn stop" aria-label="Detener mem-reset01[^"]*">Detener<\/button>/);
  assert.match(html, /data-work-id="j4"[\s\S]*?<span class="wjob-mt">buildbox · python train\.py --run j4<\/span>/);
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  // Hover shows the actions without resizing the row: they lie over its end,
  // out of the layout, and only their opacity changes.
  for (const [acts, row] of [['wjob-acts', 'wjob'], ['wq-acts', 'wq-row']]) {
    const rule = css.match(new RegExp(`\\.${acts}\\s*\\{([^}]*)\\}`))?.[1] ?? '';
    assert.match(rule, /position:\s*absolute/, acts);
    assert.match(rule, /opacity:\s*0/, acts);
    assert.doesNotMatch(rule, /display:\s*none/, acts);
    assert.match(css, new RegExp(`\\.${row}:hover \\.${acts}[^{]*\\{\\s*opacity:\\s*1;`), acts);
    assert.match(css, new RegExp(`\\.${row}\\s*\\{[^}]*position:\\s*relative`), row);
  }
  assert.match(css.match(/\.wjob-n\s*\{([^}]*)\}/)?.[1] ?? '', /text-overflow:\s*ellipsis[\s\S]*white-space:\s*nowrap/);
});

test('the queue is one closed row by default: how many wait and what runs next on each machine', () => {
  const chat = seed({ work: [
    runningJob('holder', HOUR), waitingJob('next-here', 2 * HOUR), waitingJob('then-here', HOUR),
    waitingJob('next-far', 44 * MIN, far), waitingJob('then-far', 40 * MIN, far),
  ] });
  const html = work();
  assert.equal(count(html, /class="wq-sum"/g), 1);
  assert.match(html, /<button type="button" class="wq-sum" aria-expanded="false"><span class="wq-chev" aria-hidden="true"><svg/);
  assert.match(html, /<span class="wq-lead">En espera · 4<\/span><span class="wq-nx" title="sigue next-here y next-far">sigue <span><b>next-here<\/b><\/span><span> y <b>next-far<\/b><\/span><\/span>/);
  assert.doesNotMatch(html, /class="wq-list"/);
  assert.doesNotMatch(html, /then-here|then-far/);
  // Open is remembered for this chat.
  railView.setRailQueueOpen(String((chat as { id: string }).id), true);
  const open = work();
  assert.match(open, /<button type="button" class="wq-sum" aria-expanded="true">/);
  assert.match(open, /<span class="wq-nx">en orden, por máquina<\/span>/);
  const list = open.slice(open.indexOf('<div class="wq-list">'));
  const groups = [...list.matchAll(/<div class="wq-mh">[\s\S]*?<span class="wq-mn" title="[^"]*">([^<]+)<\/span> · (\d+)<\/div>/g)].map((m) => `${m[1]} · ${m[2]}`);
  assert.equal(groups.length, 2);
  assert.match(groups[0], /^workbench · 2$/);
  assert.equal(groups[1], 'buildbox · 2');
  const rows = [...list.matchAll(/<span class="wq-p">(\d+)<\/span><span class="wq-t" title="[^"]*">([^<]+)<\/span><span class="wq-w">([^<]+)<\/span>/g)].map((m) => `${m[1]} ${m[2]} ${m[3]}`);
  assert.deepEqual(rows, ['1 next-here espera 2 h', '2 then-here espera 1 h', '1 next-far espera 44 min', '2 then-far espera 40 min']);
  // Its own fixed height that scrolls by itself, so the pane never grows a bar.
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const box = css.match(/\.wq-list\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(box, /max-height:\s*150px/);
  assert.match(box, /overflow-y:\s*auto/);
  assert.match(box, /overscroll-behavior:\s*contain/);
});

test('Terminados hoy counts today and opens the history in place; earlier days fold once more', () => {
  seed({ work: [
    runningJob('live', MIN),
    job('done-1', { status: 'exited', startedAt: ago(30 * MIN), finishedAt: ago(20 * MIN) }),
    job('done-2', { status: 'failed', startedAt: ago(30 * MIN), finishedAt: ago(10 * MIN) }),
    job('old-1', { status: 'exited', startedAt: ago(80 * HOUR), finishedAt: ago(79 * HOUR) }),
  ] });
  const html = work();
  assert.match(html, /<details class="wdone"><summary><span class="wdone-ic" aria-hidden="true"><svg[\s\S]*?<\/span><span class="wdone-l">Terminados hoy<\/span><span class="wdone-n">2<\/span><\/summary>/);
  const body = html.slice(html.indexOf('<div class="wdone-b">'));
  assert.ok(body.indexOf('done-1') > 0 && body.indexOf('done-2') > 0);
  assert.match(body, /<details class="wdone-old"><summary>1 anterior<\/summary>[\s\S]*old-1/);
  assert.doesNotMatch(body, /falló|bgr-stop|wbtn stop/);
});

test('the tabs say so in one short line when there is nothing', () => {
  const chat = seed();
  assert.match(rail(chat), /<div class="rp-body"[^>]*><section class="rp-work"><div class="rp-empty">Nada corriendo<\/div><\/section><\/div>/);
  railView.setRailTab('tab-1', 'cambios');
  assert.match(rail(chat), /<div class="rp-body"[^>]*><div class="rp-empty">Sin cambios todavía<\/div><\/div>/);
  railView.setRailTab('tab-1', 'salidas');
  assert.match(rail(chat), /<div class="rp-body"[^>]*><div class="rp-empty">Nada todavía<\/div><\/div>/);
});

test('Cambios is flat repo rows with the files grouped by who changed them', () => {
  const chat = seed({ changes: changesPayload() });
  railView.setRailTab('tab-1', 'cambios');
  setMachineRouter({ changesWatch: async () => ({ id: 'w' }) });
  const html = rail(chat);
  assert.doesNotMatch(html, /class="ecard"|class="efile"/, 'the old Archivos list is gone');
  assert.match(html, /<div class="cb-list"><div class="cb-repo open"><button type="button" class="cb-rh" aria-expanded="true">[\s\S]*?<span class="cb-name" title="\/work\/project">project<\/span>/);
  assert.match(html, /<b title="Panel">Panel<\/b><span class="cb-who-m"> · este chat · 2 archivos<\/span>/);
  assert.match(html, /Fuera de Workass · 1 archivo/);
});

test('Salidas lists pages, images and hosted files with their tag, title and Abrir', () => {
  const RESULT = '{"htmlRender":{"id":"cobertura-3f9a2b1c4d5e","title":"Cobertura por paquete","urlPath":"/workass/artifacts/cobertura-3f9a2b1c4d5e/"},"message":"ok"}';
  const chat = seed({ messages: [
    { id: 'a-1', role: 'assistant', content: '', status: 'done', at: null, events: [tool({ id: 'render', command: 'tools call workass_render_html', output: RESULT })] },
    { id: 'a-2', role: 'assistant', content: 'El [informe trimestral completo con un título muy largo.pdf](/workass/artifacts/informe-1/informe.pdf)', status: 'done', at: null, events: [],
      images: [{ mimeType: 'image/png', data: 'AAAA', name: 'Captura' }] },
  ] });
  railView.setRailTab('tab-1', 'salidas');
  const html = rail(chat);
  const rows = [...html.matchAll(/<div class="rp-out" data-kind="(\w+)"><span class="rp-out-k">([^<]+)<\/span><span class="rp-out-t" title="([^"]+)">[^<]+<\/span><(a|button)[^>]*class="rp-open"[^>]*>Abrir<\/(a|button)><\/div>/g)]
    .map((m) => [m[2], m[3], m[4]]);
  assert.deepEqual(rows, [
    ['ARCHIVO', 'informe trimestral completo con un título muy largo.pdf', 'a'],
    ['IMAGEN', 'Captura', 'button'],
    ['PÁGINA', 'Cobertura por paquete', 'a'],
  ]);
  assert.match(html, /class="rp-tab on">Salidas<span class="rp-tab-c">3<\/span>/);
  assert.match(html, /<a class="rp-open" href="\/workass\/artifacts\/cobertura-3f9a2b1c4d5e\/" target="_blank" rel="noreferrer">Abrir<\/a>/);
});

// The subagent block is the CURRENT design, untouched: the same rows, nested
// work, icons and expand behaviour the pane drew before the redesign. The
// fixture is the old TareasCard's markup for this exact scenario.
test('subagent rows render exactly as they did before the redesign', () => {
  const FIXED = Date.parse('2026-11-05T12:00:00Z');
  Date.now = () => FIXED;
  const iso = (msAgo: number) => new Date(FIXED - msAgo).toISOString();
  const events = [
    tool({ key: 'sub-a', id: 'sub-a', toolKind: 'agent', title: 'Revisión de seguridad del protocolo con un nombre largo que no cabe', status: 'in_progress', subagentId: 'sub-a', subagentHeader: true, subagentProvider: 'claude', subagentModel: 'Opus5.5-xhigh', startedAt: FIXED - 180_000 }),
    tool({ key: 'sub-a:read', id: 'sub-a:read', toolKind: 'read', title: 'Read File', status: 'in_progress', location: 'internal/wire/wire.go', subagentId: 'sub-a', subagentProvider: 'claude', startedAt: FIXED - 20_000 }),
    tool({ key: 'sub-b', id: 'sub-b', toolKind: 'agent', title: 'Comparar diseños', status: 'completed', subagentId: 'sub-b', subagentHeader: true, subagentProvider: 'gpt', subagentModel: 'GPT-6-high', startedAt: FIXED - 300_000, endedAt: FIXED - 200_000 }),
    tool({ key: 'sub-b:1', id: 'sub-b:1', toolKind: 'execute', title: 'Bash', status: 'completed', command: 'ls -la', subagentId: 'sub-b', startedAt: FIXED - 290_000, endedAt: FIXED - 289_000 }),
    tool({ key: 'sub-c', id: 'sub-c', toolKind: 'agent', title: 'Bifurcado', status: 'failed', subagentId: 'sub-c', subagentHeader: true, subagentProvider: 'claude', subagentModel: 'Haiku-low', subagentForked: true, startedAt: FIXED - 100_000, endedAt: FIXED - 90_000 }),
    tool({ key: 'main-1', id: 'main-1', toolKind: 'execute', title: 'Bash', status: 'completed', command: 'go test ./...', startedAt: FIXED - 50_000, endedAt: FIXED - 40_000 }),
  ];
  const base = { tabId: 'tab-s', chatId: 'chat-s', providerId: 'claude', updatedAt: iso(0) };
  const items = [
    { ...base, id: 'wa-sub-a', taskId: 'wa-sub-a', toolCallId: 'sub-a', kind: 'subagent', status: 'running', label: 'Revisión de seguridad', startedAt: iso(180_000), modelLabel: 'Opus5.5-xhigh', assistantBrand: 'claude', summary: 'leyendo', lastToolName: 'Read' },
    { ...base, id: 'kid-live', taskId: 'kid-live', parentTaskId: 'wa-sub-a', kind: 'bash', status: 'running', label: 'go test ./internal/wire', startedAt: iso(40_000), pid: 4242, outputFile: '/fixture/kid-live.output' },
    { ...base, id: 'kid-done', taskId: 'kid-done', parentTaskId: 'wa-sub-a', kind: 'bash', status: 'exited', label: 'go vet ./...', startedAt: iso(90_000), finishedAt: iso(60_000) },
    { ...base, id: 'bg-1', taskId: 'bg-1', kind: 'external', status: 'running', label: 'not a subagent', startedAt: iso(500_000), queuedAt: iso(500_000), execStartedAt: iso(400_000), summary: 'sleep 600' },
  ];
  store.state.meta = { profile: 'dev' };
  store.state.activeId = 'tab-s';
  store.state.chats = [{ id: 'tab-s', chatId: 'chat-s', cwd: null, messages: [{ id: 'assistant-s', role: 'assistant', content: '', status: 'running', at: null, events }] }];
  store.state.hasSpawnedWorkChannels = false;
  store.state.spawnedWorkByChat = { 'tab-s\u0000chat-s': items };
  const html = work();
  const start = html.indexOf('<div class="r-subs">');
  let depth = 0;
  let end = -1;
  const tags = /<(\/?)div\b[^>]*>/g;
  tags.lastIndex = start;
  for (let m = tags.exec(html); m; m = tags.exec(html)) {
    depth += m[1] ? -1 : 1;
    if (depth === 0) { end = m.index + m[0].length; break; }
  }
  const before = readFileSync(new URL('./fixtures/rail-subagents-before.html', import.meta.url), 'utf8');
  assert.equal(html.slice(start, end), before);
  // The block comes first in the tab, right after the totals and machines.
  assert.ok(html.indexOf('<div class="r-subs">') < html.indexOf('<div class="wbg">'));
  assert.match(html, /<div class="wsec">Subagentes<span class="wsec-h">2 trabajando<\/span><\/div><div class="r-subs">/);
});
