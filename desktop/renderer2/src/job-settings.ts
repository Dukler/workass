// Ajustes · Trabajos (daemon cmd/workass/job_settings.go). Two real things:
//
//  - "jobs".silenceNoticeAfterMin in the daemon's app-settings blob: how long
//    a running `workass run` job may go unheard before its chat is told. Zero
//    is the engine's own rule (three missed one-minute beats). It only moves
//    the notice; the job's row still reads "not reporting" at the window.
//  - resources:list on each machine's daemon: who holds each resource and how
//    many jobs wait behind it.
//
// settings:set REPLACES the blob, so a save re-reads it and touches only
// "jobs", with the read's revision so a concurrent writer is never undone.

import { call, callThrow, has } from './wire/api.ts';
import { settingsFromReply } from './settings-blob.ts';
import type { JobResource } from './wire/types';

export const JOBS_SETTINGS_KEY = 'jobs';

export interface JobSettings {
  /** Minutes; 0 is the default (the notice with the mark, three minutes). */
  silenceNoticeAfterMin: number;
}

export const DEFAULT_JOB_SETTINGS: JobSettings = { silenceNoticeAfterMin: 0 };

/** The daemon's ceiling (acp.MaxProcessSilenceNoticeAfter), in minutes. */
const MAX_SILENCE_NOTICE_MIN = 24 * 60;

/** The choices the page offers; 0 reads as the default three minutes. */
export const SILENCE_NOTICE_CHOICES: readonly number[] = [0, 10, 30, 60];

export function parseJobSettings(raw: unknown): JobSettings {
  const value = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : {};
  const minutes = Number(value.silenceNoticeAfterMin);
  if (!Number.isInteger(minutes) || minutes <= 0 || minutes > MAX_SILENCE_NOTICE_MIN) return { ...DEFAULT_JOB_SETTINGS };
  return { silenceNoticeAfterMin: minutes };
}

export function jobSettingsFromReply(reply: unknown): JobSettings {
  return parseJobSettings(settingsFromReply(reply)[JOBS_SETTINGS_KEY]);
}

export function withJobSettings(current: Record<string, unknown> | null | undefined, value: JobSettings): Record<string, unknown> {
  return { ...(current ?? {}), [JOBS_SETTINGS_KEY]: parseJobSettings(value) };
}

export function silenceChoiceLabel(minutes: number): string {
  if (minutes <= 0) return '3 min';
  if (minutes % 60 === 0) return `${minutes / 60} h`;
  return `${minutes} min`;
}

/** The choices to draw: the offered ones, plus a stored value set elsewhere. */
export function silenceChoices(current: number): number[] {
  const out = [...SILENCE_NOTICE_CHOICES];
  if (!out.includes(current)) out.push(current);
  return out.sort((a, b) => a - b);
}

export function canEditJobSettings(): boolean {
  return has('getSettings') && has('setSettings');
}

/** The stored choice, or null when this daemon does not keep one: a daemon
 *  that predates the setting leaves "jobs" out of its blob. */
export async function loadJobSettings(): Promise<JobSettings | null> {
  if (!has('getSettings')) return null;
  const reply = await call('getSettings');
  if (reply === undefined || !(JOBS_SETTINGS_KEY in settingsFromReply(reply))) return null;
  return jobSettingsFromReply(reply);
}

/** Read-modify-write of "jobs" alone. Resolves to what is stored afterwards. */
export async function saveJobSettings(next: JobSettings): Promise<JobSettings | null> {
  if (!canEditJobSettings()) return null;
  for (let attempt = 0; attempt < 3; attempt++) {
    const reply = await call('getSettings');
    if (!reply || typeof reply !== 'object') return null;   // can't confirm the blob — never clobber
    const merged = withJobSettings(settingsFromReply(reply), next);
    const revision = (reply as { settingsRevision?: unknown }).settingsRevision;
    try {
      const saved = typeof revision === 'string' && revision
        ? await callThrow('setSettings', merged, { expectedRevision: revision })
        : await callThrow('setSettings', merged);
      return parseJobSettings((saved as Record<string, unknown> | undefined)?.[JOBS_SETTINGS_KEY]);
    } catch (err) {
      if (!/settings changed since they were read/.test(String((err as Error)?.message ?? err))) throw err;
    }
  }
  return null;
}

/** A resources:list reply, defensively: numbers may arrive as strings. */
export function parseResources(reply: unknown): JobResource[] | null {
  const list = reply && typeof reply === 'object' ? (reply as { resources?: unknown }).resources : undefined;
  if (!Array.isArray(list)) return null;
  const out: JobResource[] = [];
  for (const raw of list) {
    if (!raw || typeof raw !== 'object') continue;
    const row = raw as Record<string, unknown>;
    const name = typeof row.name === 'string' ? row.name.trim() : '';
    if (!name) continue;
    const waiting = Math.max(0, Math.trunc(Number(row.waiting) || 0));
    const h = row.holder && typeof row.holder === 'object' ? row.holder as Record<string, unknown> : null;
    const heldSince = h && typeof h.heldSince === 'string' && h.heldSince ? h.heldSince : undefined;
    // The hold time is measured on the machine that holds it, so another
    // machine's clock never skews it. Absent until the holder stated its start.
    const heldForMs = h && Number(h.heldForMs) > 0 ? Number(h.heldForMs) : heldSince ? 0 : undefined;
    const holder = h && typeof h.workId === 'string' && h.workId
      ? { workId: h.workId, label: typeof h.label === 'string' ? h.label : undefined, heldSince, heldForMs }
      : undefined;
    out.push({ name, waiting, holder });
  }
  return out;
}

/** "2 h 04 min", "12 min", "menos de un minuto". */
export function fmtHeld(ms: number): string {
  const minutes = Math.floor(Math.max(0, ms) / 60_000);
  if (minutes < 1) return 'menos de un minuto';
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${String(minutes % 60).padStart(2, '0')} min`;
  const days = Math.floor(hours / 24);
  return `${days} d ${hours % 24} h`;
}

/** One resource in a line: who holds it and for how long, and the queue. */
export function resourceLine(resource: JobResource, elapsedMs = 0): string {
  if (!resource.holder) return resource.waiting > 0 ? `Libre · ${resource.waiting} en cola` : 'Libre';
  const label = resource.holder.label?.trim() || resource.holder.workId;
  let line = `Ocupado por «${label}»`;
  if (resource.holder.heldForMs !== undefined) line += ` desde hace ${fmtHeld(resource.holder.heldForMs + Math.max(0, elapsedMs))}`;
  if (resource.waiting > 0) line += ` · ${resource.waiting} en cola`;
  return line;
}
