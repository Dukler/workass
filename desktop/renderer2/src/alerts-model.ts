// Conectores + alertas (owner 2026-11-10, docs/specs/connectors-alerts.md):
// the pure part. Which alerts are undecided, what the bell shows, how the
// Parte groups them into situations and words them, when the bell swings, and
// the hand-off message the owner reads and edits before a chat opens.
//
// Owner law 1: nothing here opens a chat, writes into one or wakes an agent.

import type {
  Alert, AlertSeverity, Connector, ConnectorAdoptArgs, ConnectorHandover, ConnectorNotify, ConnectorTarget,
} from './wire/types';

/** An alert as the client holds it: with the daemon it came from. */
export interface AlertEntry {
  /** The daemon that listens: '' for this machine, else its machine id. */
  machineKey: string;
  machineName: string;
  alert: Alert;
  connector?: Connector;
}

// ---- severity ---------------------------------------------------------------

const SEVERITY_RANK: Record<AlertSeverity, number> = { critical: 3, warning: 2, info: 1 };
export const SEVERITY_WORD: Record<AlertSeverity, string> = { critical: 'crítica', warning: 'aviso', info: 'info' };

export function severityOf(alert: Pick<Alert, 'severity'>): AlertSeverity {
  return alert.severity === 'critical' || alert.severity === 'info' ? alert.severity : 'warning';
}

export function worstSeverity(alerts: ReadonlyArray<Pick<Alert, 'severity'>>): AlertSeverity | null {
  let worst: AlertSeverity | null = null;
  for (const alert of alerts) {
    const sev = severityOf(alert);
    if (!worst || SEVERITY_RANK[sev] > SEVERITY_RANK[worst]) worst = sev;
  }
  return worst;
}

// ---- decided / undecided ----------------------------------------------------

const time = (iso: string | undefined): number => {
  const t = iso ? Date.parse(iso) : NaN;
  return Number.isFinite(t) ? t : NaN;
};

export function isSilenced(alert: Alert, now: Date): boolean {
  const until = time(alert.silencedUntil);
  return Number.isFinite(until) && until > now.getTime();
}

/** Firing and not set aside by the owner (a dismissed alert is out of sight). */
export function isFiring(alert: Alert): boolean {
  return alert.state === 'firing' && !alert.dismissedAt;
}

/** Firing with nobody on it: no chat, not silenced, not dismissed. */
export function isUndecided(alert: Alert, now: Date): boolean {
  return isFiring(alert) && !alert.link && !isSilenced(alert, now);
}

export type BellState = { kind: 'none' } | { kind: 'ring' } | { kind: 'dot'; severity: AlertSeverity };

/** The bell (popover-v2 section 0): nothing when none fire; a filled dot in
 *  the worst undecided severity's colour; a hollow ring when every firing
 *  alert has a chat or is silenced. */
export function bellState(alerts: ReadonlyArray<Alert>, now: Date): BellState {
  const firing = alerts.filter(isFiring);
  if (firing.length === 0) return { kind: 'none' };
  const worst = worstSeverity(firing.filter((alert) => isUndecided(alert, now)));
  return worst ? { kind: 'dot', severity: worst } : { kind: 'ring' };
}

// ---- announcements ------------------------------------------------------------

/** Remembers each firing episode seen, so the bell swings (and the system
 *  notifies) once per new episode and never for a Grafana repeat, a replayed
 *  event, the first list after a reload, or alerts that arrive on another
 *  machine because their connector moved there. An alert id carries its
 *  connector's id ("gf-" + hex), so id + startsAt names one episode wherever
 *  the connector listens; the machine is not part of the key. */
export class AnnounceLedger {
  private seen = new Set<string>();

  private key(_machineKey: string, alert: Pick<Alert, 'id' | 'startsAt'>): string {
    return `${alert.id}\u0000${alert.startsAt ?? ''}`;
  }

  /** What a list already shows is old news. */
  prime(machineKey: string, alerts: ReadonlyArray<Alert>): void {
    for (const alert of alerts) if (alert.state === 'firing') this.seen.add(this.key(machineKey, alert));
  }

  /** The alerts of `announce` that begin an episode this client has not seen. */
  take(machineKey: string, changed: ReadonlyArray<Alert>, announce: ReadonlyArray<string> | undefined): Alert[] {
    const out: Alert[] = [];
    for (const id of announce ?? []) {
      const alert = changed.find((candidate) => candidate.id === id);
      if (!alert || alert.state !== 'firing') continue;
      const key = this.key(machineKey, alert);
      if (this.seen.has(key)) continue;
      this.seen.add(key);
      out.push(alert);
    }
    return out;
  }
}

/** Whether a connector's "Notificación del sistema" lets this severity through. */
export function notifyAllows(notify: ConnectorNotify | undefined, severity: AlertSeverity): boolean {
  const mode = notify ?? 'critical';
  if (mode === 'never') return false;
  if (mode === 'critical') return severity === 'critical';
  return true;
}

// ---- time words ---------------------------------------------------------------

const MONTH = ['ene', 'feb', 'mar', 'abr', 'may', 'jun', 'jul', 'ago', 'sep', 'oct', 'nov', 'dic'];
const pad = (n: number) => String(n).padStart(2, '0');
export const clock = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
export const clockSeconds = (d: Date) => `${clock(d)}:${pad(d.getSeconds())}`;

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}
export function sameDay(a: Date, b: Date): boolean {
  return startOfDay(a) === startOfDay(b);
}

/** "las 21:14", "ayer a las 21:14", "el 8 nov a las 21:14". */
export function atText(d: Date, now: Date): string {
  const days = Math.round((startOfDay(now) - startOfDay(d)) / 86_400_000);
  if (days === 0) return `las ${clock(d)}`;
  if (days === 1) return `ayer a las ${clock(d)}`;
  return `el ${d.getDate()} ${MONTH[d.getMonth()]} a las ${clock(d)}`;
}

/** "hace un momento", "hace 6 min", "hace 1 h 40 min", "hace 3 d". */
export function agoText(from: Date, now: Date): string {
  const minutes = Math.max(0, Math.floor((now.getTime() - from.getTime()) / 60_000));
  if (minutes < 1) return 'hace un momento';
  if (minutes < 60) return `hace ${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return minutes % 60 ? `hace ${hours} h ${minutes % 60} min` : `hace ${hours} h`;
  return `hace ${Math.floor(hours / 24)} d`;
}

/** "7 min", "1 h 5 min". */
export function durationText(ms: number): string {
  const minutes = Math.max(1, Math.round(ms / 60_000));
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  return minutes % 60 ? `${hours} h ${minutes % 60} min` : `${hours} h`;
}

const dateOf = (iso: string | undefined): Date | null => {
  const t = time(iso);
  return Number.isFinite(t) ? new Date(t) : null;
};

// ---- situations ------------------------------------------------------------------

/** One paragraph of the Parte: the alerts of one connector on one machine. */
export interface Situation {
  key: string;
  machineKey: string;
  machineName: string;
  connector?: Connector;
  /** The machine label Grafana gave, else the connector's name. */
  title: string;
  severity: AlertSeverity;
  /** Firing, not silenced, not dismissed; oldest first. */
  alerts: AlertEntry[];
  since: Date;
}

export function situationTitle(entry: AlertEntry): string {
  return entry.alert.machine || (entry.connector ? `Grafana · ${entry.connector.name}` : 'Grafana');
}

/** Every firing alert the owner has not silenced or dismissed, one situation
 *  per (connector, machine); the worst and oldest first. */
export function groupSituations(entries: ReadonlyArray<AlertEntry>, now: Date): Situation[] {
  const groups = new Map<string, Situation>();
  for (const entry of entries) {
    const { alert } = entry;
    if (!isFiring(alert) || isSilenced(alert, now)) continue;
    const key = `${entry.machineKey}\u0000${alert.connectorId}\u0000${alert.machine}`;
    let group = groups.get(key);
    if (!group) {
      group = {
        key, machineKey: entry.machineKey, machineName: entry.machineName, connector: entry.connector,
        title: situationTitle(entry), severity: severityOf(alert), alerts: [], since: new Date(time(alert.startsAt) || now.getTime()),
      };
      groups.set(key, group);
    }
    group.alerts.push(entry);
  }
  const out = [...groups.values()];
  for (const group of out) {
    group.alerts.sort((a, b) => (time(a.alert.startsAt) || 0) - (time(b.alert.startsAt) || 0));
    group.severity = worstSeverity(group.alerts.map((e) => e.alert)) ?? 'warning';
    const first = dateOf(group.alerts[0]?.alert.startsAt);
    if (first) group.since = first;
  }
  // Undecided before handled, then worst, then oldest.
  const open = (g: Situation) => (g.alerts.some((e) => isUndecided(e.alert, now)) ? 1 : 0);
  return out.sort((a, b) => open(b) - open(a)
    || SEVERITY_RANK[b.severity] - SEVERITY_RANK[a.severity]
    || a.since.getTime() - b.since.getTime());
}

/** Silenced or dismissed while still firing ("Silenciadas"). */
export function setAside(entries: ReadonlyArray<AlertEntry>, now: Date): AlertEntry[] {
  return entries.filter((e) => e.alert.state === 'firing' && (!!e.alert.dismissedAt || isSilenced(e.alert, now)));
}

/** Resolved today ("Hoy"), the latest first; `before` gives the earlier days. */
export function resolvedEntries(entries: ReadonlyArray<AlertEntry>, now: Date, which: 'today' | 'before'): AlertEntry[] {
  return entries
    .filter((e) => e.alert.state === 'resolved')
    .filter((e) => {
      const end = dateOf(e.alert.endsAt) ?? dateOf(e.alert.lastSeenAt) ?? dateOf(e.alert.startsAt);
      const today = !!end && sameDay(end, now);
      return which === 'today' ? today : !today;
    })
    .sort((a, b) => (time(b.alert.endsAt) || 0) - (time(a.alert.endsAt) || 0));
}

// ---- words -------------------------------------------------------------------------

/** A run of text; `rule` is drawn as a rule name, `muted` quieter. */
export type Seg = string | { rule: string } | { muted: string };

/** "Dispara A desde las 21:14 y, desde las 21:15, B (aviso)." */
export function firingSegments(alerts: ReadonlyArray<Alert>, now: Date): Seg[] {
  const sorted = [...alerts].sort((a, b) => (time(a.startsAt) || 0) - (time(b.startsAt) || 0));
  const worst = worstSeverity(sorted);
  const segs: Seg[] = [];
  sorted.forEach((alert, i) => {
    const at = dateOf(alert.startsAt);
    const since = at ? `desde ${atText(at, now)}` : '';
    const note: Seg[] = severityOf(alert) !== worst ? [' ', { muted: `(${SEVERITY_WORD[severityOf(alert)]})` }] : [];
    if (i === 0) {
      segs.push('Dispara ', { rule: alert.ruleTitle }, since ? ` ${since}` : '', ...note);
    } else {
      segs.push(i === sorted.length - 1 ? ' y' : ',', since ? `, ${since}, ` : ' ', { rule: alert.ruleTitle }, ...note);
    }
  });
  segs.push('.');
  return segs.filter((s) => s !== '');
}

export interface ChatInfo { key: string; title: string; working: Date | null }

/** Who takes care of it: "Nadie se ocupa." / "La lleva el chat X, que trabaja hace 12 min." */
export function ownerSegments(alerts: ReadonlyArray<Alert>, chatOf: (alert: Alert) => ChatInfo | null, now: Date): Seg[] {
  const byChat = new Map<string, { chat: ChatInfo; alerts: Alert[] }>();
  const nobody: Alert[] = [];
  for (const alert of alerts) {
    const chat = alert.link ? chatOf(alert) : null;
    if (!alert.link) { nobody.push(alert); continue; }
    const info = chat ?? { key: `${alert.link.machineId}|${alert.link.tabId}`, title: 'un chat cerrado', working: null };
    const held = byChat.get(info.key);
    if (held) held.alerts.push(alert);
    else byChat.set(info.key, { chat: info, alerts: [alert] });
  }
  if (byChat.size === 0) return ['Nadie se ocupa.'];
  const working = (chat: ChatInfo): Seg[] => (chat.working ? [`, que trabaja ${agoText(chat.working, now)}`] : []);
  if (nobody.length === 0 && byChat.size === 1) {
    const { chat } = [...byChat.values()][0];
    return ['La lleva el chat ', { rule: chat.title }, ...working(chat), '.'];
  }
  const segs: Seg[] = [];
  for (const { chat, alerts: held } of byChat.values()) {
    if (segs.length) segs.push(' ');
    held.forEach((alert, i) => { segs.push(i ? (i === held.length - 1 ? ' y ' : ', ') : '', { rule: alert.ruleTitle }); });
    segs.push(held.length === 1 ? ' la lleva el chat ' : ' las lleva el chat ', { rule: chat.title }, ...working(chat), '.');
  }
  if (nobody.length) segs.push(' Del resto no se ocupa nadie.');
  return segs.filter((s) => s !== '');
}

/** A past episode: "14:02–14:09 · arcade-silver disparó X y se resolvió sola en 7 min. Nadie la tocó." */
export function pastSegments(entry: AlertEntry, now: Date, chatOf: (alert: Alert) => ChatInfo | null): { when: string; text: Seg[] } {
  const { alert } = entry;
  const start = dateOf(alert.startsAt);
  const end = dateOf(alert.endsAt) ?? dateOf(alert.lastSeenAt);
  const day = start && !sameDay(start, now) ? `${start.getDate()} ${MONTH[start.getMonth()]} ` : '';
  const when = start ? `${day}${clock(start)}${end ? `–${clock(end)}` : ''}` : '';
  const took = start && end ? ` en ${durationText(end.getTime() - start.getTime())}` : '';
  const chat = alert.link ? chatOf(alert) : null;
  const text: Seg[] = [`${situationTitle(entry)} disparó ${alert.ruleTitle} y se resolvió${alert.link ? '' : ' sola'}${took}.`];
  if (alert.link) text.push(' La llevó el chat ', { rule: chat?.title ?? 'un chat cerrado' }, '.');
  else text.push(' Nadie la tocó.');
  return { when, text };
}

/** "Grafana repitió las dos a las 21:19; no se volvió a avisar." or ''. */
export function repeatText(alerts: ReadonlyArray<Alert>): string {
  const repeated = alerts.filter((a) => (a.repeats ?? 0) > 0);
  if (repeated.length === 0) return '';
  const last = repeated.map((a) => time(a.lastRepeatAt)).filter(Number.isFinite).sort((a, b) => b - a)[0];
  const which = repeated.length === 1 ? 'la' : repeated.length === 2 ? 'las dos' : `las ${repeated.length}`;
  return `Grafana ${which === 'la' ? 'la repitió' : `repitió ${which}`}${Number.isFinite(last) ? ` a las ${clock(new Date(last))}` : ''}; no se volvió a avisar.`;
}

// ---- registro ------------------------------------------------------------------------

export interface LogLine { at: string; text: string; quiet?: boolean }

const LABELS_SAID_ELSEWHERE = new Set(['alertname', 'severity', '__alert_rule_uid__', 'grafana_folder']);

function valuesText(values: Record<string, number> | undefined): string {
  const keys = Object.keys(values ?? {});
  return keys.map((k) => `${k}=${values![k]}`).join(' · ');
}
function labelsText(labels: Record<string, string> | undefined): string {
  return Object.entries(labels ?? {}).filter(([k]) => !LABELS_SAID_ELSEWHERE.has(k)).map(([k, v]) => `${k}=${v}`).join(' ');
}

/** The Registro box: each alert's start, its annotations, values and labels. */
export function registroLines(alerts: ReadonlyArray<Alert>): LogLine[] {
  const lines: LogLine[] = [];
  const sorted = [...alerts].sort((a, b) => (time(a.startsAt) || 0) - (time(b.startsAt) || 0));
  for (const alert of sorted) {
    const at = dateOf(alert.startsAt);
    lines.push({ at: at ? clockSeconds(at) : '', text: `${alert.ruleTitle} · dispara` });
    for (const [k, v] of Object.entries(alert.annotations ?? {})) {
      if (v) lines.push({ at: '', text: `${k}: ${v}` });
    }
    const values = valuesText(alert.values);
    if (values) lines.push({ at: '', text: values });
    const labels = labelsText(alert.labels);
    if (labels) lines.push({ at: '', text: labels, quiet: true });
  }
  return lines;
}

// ---- guide -------------------------------------------------------------------------------

/** Lowercase, no accents, words joined by "-". */
export function slugRule(title: string): string {
  return title.normalize('NFD').replace(/[̀-ͯ]/g, '').toLowerCase()
    .replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
}

/** <regla>: the alert's `alert_family` label, else its rule title slugged. */
export function guideRegla(alert: Pick<Alert, 'labels' | 'ruleTitle'>): string {
  const family = alert.labels?.alert_family?.trim();
  return family || slugRule(alert.ruleTitle);
}

/** The guide path for one alert, or '' when the connector names none. */
export function resolveGuide(pattern: string | undefined, alert: Pick<Alert, 'labels' | 'ruleTitle'>): string {
  const p = (pattern ?? '').trim();
  if (!p) return '';
  const regla = guideRegla(alert);
  if (p.includes('<regla>') && !regla) return '';
  return p.split('<regla>').join(regla);
}

/** The distinct guide paths of a hand-off, in alert order. */
export function guidesFor(pattern: string | undefined, alerts: ReadonlyArray<Alert>): string[] {
  const out: string[] = [];
  for (const alert of alerts) {
    const path = resolveGuide(pattern, alert);
    if (path && !out.includes(path)) out.push(path);
  }
  return out;
}

// ---- where the chat opens ------------------------------------------------------------------

/** A rule's own place wins over the connector's; the first alert's rule that has one. */
export function resolveTarget(connector: Pick<Connector, 'target' | 'ruleTargets'> | undefined, ruleKeys: ReadonlyArray<string>): ConnectorTarget {
  for (const key of ruleKeys) {
    const own = connector?.ruleTargets?.[key];
    if (own && (own.cwd || own.machineId)) return { machineId: own.machineId ?? '', cwd: own.cwd ?? '' };
  }
  return { machineId: connector?.target?.machineId ?? '', cwd: connector?.target?.cwd ?? '' };
}

/** The machine a target names as this client addresses it: '' for its own. */
export function clientMachine(targetMachineId: string, localMachineId: string): string {
  const id = String(targetMachineId ?? '').trim();
  return !id || id === localMachineId ? '' : id;
}

/** "~/Workspace/lagspace" for a path under the home directory. */
export function shortFolder(path: string): string {
  const m = /^\/(?:Users|home)\/[^/]+(\/.*)?$/.exec(path);
  return m ? `~${m[1] ?? ''}` : path;
}

// ---- the hand-off message ------------------------------------------------------------------

export interface Handoff {
  /** The sentence the owner reads and edits. */
  say: string;
  /** What goes with it: the alerts as Workass knows them. */
  registro: string;
  guides: string[];
}

function sinceWords(alert: Alert, now: Date): string {
  const at = dateOf(alert.startsAt);
  return at ? `desde ${atText(at, now)}` : '';
}

/** The pre-written message for the alerts of one situation. */
export function buildHandoff(alerts: ReadonlyArray<Alert>, connector: Connector | undefined, now: Date): Handoff {
  const sorted = [...alerts].sort((a, b) => (time(a.startsAt) || 0) - (time(b.startsAt) || 0));
  const machine = sorted.find((a) => a.machine)?.machine ?? '';
  const who = machine || (connector ? `Grafana · ${connector.name}` : 'Grafana');
  let say = `${who} dispara`;
  sorted.forEach((alert, i) => {
    const since = sinceWords(alert, now);
    if (i === 0) say += ` «${alert.ruleTitle}»${since ? ` ${since}` : ''}`;
    else say += `${i === sorted.length - 1 ? ' y' : ','}${since ? `, ${since},` : ''} «${alert.ruleTitle}»`;
  });
  say += '. Mirá qué pasó.';

  const head = `Alertas de Grafana${connector ? ` · ${connector.name}` : ''}${machine ? `, máquina ${machine}` : ''}:`;
  const lines = [head];
  for (const alert of sorted) {
    const at = dateOf(alert.startsAt);
    const stamp = at ? `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())} ${clockSeconds(at)}` : '';
    lines.push(`- «${alert.ruleTitle}» (${SEVERITY_WORD[severityOf(alert)]})${stamp ? `, dispara desde ${stamp}` : ''}`);
    for (const [k, v] of Object.entries(alert.annotations ?? {})) if (v) lines.push(`  ${k}: ${v}`);
    const values = valuesText(alert.values);
    if (values) lines.push(`  valores: ${values}`);
    const labels = labelsText(alert.labels);
    if (labels) lines.push(`  etiquetas: ${labels}`);
    if (alert.generatorURL) lines.push(`  Grafana: ${alert.generatorURL}`);
  }
  return { say, registro: lines.join('\n'), guides: guidesFor(connector?.guidePattern, sorted) };
}

/** What the chat receives: the sentence, the registro and each guide. */
export function composeHandoff(say: string, handoff: Pick<Handoff, 'registro' | 'guides'>): string {
  const guides = handoff.guides.map((g) => `Guía: ${g}`).join('\n');
  return [say.trim(), handoff.registro, guides].filter(Boolean).join('\n\n');
}

// ---- moving a connector to another listening machine -------------------------------

export interface MoveDaemons {
  handover(machineKey: string, args: { id: string }): Promise<ConnectorHandover>;
  adopt(machineKey: string, args: ConnectorAdoptArgs): Promise<{ connector: Connector }>;
  remove(machineKey: string, args: { id: string }): Promise<unknown>;
  /** Called with what the new daemon holds before any event of it can land,
   *  so nothing that moved swings the bell or notifies. */
  arrived?(machineKey: string, alerts: Alert[]): void;
}

export type MoveResult =
  | { ok: true; connector: Connector }
  | { ok: false; stage: 'handover' | 'adopt' | 'remove'; error: string; connector?: Connector };

/** One click "Mover": handover on the old daemon, adopt on the new one with the
 *  address as the new machine reaches Grafana, then remove on the old one. A
 *  failure before the remove leaves the old connector listening, as it was.
 *  The handed-over token and secret hash live only in this call. */
export async function moveConnector(daemons: MoveDaemons, from: string, to: string, id: string, url: string): Promise<MoveResult> {
  const message = (err: unknown) => String((err as Error)?.message || err || 'sin detalle');
  let handed: ConnectorHandover;
  try {
    handed = await daemons.handover(from, { id });
    if (!handed?.connector) throw new Error('la máquina no entregó el conector');
  } catch (err) {
    return { ok: false, stage: 'handover', error: message(err) };
  }
  const alerts = Array.isArray(handed.alerts) ? handed.alerts : [];
  daemons.arrived?.(to, alerts);
  let adopted: Connector;
  try {
    const reply = await daemons.adopt(to, {
      connector: handed.connector, token: handed.token, webhookSecretHash: handed.webhookSecretHash, alerts, url: url.trim() || handed.connector.url,
    });
    if (!reply?.connector) throw new Error('la otra máquina no devolvió el conector');
    adopted = reply.connector;
  } catch (err) {
    return { ok: false, stage: 'adopt', error: message(err) };
  }
  try {
    await daemons.remove(from, { id });
  } catch (err) {
    return { ok: false, stage: 'remove', error: message(err), connector: adopted };
  }
  return { ok: true, connector: adopted };
}

/** What the owner reads when a move stops halfway. */
export function moveFailureText(result: Exclude<MoveResult, { ok: true }>, fromName: string, toName: string): string {
  if (result.stage === 'handover') return `No se movió: ${fromName} no lo entregó (${result.error}). Sigue escuchando ${fromName}.`;
  if (result.stage === 'adopt') return `No se movió: ${toName} no lo tomó (${result.error}). Sigue escuchando ${fromName}.`;
  return `${toName} ya escucha, pero ${fromName} no lo soltó (${result.error}). Quedó en las dos; quitá el de ${fromName}.`;
}

/** "Van adjuntos el registro (5 líneas) y ops/runbooks/x.md." */
export function attachmentsText(handoff: Pick<Handoff, 'registro' | 'guides'>): string {
  const n = handoff.registro ? handoff.registro.split('\n').length : 0;
  const parts = [`el registro (${n} ${n === 1 ? 'línea' : 'líneas'})`, ...handoff.guides];
  const list = parts.length > 1 ? `${parts.slice(0, -1).join(', ')} y ${parts[parts.length - 1]}` : parts[0];
  return `Van adjunt${parts.length > 1 ? 'os' : 'o'} ${list}.`;
}
