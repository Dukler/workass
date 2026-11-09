// The events Ajustes · General can announce (store.fireNotify shows them):
// an agent that needs you, a turn that ended, and background work that ended.
// This file decides which background work counts and how a notice sounds.

import type { SpawnedWorkItem } from './wire/types';
import { isAgentWork, isCancelledWork } from './subagent-layout.ts';

/** An ending reported later than this (a replay after a reconnect, a list
 *  read long after) is old news and announces nothing. */
export const NOTICE_FRESH_MS = 2 * 60_000;

export function freshEnding(finishedAt: string | null | undefined, now = Date.now()): boolean {
  if (!finishedAt) return true;
  const at = Date.parse(finishedAt);
  return !Number.isFinite(at) || now - at <= NOTICE_FRESH_MS;
}

/** The background work in `next` that was running in `prev` and has ended:
 *  jobs, commands and servers, never a subagent (its chat's turn speaks for
 *  it) and never work someone stopped. */
export function finishedBackgroundWork(prev: readonly SpawnedWorkItem[] | undefined, next: readonly SpawnedWorkItem[], now = Date.now()): SpawnedWorkItem[] {
  if (!prev?.length) return [];
  const wasRunning = new Set(prev.filter((item) => item.status === 'running').map((item) => item.id));
  return next.filter((item) => wasRunning.has(item.id) && item.status !== 'running'
    && !isAgentWork(item) && !isCancelledWork(item) && freshEnding(item.finishedAt, now));
}

export function workEndedFailed(item: Pick<SpawnedWorkItem, 'status' | 'exitCode'>): boolean {
  return item.status === 'failed' || (typeof item.exitCode === 'number' && item.exitCode !== 0);
}

export function finishedWorkText(item: Pick<SpawnedWorkItem, 'status' | 'exitCode' | 'label'>): string {
  const label = (item.label || '').trim() || 'Un trabajo en segundo plano';
  return workEndedFailed(item) ? `Falló: ${label}` : `Terminó: ${label}`;
}

let chimeContext: AudioContext | null = null;

/** A short two-note chime. Silent where there is no Web Audio. */
export function playNoticeChime(): void {
  try {
    const Ctor = (globalThis as { AudioContext?: typeof AudioContext }).AudioContext;
    if (!Ctor) return;
    chimeContext ??= new Ctor();
    const ctx = chimeContext;
    if (ctx.state === 'suspended') void ctx.resume();
    const start = ctx.currentTime + 0.01;
    for (const [i, freq] of [880, 1318.5].entries()) {
      const at = start + i * 0.11;
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.type = 'sine';
      osc.frequency.value = freq;
      gain.gain.setValueAtTime(0.0001, at);
      gain.gain.exponentialRampToValueAtTime(0.12, at + 0.015);
      gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.22);
      osc.connect(gain).connect(ctx.destination);
      osc.start(at);
      osc.stop(at + 0.24);
    }
  } catch { /* no audio device: the notice still shows */ }
}
