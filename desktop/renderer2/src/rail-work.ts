// The Trabajos tab of the activity pane (approved mock rail-v7, 2026-11-05):
// what runs in the background for one chat, where it runs, and what waits.
// Pure and framework-free so node --test can check the counting and the
// queue's order without React.

import type { SpawnedWorkItem } from './wire/types';
import { isAgentWork, nestSpawnedWork, workHostName } from './subagent-layout.ts';
import { shortMachineId } from './machine-label.ts';

// A supervised job waiting for a resource has not started, however long it has
// been waiting (fleet-runtime §14e). Its status stays 'running' on the wire.
export function isQueuedWork(item: Pick<SpawnedWorkItem, 'status' | 'queuedAt' | 'execStartedAt'>): boolean {
  return item.status === 'running' && !!item.queuedAt && !item.execStartedAt;
}

/** The chat's own background work: running rows and the ones still waiting.
 *  Agent work is drawn as subagent rows, and work an agent started hangs under
 *  that agent, so neither is counted here. */
export function backgroundWork(work: readonly SpawnedWorkItem[]): { running: SpawnedWorkItem[]; queued: SpawnedWorkItem[] } {
  const { nested } = nestSpawnedWork(work);
  const live = work.filter((item) => item.status === 'running' && !isAgentWork(item) && !nested.has(item.id));
  // Longest running first: a new job joins at the bottom, so rows never shift
  // under the pointer, and the cap folds away the newest, not the oldest.
  const running = live.filter((item) => !isQueuedWork(item))
    .map((item, index) => ({ item, index, at: stamp(item.execStartedAt || item.startedAt) }))
    .sort((a, b) => a.at - b.at || a.index - b.index)
    .map(({ item }) => item);
  return { running, queued: live.filter(isQueuedWork) };
}

export interface WorkMachine {
  /** '' is this machine. */
  key: string;
  name: string;
  local: boolean;
}

export interface MachineContext {
  /** The machine that owns the chat; '' for this one. */
  chatMachineId?: string | null;
  localMachineId?: string | null;
  names?: Readonly<Record<string, string>>;
  /** This machine's own name: the one it was given, else its hostname. */
  localName?: string;
}

// Where one row runs. A row names its host only when that is not the machine
// that owns the chat; with no host it runs where the chat lives.
export function workMachine(item: Pick<SpawnedWorkItem, 'hostMachine' | 'hostMachineName'>, context: MachineContext = {}): WorkMachine {
  const host = String(item.hostMachine ?? '').trim();
  const id = host || String(context.chatMachineId ?? '').trim();
  const local = String(context.localMachineId ?? '').trim();
  if (!id || (local && id === local)) return { key: '', name: String(context.localName ?? '').trim() || 'Esta máquina', local: true };
  const named = String(context.names?.[id] ?? '').trim();
  return { key: id, name: named || (host ? workHostName(item) : '') || shortMachineId(id), local: false };
}

function stamp(text: string | undefined): number {
  const at = text ? Date.parse(text) : Number.NaN;
  return Number.isFinite(at) ? at : Number.POSITIVE_INFINITY;
}

export interface MachineLoad {
  machine: WorkMachine;
  running: number;
  waiting: number;
}

/** One row per machine that has background work: this machine first. */
export function machineLoads(running: readonly SpawnedWorkItem[], queued: readonly SpawnedWorkItem[], context: MachineContext = {}): MachineLoad[] {
  const byKey = new Map<string, MachineLoad>();
  const add = (item: SpawnedWorkItem, field: 'running' | 'waiting') => {
    const machine = workMachine(item, context);
    const load = byKey.get(machine.key) ?? { machine, running: 0, waiting: 0 };
    load[field] += 1;
    byKey.set(machine.key, load);
  };
  for (const item of running) add(item, 'running');
  for (const item of queued) add(item, 'waiting');
  return [...byKey.values()].sort((a, b) => Number(b.machine.local) - Number(a.machine.local)
    || a.machine.name.localeCompare(b.machine.name));
}

export interface QueueGroup {
  machine: WorkMachine;
  items: SpawnedWorkItem[];
}

// The queue, grouped by machine, each group in the order it will run: first
// asked, first served (procchild's FIFO), and the daemon's own position when
// two were asked at the same instant.
export function queueGroups(queued: readonly SpawnedWorkItem[], context: MachineContext = {}): QueueGroup[] {
  const byKey = new Map<string, QueueGroup>();
  for (const item of queued) {
    const machine = workMachine(item, context);
    const group = byKey.get(machine.key) ?? { machine, items: [] };
    group.items.push(item);
    byKey.set(machine.key, group);
  }
  const groups = [...byKey.values()];
  for (const group of groups) {
    group.items.sort((a, b) => stamp(a.queuedAt) - stamp(b.queuedAt)
      || (a.queuePosition ?? 0) - (b.queuePosition ?? 0));
  }
  return groups.sort((a, b) => Number(b.machine.local) - Number(a.machine.local)
    || a.machine.name.localeCompare(b.machine.name));
}

/** «a», «a y b», «a, b y c». */
export function joinNames(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return `${names.slice(0, -1).join(', ')} y ${names[names.length - 1]}`;
}

/** The next job on each machine, for the closed queue's «sigue …» line. */
export function nextInQueue(groups: readonly QueueGroup[]): string[] {
  return groups.map((group) => workLabel(group.items[0])).filter(Boolean);
}

export function workLabel(item: Pick<SpawnedWorkItem, 'label' | 'taskId'> | undefined): string {
  return item ? String(item.label || item.taskId || '').trim() : '';
}

export interface TotalPart { count: number; word: string }

/** «2 subagentes · 3 corriendo · 14 en espera», zero parts left out. */
export function workTotals(subagents: number, running: number, waiting: number): TotalPart[] {
  return [
    { count: subagents, word: subagents === 1 ? 'subagente' : 'subagentes' },
    { count: running, word: 'corriendo' },
    { count: waiting, word: 'en espera' },
  ].filter((part) => part.count > 0);
}

// How long a running job has run: from its command's start, not from when it
// was asked for (a supervised job's wait is not its run).
export function workRunMs(item: Pick<SpawnedWorkItem, 'execStartedAt' | 'startedAt' | 'runMs'>, nowMs: number): number | null {
  if (item.runMs) return item.runMs;
  const start = Date.parse(item.execStartedAt || item.startedAt);
  return Number.isFinite(start) ? Math.max(0, nowMs - start) : null;
}

export function workWaitMs(item: Pick<SpawnedWorkItem, 'queuedAt'>, nowMs: number): number | null {
  const at = item.queuedAt ? Date.parse(item.queuedAt) : Number.NaN;
  return Number.isFinite(at) ? Math.max(0, nowMs - at) : null;
}

/** «40 s», «41 min», «2 h 2 min», «3 d 4 h». */
export function fmtSpan(ms: number | null | undefined): string {
  if (ms == null || !(ms >= 0)) return '';
  const secs = Math.floor(ms / 1000);
  if (secs < 60) return `${secs} s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins} min`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return mins % 60 ? `${hours} h ${mins % 60} min` : `${hours} h`;
  const days = Math.floor(hours / 24);
  return hours % 24 ? `${days} d ${hours % 24} h` : `${days} d`;
}

/** A wait, coarser than a run: «40 s», «44 min», «2 h», «3 d». A queue reads
 *  as a column, and «1 h 59 min» beside «2 h» is noise. */
export function fmtWait(ms: number | null | undefined): string {
  if (ms == null || !(ms >= 0)) return '';
  const secs = Math.floor(ms / 1000);
  if (secs < 60) return `${secs} s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins} min`;
  if (mins < 24 * 60) return `${Math.round(mins / 60)} h`;
  return `${Math.round(mins / (24 * 60))} d`;
}

function sameLocalDay(at: number, nowMs: number): boolean {
  const a = new Date(at);
  const b = new Date(nowMs);
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/** Finished on this machine's calendar day. A row with no end stamp is dated
 *  by its last update. */
export function finishedToday(item: Pick<SpawnedWorkItem, 'finishedAt' | 'updatedAt'>, nowMs: number): boolean {
  const at = Date.parse(item.finishedAt || item.updatedAt || '');
  return Number.isFinite(at) && sameLocalDay(at, nowMs);
}

/** How many segments a machine's bar draws: one per job, up to a cap past
 *  which the bar keeps its proportions instead of becoming all gaps. */
export const LOAD_SEGMENTS_MAX = 24;
export function loadSegments(running: number, waiting: number, max = LOAD_SEGMENTS_MAX): { running: number; waiting: number } {
  const total = running + waiting;
  if (total <= max) return { running, waiting };
  const shownRunning = running > 0 ? Math.max(1, Math.round((running / total) * max)) : 0;
  const shownWaiting = waiting > 0 ? Math.max(1, max - shownRunning) : 0;
  return { running: Math.min(max - shownWaiting, shownRunning), waiting: shownWaiting };
}
