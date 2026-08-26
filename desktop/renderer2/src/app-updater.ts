import { useCallback, useEffect, useState } from 'react';

export type AppUpdaterPhase =
  | 'unavailable'
  | 'idle'
  | 'checking'
  | 'check_failed'
  | 'current'
  | 'available'
  | 'downloading'
  | 'staging'
  | 'ready'
  | 'busy'
  | 'installing'
  | 'healthy'
  | 'rollback_healthy'
  | 'failed';

export interface AppUpdaterBlockers {
  reason?: string;
  foregroundTurns?: number;
  backgroundWork?: number;
  providerUpdates?: number;
  admissions?: number;
}

export interface AppUpdaterReceipt {
  phase?: string;
  updateId?: string;
  previousVersion?: string;
  targetVersion?: string;
  updatedAt?: string;
  error?: string;
}

export interface AppUpdaterState {
  supported: boolean;
  phase: AppUpdaterPhase;
  currentVersion: string;
  targetVersion: string | null;
  // Background discovery keeps a failed/rollback receipt visible while still
  // advertising a later, independently verified release through this field.
  availableVersion?: string | null;
  checkedAt: string | null;
  progress: number;
  error: string | null;
  blockers: AppUpdaterBlockers | null;
  receipt: AppUpdaterReceipt | null;
  notes?: string;
  size?: number | null;
}

interface WorkassUpdaterApi {
  getState(): Promise<AppUpdaterState>;
  check(): Promise<AppUpdaterState>;
  apply(): Promise<AppUpdaterState>;
  onState(callback: (state: AppUpdaterState) => void): () => void;
}

declare global {
  interface Window { workassUpdater?: WorkassUpdaterApi; }
}

const INITIAL: AppUpdaterState = {
  supported: false,
  phase: 'unavailable',
  currentVersion: '—',
  targetVersion: null,
  checkedAt: null,
  progress: 0,
  error: null,
  blockers: null,
  receipt: null,
};

export function appUpdaterBlockerText(blockers: AppUpdaterBlockers | null): string {
  if (blockers?.reason === 'another update is committed') return 'Otra actualización ya está activándose.';
  if (blockers?.reason === 'another update is prepared') return 'Otra actualización ya está preparada para activarse.';
  if (blockers?.reason) return 'El updater no pudo obtener el control exclusivo. Reintentá en un momento.';
  if (!blockers) return 'Otra actualización ya controla la activación. Reintentá en un momento.';
  const parts: string[] = [];
  if (blockers.foregroundTurns) parts.push(`${blockers.foregroundTurns} ${blockers.foregroundTurns === 1 ? 'turno activo' : 'turnos activos'}`);
  if (blockers.backgroundWork) parts.push(`${blockers.backgroundWork} ${blockers.backgroundWork === 1 ? 'tarea en segundo plano' : 'tareas en segundo plano'}`);
  if (blockers.providerUpdates) parts.push(`${blockers.providerUpdates} ${blockers.providerUpdates === 1 ? 'agente actualizándose' : 'agentes actualizándose'}`);
  if (blockers.admissions) parts.push(`${blockers.admissions} ${blockers.admissions === 1 ? 'operación iniciándose' : 'operaciones iniciándose'}`);
  return parts.length
    ? `El trabajo activo no bloquea la actualización (${parts.join(' · ')}). Reintentá porque otra actualización controla la activación.`
    : 'Otra actualización ya controla la activación. Reintentá en un momento.';
}

export function appUpdaterPhaseText(state: AppUpdaterState): string {
  const newerOffer = state.availableVersion?.trim();
  switch (state.phase) {
    case 'unavailable': return 'Esta compilación no admite actualizaciones automáticas.';
    case 'idle': return 'Listo para buscar una versión verificada de Workass.';
    case 'checking': return 'Buscando una versión verificada…';
    case 'check_failed': return state.error || 'No se pudo consultar GitHub para buscar actualizaciones.';
    case 'current': return 'Workass está al día.';
    case 'available': return `Workass ${state.targetVersion || ''} está disponible.`.trim();
    case 'downloading': return `Descargando la versión verificada… ${Math.round(state.progress * 100)}%`;
    case 'staging': return 'Verificando checksum, plataforma y daemon incluido…';
    case 'ready': return `Workass ${state.targetVersion || ''} está verificado y listo.`.trim();
    case 'busy': return appUpdaterBlockerText(state.blockers);
    case 'installing': return `Instalando Workass ${state.targetVersion || ''} de forma segura…`.trim();
    case 'healthy': return `Workass ${state.targetVersion || state.receipt?.targetVersion || state.currentVersion} quedó actualizado.`;
    case 'rollback_healthy': return newerOffer
      ? `Workass ${newerOffer} está disponible; la actualización anterior falló y se restauró la versión saludable.`
      : 'La actualización falló; Workass restauró la versión anterior saludable.';
    case 'failed': return newerOffer
      ? `Workass ${newerOffer} está disponible; el intento anterior falló${state.error ? `: ${state.error}` : '.'}`
      : state.error || 'No se pudo completar la actualización.';
    default: return state.phase;
  }
}

export function appUpdaterCardTitle(state: AppUpdaterState): string {
  const offeredVersion = state.availableVersion?.trim()
    || (state.phase === 'available' || state.phase === 'ready' ? state.targetVersion?.trim() : '');
  if (offeredVersion) return `Workass ${offeredVersion}`;
  switch (state.phase) {
    case 'busy': return 'La actualización necesita reintento';
    case 'check_failed': return 'No se pudo buscar actualizaciones';
    case 'rollback_healthy': return 'Workass volvió a la versión anterior';
    case 'failed': return 'No se pudo actualizar Workass';
    case 'healthy': return 'Listo';
    default: return 'Actualizando Workass';
  }
}

export function appUpdaterReceiptIsRecent(receipt: AppUpdaterReceipt | null, now = Date.now(), maxAgeMs = 120_000): boolean {
  if (!receipt?.updatedAt) return false;
  const updatedAt = Date.parse(receipt.updatedAt);
  if (!Number.isFinite(updatedAt)) return false;
  const age = now - updatedAt;
  return age >= 0 && age <= maxAgeMs;
}

export function useAppUpdater() {
  const [state, setState] = useState<AppUpdaterState>(INITIAL);
  const bridge = typeof window === 'undefined' ? undefined : window.workassUpdater;

  useEffect(() => {
    if (!bridge) return undefined;
    let mounted = true;
    void bridge.getState().then((next) => { if (mounted) setState(next); }).catch(() => undefined);
    const unsubscribe = bridge.onState((next) => { if (mounted) setState(next); });
    return () => { mounted = false; unsubscribe?.(); };
  }, [bridge]);

  const run = useCallback(async (action: 'check' | 'apply') => {
    if (!bridge) return state;
    const next = await bridge[action]();
    setState(next);
    return next;
  }, [bridge, state]);

  return {
    state,
    check: useCallback(() => run('check'), [run]),
    apply: useCallback(() => run('apply'), [run]),
  };
}
