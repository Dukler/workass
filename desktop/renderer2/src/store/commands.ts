// ⌘, opens a command box. It exists for one reason before any other: a daemon
// promotion (or any relaunch that hands Electron a fresh device identity) can
// leave this app RUNNING, CONNECTED, and yet not the controller — empty model
// catalog, every send refused with lan:not-controller — with nothing in the UI
// able to recover it. That state cost the user a terminal on 2026-07-26.
//
// So "Recargar" is a recovery hatch, and the rule that shapes it is: it must not
// depend on the machinery that might be the broken thing. No store round-trip,
// no daemon reply it waits on, no controller privilege it must already hold.
// The only step it truly relies on is location.reload(), which cannot fail.

/** Marker the shell's controller-migration script writes once it has taken the
 *  lease for a device. Its presence is what stops that device stealing control
 *  back later — and therefore what stops an ordinary reload from recovering a
 *  lease stranded on a dead device identity. Clearing it arms exactly one
 *  re-take on the next load. Mirrors desktop/shell/view-server.js. */
export const CONTROLLER_MIGRATION_KEY = 'workass.shell.controllerMigration.v1';

/** A wedged socket must not hold the reload hostage: take-control is attempted,
 *  never awaited past this. */
export const TAKE_CONTROL_TIMEOUT_MS = 1500;

export interface ReconnectDeps {
  /** localStorage, or a stand-in. Absent/throwing storage is survivable. */
  storage?: Pick<Storage, 'removeItem'>;
  /** The LOCAL bridge's lan:take-control. Never the machine router's: control of
   *  a remote daemon is not what a reload on this machine is asking for. */
  takeControl?: () => Promise<unknown>;
	/** Shell-owned daemon restart. It is local-only and may be
	 * absent in a plain browser client, where reconnect still degrades safely. */
	 restartDaemon?: () => Promise<unknown>;
  reload?: () => void;
  timeoutMs?: number;
}

/** Records which steps actually ran — the receipt the caller logs, and what the
 *  test asserts against. */
export interface ReconnectReceipt {
  markerCleared: boolean;
  takeControlAttempted: boolean;
  takeControlSettled: boolean;
	 daemonRestartAttempted: boolean;
	 daemonRestartSettled: boolean;
  reloaded: boolean;
}

function defaultStorage(): Pick<Storage, 'removeItem'> | undefined {
  try { return typeof localStorage === 'undefined' ? undefined : localStorage; } catch { return undefined; }
}

function defaultTakeControl(): (() => Promise<unknown>) | undefined {
  // Deliberately window.api, not the wire/api accessor: when machines are
  // mounted that accessor routes by id tag, and this call has no id.
  const fn = typeof window === 'undefined' ? undefined : (window as unknown as {
    api?: { lanTakeControl?: () => Promise<unknown> };
  }).api?.lanTakeControl;
  return typeof fn === 'function' ? () => fn() : undefined;
}

function defaultRestartDaemon(): (() => Promise<unknown>) | undefined {
	const fn = typeof window === 'undefined' ? undefined : (window as unknown as {
		workassRecovery?: { restartDaemon?: () => Promise<unknown> };
	}).workassRecovery?.restartDaemon;
	return typeof fn === 'function' ? () => fn() : undefined;
}

/**
 * Force this client back into a known-good relationship with its daemon.
 *
 * Three steps, each best-effort except the last:
 *  1. clear the controller marker, so the next load's migration script is
 *     allowed to re-take the lease (the actual fix for "running but not the
 *     controller" — a plain browser reload does NOT do this);
 *  2. ask for the lease on the CURRENT socket, bounded, in case the page never
 *     gets to reload for some reason we haven't met yet;
 *  3. reload. The wire bridge is a script inside the page, so this tears the
 *     socket down and dials a fresh one with a new generation, then re-hydrates
 *     the whole session from the daemon.
 */
export async function forceReconnect(deps: ReconnectDeps = {}): Promise<ReconnectReceipt> {
  const receipt: ReconnectReceipt = {
	markerCleared: false, takeControlAttempted: false, takeControlSettled: false,
	daemonRestartAttempted: false, daemonRestartSettled: false, reloaded: false,
  };

  const storage = deps.storage ?? defaultStorage();
  try { storage?.removeItem(CONTROLLER_MIGRATION_KEY); receipt.markerCleared = true; } catch { /* private mode */ }

  const takeControl = deps.takeControl ?? defaultTakeControl();
  if (takeControl) {
    receipt.takeControlAttempted = true;
    const ms = deps.timeoutMs ?? TAKE_CONTROL_TIMEOUT_MS;
    const settled = await Promise.race([
      Promise.resolve().then(takeControl).then(() => true, () => true),
      new Promise<boolean>((resolve) => setTimeout(() => resolve(false), ms)),
    ]);
    receipt.takeControlSettled = settled;
  }

	const restartDaemon = deps.restartDaemon ?? defaultRestartDaemon();
	if (restartDaemon) {
		receipt.daemonRestartAttempted = true;
		const ms = deps.timeoutMs ?? TAKE_CONTROL_TIMEOUT_MS;
		receipt.daemonRestartSettled = await Promise.race([
			Promise.resolve().then(restartDaemon).then(() => true, () => true),
			new Promise<boolean>((resolve) => setTimeout(() => resolve(false), ms)),
		]);
	}

  const reload = deps.reload ?? (typeof location === 'undefined' ? undefined : () => location.reload());
  if (reload) { receipt.reloaded = true; reload(); }
  return receipt;
}

// ---- the command registry --------------------------------------------------

export interface Command {
  id: string;
  title: string;
  /** One line under the title. Says what will happen, not what the thing is. */
  detail?: string;
  /** Right-aligned keystroke, when the action already has one. */
  hint?: string;
  /** Extra match text, never rendered — lets "reconectar" or "reload" find
   *  "Recargar" without the title carrying three names. */
  keywords?: string;
  run: () => void | Promise<void>;
}

/** Case/accent-insensitive fold so "maquinas" matches "Máquinas". */
export function fold(s: string): string {
  return s.normalize('NFD').replace(/[\u0300-\u036f]/g, '').toLowerCase();
}

/**
 * Substring first, then in-order subsequence — so "rec" and "rcg" both reach
 * "Recargar", while a stray query still returns nothing rather than everything.
 * Earlier matches rank higher; ties keep registry order, which is deliberate:
 * the recovery command is registered first and stays reachable by ⌘, + Enter.
 */
export function filterCommands(commands: Command[], query: string): Command[] {
  const q = fold(query.trim());
  if (!q) return commands;
  const scored: Array<{ cmd: Command; score: number; order: number }> = [];
  commands.forEach((cmd, order) => {
    const hay = fold(`${cmd.title} ${cmd.keywords ?? ''}`);
    const at = hay.indexOf(q);
    if (at >= 0) { scored.push({ cmd, score: at, order }); return; }
    let i = 0;
    for (const ch of hay) { if (ch === q[i]) i += 1; if (i === q.length) break; }
    if (i === q.length) scored.push({ cmd, score: 1000, order });
  });
  scored.sort((a, b) => (a.score - b.score) || (a.order - b.order));
  return scored.map((s) => s.cmd);
}
