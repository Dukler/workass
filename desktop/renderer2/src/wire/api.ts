// Feature-detecting accessor for the daemon-injected window.api bridge.
// Every consumer goes through `api` (a proxy that tolerates a missing method)
// and `has(name)` for capability checks driving graceful degradation.

import type { WorkassApi } from './types';

// E3: when machines are mounted, every call goes through a router that picks a
// daemon by the tag on the id it was given. With no machines mounted this is
// undefined and the accessor behaves exactly as it did before — which is the
// property that keeps a single-machine install byte-for-byte unchanged.
let router: WorkassApi | undefined;

// Event subscriptions are made once, at boot, against whatever bridge exists
// then — which is the LOCAL one, because a machine's socket is not open yet and
// mounting is async. Without replaying them onto the router, a remote turn runs
// to completion and the client never hears a word: the file is written, the
// engine goes idle, and the transcript sits on "Trabajando…" forever. Found
// exactly that way against builder.
const eventSubscriptions: Array<[keyof WorkassApi, unknown]> = [];

/** Remote-only replay. Never the router's own method: that also re-subscribes
 *  the LOCAL bridge, and a second local subscription duplicates every chunk of
 *  every message. The registry dedupes by callback, so replaying on each book
 *  change is safe. */
type RemoteSubscribe = (method: keyof WorkassApi, cb: unknown) => void;

export function setMachineRouter(next: WorkassApi | undefined, subscribeRemote?: RemoteSubscribe): void {
  router = next;
  if (!next || !subscribeRemote) return;
  for (const [name, cb] of eventSubscriptions) {
    try { subscribeRemote(name, cb); } catch (err) { console.warn(`[api] ${String(name)} remote subscribe failed`, err); }
  }
}

function bridge(): WorkassApi | undefined {
  if (router) return router;
  return typeof window !== 'undefined' ? window.api : undefined;
}

export function has<K extends keyof WorkassApi>(name: K): boolean {
  const b = bridge();
  return !!b && typeof b[name] === 'function';
}

/** Call a bridge method if present; returns undefined if the method is absent. */
export async function call<K extends keyof WorkassApi>(
  name: K,
  ...args: Parameters<NonNullable<WorkassApi[K]>>
): Promise<Awaited<ReturnType<NonNullable<WorkassApi[K]>>> | undefined> {
  const b = bridge();
  const fn = b?.[name] as ((...a: unknown[]) => unknown) | undefined;
  if (typeof fn !== 'function') return undefined;
  try {
    return (await fn(...args)) as Awaited<ReturnType<NonNullable<WorkassApi[K]>>>;
  } catch (err) {
    console.warn(`[api] ${String(name)} threw`, err);
    return undefined;
  }
}

/** Subscribe to an event channel if the bridge exposes it. */
export function on<K extends 'onJobEvent' | 'onChatCatalog' | 'onChatSessionReplaced' | 'onChatPermissionRequest' | 'onChatPermissionResolved' | 'onChatPlanUsage' | 'onSpawnedWorkChanged' | 'onChatEnv' | 'onProcChanged' | 'onLanAccessRequest' | 'onProvidersList' | 'onProvidersUpdates' | 'onProvidersUpdateProgress' | 'onAppUpdate' | 'onChatCompacted' | 'onChatCheckpointRestored' | 'onNotify' | 'onNotifyBacklog' | 'onAgentApply' | 'onChatCommands' | 'onMachinesChanged'>(
  name: K,
  cb: Parameters<NonNullable<WorkassApi[K]>>[0],
): void {
  // Recorded before subscribing, so a machine mounted later can be given the
  // same handlers. Boot subscribes long before any machine socket is ready.
  eventSubscriptions.push([name, cb as unknown]);
  const b = bridge();
  const fn = b?.[name] as ((c: unknown) => void) | undefined;
  if (typeof fn === 'function') {
    try { fn(cb as unknown); } catch (err) { console.warn(`[api] ${String(name)} subscribe failed`, err); }
  }
}

/**
 * Like `call`, but rethrows instead of swallowing — needed for channels whose
 * `reply.error` carries a structured JSON refusal we must render (e.g.
 * chat:rewind's `chat:rewind-outside-modification`). Returns undefined only when
 * the bridge lacks the method entirely.
 */
export async function callThrow<K extends keyof WorkassApi>(
  name: K,
  ...args: Parameters<NonNullable<WorkassApi[K]>>
): Promise<Awaited<ReturnType<NonNullable<WorkassApi[K]>>> | undefined> {
  const b = bridge();
  const fn = b?.[name] as ((...a: unknown[]) => unknown) | undefined;
  if (typeof fn !== 'function') return undefined;
  return (await fn(...args)) as Awaited<ReturnType<NonNullable<WorkassApi[K]>>>;
}

export function bridgeReady(): boolean {
  return !!bridge();
}
