// One seam decides which daemon a call goes to.
//
// The alternative was threading a machine through every store method that takes
// a chat id — hundreds of call sites, each an opportunity to address the wrong
// machine. Instead the id itself carries the answer: a tagged id routes to its
// machine and is stripped on the way out, an untagged one is local and takes the
// path it always took. Nothing above this file knows machines exist.
//
// The method→channel table is copied from `internal/httpserve/lan_bridge.go`,
// which PORT-SPEC §2 names as the authoritative client-side enumeration. Only
// the methods that make sense against ANOTHER machine are here; the rest stay
// local by omission, which is the safe direction — a missing remote method
// degrades to the local daemon rather than silently addressing the wrong box.
//
// The table is shorter than the bridge's object on purpose: `WorkassApi` in
// types.ts declares only what the store actually calls, and a method the store
// cannot call is not worth a routing rule that could be wrong.

import type { WorkassApi } from './types';
import type { MachineSocket } from './machineSocket.ts';
import { isTagged, machineOf, tagPayload, untagFor } from './machineIds.ts';

type Mapper = (args: unknown[]) => unknown[];

/** [method, channel, argument shape]. Absent mapper means pass the args through. */
const REMOTE_METHODS: Array<[keyof WorkassApi, string, Mapper?]> = [
  ['appMeta', 'app:meta'],
  ['stateDigest', 'state:digest'],
  ['getSettings', 'settings:get'],
  ['getConfig', 'config:get'],
  ['getSession', 'session:get'],
  // `saveSession` is deliberately ABSENT. It is a whole-store write with no
  // single addressee, so there is no id that could route it correctly — and
  // routing it by "some tagged id in the payload" sent this Mac's entire chat
  // list into builder's session store (observed 2026-07-26: 18 chats, every
  // one of them local, with Mac cwds). A machine's session belongs to that
  // machine; this client reads it and never writes it.
  ['archiveAppend', 'chat:archive-append', (a) => [{ tabId: a[0], messages: a[1] }]],
  ['archiveLoad', 'chat:archive-load'],
  ['visualizeHost', 'visualize:host'],
  ['startJob', 'job:start'],
  ['cancelJob', 'job:cancel'],
  ['appChatReset', 'app-chat:reset'],
  ['appChatNewSession', 'app-chat:new-session'],
  ['appChatCloseSession', 'app-chat:close-session'],
  ['appChatDetectAcp', 'app-chat:detect-acp', (a) => [a[0] ?? {}]],
  ['appChatSteer', 'app-chat:steer', (a) => [{
    sessionId: a[0], prompt: a[1], images: a[2], clientUserMessageId: a[3],
    continuationAssistantMessageId: a[4], boundary: a[5],
  }]],
  ['appChatSetModel', 'app-chat:set-model', (a) => [{ sessionId: a[0], modelId: a[1] }]],
  ['appChatSetMode', 'app-chat:set-mode', (a) => [{ sessionId: a[0], modeId: a[1] }]],
  ['chatCheckpoints', 'chat:checkpoints'],
  ['chatRewind', 'chat:rewind'],
  ['chatDiff', 'chat:diff'],
  ['chatEnvGet', 'chat:env-get'],
  ['chatPermissionDecide', 'chat:permission-decide', (a) => [{ id: a[0], optionId: a[1] }]],
  ['chatPendingPermissions', 'chat:permissions-pending'],
  ['spawnedWorkList', 'spawned-work:list', (a) => [{ tabId: a[0], chatId: a[1] }]],
  ['chatCommandsGet', 'chat:commands-get', (a) => [{ tabId: a[0], chatId: a[1] }]],
  ['spawnedWorkRead', 'spawned-work:read', (a) => [{ tabId: a[0], chatId: a[1], id: a[2], tailBytes: a[3] }]],
  // Stopping work on ANOTHER machine is the point of routing it: the row is
  // rendered here and the process runs there.
  ['spawnedWorkStop', 'spawned-work:stop', (a) => [{ tabId: a[0], chatId: a[1], id: a[2] }]],
  ['providersList', 'providers:list'],
  ['providersDetect', 'providers:detect', (a) => [a[0] ?? {}]],
  ['procList', 'proc:list'],
  ['procKill', 'proc:kill', (a) => [{ id: a[0], tree: a[1] }]],
  // E4 — listing paths on the machine that runs the agent, which is the only
  // machine where they mean anything.
  ['listDir', 'fs:list-dir'],
];

/** [method, channel] for every event a remote machine may raise. */
const REMOTE_EVENTS: Array<[keyof WorkassApi, string]> = [
  ['onJobEvent', 'job:event'],
  ['onChatCatalog', 'chat:catalog'],
  ['onChatSessionReplaced', 'chat:session-replaced'],
  ['onChatCompacted', 'chat:compacted'],
  ['onChatCheckpointRestored', 'chat:checkpoint-restored'],
  ['onChatEngineRecovered', 'chat:engine-recovered'],
  ['onChatEnv', 'chat:env'],
  ['onChatPermissionRequest', 'chat:permission-request'],
  ['onChatPermissionResolved', 'chat:permission-resolved'],
  ['onChatPlanUsage', 'chat:plan-usage'],
  ['onSpawnedWorkChanged', 'spawned-work:changed'],
  ['onChatCommands', 'chat:commands'],
  ['onProcChanged', 'proc:changed'],
  ['onProvidersList', 'providers:list'],
  ['onProvidersUpdates', 'providers:updates'],
  ['onProvidersUpdateProgress', 'providers:update-progress'],
  ['onNotify', 'notify'],
  ['onNotifyBacklog', 'notify:backlog'],
];

/**
 * Which machine a call is for, found by looking for a tagged id anywhere in the
 * arguments. Tagging is unambiguous — an id either carries the `M~` prefix or it
 * does not — so this needs no per-method knowledge of which argument is the id.
 */
export function routeOf(args: unknown[]): string {
  let found = '';
  // Bounded on purpose. Every addressing id in the table above is a positional
  // argument or a top-level field of the first options object; nothing is
  // addressed from inside a nested array of chats or messages. An unbounded walk
  // let a BULK payload decide the destination — one tagged id buried in a
  // session snapshot was enough to redirect the whole write to another machine.
  const visit = (value: unknown, depth: number): void => {
    if (found || depth > 2) return;
    if (typeof value === 'string') { if (isTagged(value)) found = machineOf(value); return; }
    if (Array.isArray(value)) { for (const item of value) visit(item, depth + 1); return; }
    if (value && typeof value === 'object') { for (const item of Object.values(value)) visit(item, depth + 1); }
  };
  visit(args, 0);
  return found;
}

export interface MachineRouterOptions {
  /** The injected bridge for this machine. Every untagged call goes here. */
  local(): WorkassApi | undefined;
  /** Ready links, by machine id. A machine that is down is simply absent. */
  links(): ReadonlyMap<string, MachineSocket>;
  /**
   * Durable subscription to one channel across every machine, present and
   * future. Binding to `links()` here would capture whatever happened to be
   * connected at subscribe time — which at boot is nothing, so a remote turn ran
   * to completion and the client never heard a word.
   */
  subscribeRemote(channel: string, cb: (payload: unknown, machineId: string) => void): void;
}

/**
 * Build the `WorkassApi` the store consumes. Every method the local bridge
 * exposes stays exposed — feature detection upstream depends on the SHAPE of
 * this object, so a method must not disappear just because a remote lacks it.
 */
export function createMachineRouter(options: MachineRouterOptions): WorkassApi {
  const local = () => options.local();
  const linkFor = (machineId: string) => (machineId ? options.links().get(machineId) : undefined);
  const out: Record<string, unknown> = {};

  // Start from the local bridge so anything not routed keeps working exactly as
  // it does today, including methods added to the bridge after this was written.
  const base = local();
  if (base) for (const key of Object.keys(base) as Array<keyof WorkassApi>) out[key] = (base as Record<string, unknown>)[key];

  for (const [method, channel, mapper] of REMOTE_METHODS) {
    out[method] = async (...args: unknown[]) => {
      const machineId = routeOf(args);
      const link = linkFor(machineId);
      if (!link) {
        const fn = (local() as Record<string, unknown> | undefined)?.[method];
        return typeof fn === 'function' ? (fn as (...a: unknown[]) => unknown)(...args) : undefined;
      }
      // Out: strip the tag, because the daemon only knows its own ids.
      // Back in: retag, because the store holds three machines in one list.
      const outbound = untagFor(machineId, mapper ? mapper(args) : args);
      const result = await link.invoke(channel, ...outbound);
      return tagPayload(machineId, result);
    };
  }

  for (const [method, channel] of REMOTE_EVENTS) {
    out[method] = (cb: (payload: unknown) => void) => {
      const localFn = (local() as Record<string, unknown> | undefined)?.[method];
      if (typeof localFn === 'function') (localFn as (c: unknown) => void)(cb);
      // A remote's events are tagged on arrival, so a permission card raised on
      // another machine addresses a chat this client can actually find.
      options.subscribeRemote(channel, (payload, machineId) => cb(tagPayload(machineId, payload)));
    };
  }

  return out as WorkassApi;
}

export const ROUTED_METHODS: ReadonlyArray<keyof WorkassApi> = REMOTE_METHODS.map(([method]) => method);
export const ROUTED_EVENTS: ReadonlyArray<keyof WorkassApi> = REMOTE_EVENTS.map(([method]) => method);

/** method → channel, so a replayed subscription can reach the remote sink
 *  without going through the router method (which would also re-subscribe the
 *  local bridge and duplicate every delivery). */
export const REMOTE_EVENT_CHANNELS: ReadonlyMap<keyof WorkassApi, string> = new Map(REMOTE_EVENTS);
