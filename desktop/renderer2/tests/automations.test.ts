// Automatizaciones (owner 2026-11-09, mock docs/mocks/scheduled option E).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { createServer, type ViteDevServer } from 'vite';

async function vite(t: { after(fn: () => unknown): void }): Promise<ViteDevServer> {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent',
  });
  t.after(() => server.close());
  return server;
}

type Helpers = typeof import('../src/automations.ts');
const load = async (t: { after(fn: () => unknown): void }) => (await vite(t)).ssrLoadModule('/src/automations.ts') as Promise<Helpers>;

// Tue 10 Nov 2026, 18:33 local.
const now = new Date(2026, 10, 10, 18, 33, 20);

test('a cron finds its next minute in local time', async (t) => {
  const h = await load(t);
  assert.equal(h.nextCronFire('*/30 * * * *', now)?.getTime(), new Date(2026, 10, 10, 19, 0).getTime());
  assert.equal(h.nextCronFire('0 9 * * *', now)?.getTime(), new Date(2026, 10, 11, 9, 0).getTime());
  assert.equal(h.nextCronFire('0 9 * * 1-5', new Date(2026, 10, 14, 10, 0))?.getTime(), new Date(2026, 10, 16, 9, 0).getTime(), 'Saturday 10:00 → Monday 9:00');
  assert.equal(h.nextCronFire('15 14 12 11 *', now)?.getTime(), new Date(2026, 10, 12, 14, 15).getTime(), 'a one-shot wake');
  assert.equal(h.nextCronFire('0 9 * * 7', now)?.getDay(), 0, '7 is Sunday too');
  assert.equal(h.nextCronFire('not a cron', now), null);
  assert.equal(h.nextCronFire('61 * * * *', now), null);
});

test('a recurring cron reads as words, an odd one as itself', async (t) => {
  const h = await load(t);
  assert.equal(h.cronText('*/30 * * * *'), 'cada 30 min');
  assert.equal(h.cronText('0 * * * *'), 'cada hora');
  assert.equal(h.cronText('0 */2 * * *'), 'cada 2 h');
  assert.equal(h.cronText('5 9 * * *'), 'todos los días 09:05');
  assert.equal(h.cronText('0 9 * * 1-5'), 'días hábiles 09:00');
  assert.equal(h.cronText('0 9 * * 1'), 'los lunes 09:00');
  assert.equal(h.cronText('0 9 1,15 * *'), '0 9 1,15 * *');
});

const message = (over: Record<string, unknown>) => ({ kind: 'message', id: 'm1', tabId: 'tab-1', chatId: 'c1', text: 'Revisá el bench', ...over });
const row = (h: Helpers, item: Record<string, unknown>, machineId = '') => {
  const it = item as Parameters<Helpers['rowDue']>[0];
  return { item: it, machineId, machineName: machineId || 'Este Mac', chatKey: it.tabId, due: h.rowDue(it, now) };
};

test('each kind says when it reaches the chat', async (t) => {
  const h = await load(t);
  const at = row(h, message({ condition: 'at', at: new Date(2026, 10, 10, 19, 15).toISOString() }));
  assert.equal(h.whenText(at, now), 'hoy 19:15');
  assert.equal(h.untilText(at.due!, now), 'en 42 min');
  assert.equal(h.isSoon(at, now), true);
  const tomorrow = row(h, message({ condition: 'at', at: new Date(2026, 10, 11, 5, 30).toISOString(), late: false }));
  assert.equal(h.whenText(tomorrow, now), 'mañana 05:30');
  const friday = row(h, message({ condition: 'at', at: new Date(2026, 10, 14, 9, 0).toISOString() }));
  assert.equal(h.whenText(friday, now), 'sáb 14 nov 09:00');
  assert.equal(h.whenText(row(h, message({ condition: 'tcp', addr: '127.0.0.1:8788' })), now), 'cuando :8788 responda');
  assert.equal(h.whenText(row(h, message({ condition: 'tcp', addr: 'duklerver:22' })), now), 'cuando duklerver:22 responda');
  assert.equal(h.whenText(row(h, message({ condition: 'file', path: '/Users/x/repo/out/report.md' })), now), 'cuando exista out/report.md');
  assert.equal(h.whenText(row(h, message({ condition: 'pid', pid: 4242 })), now), 'cuando termine el proceso 4242');
  const cron = row(h, { kind: 'session-cron', id: 'k', tabId: 'tab-1', chatId: 'c1', text: 'CI', schedule: '*/30 * * * *', recurring: true });
  assert.equal(h.whenText(cron, now), 'cada 30 min');
  assert.equal(h.timeColumn(cron), 'cada 30 min');
  const wake = row(h, { kind: 'session-cron', id: 'w', tabId: 'tab-1', chatId: 'c1', text: '', schedule: '15 14 12 11 *', recurring: false });
  assert.equal(h.whenText(wake, now), 'jue 12 nov 14:15');
  assert.equal(h.rowGlyph(wake), 'clock');
  assert.equal(h.rowGlyph(cron), 'repeat');
});

test('the page groups by day, soonest first, conditions last', async (t) => {
  const h = await load(t);
  const rows = [
    row(h, message({ id: 'cond', condition: 'tcp', addr: ':8788' })),
    row(h, message({ id: 'tomorrow', condition: 'at', at: new Date(2026, 10, 11, 5, 30).toISOString() })),
    row(h, message({ id: 'later', condition: 'at', at: new Date(2026, 10, 10, 22, 0).toISOString() })),
    row(h, message({ id: 'soon', condition: 'at', at: new Date(2026, 10, 10, 19, 15).toISOString() })),
  ];
  const groups = h.groupByDay(rows, now);
  assert.deepEqual(groups.map((g) => [g.label, g.rows.map((r) => r.item.id)]), [
    ['Hoy', ['soon', 'later']],
    ['Mañana', ['tomorrow']],
    [h.WHEN_SOMETHING, ['cond']],
  ]);
});

test('the page and the section are wired where the owner chose', () => {
  const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
  const tareas = readFileSync(new URL('../src/components/TareasCard.tsx', import.meta.url), 'utf8');
  const app = readFileSync(new URL('../src/components/App.tsx', import.meta.url), 'utf8');
  assert.match(sidebar, /<AutomationsFootButton \/>/, 'a button at the foot of the left panel');
  assert.match(tareas, /<ProgramadoSection chat=\{chat\} \/>/, 'Programado inside Trabajos');
  assert.match(app, /<AutomationsPage \/>/);
});
