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
// local by omission. Once a tagged id selects another machine, however, that
// destination is binding: an unavailable remote must fail visibly and must
// never turn into the same operation against this machine.
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
  ['chatCreate', 'chat:create'],
  ['chatQueueReplace', 'chat:queue-replace'],
  ['chatQueueResume', 'chat:queue-resume'],
  ['chatPresentationSave', 'chat:presentation-save'],
  ['chatRuntimeControlsSave', 'chat:runtime-controls-save'],
  ['chatDelete', 'chat:delete'],
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
  ['appChatRefreshPlanUsage', 'app-chat:refresh-plan-usage', (a) => [{ providerId: a[0] }]],
  ['appChatCloseSession', 'app-chat:close-session'],
  ['appChatDetectAcp', 'app-chat:detect-acp', (a) => [a[0] ?? {}]],
  ['appChatSteer', 'app-chat:steer', (a) => [{
    sessionId: a[0], prompt: a[1], images: a[2], clientUserMessageId: a[3],
    continuationAssistantMessageId: a[4], boundary: a[5],
  }]],
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
  ['createDir', 'fs:create-dir', (a) => [{ parent: a[0], name: a[1] }]],
  // The tagged chat id selects the owning machine; the daemon uses only cwd.
  ['projectIcon', 'project:icon', (a) => [{ chatId: a[0], cwd: a[1] }]],
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
  ['onSpawnedWorkChanged', 'spawned-work:changed'],
  ['onChatCommands', 'chat:commands'],
  ['onNotify', 'notify'],
  ['onNotifyBacklog', 'notify:backlog'],
];

// Most machine-wide snapshots are deliberately absent from REMOTE_EVENTS:
//
//   chat:plan-usage, providers:list, proc:changed,
//   providers:updates, providers:update-progress, and app:update.
//
// Catalog is the one admitted exception because the renderer now partitions it
// by chat owner: a remote chat cannot choose a model without the catalog of the
// daemon that will run it. It is machine-scoped below without tagging catalog
// fields (`ModeOption.id` is not an entity id). The other surfaces still have one
// unpartitioned projection and matching local-only actions. Forwarding one would
// therefore replace this window's provider/process/account/update state. The
// update variant was especially dangerous: a machine without Claude Code
// advertised another machine's Claude update, then sent the click to its own
// daemon. Keep those subscriptions on the owning window until both projection
// and action are machine-scoped.

const MACHINE_SCOPED_REMOTE_EVENTS = new Set<keyof WorkassApi>(['onChatCatalog']);
const remoteEventMachines = new WeakMap<object, string>();

/** Renderer-local ownership metadata. It never changes the frozen wire payload. */
export function projectRemoteEvent(method: keyof WorkassApi, machineId: string, payload: unknown): unknown {
  if (MACHINE_SCOPED_REMOTE_EVENTS.has(method) && payload !== null && typeof payload === 'object') {
    remoteEventMachines.set(payload, machineId);
    return payload;
  }
  return tagPayload(machineId, payload);
}

/** Which remote machine emitted a partitioned snapshot; '' means local. */
export function machineScopeOf(payload: unknown): string {
  return payload !== null && typeof payload === 'object' ? remoteEventMachines.get(payload) ?? '' : '';
}

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

export class RemoteMachineUnavailableError extends Error {
  readonly code = 'workass:remote-machine-unavailable';
  readonly machineId: string;

  constructor(machineId: string) {
    super(`remote machine ${machineId} is unavailable`);
    this.name = 'RemoteMachineUnavailableError';
    this.machineId = machineId;
  }
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
        if (machineId) throw new RemoteMachineUnavailableError(machineId);
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
      // Addressed events have their ids tagged. Partitioned machine snapshots
      // retain byte-identical catalog ids and carry ownership out-of-band.
      options.subscribeRemote(channel, (payload, machineId) => cb(projectRemoteEvent(method, machineId, payload)));
    };
  }

  return out as WorkassApi;
}

/** method → channel, so a replayed subscription can reach the remote sink
 *  without going through the router method (which would also re-subscribe the
 *  local bridge and duplicate every delivery). */
export const REMOTE_EVENT_CHANNELS: ReadonlyMap<keyof WorkassApi, string> = new Map(REMOTE_EVENTS);
