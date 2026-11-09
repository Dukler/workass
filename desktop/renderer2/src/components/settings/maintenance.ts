// What Ajustes · Mantenimiento says, kept apart from the page so each phrase
// can be pinned by a test. Sizes on disk are written the way Finder writes
// them (decimal units), memory the way Activity Monitor does (binary units),
// both with a comma before the decimal.

import type { DaemonConfig, ProcessSummary, StorageUsage } from '../../wire/types';
import { liveEngines } from './fleet.ts';

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

function formatUnits(bytes: number, step: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  let value = bytes;
  let unit = 0;
  while (value >= step && unit < UNITS.length - 1) {
    value /= step;
    unit++;
  }
  // 1 KB or more keeps one decimal below 100: «14,2 GB», «980 MB».
  const digits = unit === 0 || value >= 100 ? 0 : 1;
  const rounded = Number(value.toFixed(digits));
  return `${rounded.toLocaleString('es', { minimumFractionDigits: 0, maximumFractionDigits: digits })} ${UNITS[unit]}`;
}

/** A size on disk: «14,2 GB». */
export function formatBytes(bytes: number): string {
  return formatUnits(bytes, 1000);
}

/** Resident memory, given in KiB: «2,3 GB». */
export function formatMemoryKb(kb: number): string {
  return formatUnits(kb * 1024, 1024);
}

const PART_LABELS: Array<[keyof NonNullable<StorageUsage['parts']>, string]> = [
  ['chats', 'Chats'], ['images', 'imágenes'], ['jobs', 'trabajos'], ['other', 'otros'],
];

/** «Chats 14,2 GB · imágenes 6,1 GB · trabajos 1,3 GB»; parts with nothing in
 *  them are left out. */
export function storagePartsLine(usage: StorageUsage): string {
  const parts = usage.parts ?? {};
  const out: string[] = [];
  for (const [key, label] of PART_LABELS) {
    const bytes = parts[key] ?? 0;
    if (bytes > 0) out.push(`${out.length ? label : label.charAt(0).toUpperCase() + label.slice(1)} ${formatBytes(bytes)}`);
  }
  return out.length ? out.join(' · ') : 'Todavía no hay datos guardados.';
}

/** The storage group's title: the machine is named only where the page runs
 *  in the app on that machine; a browser may be looking at another one. */
export function storageTitle(inApp: boolean, platform: string | undefined): string {
  if (!inApp) return 'Espacio en disco';
  return platform === 'darwin' ? 'Espacio en este Mac' : 'Espacio en esta computadora';
}

export function revealLabel(platform: string | undefined): string {
  if (platform === 'darwin') return 'Mostrar en Finder';
  if (platform === 'win32') return 'Mostrar en el Explorador';
  return 'Abrir carpeta';
}

export function durationLabel(ms: number): string {
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds} s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60 || minutes % 60 !== 0) return `${minutes} min`;
  const hours = minutes / 60;
  return hours === 1 ? '1 h' : `${hours} h`;
}

/** «5 procesos de agentes · 2,3 GB». */
export function agentProcessesLine(procs: readonly ProcessSummary[]): string {
  const live = liveEngines([...procs]);
  if (!live.length) return 'Ningún agente en marcha';
  const kb = live.reduce((sum, p) => sum + (p.rssKb ?? 0), 0);
  const count = live.length === 1 ? '1 proceso de agente' : `${live.length} procesos de agentes`;
  return kb > 0 ? `${count} · ${formatMemoryKb(kb)}` : count;
}

/** The idle rule the daemon runs with, or '' when it did not say. */
export function hibernateLine(config: DaemonConfig | undefined): string {
  const ttl = config?.engine?.hibernateTtlMs;
  if (typeof ttl !== 'number' || !(ttl > 0)) return '';
  return `Un chat sin actividad cierra su proceso a los ${durationLabel(ttl)} y lo retoma cuando le escribís.`;
}

/** The memory rule: the most one agent may hold, and how often it is read. */
export function recycleLine(config: DaemonConfig | undefined): { value: string; detail: string } | null {
  const maxKb = config?.engine?.maxRssKb;
  if (typeof maxKb !== 'number' || !(maxKb > 0)) return null;
  const every = config?.engine?.rssSampleIntervalMs;
  const sample = typeof every === 'number' && every > 0 ? `; se mide cada ${durationLabel(every)}` : '';
  return { value: formatMemoryKb(maxKb), detail: `Si la pasa, se cierra apenas queda sin actividad${sample}.` };
}

/** workass-diagnostico-2026-11-09-1530.json, in local time. */
export function diagnosticsFileName(at: Date): string {
  const two = (n: number) => String(n).padStart(2, '0');
  return `workass-diagnostico-${at.getFullYear()}-${two(at.getMonth() + 1)}-${two(at.getDate())}-${two(at.getHours())}${two(at.getMinutes())}.json`;
}
