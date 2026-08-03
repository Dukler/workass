// The machines this client is mounted on, and their sockets.
//
// remote-plan E3: one connection per daemon, no broker. A machine that is down
// degrades to a greyed section of the list — it does not degrade the window, and
// it cannot take another machine down with it, because nothing is proxied
// through anything.
//
// Credentials are keyed PER MACHINE. That is the whole reason a lost phone costs
// you that phone rather than the fleet: each daemon issued its own token and
// each can be revoked alone.

import type { WorkassApi } from './types';
import { MachineSocket, type MachineCredentials, type MachineLinkState, type MachineSocketLike } from './machineSocket.ts';
import { createMachineRouter, REMOTE_EVENT_CHANNELS } from './machineRouter.ts';
import { tagPayload } from './machineIds.ts';

const CREDENTIAL_PREFIX = 'workass.machine.';

export interface MachineEndpoint {
  kind?: string;
  address: string;
}

/** One row of the machine book, as `machines:list` returns it. */
export interface MachineEntry {
  machineId: string;
  name: string;
  owner?: string;
  endpoints?: MachineEndpoint[];
  secure?: boolean;
  status?: string;
  reason?: string;
  fleetIds?: string[];
}

export interface MachineView {
  machineId: string;
  name: string;
  /** '' until a socket has been opened. */
  address: string;
  link: MachineLinkState;
  /** Why it is not usable, in words, when it is not. */
  reason: string;
  secure: boolean;
  /** True when this client holds a token for it. */
  paired: boolean;
}

/**
 * Adapt a browser WebSocket to the socket the transport expects.
 *
 * This exists because of a real integration bug: a browser delivers a
 * MessageEvent, not a string, so a transport wired straight to `new WebSocket`
 * parses nothing, never becomes ready, and hydrates no chats — silently. The
 * shim is also the seam a non-browser client replaces (React Native passes its
 * own), which is why the transport never touches `WebSocket` itself.
 */
export function browserSocket(url: string): MachineSocketLike {
  const ws = new WebSocket(url);
  const shim: MachineSocketLike = {
    send: (data) => ws.send(data),
    close: () => ws.close(),
    onopen: null, onclose: null, onmessage: null, onerror: null,
  };
  ws.onopen = () => shim.onopen?.();
  ws.onclose = () => shim.onclose?.();
  ws.onerror = (event) => shim.onerror?.(event);
  ws.onmessage = (event: MessageEvent) => shim.onmessage?.(String(event.data));
  return shim;
}

export interface MachineRegistryOptions {
  local(): WorkassApi | undefined;
  deviceName: string;
  /** Defaults to the browser adapter above. */
  open?(url: string): MachineSocketLike;
  storage?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;
  onChange?(machines: MachineView[]): void;
  /** A machine reconnected. `restarted` means it lost everything it was holding. */
  onOpen?(machineId: string, info: { generation: number; restarted: boolean }): void;
}

/** An event from one machine, with the machine it came from so ids can be tagged. */
export type RemoteEventListener = (payload: unknown, machineId: string) => void;

export class MachineRegistry {
  private readonly opts: MachineRegistryOptions;
  private readonly entries = new Map<string, MachineEntry>();
  private readonly sockets = new Map<string, MachineSocket>();
  private readonly ready = new Map<string, MachineSocket>();
  private readonly states = new Map<string, { link: MachineLinkState; reason: string }>();
  private readonly remoteSubs = new Map<string, Set<RemoteEventListener>>();
  private readonly replayed = new Map<string, Set<unknown>>();
  private fleetKey = '';

  constructor(opts: MachineRegistryOptions) {
    this.opts = opts;
  }

  /** The router the api accessor should use, or undefined when nothing is mounted. */
  router(): WorkassApi | undefined {
    if (!this.sockets.size) return undefined;
    return createMachineRouter({
      local: () => this.opts.local(),
      links: () => this.ready,
      subscribeRemote: (channel, cb) => this.subscribeRemote(channel, cb),
    });
  }

  /**
   * Replay a handler subscribed before this machine existed, by API method.
   *
   * Deduped by the ORIGINAL callback, not the wrapper: the router is rebuilt on
   * every machine-book change, so replaying without this would stack one extra
   * delivery of every event per rebuild — a transcript that repeats itself.
   */
  subscribeRemoteMethod(method: keyof WorkassApi, cb: (payload: unknown) => void): void {
    const channel = REMOTE_EVENT_CHANNELS.get(method);
    if (!channel) return;
    let seen = this.replayed.get(channel);
    if (!seen) this.replayed.set(channel, (seen = new Set()));
    if (seen.has(cb)) return;
    seen.add(cb);
    this.subscribeRemote(channel, (payload, machineId) => cb(tagPayload(machineId, payload)));
  }

  /** Held in memory only. It is used to enrol and never written to storage. */
  useFleetKey(key: string): void {
    this.fleetKey = String(key ?? '').trim();
  }

  list(): MachineView[] {
    return Array.from(this.entries.values()).map((entry) => {
      const state = this.states.get(entry.machineId);
      return {
        machineId: entry.machineId,
        name: entry.name || entry.machineId,
        address: firstAddress(entry),
        link: state?.link ?? 'idle',
        reason: state?.reason || entry.reason || '',
        secure: !!entry.secure,
        paired: !!this.credentials(entry.machineId).deviceToken,
      };
    });
  }

  /** The ready link for one machine, or undefined while it is not usable. */
  linkFor(machineId: string): MachineSocket | undefined {
    return this.ready.get(machineId);
  }

  /**
   * What the machine is doing in words, past the socket being open.
   *
   * A connected machine whose chats never arrive is the worst failure this
   * feature has: the pane says "conectada", the list stays empty, and nothing
   * anywhere says why. Reading its session is a second step that can fail on its
   * own, so it reports on its own.
   */
  setReason(machineId: string, reason: string): void {
    const state = this.states.get(machineId);
    if (!state) return;
    state.reason = String(reason ?? '');
    this.opts.onChange?.(this.list());
  }

  names(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const entry of this.entries.values()) out[entry.machineId] = entry.name || entry.machineId;
    return out;
  }

  /**
   * Adopt the machine book. Machines that left the book are disconnected;
   * machines that arrived are connected if we can reach them. Called on every
   * `machines:changed`, so it must be idempotent.
   */
  sync(entries: MachineEntry[], selfMachineId = ''): void {
    const seen = new Set<string>();
    for (const entry of entries) {
      const id = String(entry?.machineId ?? '').trim();
      // The local machine is served by the injected bridge. Opening a second
      // socket to ourselves would duplicate every chat in the list.
      if (!id || id === selfMachineId) continue;
      seen.add(id);
      this.entries.set(id, entry);
      if (!this.sockets.has(id)) this.connect(entry);
    }
    for (const id of Array.from(this.sockets.keys())) {
      if (seen.has(id)) continue;
      this.sockets.get(id)?.close();
      this.sockets.delete(id);
      this.ready.delete(id);
      this.entries.delete(id);
      this.states.delete(id);
    }
    this.opts.onChange?.(this.list());
  }

  /** Drop a machine and forget what it gave us. Its chats leave the list. */
  forget(machineId: string): void {
    this.sockets.get(machineId)?.close();
    this.sockets.delete(machineId);
    this.ready.delete(machineId);
    this.entries.delete(machineId);
    this.states.delete(machineId);
    this.clearCredentials(machineId);
    this.opts.onChange?.(this.list());
  }

  closeAll(): void {
    for (const socket of this.sockets.values()) socket.close();
    this.sockets.clear();
    this.ready.clear();
  }

  private connect(entry: MachineEntry): void {
    const address = firstAddress(entry);
    if (!address) return;
    const socket = new MachineSocket({
      machineId: entry.machineId,
      // Plain ws until E5. `secure` on the card is what the badge reads, and it
      // is what decides this, so an encrypted machine needs no client change.
      url: (entry.secure ? 'wss://' : 'ws://') + address,
      deviceName: this.opts.deviceName,
      credentials: this.credentials(entry.machineId),
      fleetKey: this.fleetKey || undefined,
      open: this.opts.open ?? browserSocket,
      saveCredentials: (id, next) => this.saveCredentials(id, next),
      clearCredentials: (id) => this.clearCredentials(id),
      onState: (state, detail) => {
        this.states.set(entry.machineId, { link: state, reason: detail?.reason ?? '' });
        if (state === 'ready') this.ready.set(entry.machineId, socket);
        else this.ready.delete(entry.machineId);
        this.opts.onChange?.(this.list());
      },
      onOpen: (info) => this.opts.onOpen?.(entry.machineId, info),
      // Every event from every machine funnels here, so a subscription outlives
      // the socket that happened to exist when it was made. Subscribing to the
      // link directly is what broke: the store subscribes at boot, no machine is
      // connected yet, and the remote's events were delivered to nobody.
      onEvent: (channel, payload) => this.dispatchRemoteEvent(entry.machineId, channel, payload),
    });
    this.sockets.set(entry.machineId, socket);
    socket.connect();
  }

  /**
   * Durable subscription to one event channel across every machine, present and
   * future. Deduped by callback, because the router is rebuilt on every machine
   * book change and would otherwise stack a delivery per rebuild.
   */
  subscribeRemote(channel: string, cb: RemoteEventListener): void {
    let set = this.remoteSubs.get(channel);
    if (!set) this.remoteSubs.set(channel, (set = new Set()));
    set.add(cb);
  }

  private dispatchRemoteEvent(machineId: string, channel: string, payload: unknown): void {
    const set = this.remoteSubs.get(channel);
    if (!set) return;
    for (const cb of Array.from(set)) {
      try { cb(payload, machineId); } catch { /* one bad subscriber must not stop the rest */ }
    }
  }

  private storage(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> | undefined {
    if (this.opts.storage) return this.opts.storage;
    try { return typeof localStorage !== 'undefined' ? localStorage : undefined; } catch { return undefined; }
  }

  private credentials(machineId: string): MachineCredentials {
    try {
      const raw = this.storage()?.getItem(CREDENTIAL_PREFIX + machineId);
      return raw ? (JSON.parse(raw) as MachineCredentials) : {};
    } catch { return {}; }
  }

  private saveCredentials(machineId: string, next: MachineCredentials): void {
    try { this.storage()?.setItem(CREDENTIAL_PREFIX + machineId, JSON.stringify(next)); } catch { /* private mode */ }
    this.opts.onChange?.(this.list());
  }

  private clearCredentials(machineId: string): void {
    try { this.storage()?.removeItem(CREDENTIAL_PREFIX + machineId); } catch { /* private mode */ }
  }
}

function firstAddress(entry: MachineEntry): string {
  const endpoints = Array.isArray(entry?.endpoints) ? entry.endpoints : [];
  // D7: a machine has endpoints, not an address. The first that answers wins;
  // a VPN endpoint arriving later is another entry here, not a new mechanism.
  for (const endpoint of endpoints) {
    const address = String(endpoint?.address ?? '').trim();
    if (address) return address;
  }
  return '';
}
