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
const AUTO_CONNECT_DISABLED_SUFFIX = '.auto-connect-disabled';

export interface MachineEndpoint {
  kind?: string;
  address: string;
}

/** One row of the machine book, as `machines:list` returns it. */
export interface MachineEntry {
  machineId: string;
  name: string;
  nickname?: string;
  owner?: string;
  endpoints?: MachineEndpoint[];
  secure?: boolean;
  certFingerprint?: string;
  addedBy?: string;
  status?: string;
  reason?: string;
  fleetIds?: string[];
}

export interface MachineView {
  machineId: string;
  name: string;
  /** Name reported by the peer, before this controller's optional nickname. */
  reportedName: string;
  nickname: string;
  /** '' until a socket has been opened. */
  address: string;
  link: MachineLinkState;
  /** Why it is not usable, in words, when it is not. */
  reason: string;
  secure: boolean;
	reachable: boolean;
  /** True when this client holds a token for it. */
  paired: boolean;
  /** Discovered machines remain passive until the user requests access. */
  requested: boolean;
  discovered: boolean;
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
  /**
   * The user's authority to read this machine ended. The store must evict every
   * projection owned by this machine synchronously; an ordinary network close
   * deliberately does not fire this because offline chats remain visible.
   */
  onUnmount?(machineId: string): void;
	/** Deterministic scheduler seam used by transport lifecycle tests. */
	setTimer?(fn: () => void, ms: number): unknown;
	clearTimer?(handle: unknown): void;
	trustEndpoint?(address: string, certFingerprint: string): boolean;
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
  private readonly requested = new Set<string>();
  /** Machines whose chats have been admitted into the mounted projection. */
  private readonly mounted = new Set<string>();
  /** Current-runtime authority even when browser storage is unavailable. */
  private readonly autoConnectDisabledMachines = new Set<string>();
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
      const reportedName = String(entry.name || entry.machineId).trim() || entry.machineId;
      const nickname = String(entry.nickname ?? '').trim();
      return {
        machineId: entry.machineId,
        name: nickname || reportedName,
        reportedName,
        nickname,
        address: firstAddress(entry),
        link: state?.link ?? 'idle',
        reason: state?.reason || entry.reason || '',
        secure: !!entry.secure,
		reachable: !entry.status || entry.status === 'ok',
        paired: !!this.credentials(entry.machineId).deviceToken,
        requested: this.requested.has(entry.machineId),
        discovered: entry.addedBy === 'beacon',
      };
    });
  }

  /** The ready link for one machine, or undefined while it is not usable. */
  linkFor(machineId: string): MachineSocket | undefined {
    return this.ready.get(machineId);
  }

  /**
   * Proves that an async read still belongs to the currently-authorized link.
   * Socket generation alone is insufficient because forget/reject may replace
   * the whole MachineSocket while an already-resolved promise is queued.
   */
  ownsLink(machineId: string, link: MachineSocket): boolean {
    return this.ready.get(machineId) === link && this.sockets.get(machineId) === link;
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
    for (const machine of this.list()) out[machine.machineId] = machine.name;
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
		const credentials = this.credentials(id);
		const fingerprint = normalizeFingerprint(entry.certFingerprint);
		if (credentials.deviceToken && (!credentials.certFingerprint || credentials.certFingerprint !== fingerprint)) {
			// Old releases did not persist the TLS identity. Never send their token
			// under TOFU after an upgrade, and never follow a changed certificate.
			// Keep the discovered machine row so one explicit approval can pair it
			// again under the fingerprint it currently proves.
			this.detachSocket(id);
			this.clearCredentials(id);
			this.disableAutoConnect(id);
			this.unmount(id);
			this.states.set(id, { link: 'idle', reason: credentials.certFingerprint ? 'la identidad TLS cambió; volvé a aprobarla' : 'volvé a aprobarla para fijar su identidad TLS' });
			continue;
		}
      // The explicit request path already refuses an endpoint that did not
      // prove TLS. Automatic reconnect must enforce the same gate: otherwise a
      // previously paired machine that later becomes stale/insecure opens a
      // doomed wss:// socket every 1.5s forever. Keep its book entry and
      // credential so a later secure refresh reconnects without re-pairing.
		if (!entry.secure || (entry.status && entry.status !== 'ok')) {
			this.detachSocket(id);
			continue;
		}
      // Seeing a beacon never opens a socket or prompts the other machine.
      // A past approval reconnects automatically; a new request is always an
      // explicit local action, except the separately opted-in fleet mechanism.
      const automatic = !this.autoConnectDisabled(id) && (this.credentials(id).deviceToken || this.fleetKey);
      if (!this.sockets.has(id) && (this.requested.has(id) || automatic)) this.connect(entry);
    }
    for (const id of Array.from(this.entries.keys())) {
      if (seen.has(id)) continue;
		this.detachSocket(id);
      this.entries.delete(id);
      this.states.delete(id);
		this.requested.delete(id);
		this.unmount(id);
    }
    this.opts.onChange?.(this.list());
  }

  /** Drop a machine and forget what it gave us. Its chats leave the list. */
  forget(machineId: string): void {
		this.disableAutoConnect(machineId);
		this.detachSocket(machineId);
    this.entries.delete(machineId);
    this.states.delete(machineId);
    this.requested.delete(machineId);
    this.clearCredentials(machineId);
		this.unmount(machineId);
    this.opts.onChange?.(this.list());
  }

  closeAll(): void {
    for (const socket of this.sockets.values()) socket.close();
    this.sockets.clear();
    this.ready.clear();
  }

  /** Open one TLS socket to a discovered candidate after the user clicks Request access. */
  requestAccess(machineId: string): boolean {
    const entry = this.entries.get(machineId);
	if (!entry || !entry.secure || (entry.status && entry.status !== 'ok')) return false;
	this.enableAutoConnect(machineId);
    this.requested.add(machineId);
    if (!this.sockets.has(machineId)) this.connect(entry);
    this.opts.onChange?.(this.list());
    return true;
  }

  private connect(entry: MachineEntry): void {
    const address = firstAddress(entry);
    if (!address) return;
	const fingerprint = normalizeFingerprint(entry.certFingerprint);
	const trust = this.opts.trustEndpoint ?? trustMachineEndpoint;
	if (!fingerprint || !trust(address, fingerprint)) {
		this.states.set(entry.machineId, { link: 'idle', reason: 'no se pudo fijar la identidad TLS' });
		this.opts.onChange?.(this.list());
		return;
	}
    const socket = new MachineSocket({
      machineId: entry.machineId,
      // There is no plaintext fallback. A stale/old discovery card is shown as
      // unavailable rather than allowing a message, token, or transcript onto
      // an unencrypted socket.
      url: 'wss://' + address,
      deviceName: this.opts.deviceName,
      credentials: this.credentials(entry.machineId),
		certFingerprint: fingerprint,
      fleetKey: this.fleetKey || undefined,
      open: this.opts.open ?? browserSocket,
      saveCredentials: (id, next) => this.saveCredentials(id, next),
      clearCredentials: (id) => this.clearCredentials(id),
      onState: (state, detail) => {
		if (this.sockets.get(entry.machineId) !== socket) return;
        if (state === 'ready' || state === 'rejected') this.requested.delete(entry.machineId);
        this.states.set(entry.machineId, { link: state, reason: detail?.reason ?? '' });
		if (state === 'ready') {
			this.ready.set(entry.machineId, socket);
			this.mounted.add(entry.machineId);
		} else {
			this.ready.delete(entry.machineId);
		}
		if (state === 'rejected') {
			// A human denial is terminal for automatic reconnect, including after
			// reload with a fleet key. Only a later explicit Request access clears it.
			this.disableAutoConnect(entry.machineId);
			this.sockets.delete(entry.machineId);
			socket.close();
			this.unmount(entry.machineId);
		}
        this.opts.onChange?.(this.list());
      },
		onOpen: (info) => {
			if (this.ownsLink(entry.machineId, socket)) this.opts.onOpen?.(entry.machineId, info);
		},
		setTimer: this.opts.setTimer,
		clearTimer: this.opts.clearTimer,
      // Every event from every machine funnels here, so a subscription outlives
      // the socket that happened to exist when it was made. Subscribing to the
      // link directly is what broke: the store subscribes at boot, no machine is
      // connected yet, and the remote's events were delivered to nobody.
		onEvent: (channel, payload) => {
			if (this.sockets.get(entry.machineId) === socket) this.dispatchRemoteEvent(entry.machineId, channel, payload);
		},
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

	private autoConnectDisabled(machineId: string): boolean {
		if (this.autoConnectDisabledMachines.has(machineId)) return true;
		try {
			const disabled = this.storage()?.getItem(CREDENTIAL_PREFIX + machineId + AUTO_CONNECT_DISABLED_SUFFIX) === '1';
			if (disabled) this.autoConnectDisabledMachines.add(machineId);
			return disabled;
		}
		catch { return false; }
	}

	private disableAutoConnect(machineId: string): void {
		this.autoConnectDisabledMachines.add(machineId);
		try { this.storage()?.setItem(CREDENTIAL_PREFIX + machineId + AUTO_CONNECT_DISABLED_SUFFIX, '1'); }
		catch { /* private mode: the in-memory authority still remains fenced */ }
	}

	private enableAutoConnect(machineId: string): void {
		this.autoConnectDisabledMachines.delete(machineId);
		try { this.storage()?.removeItem(CREDENTIAL_PREFIX + machineId + AUTO_CONNECT_DISABLED_SUFFIX); }
		catch { /* private mode: the explicit request still opens now */ }
	}

	/** Remove one transport before closing it so every synchronous late callback is stale. */
	private detachSocket(machineId: string): void {
		const socket = this.sockets.get(machineId);
		this.sockets.delete(machineId);
		this.ready.delete(machineId);
		if (socket) socket.close();
	}

	private unmount(machineId: string): void {
		if (!this.mounted.delete(machineId)) return;
		this.opts.onUnmount?.(machineId);
	}
}

function normalizeFingerprint(value: unknown): string {
	const fingerprint = String(value ?? '').replaceAll(':', '').trim().toLowerCase();
	return /^[a-f0-9]{64}$/.test(fingerprint) ? fingerprint : '';
}

function trustMachineEndpoint(address: string, certFingerprint: string): boolean {
	try {
		const bridge = typeof window !== 'undefined' ? window.workassMachines : undefined;
		return bridge ? bridge.trustEndpoint({ address, certFingerprint }) : true;
	} catch {
		return false;
	}
}

declare global {
	interface Window {
		workassMachines?: { trustEndpoint(payload: { address: string; certFingerprint: string }): boolean };
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
