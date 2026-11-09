// Ajustes · Actualizaciones (owner-approved Settings rework, 2026-11-09, mock
// .dev/mocks/settings-v2.html page 8): Workass and the agents, on every machine.
//  - This machine: the same shell updater the left panel's update icon uses
//    (useAppUpdater), with the same single "Actualizar" and its warning about
//    running work. No "check now": the row says when it last looked.
//  - Instalar solo: the shell installs a staged release by itself only while
//    nothing would be cut (desktop/shell/update-manager.js autoInstall).
//  - Other machines: each paired machine's own updater, through the
//    exact-machine route of workass_get_update_status / workass_apply_update.
//  - Agents: providers:updates, updated through providers:update.
//  - Actualizar agentes solos: the agents' own switch, separate from the
//    Workass one; the daemon installs a pending agent update by itself once
//    that agent has no turn running (internal/acp/provider_auto_update.go).

import { useEffect, useState } from 'react';
import { appUpdaterBlockerText, appUpdateWorkWarning, UPDATE_ARM_MS, useAppUpdater } from '../../app-updater';
import { store, useApp } from '../../store/store';
import type { MachineView } from '../../store/types';
import { call, has } from '../../wire/api';
import {
  agentUpdateDetail, agoText, AUTO_UPDATE_PROVIDERS_DESCRIPTION, autoInstallDetail, localMachineLabel, machineUpdateDetail, machineUpdateView,
  machineUpdateWarning, selfUpdateAction, selfUpdateDetail, type MachineUpdateView,
} from './updates';

/** Re-renders now and then so "hace N min" stays true while the page is open. */
function useMinuteClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

/** A click that first says what it would stop; the second click goes ahead.
 *  Disarms on its own so a stale warning never stands in for a fresh count. */
function useArmed(): [string, (warning: string) => void] {
  const [armed, setArmed] = useState('');
  useEffect(() => {
    if (!armed) return undefined;
    const timer = window.setTimeout(() => setArmed(''), UPDATE_ARM_MS);
    return () => window.clearTimeout(timer);
  }, [armed]);
  return [armed, setArmed];
}

function SelfRow({ updater, now }: { updater: ReturnType<typeof useAppUpdater>; now: number }) {
  const app = useApp();
  const state = updater.state;
  const [busy, setBusy] = useState(false);
  const [armed, setArmed] = useArmed();
  const action = selfUpdateAction(state);
  const run = async () => {
    if (busy) return;
    setBusy(true);
    try {
      if (!armed) {
        const warning = appUpdateWorkWarning(
          await call('appUpdateWork'),
          (tabId) => app.chats.find((chat) => chat.id === tabId)?.title ?? '',
        );
        if (warning) { setArmed(warning); return; }
      }
      setArmed('');
      const next = await updater.apply();
      if (next.phase === 'busy') store.addToast('La actualización necesita reintento', appUpdaterBlockerText(next.blockers));
    } catch (error) {
      store.addToast('No se pudo actualizar', String((error as Error)?.message || error));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="set">
      <div className="lbl">
        <div className="n">{localMachineLabel()}</div>
        <div className={`d${armed ? ' ag-issue' : ''}`} title={state.error || undefined}>{armed || selfUpdateDetail(state, now)}</div>
      </div>
      {action && (
        <div className="ctl">
          <button type="button" className="btn sm" disabled={busy} onClick={() => void run()}>
            {busy ? 'Comprobando…' : armed ? 'Actualizar igual' : action}
          </button>
        </div>
      )}
    </div>
  );
}

function AutoInstallRow({ updater }: { updater: ReturnType<typeof useAppUpdater> }) {
  const app = useApp();
  if (!updater.state.autoInstall?.supported || !has('setSettings')) return null;
  const on = app.autoInstallUpdates;
  return (
    <div className="set">
      <div className="lbl">
        <div className="n">Actualizar Workass solo</div>
        <div className="d">{autoInstallDetail(updater.state)}</div>
      </div>
      <div className="ctl">
        <button type="button" className={`toggle ${on ? 'on' : ''}`} role="switch" aria-checked={on}
          aria-label="Actualizar Workass solo" onClick={() => store.setAutoInstallUpdates(!on)}>
          <span className="knob" />
        </button>
      </div>
    </div>
  );
}

interface MachineRead { view: MachineUpdateView | null; error: string }
interface MachineApply { target: string; since: number }

const MACHINE_READ_MS = 60_000;
const MACHINE_APPLYING_READ_MS = 10_000;
const MACHINE_APPLY_PATIENCE_MS = 10 * 60_000;

function OtherMachineRows() {
  const app = useApp();
  const machines = app.machines.filter((m) => m.paired);
  const ready = machines.filter((m) => m.link === 'ready').map((m) => m.machineId).join('\n');
  const [reads, setReads] = useState<Record<string, MachineRead>>({});
  const [applying, setApplying] = useState<Record<string, MachineApply>>({});
  const anyApplying = Object.keys(applying).length > 0;

  useEffect(() => {
    let alive = true;
    const ids = ready ? ready.split('\n') : [];
    const readAll = () => {
      for (const id of ids) {
        void store.machineUpdateStatus(id).then(
          (status) => { if (alive) setReads((all) => ({ ...all, [id]: { view: machineUpdateView(status), error: '' } })); },
          (error) => {
            if (alive) setReads((all) => ({ ...all, [id]: { view: all[id]?.view ?? null, error: String((error as Error)?.message || error) } }));
          },
        );
      }
    };
    readAll();
    const timer = window.setInterval(readAll, anyApplying ? MACHINE_APPLYING_READ_MS : MACHINE_READ_MS);
    return () => { alive = false; window.clearInterval(timer); };
  }, [ready, anyApplying]);

  // An apply is over once that machine reports the version it went to, or
  // after a long while in which it never came back with it.
  useEffect(() => {
    const done = Object.entries(applying).filter(([id, apply]) => reads[id]?.view?.current === apply.target
      || Date.now() - apply.since > MACHINE_APPLY_PATIENCE_MS);
    if (!done.length) return;
    setApplying((all) => {
      const next = { ...all };
      for (const [id] of done) delete next[id];
      return next;
    });
  }, [reads, applying]);

  if (!machines.length) return null;
  return (
    <>
      {machines.map((m) => (
        <OtherMachineRow key={m.machineId} machine={m} read={reads[m.machineId]} applying={applying[m.machineId]}
          onApplied={(target) => setApplying((all) => ({ ...all, [m.machineId]: { target, since: Date.now() } }))} />
      ))}
    </>
  );
}

function OtherMachineRow({ machine, read, applying, onApplied }: {
  machine: MachineView;
  read: MachineRead | undefined;
  applying: MachineApply | undefined;
  onApplied: (target: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [armed, setArmed] = useArmed();
  const live = machine.link === 'ready';
  const view = read?.view ?? null;
  let detail: string;
  if (applying) detail = view ? machineUpdateDetail(view, applying.target) : `actualizando a ${applying.target}…`;
  else if (!live) detail = 'Sin conexión ahora.';
  else if (read?.error && !view) detail = 'No respondió con su versión.';
  else if (!view) detail = 'Consultando…';
  else detail = machineUpdateDetail(view);
  const canApply = live && !applying && !!view?.offered && !view.updating;

  const run = async () => {
    if (busy || !view?.offered) return;
    setBusy(true);
    try {
      if (!armed) {
        const warning = machineUpdateWarning(machine.name, await store.machineUpdateWork(machine.machineId), view.wouldDie);
        if (warning) { setArmed(warning); return; }
      }
      setArmed('');
      await store.applyMachineUpdate(machine.machineId, view.current, view.offered);
      onApplied(view.offered);
    } catch (error) {
      store.addToast(`No se pudo actualizar ${machine.name}`, String((error as Error)?.message || error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="set">
      <div className="lbl">
        <div className="n">{machine.name}</div>
        <div className={`d${armed ? ' ag-issue' : ''}`} title={read?.error || view?.error || undefined}>{armed || detail}</div>
      </div>
      {canApply && (
        <div className="ctl">
          <button type="button" className="btn sm" disabled={busy} onClick={() => void run()}>
            {busy ? 'Comprobando…' : armed ? 'Actualizar igual' : `Actualizar ${machine.name}`}
          </button>
        </div>
      )}
    </div>
  );
}

function AgentRows({ now }: { now: number }) {
  const app = useApp();
  const enabled = new Set(app.providers.filter((p) => p.enabled && !p.disabledByUser).map((p) => p.id));
  const rows = app.providersUpdates.filter((u) => enabled.has(u.providerId) || u.updateAvailable);
  const canSave = has('setSettings');
  if (!rows.length && !canSave) return null;
  const canUpdate = has('providersUpdate') && app.connection === 'connected';
  const auto = canSave && app.autoUpdateProviders;
  const checked = agoText(app.providersCheckedAt, now);
  return (
    <>
      <div className="gtitle">Agentes{checked && <span className="gtag">revisado {checked}</span>}</div>
      <div className="group">
        {canSave && (
          <div className="set">
            <div className="lbl">
              <div className="n">Actualizar agentes solos</div>
              <div className="d">{AUTO_UPDATE_PROVIDERS_DESCRIPTION}</div>
            </div>
            <div className="ctl">
              <button type="button" className={`toggle ${auto ? 'on' : ''}`} role="switch" aria-checked={auto}
                aria-label="Actualizar agentes solos" onClick={() => store.setAutoUpdateProviders(!auto)}>
                <span className="knob" />
              </button>
            </div>
          </div>
        )}
        {rows.map((u) => {
          const progress = app.updateProgress[u.providerId];
          const running = progress?.status === 'running';
          return (
            <div className="set" key={u.providerId}>
              <div className="lbl">
                <div className="n">{store.providerName(u.providerId) || u.cli}</div>
                <div className="d" title={u.lastError || progress?.error || undefined}>{agentUpdateDetail(u, progress, auto && !u.lastError)}</div>
              </div>
              {u.updateAvailable && !running && canUpdate && (
                <div className="ctl">
                  <button type="button" className="btn sm" onClick={() => void store.updateProvider(u.providerId)}>Actualizar</button>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </>
  );
}

export function ActualizacionesPage() {
  const app = useApp();
  const updater = useAppUpdater();
  const now = useMinuteClock();
  // A browser client has no shell here: it cannot update this machine's app.
  const shell = typeof window !== 'undefined' && !!window.workassUpdater;
  const others = app.machines.some((m) => m.paired);
  return (
    <section className="stgs-panel">
      {(shell || others) && (
        <>
          <div className="gtitle">Workass</div>
          <div className="group">
            {shell && <SelfRow updater={updater} now={now} />}
            {shell && <AutoInstallRow updater={updater} />}
            <OtherMachineRows />
          </div>
        </>
      )}
      <AgentRows now={now} />
    </section>
  );
}
