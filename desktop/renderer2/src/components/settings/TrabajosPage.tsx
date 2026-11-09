// Ajustes · Trabajos (Settings rework, mock .dev/mocks/settings-v2.html page 7):
// the long jobs agents start with `workass run`. Only what the daemon really
// does is here (job-settings.ts says what each piece is):
//  - when a chat is told that one of this machine's jobs stopped reporting;
//  - each machine's resources: who holds one, since when, and the queue.
// The mock's other rows are left out on purpose: a job's end always reaches
// its chat (agents are promised it, and an off switch would strand them
// waiting), finished jobs have no day-based retention to set, and a resource
// is always held by one job at a time.

import { useEffect, useState } from 'react';
import { store, useApp } from '../../store/store';
import { call, has } from '../../wire/api';
import type { JobResource } from '../../wire/types';
import {
  canEditJobSettings, loadJobSettings, parseResources, resourceLine, saveJobSettings, silenceChoiceLabel,
  silenceChoices, type JobSettings,
} from '../../job-settings';

const RESOURCES_POLL_MS = 10_000;

function SilenceNoticeGroup() {
  const [settings, setSettings] = useState<JobSettings | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    void loadJobSettings().then((loaded) => { if (live) setSettings(loaded); });
    return () => { live = false; };
  }, []);
  // A daemon that does not keep the setting leaves it out of its blob.
  if (!settings || !canEditJobSettings()) return null;
  const current = settings.silenceNoticeAfterMin;
  const choose = async (minutes: number) => {
    if (saving || minutes === current) return;
    const previous = settings;
    setSettings({ silenceNoticeAfterMin: minutes });
    setSaving(true);
    setError('');
    try {
      const saved = await saveJobSettings({ silenceNoticeAfterMin: minutes });
      setSettings(saved ?? previous);
      if (!saved) setError('No se pudo guardar.');
    } catch (err) {
      setSettings(previous);
      setError(`No se pudo guardar: ${String((err as Error)?.message ?? err)}`);
    } finally {
      setSaving(false);
    }
  };
  return (
    <>
      <div className="gtitle">Avisos al chat</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">Avisar si un trabajo deja de reportar</div>
            {error
              ? <div className="d" role="alert" style={{ color: 'var(--del)' }}>{error}</div>
              : <div className="d">Trabajos de esta máquina. Tras un reinicio espera al menos 6 min, para no dar falsas alarmas.</div>}
          </div>
          <div className="ctl"><div className="seg" aria-label="Avisar después de">
            {silenceChoices(current).map((minutes) => (
              <button key={minutes} className={minutes === current ? 'on' : ''} disabled={saving}
                onClick={() => void choose(minutes)}>{silenceChoiceLabel(minutes)}</button>
            ))}
          </div></div>
        </div>
      </div>
    </>
  );
}

interface MachineResources {
  machineId: string;
  name: string;
  resources: JobResource[];
}

// Every machine answers for its own resources: this one through the bridge,
// each connected one through its own link. A machine whose daemon predates
// resources:list, or that is not connected, is simply not listed.
async function readResources(remotes: ReadonlyArray<{ machineId: string; name: string }>): Promise<MachineResources[] | null> {
  const local = (async (): Promise<MachineResources | null> => {
    if (!has('resourcesList')) return null;
    const resources = parseResources(await call('resourcesList'));
    return resources ? { machineId: '', name: store.localMachineName(), resources } : null;
  })();
  const others = remotes.map(async (machine): Promise<MachineResources | null> => {
    try {
      const resources = parseResources(await store.readOnMachine(machine.machineId, 'resources:list'));
      return resources ? { machineId: machine.machineId, name: machine.name, resources } : null;
    } catch {
      return null;
    }
  });
  const answered = (await Promise.all([local, ...others])).filter((row): row is MachineResources => !!row);
  return answered.length ? answered : null;
}

function ResourcesGroup() {
  const app = useApp();
  const [machines, setMachines] = useState<MachineResources[] | null>(null);
  const remotes = app.machines
    .filter((machine) => machine.paired && machine.link === 'ready')
    .map((machine) => ({ machineId: machine.machineId, name: machine.name || machine.reportedName || machine.machineId }));
  const remoteKey = remotes.map((machine) => `${machine.machineId}\u0000${machine.name}`).join('\u0001');
  useEffect(() => {
    let live = true;
    const read = () => { void readResources(remotes).then((next) => { if (live) setMachines(next); }); };
    read();
    const timer = window.setInterval(read, RESOURCES_POLL_MS);
    return () => { live = false; window.clearInterval(timer); };
    // remotes is derived from remoteKey on every render.
  }, [remoteKey]);
  if (!machines) return null;
  const rows = machines.flatMap((machine) => machine.resources.map((resource) => ({ machine, resource })));
  return (
    <>
      <div className="gtitle">Recursos por máquina <span className="gtag">un trabajo a la vez; los demás esperan su turno</span></div>
      <div className="group">
        {rows.length === 0
          ? <div className="lrow"><div className="body"><span className="empty">Ningún trabajo pidió un recurso todavía (<span className="mono">workass run --resource</span>).</span></div></div>
          : rows.map(({ machine, resource }) => (
            <div className="set" key={`${machine.machineId}\u0000${resource.name}`}>
              <div className="lbl">
                <div className="n">{machine.name} · {resource.name}</div>
                <div className="d">{resourceLine(resource)}</div>
              </div>
            </div>
          ))}
      </div>
    </>
  );
}

export function TrabajosPage() {
  return (
    <section className="stgs-panel">
      <SilenceNoticeGroup />
      <ResourcesGroup />
    </section>
  );
}
