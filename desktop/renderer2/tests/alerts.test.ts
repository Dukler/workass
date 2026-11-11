// Conectores + alertas (owner 2026-11-10, docs/specs/connectors-alerts.md,
// mocks desktop/docs/mocks/connectors-alerts/mock.html §1 and popover-v2.html
// §0 + variant A).
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

type Model = typeof import('../src/alerts-model.ts');
type Alert = import('../src/wire/types.ts').Alert;
type Connector = import('../src/wire/types.ts').Connector;
const load = async (t: { after(fn: () => unknown): void }) => (await vite(t)).ssrLoadModule('/src/alerts-model.ts') as Promise<Model>;

// Tue 10 Nov 2026, 21:20 local.
const now = new Date(2026, 10, 10, 21, 20, 0);
const at = (h: number, m: number, s = 0, day = 10) => new Date(2026, 10, day, h, m, s).toISOString();

const alert = (over: Partial<Alert>): Alert => ({
  id: 'gf-1:a', connectorId: 'gf-1', ruleKey: 'uid-a', ruleTitle: 'DepthCut camera moved: calibration stale',
  severity: 'critical', machine: 'arcade-gold', state: 'firing', startsAt: at(21, 14, 7), ...over,
});
const connector: Connector = {
  id: 'gf-1', kind: 'grafana', name: 'fiumba', url: 'http://127.0.0.1:13000', mode: 'poll',
  target: { machineId: '', cwd: '/Users/me/Workspace/lagspace' },
  ruleTargets: { 'uid-disk': { machineId: 'm-fin', cwd: '/home/me/fiumba-net' } },
  guidePattern: 'ops/runbooks/<regla>.md',
};
const entry = (a: Alert, machineKey = '') => ({ machineKey, machineName: machineKey || 'Mac', alert: a, connector });

test('the bell: nothing when none fire, a dot in the worst undecided colour, a ring when all are decided', async (t) => {
  const h = await load(t);
  assert.deepEqual(h.bellState([], now), { kind: 'none' });
  assert.deepEqual(h.bellState([alert({ state: 'resolved' })], now), { kind: 'none' });
  assert.deepEqual(h.bellState([alert({ dismissedAt: at(21, 15) })], now), { kind: 'none' }, 'a dismissed alert is out of sight');
  assert.deepEqual(h.bellState([alert({ severity: 'warning' }), alert({ id: 'b', severity: 'critical' })], now), { kind: 'dot', severity: 'critical' });
  // The critical one has a chat: the dot takes the colour of what is still undecided.
  const link = { machineId: 'mac', tabId: 'tab-1', chatId: 'c1' };
  assert.deepEqual(h.bellState([alert({ severity: 'warning' }), alert({ id: 'b', link })], now), { kind: 'dot', severity: 'warning' });
  assert.deepEqual(h.bellState([alert({ link }), alert({ id: 'b', silencedUntil: at(21, 30) })], now), { kind: 'ring' });
  // A silence that ran out is undecided again.
  assert.deepEqual(h.bellState([alert({ silencedUntil: at(21, 0) })], now), { kind: 'dot', severity: 'critical' });
  assert.deepEqual(h.bellState([alert({ severity: 'info' })], now), { kind: 'dot', severity: 'info' });
});

test('the bell swings once per new episode: never for the first list, a repeat or a replayed event', async (t) => {
  const h = await load(t);
  const ledger = new h.AnnounceLedger();
  const old = alert({ id: 'gf-1:old' });
  ledger.prime('', [old]);
  assert.deepEqual(ledger.take('', [old], ['gf-1:old']), [], 'what the first list showed is old news');
  const fresh = alert({ id: 'gf-1:new', startsAt: at(21, 19) });
  assert.equal(ledger.take('', [fresh], ['gf-1:new']).length, 1);
  assert.equal(ledger.take('', [fresh], ['gf-1:new']).length, 0, 'the same episode again');
  assert.equal(ledger.take('', [{ ...fresh, repeats: 3 }], []).length, 0, 'a Grafana repeat is not announced');
  // Resolved, then firing again: a new episode, announced.
  assert.equal(ledger.take('', [{ ...fresh, startsAt: at(21, 25) }], ['gf-1:new']).length, 1);
  // The same episode on another daemon (its connector moved there) is old news.
  assert.equal(ledger.take('m-fin', [{ ...fresh, startsAt: at(21, 25) }], ['gf-1:new']).length, 0);
  assert.equal(ledger.take('', [alert({ id: 'gone', state: 'resolved' })], ['gone']).length, 0);
});

test('a system notification follows the connector: never, critical only, or all', async (t) => {
  const h = await load(t);
  assert.equal(h.notifyAllows('never', 'critical'), false);
  assert.equal(h.notifyAllows('critical', 'critical'), true);
  assert.equal(h.notifyAllows('critical', 'warning'), false);
  assert.equal(h.notifyAllows(undefined, 'warning'), false, 'default is critical');
  assert.equal(h.notifyAllows('all', 'info'), true);
});

test('the Parte reads one situation per connector and machine, the undecided and worst first', async (t) => {
  const h = await load(t);
  const entries = [
    entry(alert({ id: 'gf-1:disk', ruleKey: 'uid-disk', ruleTitle: 'Disk space low', severity: 'warning', machine: 'duklerfin', startsAt: at(19, 40), link: { machineId: 'mac', tabId: 'tab-9', chatId: 'c9' } })),
    entry(alert({ id: 'gf-1:bridge', ruleKey: 'uid-b', ruleTitle: 'DepthCut camera moved: running on re-aim bridge', severity: 'warning', startsAt: at(21, 15) })),
    entry(alert({})),
    entry(alert({ id: 'gf-1:silver', machine: 'arcade-silver', state: 'resolved', startsAt: at(14, 2), endsAt: at(14, 9) })),
    entry(alert({ id: 'gf-1:quiet', machine: 'arcade-blue', silencedUntil: at(22, 0) })),
    entry(alert({ id: 'gf-1:old', machine: 'arcade-silver', state: 'resolved', startsAt: at(9, 0, 0, 8), endsAt: at(9, 30, 0, 8) })),
  ];
  const situations = h.groupSituations(entries, now);
  assert.deepEqual(situations.map((s) => s.title), ['arcade-gold', 'duklerfin']);
  const gold = situations[0];
  assert.equal(gold.severity, 'critical');
  assert.deepEqual(gold.alerts.map((e) => e.alert.id), ['gf-1:a', 'gf-1:bridge'], 'oldest first');
  assert.equal(h.agoText(gold.since, now), 'hace 5 min');
  assert.equal(h.agoText(new Date(2026, 10, 10, 19, 40), now), 'hace 1 h 40 min');
  const words = (segs: ReturnType<Model['firingSegments']>) => segs.map((s) => (typeof s === 'string' ? s : 'rule' in s ? s.rule : s.muted)).join('');
  assert.equal(words(h.firingSegments(gold.alerts.map((e) => e.alert), now)),
    'Dispara DepthCut camera moved: calibration stale desde las 21:14 y, desde las 21:15, DepthCut camera moved: running on re-aim bridge (aviso).');
  assert.equal(words(h.ownerSegments(gold.alerts.map((e) => e.alert), () => null, now)), 'Nadie se ocupa.');
  const disk = situations[1].alerts.map((e) => e.alert);
  const chat = { key: 'tab-9', title: 'Disco de duklerfin', working: new Date(2026, 10, 10, 21, 8) };
  assert.equal(words(h.ownerSegments(disk, () => chat, now)), 'La lleva el chat Disco de duklerfin, que trabaja hace 12 min.');
  // Same severity: no "(aviso)" note; another day says which.
  assert.equal(words(h.firingSegments([alert({ startsAt: at(23, 0, 0, 9) })], now)), 'Dispara DepthCut camera moved: calibration stale desde ayer a las 23:00.');

  assert.deepEqual(h.resolvedEntries(entries, now, 'today').map((e) => e.alert.id), ['gf-1:silver']);
  assert.deepEqual(h.resolvedEntries(entries, now, 'before').map((e) => e.alert.id), ['gf-1:old']);
  assert.deepEqual(h.setAside(entries, now).map((e) => e.alert.id), ['gf-1:quiet']);
  const past = h.pastSegments(h.resolvedEntries(entries, now, 'today')[0], now, () => null);
  assert.equal(past.when, '14:02–14:09');
  assert.equal(words(past.text), 'arcade-silver disparó DepthCut camera moved: calibration stale y se resolvió sola en 7 min. Nadie la tocó.');
  assert.equal(h.repeatText([alert({ repeats: 2, lastRepeatAt: at(21, 19) }), alert({ id: 'b', repeats: 1, lastRepeatAt: at(21, 18) })]),
    'Grafana repitió las dos a las 21:19; no se volvió a avisar.');
  assert.equal(h.repeatText([alert({})]), '');
});

test('the hand-off message: rules, machine, since when, annotations, values, the Grafana link and the guide', async (t) => {
  const h = await load(t);
  const alerts = [
    alert({
      labels: { alertname: 'x', severity: 'critical', machine: 'arcade-gold', alert_family: 'camera_move' },
      annotations: { summary: 'IMU tilt 0.61°' }, values: { A: 0.61 }, generatorURL: 'http://127.0.0.1:13000/alerting/grafana/uid-a/view',
    }),
    alert({ id: 'gf-1:bridge', ruleKey: 'uid-b', ruleTitle: 'DepthCut camera moved: running on re-aim bridge', severity: 'warning', startsAt: at(21, 15) }),
  ];
  const handoff = h.buildHandoff(alerts, connector, now);
  assert.equal(handoff.say,
    'arcade-gold dispara «DepthCut camera moved: calibration stale» desde las 21:14 y, desde las 21:15, «DepthCut camera moved: running on re-aim bridge». Mirá qué pasó.');
  assert.match(handoff.registro, /^Alertas de Grafana · fiumba, máquina arcade-gold:/);
  assert.match(handoff.registro, /- «DepthCut camera moved: calibration stale» \(crítica\), dispara desde 2026-11-10 21:14:07/);
  assert.match(handoff.registro, /\n {2}summary: IMU tilt 0\.61°/);
  assert.match(handoff.registro, /\n {2}valores: A=0\.61/);
  assert.match(handoff.registro, /\n {2}etiquetas: machine=arcade-gold alert_family=camera_move/);
  assert.match(handoff.registro, /\n {2}Grafana: http:\/\/127\.0\.0\.1:13000\/alerting\/grafana\/uid-a\/view/);
  assert.deepEqual(handoff.guides, ['ops/runbooks/camera_move.md', 'ops/runbooks/depthcut-camera-moved-running-on-re-aim-bridge.md']);
  const message = h.composeHandoff('Mirá esto.', handoff);
  assert.ok(message.startsWith('Mirá esto.\n\nAlertas de Grafana'), 'the edited sentence leads');
  assert.match(message, /\n\nGuía: ops\/runbooks\/camera_move\.md\nGuía: ops\/runbooks\/depthcut/);
  assert.match(h.attachmentsText(handoff), /^Van adjuntos el registro \(\d+ líneas\), ops\/runbooks\/camera_move\.md y ops\/runbooks\/depthcut/);
  // No machine label: the connector names it; no guide pattern: no guide.
  const bare = h.buildHandoff([alert({ machine: '' })], { ...connector, guidePattern: '' }, now);
  assert.match(bare.say, /^Grafana · fiumba dispara «/);
  assert.deepEqual(bare.guides, []);
  assert.equal(h.composeHandoff(bare.say, bare).includes('Guía:'), false);
  assert.match(h.attachmentsText(bare), /^Van adjunto el registro \(\d+ líneas\)\.$/);
});

test('the guide: alert_family, else the rule title slugged; empty pattern is none', async (t) => {
  const h = await load(t);
  assert.equal(h.guideRegla({ ruleTitle: 'Disk space low', labels: { alert_family: 'disk' } }), 'disk');
  assert.equal(h.guideRegla({ ruleTitle: 'Cámara movida: calibración vieja!' }), 'camara-movida-calibracion-vieja');
  assert.equal(h.resolveGuide('ops/runbooks/<regla>.md', { ruleTitle: 'Disk space low' }), 'ops/runbooks/disk-space-low.md');
  assert.equal(h.resolveGuide('docs/oncall.md', { ruleTitle: 'Disk space low' }), 'docs/oncall.md', 'a pattern without <regla> is one file');
  assert.equal(h.resolveGuide('', { ruleTitle: 'x' }), '');
  assert.equal(h.resolveGuide('ops/<regla>.md', { ruleTitle: '¡¡!!' }), '', 'nothing to put in <regla>');
});

test('where the chat opens: the rule\'s own place over the connector\'s', async (t) => {
  const h = await load(t);
  assert.deepEqual(h.resolveTarget(connector, ['uid-a']), { machineId: '', cwd: '/Users/me/Workspace/lagspace' });
  assert.deepEqual(h.resolveTarget(connector, ['uid-a', 'uid-disk']), { machineId: 'm-fin', cwd: '/home/me/fiumba-net' });
  assert.deepEqual(h.resolveTarget(undefined, ['uid-a']), { machineId: '', cwd: '' });
  assert.equal(h.clientMachine('', 'mac-id'), '', '"" is the client\'s own machine');
  assert.equal(h.clientMachine('mac-id', 'mac-id'), '');
  assert.equal(h.clientMachine('m-fin', 'mac-id'), 'm-fin');
  assert.equal(h.shortFolder('/Users/me/Workspace/lagspace'), '~/Workspace/lagspace');
  assert.equal(h.shortFolder('/home/me'), '~');
  assert.equal(h.shortFolder('/srv/x'), '/srv/x');
});

// Owner 2026-11-10: "Máquina que escucha" changes with one click. Two fake
// daemons: the old one hands the connector over, the new one adopts it with
// the address as it reaches Grafana, then the old one lets go.
function daemons(fail: '' | 'handover' | 'adopt' | 'remove' = '') {
  const calls: string[] = [];
  const held = { mac: [{ ...connector }], fin: [] as Connector[] };
  const linked = alert({ link: { machineId: 'mac', tabId: 'tab-1', chatId: 'c1' }, silencedUntil: at(22, 0) });
  const ledger = { primed: [] as string[] };
  const d = {
    async handover(key: string, args: { id: string }) {
      calls.push(`handover ${key} ${args.id}`);
      if (fail === 'handover') throw new Error('refused');
      return { connector: held.mac[0], token: 'glsa_secret', webhookSecretHash: 'h', alerts: [linked] };
    },
    async adopt(key: string, args: import('../src/wire/types.ts').ConnectorAdoptArgs) {
      calls.push(`adopt ${key} ${args.url} token=${args.token} alerts=${args.alerts.length}`);
      assert.deepEqual(args.alerts[0].link, linked.link, 'links travel');
      assert.equal(args.alerts[0].silencedUntil, linked.silencedUntil, 'silences travel');
      if (fail === 'adopt') throw new Error('unreachable');
      const moved = { ...args.connector, url: args.url };
      held.fin.push(moved);
      return { connector: moved };
    },
    async remove(key: string, args: { id: string }) {
      calls.push(`remove ${key} ${args.id}`);
      if (fail === 'remove') throw new Error('busy');
      held.mac = held.mac.filter((c) => c.id !== args.id);
      return { ok: true };
    },
    arrived(key: string, alerts: Alert[]) { ledger.primed.push(`${key}:${alerts.map((a) => a.id).join(',')}`); },
  };
  return { d, calls, held, ledger };
}

test('moving a connector: handover on the old daemon, adopt on the new one with its own address, remove on the old', async (t) => {
  const h = await load(t);
  const ok = daemons();
  const result = await h.moveConnector(ok.d, 'mac', 'fin', 'gf-1', ' http://100.64.0.2/grafana ');
  assert.equal(result.ok, true);
  assert.deepEqual(ok.calls, ['handover mac gf-1', 'adopt fin http://100.64.0.2/grafana token=glsa_secret alerts=1', 'remove mac gf-1']);
  assert.deepEqual(ok.held.mac, []);
  assert.equal(ok.held.fin[0].url, 'http://100.64.0.2/grafana');
  assert.deepEqual(ok.ledger.primed, ['fin:gf-1:a'], 'what arrives is primed before any event, so nothing swings');
  // The token lives only in the flow: not in the result.
  assert.doesNotMatch(JSON.stringify(result), /glsa_secret/);

  // An empty address keeps the old one.
  const same = daemons();
  await h.moveConnector(same.d, 'mac', 'fin', 'gf-1', '');
  assert.match(same.calls[1], /^adopt fin http:\/\/127\.0\.0\.1:13000 /);

  // A refusal before the remove leaves the old one listening, untouched.
  for (const stage of ['handover', 'adopt'] as const) {
    const bad = daemons(stage);
    const r = await h.moveConnector(bad.d, 'mac', 'fin', 'gf-1', 'http://x');
    assert.equal(r.ok, false);
    assert.equal(!r.ok && r.stage, stage);
    assert.equal(bad.held.mac.length, 1);
    assert.equal(bad.calls.some((c) => c.startsWith('remove')), false);
    assert.match(h.moveFailureText(r as Exclude<typeof r, { ok: true }>, 'Mac', 'duklerfin'), /Sigue escuchando Mac\.$/);
  }
  // The old one would not let go: both listen, and the owner is told.
  const stuck = daemons('remove');
  const r = await h.moveConnector(stuck.d, 'mac', 'fin', 'gf-1', 'http://x');
  assert.equal(!r.ok && r.stage, 'remove');
  assert.equal(stuck.held.mac.length, 1);
  assert.equal(stuck.held.fin.length, 1);
  assert.match(h.moveFailureText(r as Exclude<typeof r, { ok: true }>, 'Mac', 'duklerfin'), /Quedó en las dos; quitá el de Mac\.$/);
});

// The surfaces, by their source: the bell is the fifth foot icon, the page
// sits under "Tus máquinas" after Trabajos, and a hand-off goes through the
// app's own new-chat path and then links the alert on its daemon.
const read = (path: string) => readFileSync(new URL(path, import.meta.url), 'utf8');

test('Conectores sits after Trabajos; the bell swings without asking prefers-reduced-motion', () => {
  const settings = read('../src/components/Settings.tsx');
  assert.match(settings, /\{ label: 'Tus máquinas', items: \['maquinas', 'trabajos', 'conectores'\] \}/);
  const css = read('../src/styles/app.css');
  assert.match(css, /\.footbtn\.bell\.swing svg \{ animation: bell-swing \.9s ease-in-out 1;/);
  const block = css.slice(css.indexOf('Alertas: the bell and its Parte'), css.indexOf('Ajustes · Conectores ============'));
  assert.doesNotMatch(block, /@media \(prefers-reduced-motion/);
  assert.match(css, /\.footdot\.ring \{ background: var\(--side\); box-shadow: inset 0 0 0 1\.5px var\(--muted\), 0 0 0 2px var\(--side\); \}/);
});

test('a hand-off opens the chat through store.newChat, then links the alert; nothing else opens one', () => {
  const ui = read('../src/components/Alerts.tsx');
  const hand = ui.slice(ui.indexOf('const go = async'), ui.indexOf('const token ='));
  assert.match(hand, /store\.newChat\(true, cwd\.trim\(\), machine\)/);
  assert.match(hand, /if \(send\) \{\s*const sent = await store\.sendTo\(chat\.id, message\);/);
  assert.match(hand, /store\.setDraft\(chat\.id, message\);/);
  assert.match(hand, /action: 'link', link: \{ machineId: machine \|\| store\.localMachineId\(\), tabId: tab\.id, chatId: conv\.id \}/);
  // Law 1: the live module never creates, writes to or sends to a chat.
  const live = read('../src/alerts.ts');
  assert.doesNotMatch(live, /newChat|sendTo|setDraft|steerRunning/);
  assert.doesNotMatch(ui.replace(hand, ''), /newChat\(|sendTo\(|setDraft\(/);
});
