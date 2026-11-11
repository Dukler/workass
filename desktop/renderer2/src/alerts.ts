// Conectores + alertas, live (owner 2026-11-10, docs/specs/connectors-alerts.md).
// Each daemon that listens to a Grafana keeps its connectors and their alerts;
// this client reads them from this machine and every linked machine, hears
// alerts:changed / connectors:changed from each, and acts on an alert through
// the daemon that holds it. The pure rules are in alerts-model.ts.
//
// Owner law 1: nothing here opens a chat, writes into one or wakes an agent.
// The bell swings and, if allowed, one system notification shows; a chat opens
// only from the Parte's "Abrir sin enviar" / "Abrir y enviar".

import { useEffect, useSyncExternalStore } from 'react';
import { call, callThrow, has, on } from './wire/api';
import type {
  Alert, AlertActArgs, AlertsChanged, Connector, ConnectorHandover, ConnectorSetArgs, ConnectorTestResult,
} from './wire/types';
import { store } from './store/store';
import {
  AnnounceLedger, moveConnector, notifyAllows, severityOf, SEVERITY_WORD, situationTitle,
  type AlertEntry, type MoveResult,
} from './alerts-model';

interface MachineAlerts {
  machineKey: string;
  machineName: string;
  /** This daemon answered connectors:list / alerts:list at least once. */
  supported: boolean;
  connectors: Connector[];
  alerts: Alert[];
}

const POLL_MS = 30_000;

let byMachine = new Map<string, MachineAlerts>();
let version = 0;
let swingSeq = 0;
const listeners = new Set<() => void>();
const ledger = new AnnounceLedger();
const primed = new Set<string>();

function changed() {
  version += 1;
  for (const listener of listeners) listener();
}

function remoteMachines() {
  const self = store.localMachineId();
  return store.state.machines.filter((m) => m.paired && m.link === 'ready' && m.machineId !== self);
}

function machineName(machineKey: string): string {
  if (!machineKey) return store.localMachineName();
  const m = store.state.machines.find((x) => x.machineId === machineKey);
  return m?.name || m?.reportedName || machineKey;
}

async function invoke<T>(machineKey: string, channel: string, method: string, args?: unknown): Promise<T> {
  if (machineKey) return store.invokeOnMachine<T>(machineKey, channel, ...(args === undefined ? [] : [args]));
  // The local bridge names the channel as a method (internal/httpserve/lan_bridge.go).
  if (!has(method as 'alertsList')) throw new Error('Esta versión del daemon no tiene conectores.');
  const fn = (callThrow as unknown as (name: string, ...a: unknown[]) => Promise<T>);
  return fn(method, ...(args === undefined ? [] : [args]));
}

function localSupported(): boolean {
  return has('alertsList') || has('connectorsList');
}

// ---- reads ------------------------------------------------------------------

async function readMachine(machineKey: string): Promise<MachineAlerts | null> {
  const held = byMachine.get(machineKey);
  const name = machineName(machineKey);
  try {
    let connectors: Connector[] | undefined;
    let alerts: Alert[] | undefined;
    if (machineKey) {
      const [c, a] = await Promise.all([
        store.invokeOnMachine<{ connectors?: Connector[] }>(machineKey, 'connectors:list').catch(() => undefined),
        store.invokeOnMachine<{ alerts?: Alert[] }>(machineKey, 'alerts:list').catch(() => undefined),
      ]);
      connectors = c?.connectors;
      alerts = a?.alerts;
    } else {
      if (!localSupported()) return null;
      const [c, a] = await Promise.all([call('connectorsList'), call('alertsList')]);
      connectors = c?.connectors;
      alerts = a?.alerts;
    }
    if (!Array.isArray(connectors) && !Array.isArray(alerts)) return held ?? null;
    return {
      machineKey, machineName: name, supported: true,
      connectors: Array.isArray(connectors) ? connectors : held?.connectors ?? [],
      alerts: Array.isArray(alerts) ? alerts : held?.alerts ?? [],
    };
  } catch {
    return held ?? null;
  }
}

let refreshing: Promise<void> | null = null;

/** Read every machine's connectors and alerts now. */
export function refreshAlerts(): Promise<void> {
  if (refreshing) return refreshing;
  refreshing = (async () => {
    const keys = ['', ...remoteMachines().map((m) => m.machineId)];
    const results = await Promise.all(keys.map((key) => readMachine(key)));
    const next = new Map<string, MachineAlerts>();
    results.forEach((result) => {
      if (!result) return;
      // The first list a machine gives is what is already there: no swing.
      if (!primed.has(result.machineKey)) { ledger.prime(result.machineKey, result.alerts); primed.add(result.machineKey); }
      next.set(result.machineKey, result);
    });
    byMachine = next;
    changed();
  })().finally(() => { refreshing = null; });
  return refreshing;
}

// ---- events -------------------------------------------------------------------

function mergeAlerts(machineKey: string, incoming: ReadonlyArray<Alert>) {
  const held = byMachine.get(machineKey) ?? {
    machineKey, machineName: machineName(machineKey), supported: true, connectors: [], alerts: [],
  };
  const map = new Map(held.alerts.map((a) => [a.id, a]));
  for (const alert of incoming) if (alert?.id) map.set(alert.id, alert);
  byMachine.set(machineKey, { ...held, supported: true, alerts: [...map.values()] });
}

function onAlertsChanged(machineKey: string, payload: AlertsChanged | null | undefined) {
  if (!payload || !Array.isArray(payload.alerts)) return;
  if (!primed.has(machineKey)) primed.add(machineKey);
  const fresh = ledger.take(machineKey, payload.alerts, payload.announce);
  mergeAlerts(machineKey, payload.alerts);
  if (Array.isArray(payload.removed) && payload.removed.length) {
    const gone = new Set(payload.removed);
    const held = byMachine.get(machineKey);
    if (held) byMachine.set(machineKey, { ...held, alerts: held.alerts.filter((a) => !gone.has(a.id)) });
  }
  if (fresh.length) {
    swingSeq += 1;
    notifyFresh(machineKey, fresh);
  }
  changed();
}

function onConnectorsChanged(machineKey: string, payload: { connectors?: Connector[] } | null | undefined) {
  if (!payload || !Array.isArray(payload.connectors)) return;
  const held = byMachine.get(machineKey);
  byMachine.set(machineKey, {
    machineKey, machineName: machineName(machineKey), supported: true,
    connectors: payload.connectors, alerts: held?.alerts ?? [],
  });
  changed();
  // Switching a rule off only says connectors:changed; its alerts leave the
  // list on the daemon, so read the list again.
  void refreshMachineAlerts(machineKey);
}

async function refreshMachineAlerts(machineKey: string) {
  try {
    const reply = machineKey
      ? await store.invokeOnMachine<{ alerts?: Alert[] }>(machineKey, 'alerts:list')
      : await call('alertsList');
    if (!Array.isArray(reply?.alerts)) return;
    const held = byMachine.get(machineKey);
    if (!held) return;
    byMachine.set(machineKey, { ...held, alerts: reply.alerts });
    changed();
  } catch { /* the next poll reads it */ }
}

/** One system notification per announced alert the connector lets through,
 *  only with the app's own notifications on (Ajustes · General). Silent: the
 *  bell is the signal, and no chat hears of it. */
function notifyFresh(machineKey: string, fresh: Alert[]) {
  if (!store.state.notifEnabled || store.state.notifPermission !== 'granted' || typeof Notification === 'undefined') return;
  const connectors = byMachine.get(machineKey)?.connectors ?? [];
  for (const alert of fresh) {
    const connector = connectors.find((c) => c.id === alert.connectorId);
    if (!notifyAllows(connector?.notify, severityOf(alert))) continue;
    const entry: AlertEntry = { machineKey, machineName: machineName(machineKey), alert, connector };
    try {
      const n = new Notification(`${situationTitle(entry)} · ${SEVERITY_WORD[severityOf(alert)]}`, { body: alert.ruleTitle, silent: true });
      n.onclick = () => { try { window.focus(); } catch { /* ignore */ } openParte(); n.close(); };
    } catch { /* no notification here: the bell still says it */ }
  }
}

let started = false;
let pollTimer: ReturnType<typeof setInterval> | null = null;

/** Listen to every machine and read every 30 s; once, from the bell. */
export function startAlerts() {
  if (started) return;
  started = true;
  on('onAlertsChanged', (payload: AlertsChanged) => onAlertsChanged('', payload));
  on('onConnectorsChanged', (payload: { connectors: Connector[] }) => onConnectorsChanged('', payload));
  store.subscribeMachineEvent('alerts:changed', (payload, machineId) => onAlertsChanged(machineId, payload as AlertsChanged));
  store.subscribeMachineEvent('connectors:changed', (payload, machineId) => onConnectorsChanged(machineId, payload as { connectors?: Connector[] }));
  void refreshAlerts();
  pollTimer = setInterval(() => { void refreshAlerts(); }, POLL_MS);
}

/** Tests and the dev rig. */
export function stopAlerts() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
  started = false;
}

// ---- reading the state -----------------------------------------------------------

export function alertEntries(): AlertEntry[] {
  const out: AlertEntry[] = [];
  for (const machine of byMachine.values()) {
    for (const alert of machine.alerts) {
      out.push({
        machineKey: machine.machineKey, machineName: machine.machineName, alert,
        connector: machine.connectors.find((c) => c.id === alert.connectorId),
      });
    }
  }
  return out;
}

export interface ConnectorEntry { machineKey: string; machineName: string; connector: Connector }

export function connectorEntries(): ConnectorEntry[] {
  const out: ConnectorEntry[] = [];
  for (const machine of byMachine.values()) {
    for (const connector of machine.connectors) out.push({ machineKey: machine.machineKey, machineName: machine.machineName, connector });
  }
  return out;
}

/** Machines that have connectors at all (the bell shows with one). */
export function alertsSupported(): boolean {
  if (localSupported()) return true;
  for (const machine of byMachine.values()) if (machine.supported) return true;
  return false;
}

/** The machines whose daemon keeps connectors, for "Máquina que escucha". */
export function listeningMachines(): Array<{ machineKey: string; machineName: string }> {
  const out: Array<{ machineKey: string; machineName: string }> = [];
  if (localSupported()) out.push({ machineKey: '', machineName: store.localMachineName() });
  for (const m of remoteMachines()) {
    const held = byMachine.get(m.machineId);
    if (held?.supported) out.push({ machineKey: m.machineId, machineName: held.machineName });
  }
  return out;
}

export function currentSwing(): number { return swingSeq; }

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
const snapshot = () => version;

/** Re-renders on every change. */
export function useAlertsVersion(): number {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/** Re-renders on every change and reads now when mounted (settings). */
export function useAlertsRefresh(): number {
  useEffect(() => { void refreshAlerts(); }, []);
  return useAlertsVersion();
}

// ---- the Parte open / closed --------------------------------------------------------

let parteOpen = false;
const openListeners = new Set<() => void>();
function setParte(next: boolean) {
  if (parteOpen === next) return;
  parteOpen = next;
  for (const listener of openListeners) listener();
}
export function openParte() { setParte(true); }
export function closeParte() { setParte(false); }
export function toggleParte() { setParte(!parteOpen); }
function subscribeOpen(listener: () => void) {
  openListeners.add(listener);
  return () => { openListeners.delete(listener); };
}
const openSnapshot = () => parteOpen;
export function useParteOpen(): boolean {
  return useSyncExternalStore(subscribeOpen, openSnapshot, openSnapshot);
}

// ---- acting -----------------------------------------------------------------------------

/** One action on alerts of one daemon; the reply replaces them at once. */
export async function actOnAlerts(machineKey: string, args: AlertActArgs): Promise<void> {
  const reply = await invoke<{ alerts?: Alert[] }>(machineKey, 'alerts:act', 'alertsAct', args);
  if (Array.isArray(reply?.alerts)) { mergeAlerts(machineKey, reply.alerts); changed(); }
  else void refreshAlerts();
}

/** The same action on entries that may live on several daemons. */
export async function actOnEntries(entries: ReadonlyArray<AlertEntry>, act: (ids: string[]) => AlertActArgs): Promise<void> {
  const byKey = new Map<string, string[]>();
  for (const e of entries) byKey.set(e.machineKey, [...(byKey.get(e.machineKey) ?? []), e.alert.id]);
  await Promise.all([...byKey.entries()].map(([key, ids]) => actOnAlerts(key, act(ids))));
}

// ---- connectors ------------------------------------------------------------------------------

function replaceConnector(machineKey: string, connector: Connector) {
  const held = byMachine.get(machineKey) ?? {
    machineKey, machineName: machineName(machineKey), supported: true, connectors: [], alerts: [],
  };
  const others = held.connectors.filter((c) => c.id !== connector.id);
  byMachine.set(machineKey, { ...held, connectors: [...others, connector] });
  changed();
}

export async function saveConnector(machineKey: string, args: ConnectorSetArgs): Promise<{ connector: Connector; webhookSecret?: string }> {
  const reply = await invoke<{ connector: Connector; webhookSecret?: string }>(machineKey, 'connectors:set', 'connectorsSet', args);
  if (reply?.connector) replaceConnector(machineKey, reply.connector);
  return reply;
}

export async function removeConnector(machineKey: string, id: string): Promise<void> {
  await invoke(machineKey, 'connectors:remove', 'connectorsRemove', { id });
  const held = byMachine.get(machineKey);
  if (held) {
    byMachine.set(machineKey, {
      ...held,
      connectors: held.connectors.filter((c) => c.id !== id),
      alerts: held.alerts.filter((a) => a.connectorId !== id),
    });
    changed();
  }
}

export function testConnector(machineKey: string, args: { id: string } | { url: string; token: string }): Promise<ConnectorTestResult> {
  return invoke<ConnectorTestResult>(machineKey, 'connectors:test', 'connectorsTest', args);
}

export async function rotateSecret(machineKey: string, id: string): Promise<string> {
  const reply = await invoke<{ webhookSecret: string }>(machineKey, 'connectors:rotate-secret', 'connectorsRotateSecret', { id });
  return reply?.webhookSecret ?? '';
}

/** Move a connector to another listening machine (one click, "Mover"). The
 *  alerts it carries keep their chats and silences and never re-announce. */
export async function moveConnectorTo(from: string, to: string, id: string, url: string): Promise<MoveResult> {
  const result = await moveConnector({
    handover: (key, args) => invoke<ConnectorHandover>(key, 'connectors:handover', 'connectorsHandover', args),
    adopt: (key, args) => invoke<{ connector: Connector }>(key, 'connectors:adopt', 'connectorsAdopt', args),
    remove: (key, args) => invoke(key, 'connectors:remove', 'connectorsRemove', args),
    arrived: (key, alerts) => ledger.prime(key, alerts),
  }, from, to, id, url);
  if (result.ok || result.stage === 'remove') {
    if (result.connector) replaceConnector(to, result.connector);
    if (result.ok) {
      const held = byMachine.get(from);
      if (held) {
        byMachine.set(from, { ...held, connectors: held.connectors.filter((c) => c.id !== id), alerts: held.alerts.filter((a) => a.connectorId !== id) });
      }
    }
    changed();
    void refreshAlerts();
  }
  return result;
}

/** The full set args of a connector, with a patch. */
export function setArgsOf(connector: Connector, patch: Partial<ConnectorSetArgs> = {}): ConnectorSetArgs {
  return {
    id: connector.id,
    name: connector.name,
    url: connector.url,
    mode: connector.mode,
    notify: connector.notify ?? 'critical',
    target: connector.target ?? { machineId: '', cwd: '' },
    ruleTargets: connector.ruleTargets ?? {},
    guidePattern: connector.guidePattern ?? '',
    ...patch,
  };
}

/** Where Grafana posts for a "Recibir" connector: the daemon's own address. */
export function webhookURL(machineKey: string, connector: Connector): string {
  const path = connector.webhookPath || `/workass/connectors/${connector.id}/webhook`;
  if (!machineKey) {
    const origin = typeof location !== 'undefined' && /^https?:/.test(location.origin) ? location.origin : '';
    return `${origin}${path}`;
  }
  const m = store.state.machines.find((x) => x.machineId === machineKey);
  const address = m?.address ?? '';
  if (!address) return path;
  const base = /^https?:\/\//.test(address) ? address : `${m?.secure === false ? 'http' : 'https'}://${address}`;
  return `${base.replace(/\/+$/, '')}${path}`;
}
