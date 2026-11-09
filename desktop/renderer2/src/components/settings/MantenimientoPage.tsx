// Ajustes · Mantenimiento (Settings rework, mock .dev/mocks/settings-v2.html
// page 10): how much disk Workass uses, when quiet chats file themselves away,
// the agents' processes, and a diagnostic report. Every row is real and shows
// only where it works: the disk size and the report come from the daemon
// (storage:usage, diagnostics:export), the folders from the Electron shell
// (preload workassMaintenance), the policy from the settings blob, and the
// process rules from config:get.

import { useEffect, useState } from 'react';
import { store, useApp, useProc } from '../../store/store';
import { callThrow, has } from '../../wire/api';
import type { StorageUsage } from '../../wire/types';
import { ARCHIVE_AFTER_CHOICES, SETTLE_AFTER_CHOICES, lifecycleChoices, lifecycleDaysLabel } from '../../chat-lifecycle';
import {
  agentProcessesLine, diagnosticsFileName, formatBytes, hibernateLine, recycleLine, revealLabel, storagePartsLine, storageTitle,
} from './maintenance';

const STORAGE_RETRY_MS = 1500;
const STORAGE_RETRIES = 40;

function inApp(): boolean {
  return typeof window !== 'undefined' && !!window.workassWindow;
}
function platform(): string | undefined {
  return typeof window !== 'undefined' ? window.workassWindow?.platform : undefined;
}
function folders() {
  return typeof window !== 'undefined' ? window.workassMaintenance : undefined;
}
function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err ?? '');
}

// The first measurement of a large state directory can outlast one read; the
// daemon answers {pending} and keeps walking, so ask again for a while.
function useStorageUsage(): { usage: StorageUsage | null; error: string } {
  const [usage, setUsage] = useState<StorageUsage | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    if (!has('storageUsage')) return;
    let cancelled = false;
    let timer = 0;
    let tries = 0;
    const read = async () => {
      try {
        const reply = await callThrow('storageUsage');
        if (cancelled) return;
        if (reply && !reply.pending) { setUsage(reply); return; }
        if (tries++ < STORAGE_RETRIES) { timer = window.setTimeout(() => { void read(); }, STORAGE_RETRY_MS); return; }
        setError('Todavía se está midiendo. Volvé a esta página en un rato.');
      } catch (err) {
        if (!cancelled) setError(`No se pudo medir: ${errorText(err)}`);
      }
    };
    void read();
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, []);
  return { usage, error };
}

async function showFolder(open: (() => Promise<{ ok: boolean; error?: string }>) | undefined): Promise<string> {
  if (!open) return '';
  try {
    const result = await open();
    return result?.ok ? '' : (result?.error || 'No se pudo abrir la carpeta.');
  } catch (err) {
    return errorText(err) || 'No se pudo abrir la carpeta.';
  }
}

function StorageGroup() {
  const shell = folders();
  const canMeasure = has('storageUsage');
  const canReveal = typeof shell?.showDataFolder === 'function';
  const { usage, error } = useStorageUsage();
  const [revealError, setRevealError] = useState('');
  if (!canMeasure && !canReveal) return null;
  const detail = revealError || error || (usage ? storagePartsLine(usage) : canMeasure ? 'Midiendo…' : '');
  return (
    <>
      <div className="gtitle">{storageTitle(inApp(), platform())}</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Datos de Workass</div>{detail && <div className="d">{detail}</div>}</div>
          <div className="ctl">
            {usage && <span className="stat mono">{formatBytes(usage.totalBytes ?? 0)}</span>}
            {canReveal && (
              <button className="btn sm" onClick={async () => { setRevealError(''); setRevealError(await showFolder(shell?.showDataFolder)); }}>
                {revealLabel(platform())}
              </button>
            )}
          </div>
        </div>
      </div>
    </>
  );
}

function DaysChoice({ label, offered, value, onPick }: { label: string; offered: readonly number[]; value: number; onPick: (days: number) => void }) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {lifecycleChoices(offered, value).map((days) => (
        <button key={days} className={days === value ? 'on' : ''} aria-pressed={days === value} onClick={() => onPick(days)}>
          {lifecycleDaysLabel(days)}
        </button>
      ))}
    </div>
  );
}

// Nothing here deletes or rewrites a chat: the sidebar files a chat by when it
// was last active and when it was put to rest (chat-lifecycle.ts).
function QuietChatsGroup() {
  const app = useApp();
  if (!has('getSettings') || !has('setSettings')) return null;
  const policy = app.chatLifecycle;
  return (
    <>
      <div className="gtitle">Chats sin actividad</div>
      <div className="group">
        <div className="set">
          <div className="lbl"><div className="n">Poner en reposo los chats sin actividad</div><div className="d">Pasan a «En reposo»; nada se borra.</div></div>
          <div className="ctl">
            <DaysChoice label="Poner en reposo después de" offered={SETTLE_AFTER_CHOICES} value={policy.settleAfterDays}
              onPick={(days) => store.setChatLifecycle({ ...policy, settleAfterDays: days })} />
          </div>
        </div>
        <div className="set">
          <div className="lbl"><div className="n">Archivar los chats en reposo</div><div className="d">Salen de la lista; aparecen al buscar y vuelven con Reactivar.</div></div>
          <div className="ctl">
            <DaysChoice label="Archivar después de" offered={ARCHIVE_AFTER_CHOICES} value={policy.archiveAfterDays}
              onPick={(days) => store.setChatLifecycle({ ...policy, archiveAfterDays: days })} />
          </div>
        </div>
      </div>
    </>
  );
}

function AgentsRunningGroup() {
  const app = useApp();
  useProc();
  const hibernate = hibernateLine(app.daemonConfig);
  const recycle = recycleLine(app.daemonConfig);
  if (!app.hasProcChannels && !hibernate && !recycle) return null;
  return (
    <>
      <div className="gtitle">Agentes en marcha</div>
      <div className="group">
        <div className="set">
          <div className="lbl">
            <div className="n">{app.hasProcChannels ? agentProcessesLine(app.processes) : 'Procesos de agentes'}</div>
            {hibernate && <div className="d">{hibernate}</div>}
          </div>
        </div>
        {recycle && (
          <div className="set">
            <div className="lbl"><div className="n">Memoria máxima por agente</div><div className="d">{recycle.detail}</div></div>
            <div className="ctl"><span className="stat mono">{recycle.value}</span></div>
          </div>
        )}
      </div>
    </>
  );
}

function saveJson(value: unknown, fileName: string) {
  const blob = new Blob([JSON.stringify(value, null, 2) + '\n'], { type: 'application/json;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = fileName;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function DiagnosticsGroup() {
  const shell = folders();
  const canOpenLogs = typeof shell?.openLogsFolder === 'function';
  const canExport = has('diagnosticsExport');
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');
  const exportReport = async () => {
    setBusy(true);
    setNote('');
    try {
      const report = await callThrow('diagnosticsExport');
      if (!report || typeof report !== 'object') throw new Error('el servidor no respondió');
      saveJson(report, diagnosticsFileName(new Date()));
    } catch (err) {
      setNote(`No se pudo exportar: ${errorText(err)}`);
    } finally {
      setBusy(false);
    }
  };
  const detail = note || (canExport ? 'El diagnóstico se guarda con las claves y los tokens borrados.' : '');
  return (
    <>
      <div className="gtitle">Diagnóstico</div>
      <div className="group">
        {(canOpenLogs || canExport) && (
          <div className="set">
            <div className="lbl"><div className="n">Registros</div>{detail && <div className="d">{detail}</div>}</div>
            <div className="ctl">
              {canOpenLogs && <button className="btn sm" onClick={async () => { setNote(''); setNote(await showFolder(shell?.openLogsFolder)); }}>Abrir carpeta</button>}
              {canExport && <button className="btn sm" disabled={busy} onClick={() => { void exportReport(); }}>{busy ? 'Exportando…' : 'Exportar diagnóstico'}</button>}
            </div>
          </div>
        )}
        {/* Verified 2026-11-09: no analytics or crash-report code or
            dependency in the daemon, shell or renderer. Provider CLIs are
            their vendors' programs and keep their own behavior. */}
        <div className="set">
          <div className="lbl"><div className="n">Privacidad</div><div className="d">Workass no manda telemetría, analíticas ni reportes de errores. Cada agente sigue las reglas de su proveedor.</div></div>
        </div>
      </div>
    </>
  );
}

export function MantenimientoPage() {
  return (
    <section className="stgs-panel">
      <StorageGroup />
      <QuietChatsGroup />
      <AgentsRunningGroup />
      <DiagnosticsGroup />
    </section>
  );
}
