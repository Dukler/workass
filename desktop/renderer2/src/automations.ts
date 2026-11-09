// Automatizaciones (owner 2026-11-09, mock desktop/docs/mocks/scheduled option
// E): what is scheduled to reach a chat later. Two kinds come from each
// machine's daemon (automations:list):
//  - "message": a Workass scheduled message (workass_schedule_message), at a
//    time or when a port answers, a file appears or a process ends. The owner
//    can cancel it.
//  - "session-cron": a cron the chat's provider session reported at its last
//    turn end (Claude's CronCreate, ScheduleWakeup, /loop). It lives in that
//    session, so only the agent can drop it.
// The Trabajos tab shows the open chat's (Programado); the page opened from the
// foot of the left panel shows every chat's, on every linked machine.

import { useEffect, useSyncExternalStore } from 'react';
import { call, callThrow, has } from './wire/api';
import { tagId } from './wire/machineIds';
import type { AutomationItem } from './wire/types';
import { store } from './store/store';

// ---- time words ------------------------------------------------------------

const WEEKDAY = ['dom', 'lun', 'mar', 'mié', 'jue', 'vie', 'sáb'];
const WEEKDAY_LONG = ['domingos', 'lunes', 'martes', 'miércoles', 'jueves', 'viernes', 'sábados'];
const MONTH = ['ene', 'feb', 'mar', 'abr', 'may', 'jun', 'jul', 'ago', 'sep', 'oct', 'nov', 'dic'];

const pad = (n: number) => String(n).padStart(2, '0');
export const clockText = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

/** "hoy", "mañana", or "vie 14 nov". */
export function dayText(d: Date, now: Date): string {
  const days = Math.round((startOfDay(d) - startOfDay(now)) / 86_400_000);
  if (days === 0) return 'hoy';
  if (days === 1) return 'mañana';
  if (days === -1) return 'ayer';
  return `${WEEKDAY[d.getDay()]} ${d.getDate()} ${MONTH[d.getMonth()]}`;
}

/** "en 42 min", "en 3 h"; empty past a day, "ahora" inside a minute. */
export function untilText(d: Date, now: Date): string {
  const ms = d.getTime() - now.getTime();
  if (ms < 60_000) return 'ahora';
  const minutes = Math.round(ms / 60_000);
  if (minutes < 60) return `en ${minutes} min`;
  const hours = Math.round(minutes / 60);
  return hours <= 24 ? `en ${hours} h` : '';
}

// ---- cron ------------------------------------------------------------------

type CronFields = [Set<number>, Set<number>, Set<number>, Set<number>, Set<number>];
const CRON_RANGES: Array<[number, number]> = [[0, 59], [0, 23], [1, 31], [1, 12], [0, 7]];

function cronField(raw: string, [min, max]: [number, number]): Set<number> | null {
  const out = new Set<number>();
  for (const part of raw.split(',')) {
    const [range, stepText] = part.split('/');
    const step = stepText === undefined ? 1 : Number(stepText);
    if (!Number.isInteger(step) || step < 1) return null;
    let lo = min;
    let hi = max;
    if (range !== '*') {
      const [a, b] = range.split('-');
      lo = Number(a);
      hi = b === undefined ? (stepText === undefined ? lo : max) : Number(b);
      if (!Number.isInteger(lo) || !Number.isInteger(hi) || lo < min || hi > max || lo > hi) return null;
    }
    for (let v = lo; v <= hi; v += step) out.add(v);
  }
  return out;
}

export function parseCron(schedule: string): CronFields | null {
  const parts = schedule.trim().split(/\s+/);
  if (parts.length !== 5) return null;
  const fields = parts.map((part, i) => cronField(part, CRON_RANGES[i]));
  if (fields.some((f) => !f)) return null;
  const dow = fields[4] as Set<number>;
  if (dow.has(7)) dow.add(0);
  return fields as CronFields;
}

/** The next minute (local time) the cron matches after `from`, within 8 days. */
export function nextCronFire(schedule: string, from: Date): Date | null {
  const fields = parseCron(schedule);
  if (!fields) return null;
  const [minute, hour, dom, month, dow] = fields;
  const parts = schedule.trim().split(/\s+/);
  const domAny = parts[2] === '*';
  const dowAny = parts[4] === '*';
  const t = new Date(from.getTime());
  t.setSeconds(0, 0);
  t.setMinutes(t.getMinutes() + 1);
  for (let i = 0; i < 8 * 24 * 60; i++) {
    const dayOk = domAny && dowAny ? true
      : domAny ? dow.has(t.getDay())
        : dowAny ? dom.has(t.getDate())
          : dom.has(t.getDate()) || dow.has(t.getDay());
    if (minute.has(t.getMinutes()) && hour.has(t.getHours()) && month.has(t.getMonth() + 1) && dayOk) return t;
    t.setMinutes(t.getMinutes() + 1);
  }
  return null;
}

/** A recurring cron in words: "cada 30 min", "todos los días 09:00"… */
export function cronText(schedule: string): string {
  const parts = schedule.trim().split(/\s+/);
  if (parts.length !== 5 || !parseCron(schedule)) return schedule;
  const [m, h, dom, mon, dow] = parts;
  const every = /^\*\/(\d+)$/;
  const num = /^\d+$/;
  if (dom === '*' && mon === '*' && dow === '*') {
    if (m === '*' && h === '*') return 'cada minuto';
    if (every.test(m) && h === '*') return `cada ${every.exec(m)![1]} min`;
    if (num.test(m) && h === '*') return 'cada hora';
    if (num.test(m) && every.test(h)) return `cada ${every.exec(h)![1]} h`;
    if (num.test(m) && num.test(h)) return `todos los días ${pad(+h)}:${pad(+m)}`;
  }
  if (dom === '*' && mon === '*' && num.test(m) && num.test(h)) {
    if (dow === '1-5') return `días hábiles ${pad(+h)}:${pad(+m)}`;
    if (num.test(dow)) return `los ${WEEKDAY_LONG[+dow % 7]} ${pad(+h)}:${pad(+m)}`;
  }
  return schedule;
}

// ---- rows --------------------------------------------------------------------

export interface AutomationRow {
  item: AutomationItem;
  /** '' for this machine. */
  machineId: string;
  machineName: string;
  /** The store's chat id: the tab id here, tagged for another machine. */
  chatKey: string;
  /** When it reaches the chat; null for a condition that has not happened. */
  due: Date | null;
}

export function rowDue(item: AutomationItem, now: Date): Date | null {
  if (item.kind === 'message') {
    if (item.condition !== 'at' || !item.at) return null;
    const at = new Date(item.at);
    return Number.isNaN(at.getTime()) ? null : at;
  }
  return item.schedule ? nextCronFire(item.schedule, now) : null;
}

function shortAddr(addr: string): string {
  const m = /^(?:127\.0\.0\.1|localhost|\[::1\]|::1)?:(\d+)$/.exec(addr.trim());
  return m ? `:${m[1]}` : addr.trim();
}

function shortPath(path: string): string {
  const parts = path.split('/').filter(Boolean);
  return parts.length > 2 ? parts.slice(-2).join('/') : path;
}

/** When it arrives, in words: "hoy 19:15", "cuando :8788 responda", "cada 30 min". */
export function whenText(row: AutomationRow, now: Date): string {
  const { item, due } = row;
  if (item.kind === 'session-cron') {
    if (item.recurring) return cronText(item.schedule ?? '');
    return due ? `${dayText(due, now)} ${clockText(due)}` : (item.schedule ?? '');
  }
  switch (item.condition) {
    case 'at': return due ? `${dayText(due, now)} ${clockText(due)}${item.late ? ' · atrasado' : ''}` : '';
    case 'tcp': return `cuando ${shortAddr(item.addr ?? '')} responda`;
    case 'file': return `cuando exista ${shortPath(item.path ?? '')}`;
    case 'pid': return `cuando termine el proceso ${item.pid ?? ''}`;
    default: return '';
  }
}

/** The page's narrow time column: "19:15", "cada 30 min", ":8788". */
export function timeColumn(row: AutomationRow): string {
  const { item, due } = row;
  if (item.kind === 'session-cron') return item.recurring ? cronText(item.schedule ?? '') : due ? clockText(due) : '';
  switch (item.condition) {
    case 'at': return due ? clockText(due) : '';
    case 'tcp': return shortAddr(item.addr ?? '');
    case 'file': return 'archivo';
    case 'pid': return `pid ${item.pid ?? ''}`;
    default: return '';
  }
}

export function rowGlyph(row: AutomationRow): 'clock' | 'repeat' | 'plug' | 'file' | 'process' {
  if (row.item.kind === 'session-cron') return row.item.recurring ? 'repeat' : 'clock';
  switch (row.item.condition) {
    case 'tcp': return 'plug';
    case 'file': return 'file';
    case 'pid': return 'process';
    default: return 'clock';
  }
}

/** The soonest first; conditions after everything with a time. */
export function sortRows(rows: AutomationRow[]): AutomationRow[] {
  return [...rows].sort((a, b) => {
    if (a.due && b.due) return a.due.getTime() - b.due.getTime();
    if (a.due) return -1;
    if (b.due) return 1;
    return (a.item.createdAt ?? '').localeCompare(b.item.createdAt ?? '');
  });
}

export const WHEN_SOMETHING = 'Cuando pase algo';

/** The page's day groups: "Hoy", "Mañana", "Vie 14 nov", then the conditions. */
export function groupByDay(rows: AutomationRow[], now: Date): Array<{ label: string; rows: AutomationRow[] }> {
  const groups: Array<{ label: string; rows: AutomationRow[] }> = [];
  for (const row of sortRows(rows)) {
    const day = row.due ? dayText(row.due, now) : WHEN_SOMETHING;
    const label = day.charAt(0).toUpperCase() + day.slice(1);
    const last = groups[groups.length - 1];
    if (last && last.label === label) last.rows.push(row);
    else groups.push({ label, rows: [row] });
  }
  return groups;
}

/** Due inside the next hour: drawn as the next thing to happen. */
export function isSoon(row: AutomationRow, now: Date): boolean {
  return !!row.due && row.due.getTime() - now.getTime() < 3_600_000;
}

// ---- the list, from every machine ---------------------------------------------

const POLL_MS = 20_000;

let rowsByMachine = new Map<string, AutomationRow[]>();
let version = 0;
let refreshing: Promise<void> | null = null;
let lastRefresh = 0;
const listeners = new Set<() => void>();

function changed() {
  version += 1;
  for (const listener of listeners) listener();
}

function toRows(items: AutomationItem[] | undefined, machineId: string, machineName: string, now: Date): AutomationRow[] {
  return (items ?? []).filter((item) => item && item.tabId && item.chatId).map((item) => ({
    item,
    machineId,
    machineName,
    chatKey: machineId ? tagId(machineId, item.tabId) : item.tabId,
    due: rowDue(item, now),
  }));
}

/** Read every linked machine's list now (one read at a time). */
export function refreshAutomations(): Promise<void> {
  if (refreshing) return refreshing;
  refreshing = (async () => {
    const now = new Date();
    const next = new Map<string, AutomationRow[]>();
    if (has('automationsList')) {
      const reply = await call('automationsList');
      if (reply) next.set('', toRows(reply.items, '', store.localMachineName(), now));
      else if (rowsByMachine.has('')) next.set('', rowsByMachine.get('')!);
    }
    const remotes = store.state.machines.filter((m) => m.paired && m.link === 'ready' && m.machineId !== store.localMachineId());
    await Promise.all(remotes.map(async (machine) => {
      try {
        const reply = await store.invokeOnMachine<{ items?: AutomationItem[] }>(machine.machineId, 'automations:list');
        next.set(machine.machineId, toRows(reply?.items, machine.machineId, machine.name, now));
      } catch {
        // An older daemon has no list; a dropped link keeps what it last said.
        const held = rowsByMachine.get(machine.machineId);
        if (held) next.set(machine.machineId, held);
      }
    }));
    rowsByMachine = next;
    lastRefresh = Date.now();
    changed();
  })().finally(() => { refreshing = null; });
  return refreshing;
}

export function automationRows(): AutomationRow[] {
  return sortRows([...rowsByMachine.values()].flat());
}

/** The open chat's rows (Trabajos · Programado). */
export function chatAutomationRows(chat: { id: string; chatId?: string } | null): AutomationRow[] {
  if (!chat?.chatId) return [];
  return automationRows().filter((row) => row.chatKey === chat.id && row.item.chatId === chat.chatId);
}

/** Cancel one scheduled message, on the machine that holds it. */
export async function cancelAutomation(row: AutomationRow): Promise<void> {
  if (row.item.kind !== 'message') throw new Error('Esta tarea vive en la sesión del agente; pedile que la quite.');
  const args = { tabId: row.item.tabId, chatId: row.item.chatId, id: row.item.id };
  if (row.machineId) await store.invokeOnMachine(row.machineId, 'automations:cancel', args);
  else await callThrow('automationsCancel', args);
  // Drop it at once; the read that follows confirms.
  for (const [machineId, rows] of rowsByMachine) {
    rowsByMachine.set(machineId, rows.filter((r) => r !== row));
  }
  changed();
  void refreshAutomations();
}

let pollers = 0;
let pollTimer: ReturnType<typeof setInterval> | null = null;

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
const snapshot = () => version;

/** Re-renders on every new list; while mounted, reads every 20 s. */
export function useAutomations(): number {
  useEffect(() => {
    pollers += 1;
    if (Date.now() - lastRefresh > 5_000) void refreshAutomations();
    if (!pollTimer) pollTimer = setInterval(() => { void refreshAutomations(); }, POLL_MS);
    return () => {
      pollers -= 1;
      if (pollers === 0 && pollTimer) { clearInterval(pollTimer); pollTimer = null; }
    };
  }, []);
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/** Re-renders on every new list without reading (the foot button's count). */
export function useAutomationsVersion(): number {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}
