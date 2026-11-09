import { useCallback, useEffect, useState } from 'react';
import type { AppUpdateWork } from './wire/types';

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
  | 'installed'
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
  // "Instalar solo" (Mac shell). Absent from a shell that predates it.
  autoInstall?: AppUpdaterAutoInstall;
}

export interface AppUpdaterAutoInstall {
  /** The shell can install a staged release by itself (it stages in the background). */
  supported: boolean;
  /** The owner's setting, as the shell last received it. */
  enabled: boolean;
  /** The last unattended attempt found work running and is waiting for it. */
  waiting: boolean;
}

export interface AppUpdaterDiagnostics {
  schemaVersion: number;
  state: Omit<AppUpdaterState, 'receipt' | 'notes' | 'size'>;
  receipt?: Record<string, unknown> | null;
  journal?: Record<string, unknown> | null;
  progressReceipt?: Record<string, unknown> | null;
  workerLogTail?: string;
  lastAgentAuthorization?: Record<string, unknown> | null;
}

export interface AppUpdaterAuthorizedRequest {
  operation_id: string;
  machine_id: string;
  expected_current_version: string;
  expected_target_version: string;
  authorization: string;
}

interface WorkassUpdaterApi {
  getState(): Promise<AppUpdaterState>;
  diagnostics(): Promise<AppUpdaterDiagnostics>;
  check(): Promise<AppUpdaterState>;
  apply(): Promise<AppUpdaterState>;
  applyAuthorized(request: AppUpdaterAuthorizedRequest): Promise<AppUpdaterState & {
    authorizationReceipt?: Record<string, unknown>;
    replayed?: boolean;
  }>;
  onState(callback: (state: AppUpdaterState) => void): () => void;
  setAutoInstall?(enabled: boolean): Promise<AppUpdaterState | null>;
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

/** How long the update card stays armed after warning about running work. */
export const UPDATE_ARM_MS = 12_000;

/** The confirm step's warning: what clicking update again will stop. Empty
 *  means nothing is running and the first click updates straight away. This is
 *  a heads-up, never a gate — the second click always proceeds. */
export function appUpdateWorkWarning(work: AppUpdateWork | null | undefined, titleOf: (tabId: string) => string): string {
  if (!work) return '';
  const parts: string[] = [];
  if (work.foregroundTurns) parts.push(`${work.foregroundTurns} ${work.foregroundTurns === 1 ? 'turno activo' : 'turnos activos'}`);
  if (work.backgroundWork) parts.push(`${work.backgroundWork} ${work.backgroundWork === 1 ? 'tarea en segundo plano' : 'tareas en segundo plano'}`);
  if (!parts.length) return '';
  const names = [...new Set((work.chats ?? []).map((chat) => titleOf(chat.tabId).trim()).filter(Boolean))];
  const where = names.length
    ? ` en ${names.slice(0, 2).join(', ')}${names.length > 2 ? ` y ${names.length - 2} más` : ''}`
    : '';
  const verb = work.foregroundTurns + work.backgroundWork === 1 ? 'Se detendrá' : 'Se detendrán';
  return `${verb} ${parts.join(' y ')}${where}.`;
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
    case 'ready': return state.targetVersion
      ? `Workass ${state.targetVersion} ya está descargado y verificado: actualizar solo reinicia Workass.`
      : 'La actualización ya está descargada y verificada: actualizar solo reinicia Workass.';
    case 'busy': return appUpdaterBlockerText(state.blockers);
    case 'installing': return `Instalando Workass ${state.targetVersion || ''} de forma segura…`.trim();
    case 'healthy':
    case 'installed': return `Workass ${state.targetVersion || state.receipt?.targetVersion || state.currentVersion} quedó actualizado.`;
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
    case 'healthy':
    case 'installed': return 'Listo';
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

// The phases the left panel's update icon marks with its dot: something to
// install, an install under way, or one that failed and can be retried. The
// same set shows a card inside its popover; a quiet background check does not.
const ATTENTION_PHASES: ReadonlySet<AppUpdaterPhase> = new Set<AppUpdaterPhase>([
  'available', 'ready', 'busy', 'downloading', 'staging', 'installing', 'failed', 'rollback_healthy', 'check_failed',
]);

/** Whether the update icon carries its dot. */
export function updatesNeedAttention(
  self: Pick<AppUpdaterState, 'supported' | 'phase'>,
  providers: ReadonlyArray<{ updateAvailable: boolean }>,
  providerRunning: boolean,
  chain: unknown,
): boolean {
  if (providerRunning || !!chain || providers.some((u) => u.updateAvailable)) return true;
  return self.supported && ATTENTION_PHASES.has(self.phase);
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
