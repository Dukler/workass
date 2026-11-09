// Ajustes · Agentes y modelos (owner-approved Settings rework, 2026-11-08,
// mock .dev/mocks/settings-v2.html page 3): one tab per machine, that
// machine's agents on the left, the chosen one's detail on the right. Only
// what Workass really knows is shown: where the CLI was found and when, what
// it still lacks, its version and update, and the environment Workass adds.
// Plan usage lives on the Cuentas page; the per-model scores are gone.

import { useEffect, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent, MouseEvent as ReactMouseEvent } from 'react';
import type { ProviderRecord } from '../../wire/types';
import { store, useApp, type ProviderSettingsMachine } from '../../store/store';
import { has } from '../../wire/api';
import { hasModelIcon, ModelIcon, providerIconBrand } from '../../icons';

export function providerStatusLabel(provider: ProviderRecord): string {
  if (provider.disabledByUser) return 'Desactivado';
  switch (provider.status) {
    case 'ready': return 'Listo';
    case 'needs-login': return 'Requiere login';
    case 'not-found': return 'No instalado';
    case 'error': return 'Error';
    case 'inactive': return 'Inactivo';
    default: return provider.status || 'Desconocido';
  }
}

function statusTone(provider: ProviderRecord): 'run' | 'warn' | 'error' | 'off' {
  if (provider.disabledByUser) return 'off';
  if (provider.status === 'ready') return 'run';
  if (provider.status === 'error') return 'error';
  if (provider.status === 'needs-login' || provider.status === 'not-found') return 'warn';
  return 'off';
}

/** What still keeps an agent from running, in its own words when it gave any. */
function providerIssue(provider: ProviderRecord, machine: ProviderSettingsMachine): string {
  if (provider.disabledByUser || provider.status === 'ready' || provider.status === 'inactive') return '';
  return provider.fixHint || provider.error || provider.message
    || (provider.status === 'not-found' ? `No se encontró el programa en ${machine.name}.` : providerStatusLabel(provider));
}

function ago(iso: string | undefined, now = Date.now()): string {
  const at = iso ? Date.parse(iso) : NaN;
  if (!Number.isFinite(at)) return '';
  const minutes = Math.max(0, Math.round((now - at) / 60_000));
  if (minutes < 1) return 'recién';
  if (minutes < 60) return `hace ${minutes} min`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `hace ${hours} h`;
  return `hace ${Math.round(hours / 24)} d`;
}

function AgentMark({ provider }: { provider: ProviderRecord }) {
  const app = useApp();
  const group = app.groups.find((g) => g.providerId === provider.id);
  const brand = providerIconBrand(provider.id, provider.assistantBrand || group?.assistantBrand);
  const label = provider.name || provider.id;
  return (
    <span className="agent-provider-icon-wrap" aria-hidden="true">
      <span className={`ic agent-provider-icon ${hasModelIcon(brand) ? 'brand' : 'mono'}`}>
        {hasModelIcon(brand) ? <ModelIcon provider={brand} /> : label.charAt(0)}
      </span>
      <span className={`agent-status-dot ${statusTone(provider)}`} />
    </span>
  );
}

function AgentSwitch({ provider, machine }: { provider: ProviderRecord; machine: ProviderSettingsMachine }) {
  const [saving, setSaving] = useState(false);
  const active = !provider.disabledByUser;
  if (!has('providersToggle') || !machine.connected) return null;
  const label = provider.name || provider.id;
  const toggle = async (e: ReactMouseEvent) => {
    e.stopPropagation();
    if (saving) return;
    setSaving(true);
    try { await store.toggleProvider(provider.id, !active, machine.machineId); }
    finally { setSaving(false); }
  };
  return (
    <button
      type="button"
      className={`agent-switch ${active ? 'on' : ''}`}
      role="switch"
      aria-checked={active}
      aria-label={`${active ? 'Desactivar' : 'Activar'} ${label} en ${machine.name}`}
      title={active ? 'Ocultar agente' : 'Mostrar agente'}
      disabled={saving}
      aria-busy={saving}
      onClick={(e) => void toggle(e)}
    >
      <span className="agent-switch-track" aria-hidden="true"><span /></span>
    </button>
  );
}

export function AgentsPage({ initialMachineId = '' }: { initialMachineId?: string }) {
  const app = useApp();
  const machines = store.providerSettingsMachines();
  const [machineId, setMachineId] = useState(initialMachineId);
  const [selected, setSelected] = useState('');
  useEffect(() => { void store.refreshProviderSettings(); }, []);

  const machine = machines.find((m) => m.machineId === machineId) ?? machines[0];
  const providers = machine?.providers ?? [];
  const provider = providers.find((p) => p.id === selected) ?? providers[0];
  const versionOf = (p: ProviderRecord) => (machine && !machine.remote
    ? app.providersUpdates.find((u) => u.providerId === p.id)?.installed : '') || p.cliVersion?.version || '';

  if (!machine) return <section className="stgs-panel"><div className="ag-empty">Sin agentes detectados</div></section>;
  const choose = (id: string) => setSelected(id);
  const onKey = (e: ReactKeyboardEvent, id: string) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); choose(id); }
  };

  return (
    <section className="stgs-panel">
      {machines.length > 1 && (
        <div className="ag-tabs" role="tablist" aria-label="Máquinas">
          {machines.map((m) => (
            <button key={m.machineId || 'local'} type="button" role="tab" aria-selected={m.machineId === machine.machineId}
              className={`ag-tab${m.machineId === machine.machineId ? ' on' : ''}`}
              title={m.connected ? m.name : `${m.name} · sin conexión`}
              onClick={() => { setMachineId(m.machineId); setSelected(''); }}>
              <span className={`dot${m.connected ? ' run' : ''}`} aria-hidden="true" />{m.name}
            </button>
          ))}
        </div>
      )}
      <div className="ag-two">
        <div className="ag-list" role="list" aria-label={`Agentes en ${machine.name}`}>
          {providers.length === 0 && (
            <div className="ag-empty">{machine.connected ? 'Sin agentes detectados' : 'Sin conexión con esta máquina'}</div>
          )}
          {providers.map((p) => {
            const version = versionOf(p);
            const status = providerStatusLabel(p);
            return (
              <div key={p.id} role="listitem" tabIndex={0}
                className={`ag-item${p.id === provider?.id ? ' on' : ''}${p.disabledByUser ? ' disabled' : ''}`}
                onClick={() => choose(p.id)} onKeyDown={(e) => onKey(e, p.id)}>
                <AgentMark provider={p} />
                <span className="ag-tx">
                  <span className="ag-n">{p.name || p.id}</span>
                  {/* Quiet when it works: the version alone; words only for a problem. */}
                  <span className="ag-s">{p.status === 'ready' && !p.disabledByUser ? version : status}</span>
                </span>
                <AgentSwitch provider={p} machine={machine} />
              </div>
            );
          })}
        </div>
        {provider && <AgentDetail provider={provider} machine={machine} version={versionOf(provider)} />}
      </div>
    </section>
  );
}

function AgentDetail({ provider, machine, version }: { provider: ProviderRecord; machine: ProviderSettingsMachine; version: string }) {
  const app = useApp();
  const issue = providerIssue(provider, machine);
  const update = machine.remote ? undefined : app.providersUpdates.find((u) => u.providerId === provider.id && u.updateAvailable);
  const running = store.runningUpdate()?.providerId === provider.id;
  const canDetect = has('providersDetect') && machine.connected;
  const canUpdate = has('providersUpdate') && app.connection === 'connected';
  const found = provider.resolvedCommand
    ? `Encontrado en ${provider.resolvedCommand}`
    : provider.status === 'not-found' ? 'No se encontró el programa' : 'Sin ruta conocida';
  const checked = ago(provider.detectedAt);
  const env = Object.keys(provider.autoEnv ?? {});
  return (
    <div className="ag-det">
      <div className="ag-dh">
        <AgentMark provider={provider} />
        <span className="ag-dn">{provider.name || provider.id}</span>
        <span className="stat" title={providerStatusLabel(provider)}>
          <span className={`dot ${statusTone(provider) === 'run' ? 'run' : statusTone(provider) === 'warn' ? 'warn' : ''}`} />
          {provider.status === 'ready' && !provider.disabledByUser ? null : providerStatusLabel(provider)}
        </span>
      </div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Estado</div><div className="d ag-path">{found}{checked ? ` · comprobado ${checked}` : ''}</div></div>
          {canDetect && <div className="ctl"><button className="btn sm" onClick={() => void store.detectProvider(provider.id, machine.machineId)}>Comprobar</button></div>}
        </div>
        {issue && (
          <div className="set">
            <div className="lbl"><div className="n">Qué falta</div><div className={`d ag-issue${provider.status === 'error' ? ' error' : ''}`}>{issue}</div></div>
          </div>
        )}
        <div className="set">
          <div className="lbl">
            <div className="n">Versión</div>
            <div className="d">{running ? 'Actualizando…' : update ? `Hay una nueva: ${update.latest}` : version ? 'Al día' : 'Sin versión conocida'}</div>
          </div>
          <div className="ctl">
            {version && <span className="stat mono">{version}</span>}
            {update && !running && canUpdate && <button className="btn sm" onClick={() => void store.updateProvider(provider.id)}>Actualizar</button>}
          </div>
        </div>
      </div>
      <details className="ag-adv">
        <summary>Avanzado</summary>
        <div className="group">
          <div className="set">
            <div className="lbl"><div className="n">Comando</div><div className="d ag-path">{provider.resolvedCommand || '—'}</div></div>
          </div>
          <div className="set">
            <div className="lbl">
              <div className="n">Variables que agrega Workass</div>
              <div className="d ag-path">{env.length ? env.join(' · ') : 'Ninguna'}</div>
            </div>
          </div>
        </div>
      </details>
    </div>
  );
}
