// One socket to one daemon.
//
// The browser bridge (`internal/httpserve/lan_bridge.go`) can only ever talk to
// the origin that injected it. E3 needs a client that holds one connection per
// machine, so this is that bridge's semantics reimplemented where the client can
// aim it — plus the two things the bridge never had to do: enrol itself with a
// fleet key, and notice that the daemon on the other end restarted.
//
// Everything here is a faithful port and the comments say which line it mirrors.
// Where this and lan_bridge.go disagree, that file wins: the renderer must keep
// working unchanged against both.

import { answerFleetChallenge, type FleetChallenge } from './fleet.ts';

export const INVOKE_TIMEOUT_MS = 30_000;
export const SESSION_INVOKE_TIMEOUT_MS = 120_000;
export const RECONNECT_DELAY_MS = 1_500;
const SESSION_CHANNELS = new Set(['session:get', 'session:save']);

/**
 * The channels an unapproved client may send. Everything else queues until the
 * daemon approves it — and these are exactly the calls that PRODUCE an approval,
 * so queueing them deadlocks: the client waits for an approval only enrolment
 * can grant while enrolment waits in the queue for that approval.
 * Mirrors PRE_READY_CHANNELS in lan_bridge.go.
 */
export const PRE_READY_CHANNELS = new Set(['lan:pairing-info', 'fleet:challenge', 'fleet:enroll']);

export type InvokeErrorCode = 'socket-replaced' | 'socket-closed' | 'invoke-timeout';

/**
 * A transport-lifecycle failure, as opposed to a channel that returned an
 * error. The store tells them apart by NAME alone (`store.ts:3564`), so an old
 * daemon missing an additive channel must never produce one of these — it would
 * flap the connection monitor offline over a feature check.
 */
export class WorkassInvokeError extends Error {
  readonly name = 'WorkassInvokeError';
  readonly code: InvokeErrorCode;
  readonly channel: string;
  readonly generation: number;
  constructor(code: InvokeErrorCode, channel: string, generation: number) {
    super(`Workass invoke ${channel} failed: ${code}`);
    this.code = code;
    this.channel = channel;
    this.generation = generation;
  }
}

export type MachineLinkState = 'idle' | 'connecting' | 'open' | 'ready' | 'closed' | 'rejected';

export interface MachineSocketLike {
  send(data: string): void;
  close(): void;
  onopen: (() => void) | null;
  onclose: (() => void) | null;
  onmessage: ((data: string) => void) | null;
  onerror: ((err: unknown) => void) | null;
}

/** Per-machine credential storage. Keyed by machine so one lost token is one machine. */
export interface MachineCredentials {
  deviceToken?: string;
  deviceId?: string;
  deviceName?: string;
}

export interface MachineSocketOptions {
  machineId: string;
  /** `ws://host:port` — no trailing slash needed. */
  url: string;
  deviceName: string;
  credentials?: MachineCredentials;
  /** Held only in memory by the caller; used once, when the daemon parks us. */
  fleetKey?: string;
  open(url: string): MachineSocketLike;
  /** Persist what enrolment produced. Never called with a token we did not verify. */
  saveCredentials?(machineId: string, next: MachineCredentials): void;
  /** A rejected pairing drops the token so the next connect re-requests access. */
  clearCredentials?(machineId: string): void;
  onEvent?(channel: string, payload: unknown): void;
  onState?(state: MachineLinkState, detail?: { reason?: string }): void;
  /**
   * Fired per successful open, with a generation strictly greater than the last.
   * `restarted` is true when the daemon's instanceId changed, which means it
   * lost every engine and all in-memory session state — a resync, not a
   * reconcile. This is the signal the DOM event used to carry, in band.
   */
  onOpen?(info: { generation: number; instanceId: string; restarted: boolean }): void;
  setTimer?(fn: () => void, ms: number): unknown;
  clearTimer?(handle: unknown): void;
}

interface Pending {
  resolve(value: unknown): void;
  reject(err: unknown): void;
  timer: unknown;
  channel: string;
  generation: number;
}

interface Queued {
  id: number;
  data: string;
  generation: number;
}

export class MachineSocket {
  private socket: MachineSocketLike | null = null;
  private opened = false;
  private ready = false;
  private closedByCaller = false;
  private seq = 0;
  private gen = 0;
  private readonly pending = new Map<number, Pending>();
  private queue: Queued[] = [];
  private readonly eventCache: Record<string, unknown> = Object.create(null);
  private readonly subs = new Map<string, Set<(payload: unknown) => void>>();
  private credentials: MachineCredentials;
  private lastInstanceId = '';
  private enrolling = false;
  private state: MachineLinkState = 'idle';

  private readonly opts: MachineSocketOptions;

  constructor(opts: MachineSocketOptions) {
    this.opts = opts;
    this.credentials = { ...(opts.credentials ?? {}) };
  }

  get machineId(): string { return this.opts.machineId; }
  get generation(): number { return this.gen; }
  get linkState(): MachineLinkState { return this.state; }
  get instanceId(): string { return this.lastInstanceId; }

  connect(): void {
    this.closedByCaller = false;
    // A superseded socket's in-flight invokes are rejected, never resolved by
    // whatever the new socket happens to answer (lan_bridge.go:64).
    if (this.pending.size) this.rejectPending('socket-replaced');
    if (this.socket) {
      const prior = this.socket;
      this.socket = null;
      try { prior.close(); } catch { /* already gone */ }
    }
    this.opened = false;
    this.ready = false;
    const gen = ++this.gen;
    this.setState('connecting');
    const socket = this.opts.open(this.socketURL());
    this.socket = socket;

    socket.onopen = () => {
      if (this.socket !== socket || gen !== this.gen) return;
      this.opened = true;
      this.setState('open');
    };
    socket.onclose = () => {
      if (this.socket !== socket || gen !== this.gen) return;
      this.rejectPending('socket-closed');
      this.socket = null;
      this.opened = false;
      this.ready = false;
      this.setState('closed');
      if (!this.closedByCaller) this.later(() => this.connect(), RECONNECT_DELAY_MS);
    };
    socket.onerror = () => { /* onclose follows; nothing useful to add here */ };
    socket.onmessage = (data) => {
      if (this.socket !== socket || gen !== this.gen) return;
      this.handleFrame(data, gen);
    };
  }

  close(): void {
    this.closedByCaller = true;
    this.rejectPending('socket-closed');
    const socket = this.socket;
    this.socket = null;
    this.opened = false;
    this.ready = false;
    if (socket) { try { socket.close(); } catch { /* already gone */ } }
    this.setState('idle');
  }

  invoke<T = unknown>(channel: string, ...args: unknown[]): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const id = ++this.seq;
      const generation = this.gen;
      const budget = SESSION_CHANNELS.has(channel) ? SESSION_INVOKE_TIMEOUT_MS : INVOKE_TIMEOUT_MS;
      const timer = this.later(() => {
        this.failInvoke(id, new WorkassInvokeError('invoke-timeout', channel, generation));
      }, budget);
      this.pending.set(id, { resolve: resolve as (v: unknown) => void, reject, timer, channel, generation });
      const entry: Queued = { id, data: JSON.stringify({ t: 'invoke', id, channel, args }), generation };
      if (this.ready || PRE_READY_CHANNELS.has(channel)) this.send(entry);
      else this.queue.push(entry);
    });
  }

  /**
   * Late subscribers get the last payload on a microtask, re-checking that the
   * callback is still subscribed. The daemon replays provider/catalog/plan-usage
   * events to fresh clients and this cache is why a reload repaints without a
   * turn (lan_bridge.go:133-147).
   */
  on(channel: string, cb: (payload: unknown) => void): () => void {
    let set = this.subs.get(channel);
    if (!set) { set = new Set(); this.subs.set(channel, set); }
    set.add(cb);
    if (Object.prototype.hasOwnProperty.call(this.eventCache, channel)) {
      const payload = this.eventCache[channel];
      void Promise.resolve().then(() => {
        if (this.subs.get(channel)?.has(cb)) { try { cb(payload); } catch { /* one subscriber must not break the rest */ } }
      });
    }
    return () => { this.subs.get(channel)?.delete(cb); };
  }

  // ---- internals ---------------------------------------------------------

  private socketURL(): string {
    const base = this.opts.url.replace(/\/+$/, '');
    const params: string[] = [];
    if (this.credentials.deviceToken) params.push('deviceToken=' + encodeURIComponent(this.credentials.deviceToken));
    const name = this.credentials.deviceName || this.opts.deviceName;
    if (name) params.push('deviceName=' + encodeURIComponent(name));
    return base + '/' + (params.length ? '?' + params.join('&') : '');
  }

  private handleFrame(raw: string, gen: number): void {
    let message: { t?: string; id?: number; result?: unknown; error?: string | null; channel?: string; payload?: unknown };
    try { message = JSON.parse(raw); } catch { return; }

    if (message.t === 'reply') {
      const entry = this.pending.get(message.id as number);
      // A reply from a superseded generation is dropped, never resolved.
      if (!entry || entry.generation !== gen) return;
      this.pending.delete(message.id as number);
      this.opts.clearTimer?.(entry.timer);
      if (message.error) entry.reject(new Error(message.error));
      else entry.resolve(message.result);
      return;
    }
    if (message.t !== 'event' || typeof message.channel !== 'string') return;

    const channel = message.channel;
    const payload = message.payload;
    this.eventCache[channel] = payload;
    if (channel === 'lan:access-state') this.handleAccessState(payload, gen);
    this.opts.onEvent?.(channel, payload);
    const set = this.subs.get(channel);
    if (set) for (const cb of Array.from(set)) { try { cb(payload); } catch { /* isolate subscribers */ } }
  }

  private handleAccessState(payload: unknown, gen: number): void {
    const state = (payload ?? {}) as Record<string, unknown>;
    const instanceId = typeof state.instanceId === 'string' ? state.instanceId : '';

    if (state.state === 'approved') {
      const next: MachineCredentials = { ...this.credentials };
      // A fleet enrolment sends no token — the client derived its own — so this
      // only fires for the classic approve-on-the-host path.
      if (typeof state.deviceToken === 'string' && state.deviceToken) next.deviceToken = state.deviceToken;
      if (typeof state.deviceId === 'string' && state.deviceId) next.deviceId = state.deviceId;
      if (typeof state.name === 'string' && state.name) next.deviceName = state.name;
      this.commitCredentials(next);
      this.ready = true;
      this.setState('ready');
      this.announceOpen(gen, instanceId);
      this.flush();
      return;
    }

    this.ready = false;
    if (state.state === 'waiting') {
      this.setState('open', { reason: 'waiting' });
      // Parked, and we hold the key that makes waiting unnecessary.
      if (this.opts.fleetKey) void this.enrol(gen);
      return;
    }
    if (state.state === 'rejected' || state.state === 'denied' || state.state === 'timeout') {
      if (state.reason === 'invalid-token' || state.state === 'rejected') {
        this.credentials = { ...this.credentials, deviceToken: undefined };
        this.opts.clearCredentials?.(this.opts.machineId);
      }
      // Pairing is an explicit user action.  A terminal response must not make
      // a rejected request silently reconnect forever in the background.
      this.closedByCaller = true;
      this.setState('rejected', { reason: String(state.reason ?? state.state ?? '') });
    }
  }

  private announceOpen(generation: number, instanceId: string): void {
    // The generation is ours; the instance id is the daemon's. Together they
    // separate "reconnected" from "reconnected into a daemon that restarted and
    // lost everything it was holding".
    const restarted = !!instanceId && !!this.lastInstanceId && instanceId !== this.lastInstanceId;
    if (instanceId) this.lastInstanceId = instanceId;
    this.opts.onOpen?.({ generation, instanceId, restarted });
  }

  /**
   * One key, one round trip, no human. The proof never contains the key and the
   * reply never contains the token: both sides derive it from the two nonces, so
   * a listener on this network learns nothing reusable.
   */
  private async enrol(gen: number): Promise<void> {
    if (this.enrolling || !this.opts.fleetKey) return;
    this.enrolling = true;
    try {
      const challenge = await this.invoke<FleetChallenge>('fleet:challenge');
      if (gen !== this.gen) return;
      const answer = answerFleetChallenge(this.opts.fleetKey, challenge);
      await this.invoke('fleet:enroll', {
        clientNonce: answer.clientNonce,
        proof: answer.proof,
        name: this.credentials.deviceName || this.opts.deviceName,
      });
      if (gen !== this.gen) return;
      // Persist only after the daemon accepted the proof. The approval event may
      // already have arrived — frames interleave — so this must be idempotent
      // with handleAccessState rather than ordered against it.
      this.commitCredentials({ ...this.credentials, deviceToken: answer.deviceToken });
    } catch (err) {
      this.setState('rejected', { reason: err instanceof Error ? err.message : String(err) });
    } finally {
      this.enrolling = false;
    }
  }

  private commitCredentials(next: MachineCredentials): void {
    const changed = next.deviceToken !== this.credentials.deviceToken
      || next.deviceId !== this.credentials.deviceId
      || next.deviceName !== this.credentials.deviceName;
    this.credentials = next;
    if (changed) this.opts.saveCredentials?.(this.opts.machineId, { ...next });
  }

  private send(entry: Queued): void {
    if (this.opened && this.socket) this.socket.send(entry.data);
    else this.queue.push(entry);
  }

  private flush(): void {
    while (this.ready && this.queue.length) {
      const entry = this.queue.shift() as Queued;
      // Only still-pending invokes go out: a timed-out one must not be sent late.
      if (this.pending.has(entry.id) && this.socket) this.socket.send(entry.data);
    }
  }

  private failInvoke(id: number, error: WorkassInvokeError): void {
    const entry = this.pending.get(id);
    if (!entry) return;
    this.pending.delete(id);
    this.opts.clearTimer?.(entry.timer);
    const queued = this.queue.findIndex((q) => q.id === id);
    if (queued >= 0) this.queue.splice(queued, 1);
    entry.reject(error);
  }

  private rejectPending(code: InvokeErrorCode): void {
    for (const [id, entry] of Array.from(this.pending)) {
      this.pending.delete(id);
      this.opts.clearTimer?.(entry.timer);
      entry.reject(new WorkassInvokeError(code, entry.channel, entry.generation));
    }
    this.queue = [];
  }

  private setState(state: MachineLinkState, detail?: { reason?: string }): void {
    if (this.state === state && !detail) return;
    this.state = state;
    this.opts.onState?.(state, detail);
  }

  private later(fn: () => void, ms: number): unknown {
    if (this.opts.setTimer) return this.opts.setTimer(fn, ms);
    return setTimeout(fn, ms);
  }
}
