// Client-side daemon-connection health monitor.
//
// The daemon-injected shim (internal/httpserve/lan_bridge.go) auto-reconnects
// its WebSocket (`ws.onclose → setTimeout(connect, 1500)`) and re-flushes queued
// invokes on reconnect, but it exposes ZERO open/close/ready signal to the
// renderer: a dropped socket looks identical to a slow daemon — invokes silently
// queue forever and never resolve or reject. That is why a dead daemon left the
// composer accepting sends and the turn row spinning "Trabajando…" indefinitely.
//
// Since we cannot edit the frozen wire contract to add a close event, we detect
// liveness ourselves: a periodic cheap invoke (app:meta) raced against a timeout.
// A queued ping resolves the instant the shim reconnects and re-flushes it, so a
// generous probe timeout gives snappy reconnect detection (~2s after the daemon
// returns) without spamming — and every ping rejection/timeout is swallowed, so
// reconnect attempts never touch the console.

export type ConnStatus = 'connected' | 'disconnected' | 'reconnecting';

const HEALTH_INTERVAL = 5000; // gap between pings while healthy
const HEALTH_TIMEOUT = 5000;  // a healthy daemon must answer a ping within this
const PROBE_TIMEOUT = 9000;   // during an outage, long enough for the shim's 1.5s reconnect to flush a queued ping
const BACKOFF_MIN = 1000;     // exponential backoff between failed probes
const BACKOFF_MAX = 10000;
const RECONNECT_DEADLINE = 15000;

/** Resolves iff the daemon answered; rejects/hangs otherwise. */
type Pinger = () => Promise<unknown>;

export class ConnectionMonitor {
  private status: ConnStatus = 'connected';
  private backoff = BACKOFF_MIN;
  private running = false;
  private wakeResolve: (() => void) | null = null;
  private sleepTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly ping: Pinger,
    private readonly onStatus: (s: ConnStatus) => void,
    private readonly onReconnect: () => void | Promise<void>,
  ) {}

  start(): void {
    if (this.running) return;
    this.running = true;
    void this.loop();
  }
  stop(): void { this.running = false; this.wake(); }
  connected(): boolean { return this.status === 'connected'; }

  /** Force the loop to re-check now — e.g. a send that looks stuck. */
  probeNow(): void { this.wake(); }
  /** A failed hydration is also a lost-sync condition. Re-enter the same
   * backoff/reconnect path instead of leaving boot permanently partial. */
  markDisconnected(): void {
    this.backoff = BACKOFF_MIN;
    this.setStatus('disconnected');
    this.wake();
  }

  private wake(): void {
    const r = this.wakeResolve;
    this.wakeResolve = null;
    if (this.sleepTimer) { clearTimeout(this.sleepTimer); this.sleepTimer = null; }
    if (r) r();
  }
  private sleep(ms: number): Promise<void> {
    return new Promise<void>((resolve) => {
      this.wakeResolve = resolve;
      this.sleepTimer = setTimeout(() => {
        if (this.wakeResolve === resolve) {
          this.wakeResolve = null;
          this.sleepTimer = null;
          resolve();
        }
      }, ms);
    });
  }

  // Every rejection/timeout is caught here so a downed daemon never logs.
  private pingWithTimeout(timeout: number): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      let settled = false;
      const done = (ok: boolean) => { if (!settled) { settled = true; resolve(ok); } };
      const to = setTimeout(() => done(false), timeout);
      this.ping().then(() => { clearTimeout(to); done(true); }, () => { clearTimeout(to); done(false); });
    });
  }

  private setStatus(s: ConnStatus): void {
    if (this.status === s) return;
    this.status = s;
    this.onStatus(s);
  }

  private async loop(): Promise<void> {
    while (this.running) {
      if (this.status === 'connected') {
        await this.sleep(HEALTH_INTERVAL);
        if (!this.running) break;
        const ok = await this.pingWithTimeout(HEALTH_TIMEOUT);
        if (!this.running) break;
        if (!ok) { this.backoff = BACKOFF_MIN; this.setStatus('disconnected'); }
      } else {
        this.setStatus('reconnecting');
        const ok = await this.pingWithTimeout(PROBE_TIMEOUT);
        if (!this.running) break;
        if (ok) {
          this.setStatus('connected');
          this.backoff = BACKOFF_MIN;
          let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
          try {
            await Promise.race([
              Promise.resolve(this.onReconnect()),
              new Promise<void>((_, reject) => {
                reconnectTimer = setTimeout(() => reject(new Error('reconnect reconciliation timed out')), RECONNECT_DEADLINE);
              }),
            ]);
          } catch { /* keep the loop alive across a resync hiccup */ }
          finally { if (reconnectTimer) clearTimeout(reconnectTimer); }
        } else {
          await this.sleep(this.backoff);
          this.backoff = Math.min(this.backoff * 2, BACKOFF_MAX);
        }
      }
    }
  }
}
