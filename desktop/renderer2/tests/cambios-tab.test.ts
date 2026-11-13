import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, afterEach, before, beforeEach, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { ChangesDiff, ChangesState } from '../src/wire/types.ts';

// «Cambios» (approved mock rail-cambios-v2, 2026-11-05): one foldable row per
// repo the owner's chats touched, files grouped by who changed them with THIS
// chat first, a hover peek at the diff, clean repos on one line, and no scope
// switch or refresh button.

const NOW = Date.parse('2026-11-05T15:00:00Z');
const MIN = 60_000;
const realNow = Date.now;

type View = typeof import('../src/changes-view.ts');
type Store = typeof import('../src/changes-store.ts');
let server: ViteDevServer;
let CambiosTab: React.ComponentType<{ chat: unknown }>;
let Peek: React.ComponentType<{ file: unknown; diff: ChangesDiff; above: boolean; onExpand: () => void; onCopy: () => void }>;
let FullDiff: React.ComponentType<{ diff: ChangesDiff; onClose: () => void }>;
let cambios: { resetCambiosView(): void; setCambiosFold(root: string, open: boolean): void };
let view: View;
let changes: Store;
let appStore: { state: Record<string, any>; clearToastTimers(): void };
let setMachineRouter: (router: unknown) => void;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent' });
  const tab = await server.ssrLoadModule('/src/components/CambiosTab.tsx') as Record<string, any>;
  ({ CambiosTab, Peek, FullDiff } = tab as typeof tab & { CambiosTab: typeof CambiosTab });
  cambios = tab as typeof cambios;
  view = await server.ssrLoadModule('/src/changes-view.ts') as View;
  changes = await server.ssrLoadModule('/src/changes-store.ts') as Store;
  ({ store: appStore } = await server.ssrLoadModule('/src/store/store.ts') as { store: typeof appStore });
  ({ setMachineRouter } = await server.ssrLoadModule('/src/wire/api.ts') as { setMachineRouter: typeof setMachineRouter });
});

// Each test pins the clock; give it back after each one, so a file run after
// this one in the same process sees the real time.
afterEach(() => { Date.now = realNow; });

after(async () => { Date.now = realNow; setMachineRouter(undefined); appStore.clearToastTimers(); await server.close(); });

interface Calls { watch: unknown[]; unwatch: unknown[]; diff: unknown[] }
let calls: Calls;

beforeEach(() => {
  Date.now = () => NOW;
  calls = { watch: [], unwatch: [], diff: [] };
  setMachineRouter({
    changesWatch: async (o: unknown) => { calls.watch.push(o); return { id: 'w-1' }; },
    changesUnwatch: async (o: unknown) => { calls.unwatch.push(o); return { ok: true }; },
    changesFocus: async () => ({ ok: true }),
    changesDiff: async (o: { root: string; path: string }) => { calls.diff.push(o); return { root: o.root, path: o.path, text: QUEUE_DIFF }; },
  });
  cambios.resetCambiosView();
  changes.resetChangesStore(null);
  appStore.state.chats = [
    { id: 'tab-1', chatId: 'chat-1', title: 'Panel derecho', providerId: 'claude', cwd: '/work/workass', messages: [] },
    { id: 'tab-2', chatId: 'chat-2', title: 'Revisión de seguridad', providerId: 'codex', cwd: '/work/workass', messages: [] },
  ];
});

const QUEUE_DIFF = [
  'diff --git a/x b/x', 'index 1..2 100644', '--- a/QueueList.tsx', '+++ b/QueueList.tsx',
  '@@ -30,9 +30,10 @@ export function QueueList() {',
  '   const open = useOpen();',
  '   return (',
  '-  {!!queue.length && <div className="qlabel">',
  '+  <QueueSummary next={nextByMachine} count={queue.length} />',
  '+  {open && <QueueByMachine rows={queue} />}',
  '   </div>',
  '   );',
  ' }',
  ' ',
  ' export default QueueList;',
  ' // end',
  '@@ -80,3 +81,4 @@',
  '   last();',
  '+  added();',
  '',
].join('\n');

function snapshot(): ChangesState {
  return {
    seq: 4, windowDays: 7,
    chats: [
      { chatId: 'chat-1', tabId: 'tab-1', title: 'Panel derecho', providerId: 'claude', at: NOW - 2 * MIN },
      { chatId: 'chat-2', tabId: 'tab-2', title: 'Revisión de seguridad', providerId: 'codex', at: NOW - 30 * MIN },
      { chatId: 'chat-3', tabId: 'tab-3', title: 'Una tarea con un título larguísimo que no entra en el panel', providerId: 'claude', at: NOW - 10 * MIN },
    ],
    repos: [
      {
        root: '/work/workass', name: 'workass', branch: 'main', upstream: 'origin/main', ahead: 2, adds: 45, dels: 7,
        checked: true, checking: true, activityAt: NOW - 2 * MIN,
        files: [
          { path: 'cmd/workass/permission_ceiling.go', adds: 6, dels: 0, new: true, chatId: 'chat-2', tabId: 'tab-2', at: NOW - 30 * MIN },
          { path: 'desktop/renderer2/src/components/QueueList.tsx', adds: 18, dels: 3, chatId: 'chat-1', tabId: 'tab-1', at: NOW - 2 * MIN },
          { path: 'desktop/renderer2/src/styles/app.css', adds: 18, dels: 4, chatId: 'chat-1', tabId: 'tab-1', at: NOW - 3 * MIN },
          { path: '.dev/notes.md', adds: 3, dels: 0, new: true },
          { path: 'internal/other.go', adds: 0, dels: 0, chatId: 'chat-3', tabId: 'tab-3', at: NOW - 10 * MIN },
        ],
      },
      { root: '/work/workass-mobile', name: 'workass-mobile', branch: 'feature/steer', adds: 12, dels: 0, checked: true, activityAt: NOW - 60 * MIN,
        files: [{ path: 'src/Steer.tsx', adds: 12, dels: 0, chatId: 'chat-2', tabId: 'tab-2', at: NOW - 60 * MIN }] },
      { root: '/work/myproject', name: 'myproject', branch: 'main', behind: 1, adds: 3, dels: 2, checked: true, activityAt: NOW - 180 * MIN,
        files: [{ path: 'main.go', adds: 3, dels: 2 }] },
      ...['alpha', 'beta', 'gamma', 'delta'].map((name) => ({ root: `/work/${name}`, name, branch: 'main', adds: 0, dels: 0, files: [], checked: true })),
      // Not answered yet: not drawn, not counted as clean.
      { root: '/work/pending', name: 'pending', adds: 0, dels: 0, files: [], checked: false, checking: true },
    ],
  };
}

const thisChat = { id: 'tab-1', chatId: 'chat-1', title: 'Panel derecho', messages: [] };
const render = (chat: unknown = thisChat) => renderToStaticMarkup(React.createElement(CambiosTab, { chat }));
const repoBlock = (html: string, name: string) => {
  const start = html.indexOf(`<span class="cb-name" title="/work/${name}">${name}</span>`);
  assert.ok(start >= 0, `repo ${name} is drawn`);
  const begin = html.lastIndexOf('<div class="cb-repo', start);
  const next = html.indexOf('<div class="cb-repo', start);
  const clean = html.indexOf('<div class="cb-clean"', start);
  const end = [next, clean].filter((i) => i > 0).sort((a, b) => a - b)[0] ?? html.length;
  return html.slice(begin, end);
};

test('this chat comes first, then other chats by most recent edit, then Fuera de Workass', () => {
  changes.resetChangesStore(snapshot());
  const html = repoBlock(render(), 'workass');
  const who = [...html.matchAll(/<div class="cb-who (\w+)">([\s\S]*?)<\/div>/g)].map((m) => [m[1], m[2].replace(/<svg[\s\S]*?<\/svg>/g, '<svg/>').replace(/<[^>]+>/g, '')]);
  assert.deepEqual(who, [
    ['this', 'Panel derecho · este chat · 2 archivos'],
    ['chat', 'Una tarea con un título larguísimo que no entra en el panel · 1 archivo'],
    ['chat', 'Revisión de seguridad · 1 archivo'],
    ['outside', 'Fuera de Workass · 1 archivo'],
  ]);
  // The same provider glyphs the app uses: Claude's burst, Codex's mark, and a
  // person for the hand-made changes.
  const marks = [...html.matchAll(/<span class="cb-mk" aria-hidden="true">([\s\S]*?)<\/span>/g)].map((m) => m[1]);
  assert.match(marks[0], /stroke="#cc7a52"/);
  assert.match(marks[2], /<path d="M22\.2819/);
  assert.match(marks[3], /<circle cx="8" cy="5\.5" r="2\.5"/);
  // Files of this chat, newest first: name, muted folder, time, counts.
  const files = [...html.matchAll(/<span class="cb-fn" title="([^"]+)">([^<]+)<\/span>/g)].map((m) => m[2]);
  assert.deepEqual(files, ['QueueList.tsx', 'app.css', 'other.go', 'permission_ceiling.go', 'notes.md']);
  assert.match(html, /<span class="cb-fn" title="desktop\/renderer2\/src\/components\/QueueList\.tsx">QueueList\.tsx<\/span><span class="cb-dir" title="[^"]+">components<\/span><span class="cb-tm">hace 2 min<\/span><span class="cb-s"><span class="a">\+18<\/span><span class="d">−3<\/span><\/span>/);
  assert.match(html, /permission_ceiling\.go<\/span><span class="cb-dir" title="[^"]+">cmd\/workass<\/span><span class="cb-tm">hace 30 min<\/span><span class="cb-tag">nuevo<\/span><span class="cb-s"><span class="a">\+6<\/span><\/span>/);
  assert.match(html, /notes\.md<\/span><span class="cb-dir" title="[^"]+">\.dev<\/span><span class="cb-tag">nuevo<\/span>/, 'no time for a hand-made change');
  // Long names clamp; the full text is on hover.
  assert.match(html, /<b title="Una tarea con un título larguísimo que no entra en el panel">/);
});

test('another chat sees its own group first and this one by its title', () => {
  changes.resetChangesStore(snapshot());
  const html = repoBlock(render({ id: 'tab-2', chatId: 'chat-2', title: 'Revisión de seguridad', messages: [] }), 'workass');
  const who = [...html.matchAll(/<div class="cb-who (\w+)">/g)].map((m) => m[1]);
  assert.deepEqual(who, ['this', 'chat', 'chat', 'outside']);
  assert.match(html, /<b title="Revisión de seguridad">Revisión de seguridad<\/b><span class="cb-who-m"> · este chat · 1 archivo<\/span>/);
});

test('repo rows: name, branch, ↑/↓ only when not zero, a five-square bar and +/−; the branch pulses while checked', () => {
  changes.resetChangesStore(snapshot());
  const html = render();
  assert.match(repoBlock(html, 'workass'), /<span class="cb-br pulse" title="main"><svg[\s\S]*?<\/svg><span class="cb-br-t">main<\/span><\/span><span class="cb-sync" title="2 sin subir">↑2<\/span><span class="cb-ds"><span class="cb-blk" aria-hidden="true"><i class="a"><\/i><i class="a"><\/i><i class="a"><\/i><i class="a"><\/i><i class="d"><\/i><\/span><span class="a">\+45<\/span><span class="d">−7<\/span><\/span>/);
  assert.match(repoBlock(html, 'workass-mobile'), /<span class="cb-br" title="feature\/steer">[\s\S]*?<\/span><span class="cb-ds"><span class="cb-blk" aria-hidden="true"><i class="a"><\/i><i class="a"><\/i><i class=""><\/i><i class=""><\/i><i class=""><\/i><\/span><span class="a">\+12<\/span><\/span>/, 'no ↑0 or ↓0');
  assert.match(repoBlock(html, 'myproject'), /<span class="cb-sync" title="1 por bajar">↓1<\/span><span class="cb-ds"><span class="cb-blk" aria-hidden="true"><i class="a"><\/i><i class="d"><\/i><i class=""><\/i>/);
  assert.doesNotMatch(html, /↑0|↓0/);
  assert.doesNotMatch(html, /pending/, 'a repo that has not answered is not drawn');
  assert.deepEqual(view.changeBar(42, 7), ['a', 'a', 'a', 'a', 'd']);
  assert.deepEqual(view.changeBar(12, 0), ['a', 'a', '', '', '']);
  assert.deepEqual(view.changeBar(3, 2), ['a', 'd', '', '', '']);
  assert.deepEqual(view.changeBar(0, 300), ['d', 'd', 'd', 'd', 'd']);
  assert.deepEqual(view.changeBar(0, 0), ['', '', '', '', '']);
});

test('the most recently changed repo starts unfolded, the rest folded, and a fold is remembered', () => {
  changes.resetChangesStore(snapshot());
  let html = render();
  const order = [...html.matchAll(/<span class="cb-name" title="[^"]+">([^<]+)<\/span>/g)].map((m) => m[1]);
  assert.deepEqual(order, ['workass', 'workass-mobile', 'myproject']);
  assert.match(repoBlock(html, 'workass'), /^<div class="cb-repo open"><button type="button" class="cb-rh" aria-expanded="true">/);
  assert.match(repoBlock(html, 'workass-mobile'), /^<div class="cb-repo"><button type="button" class="cb-rh" aria-expanded="false">/);
  assert.doesNotMatch(repoBlock(html, 'workass-mobile'), /cb-body/);
  cambios.setCambiosFold('/work/workass', false);
  cambios.setCambiosFold('/work/myproject', true);
  html = render();
  assert.doesNotMatch(repoBlock(html, 'workass'), /cb-body/);
  assert.match(repoBlock(html, 'myproject'), /<div class="cb-body">[\s\S]*Fuera de Workass · 1 archivo[\s\S]*main\.go/);
});

test('clean repos collapse into one line; no scope switch and no refresh button', () => {
  changes.resetChangesStore(snapshot());
  const html = render();
  assert.match(html, /<div class="cb-clean" title="alpha, beta, delta, gamma"><svg[\s\S]*?<\/svg>4 repos sin cambios<\/div><\/div>$/);
  assert.equal(view.cleanLine(1), '1 repo sin cambios');
  assert.doesNotMatch(html, /Actualizar|Refrescar|Este chat<\/|Todos los chats|scope|<button[^>]*title="Actualizar/);
  assert.equal([...html.matchAll(/<button/g)].length, 3, 'the only buttons are the three repo rows');
});

test('empty: «Sin cambios todavía» once the daemon has answered; nothing before it does', () => {
  changes.resetChangesStore({ seq: 1, windowDays: 7, chats: [], repos: [] });
  assert.equal(render(), '<div class="rp-empty">Sin cambios todavía</div>');
  changes.resetChangesStore({ seq: 1, windowDays: 7, chats: [], repos: [{ root: '/work/a', name: 'a', adds: 0, dels: 0, files: [], checked: true }] });
  assert.equal(render(), '<div class="rp-empty">Sin cambios todavía</div>', 'only clean repos is still nothing to show');
  changes.resetChangesStore(null);
  assert.equal(render(), '<div class="cb-list" aria-busy="true"></div>', 'no claim before the first answer');
  setMachineRouter(undefined);
  assert.equal(render(), '<div class="rp-empty">Sin cambios todavía</div>', 'a daemon without the channels');
});

test('the peek shows about eight lines around the first change, struck removals, Ver diff completo and Copiar ruta', () => {
  const file = view.changesView(snapshot(), { chatId: 'chat-1' }).repos[0].groups[0].files[0];
  const html = renderToStaticMarkup(React.createElement(Peek, { file, diff: { root: '/work/workass', path: file.path, text: QUEUE_DIFF }, above: false, onExpand() {}, onCopy() {} }));
  assert.match(html, /^<div class="cb-peek" role="tooltip"><div class="cb-peek-hd"><span class="cb-peek-t">QueueList\.tsx · 2 cambios<\/span><span class="cb-peek-r">líneas 31–34<\/span><\/div>/);
  const lines = [...html.matchAll(/<div class="cb-ln( [\w-]+)?">/g)].map((m) => (m[1] ?? '').trim());
  assert.deepEqual(lines, ['', 'cb-del', 'cb-add', 'cb-add', ''], 'tight around the change: one line of context on each side');
  assert.match(html, /<div class="cb-ln cb-del">  \{!!queue\.length &amp;&amp; &lt;div className=&quot;qlabel&quot;&gt;<\/div>/);
  assert.match(html, /<button type="button" class="cb-link">Ver diff completo<\/button><button type="button" class="cb-link muted">Copiar ruta<\/button>/);
});

test('Ver diff completo shows the whole diff in a bounded scroll area in the pane', async () => {
  const html = renderToStaticMarkup(React.createElement(FullDiff, { diff: { root: '/work/workass', path: 'desktop/renderer2/src/components/QueueList.tsx', text: QUEUE_DIFF }, onClose() {} }));
  assert.match(html, /^<div class="cb-full"><div class="cb-full-hd"><span class="cb-full-t" title="desktop\/renderer2\/src\/components\/QueueList\.tsx">/);
  assert.match(html, /<div class="cb-full-body">/);
  assert.equal([...html.matchAll(/class="cb-ln cb-hunk"/g)].length, 2, 'both hunks');
  assert.match(html, /<div class="cb-ln cb-add">  added\(\);<\/div>/, 'the second hunk too');
  const { readFileSync } = await import('node:fs');
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css.match(/\.cb-full-body\s*\{([^}]*)\}/)?.[1] ?? '', /max-height:\s*260px;\s*overflow:\s*auto/);
  const struck = css.match(/\.cb-ln\.cb-del\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(struck, /line-through/);
  assert.match(struck, /var\(--del\)/);
  assert.match(css.match(/\.cb-ln\.cb-add\s*\{([^}]*)\}/)?.[1] ?? '', /var\(--add\)/);
  assert.match(css.match(/\.cb-br\.pulse\s*\{([^}]*)\}/)?.[1] ?? '', /animation/);
  assert.doesNotMatch(css.match(/\.cb-repo\s*\{([^}]*)\}/)?.[1] ?? '', /border-radius|background/, 'flat: no card');
});

test('a diff is fetched on demand and kept briefly; a new edit fetches again', async () => {
  const first = await changes.changesDiff('/work/workass', 'a.go', '1:0:0');
  const again = await changes.changesDiff('/work/workass', 'a.go', '1:0:0');
  assert.equal(calls.diff.length, 1);
  assert.equal(first?.text, again?.text);
  await changes.changesDiff('/work/workass', 'a.go', '2:0:0');
  assert.equal(calls.diff.length, 2, 'the counts changed: fetched again');
  Date.now = () => NOW + 60_000;
  await changes.changesDiff('/work/workass', 'a.go', '2:0:0');
  assert.equal(calls.diff.length, 3, 'kept only briefly');
});

test('the watch opens with the tab and closes with it; nothing is watched otherwise', async () => {
  assert.equal(calls.watch.length, 0);
  const release = changes.openChangesWatch();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(calls.watch.length, 1);
  assert.equal(changes.changesWatching(), true);
  release();
  assert.equal(changes.changesWatching(), false);
  assert.deepEqual(calls.unwatch, [{ id: 'w-1' }]);
  release();
  assert.equal(calls.unwatch.length, 1, 'released once');
});

test('the tab count is the changed files of the last snapshot', () => {
  const count = (cambios as unknown as { cambiosCount(state: ChangesState | null, chatId: string): number }).cambiosCount;
  assert.equal(count(snapshot(), 'chat-1'), 7);
  assert.equal(count(null, 'chat-1'), 0);
});

// 2026-11-05: a chat showed a repo only another chat works in, with a count
// stuck at the 300 files the list keeps.
const scopedSnapshot = (scope: string[] = ['chat-1']): ChangesState => ({
  seq: 9, windowDays: 7, scope, chats: [],
  repos: [
    { root: '/work/workass', name: 'workass', branch: 'main', adds: 0, dels: 0, files: [], checked: true, chatIds: ['chat-1', 'chat-2'] },
    { root: '/work/bigrepo', name: 'bigrepo', branch: 'port', adds: 9162, dels: 66338, fileCount: 353, filesTruncated: true, checked: true, chatIds: ['chat-9'],
      files: [{ path: 'a.py', adds: 1, dels: 2 }] },
    { root: '/work/tools', name: 'tools', branch: 'main', adds: 4, dels: 1, fileCount: 2, checked: true, chatIds: ['chat-1'],
      files: [{ path: 'x.go', adds: 3, dels: 1, chatId: 'chat-1', tabId: 'tab-1', at: NOW - MIN }, { path: 'y.go', adds: 1, dels: 0 }] },
  ],
});

test('a chat sees only the repos it works in or edited; another chat\'s repo is not drawn or counted', () => {
  changes.resetChangesStore(scopedSnapshot());
  const html = render();
  assert.doesNotMatch(html, /bigrepo/);
  assert.match(html, /<span class="cb-name" title="\/work\/tools">tools<\/span>/);
  assert.match(html, /<div class="cb-clean" title="workass">[\s\S]*?1 repo sin cambios<\/div>/);
  const count = (cambios as unknown as { cambiosCount(state: ChangesState | null, chatId: string): number }).cambiosCount;
  assert.equal(count(scopedSnapshot(), 'chat-1'), 2);
});

test('a snapshot built for another chat\'s tab draws nothing for this one, not «Sin cambios»', () => {
  changes.resetChangesStore(scopedSnapshot(['chat-9']));
  assert.equal(render(), '<div class="cb-list" aria-busy="true"></div>');
});

test('a repo past the list cap counts every changed file and says how many are listed', () => {
  const state = scopedSnapshot(['chat-9']);
  changes.resetChangesStore(state);
  cambios.setCambiosFold('/work/bigrepo', true);
  const html = render({ id: 'tab-9', chatId: 'chat-9', title: 'Port', messages: [] });
  assert.match(html, /<span class="a">\+9162<\/span><span class="d">−66338<\/span>/);
  assert.match(html, /<div class="cb-trunc">se muestran 1 de 353 archivos<\/div>/);
  const count = (cambios as unknown as { cambiosCount(state: ChangesState | null, chatId: string): number }).cambiosCount;
  assert.equal(count(state, 'chat-9'), 353);
});

test('the watch names the chat; moving to another chat moves the open watch', async () => {
  changes.setChangesChat('chat-1');
  const release = changes.openChangesWatch();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(calls.watch, [{ chatId: 'chat-1' }]);
  changes.setChangesChat('chat-2');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(calls.watch[1], { id: 'w-1', chatId: 'chat-2' });
  changes.setChangesChat('chat-2');
  assert.equal(calls.watch.length, 2, 'the same chat again asks nothing');
  release();
});

test('diffPeek numbers the new file\'s lines and counts hunks', () => {
  const peek = view.diffPeek(QUEUE_DIFF);
  assert.equal(peek.hunks, 2);
  assert.equal(peek.from, 31);
  assert.equal(peek.to, 34);
  assert.deepEqual(peek.lines.map((l) => l.kind).join(''), ' -++ ');
  // Two changes far apart in one hunk: unchanged lines between are left out,
  // and the peek stays about eight lines.
  const far = ['@@ -1,11 +1,13 @@', ' a', '+b', ' c', ' d', ' e', ' f', ' g', '-h', '-i', '+j', '+k', ' l', ' m'].join('\n');
  const tight = view.diffPeek(far);
  assert.deepEqual(tight.lines.map((l) => l.kind).join(''), ' +… --++', 'eight rows, the gap mark counted');
  assert.deepEqual([tight.from, tight.to], [1, 9]);
  const added = view.diffPeek('--- /dev/null\n+++ b/n.md\n@@ -0,0 +1,2 @@\n+one\n+two\n');
  assert.deepEqual([added.from, added.to, added.lines.map((l) => l.kind).join('')], [1, 2, '++']);
  assert.deepEqual(view.fileLabel('desktop/renderer2/src/components/QueueList.tsx'), { name: 'QueueList.tsx', folder: 'components' });
  assert.deepEqual(view.fileLabel('cmd/workass/x.go'), { name: 'x.go', folder: 'cmd/workass' });
  assert.deepEqual(view.fileLabel('README.md'), { name: 'README.md', folder: '' });
});
