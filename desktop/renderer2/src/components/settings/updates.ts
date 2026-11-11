// What Ajustes · Actualizaciones says, kept apart from the page so it can be
// tested: this machine's Workass updater (the shell's UpdateManager through
// useAppUpdater), the other fleet machines' updaters (the exact-machine status
// the agent tools read), and each agent CLI's version (providers:updates).

import { appUpdaterBlockerText, appUpdaterPhaseText, type AppUpdaterState } from '../../app-updater';
import type { AppUpdateWork, ProviderUpdate, ProviderUpdateProgress } from '../../wire/types';

export function agoText(iso: string | null | undefined, now = Date.now()): string {
  const at = iso ? Date.parse(iso) : NaN;
  if (!Number.isFinite(at)) return '';
  const minutes = Math.max(0, Math.round((now - at) / 60_000));
  if (minutes < 1) return 'recién';
  if (minutes < 60) return `hace ${minutes} min`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `hace ${hours} h`;
  return `hace ${Math.round(hours / 24)} d`;
}

export function localMachineLabel(platform = typeof navigator === 'undefined' ? '' : navigator.platform): string {
  if (/mac/i.test(platform)) return 'Esta Mac';
  if (/win/i.test(platform)) return 'Esta PC';
  return 'Esta computadora';
}

function parseVersion(value: unknown): number[] | null {
  const match = /^(\d+)\.(\d+)\.(\d+)$/.exec(String(value ?? '').trim());
  return match ? match.slice(1).map(Number) : null;
}

/** Whether `candidate` is a newer X.Y.Z than `current`. */
export function isNewerVersion(candidate: unknown, current: unknown): boolean {
  const next = parseVersion(candidate);
  const now = parseVersion(current);
  if (!next || !now) return false;
  for (let i = 0; i < 3; i++) {
    if (next[i] !== now[i]) return next[i] > now[i];
  }
  return false;
}

// ---- this machine --------------------------------------------------------

/** The single action on this machine's row. Never a "check now": the owner
 *  rejected it; the row says when it last looked instead. */
export function selfUpdateAction(state: AppUpdaterState): 'Actualizar' | 'Reintentar' | null {
  if (!state.supported) return null;
  switch (state.phase) {
    case 'available':
    case 'ready':
      return 'Actualizar';
    case 'busy':
      return 'Reintentar';
    case 'failed':
    case 'rollback_healthy':
      return state.availableVersion ? 'Actualizar' : 'Reintentar';
    default:
      return null;
  }
}

export function selfUpdateDetail(state: AppUpdaterState, now = Date.now()): string {
  const current = state.currentVersion && state.currentVersion !== '—' ? state.currentVersion : '';
  const lead = current ? `${current} · ` : '';
  const target = state.targetVersion || state.availableVersion || '';
  const checked = agoText(state.checkedAt, now);
  switch (state.phase) {
    case 'unavailable': return `${lead}esta compilación no se actualiza desde la app.`;
    case 'idle': return `${lead}todavía no buscó una versión nueva.`;
    case 'checking': return `${lead}buscando una versión nueva…`;
    case 'current':
    case 'healthy':
    case 'installed': return `${lead}la última${checked ? ` · revisado ${checked}` : ''}.`;
    case 'check_failed': return `${lead}no se pudo buscar una versión nueva${checked ? ` (${checked})` : ''}. Lo vuelve a intentar sola.`;
    case 'available': return `${lead}hay ${target}.`;
    case 'downloading': return `${lead}bajando ${target}… ${Math.round((state.progress || 0) * 100)}%`;
    case 'staging': return state.stagingStep === 'preflight'
      ? `${lead}probando ${target} antes de instalarla…`
      : `${lead}verificando ${target}…`;
    case 'ready': return `${lead}${target} ya está bajada: actualizar solo reinicia Workass.`;
    case 'busy': return appUpdaterBlockerText(state.blockers);
    case 'installing': return `Instalando ${target}…`;
    default: return appUpdaterPhaseText(state);
  }
}

export const AUTO_INSTALL_DESCRIPTION = 'Cuando sale una versión, la instala apenas no corte ningún trabajo ni subagente. Si no, espera.';

/** The "Instalar solo" row: what it does, or, with a release staged, what it is
 *  doing about it right now (the shell's own account, not the setting's). */
export function autoInstallDetail(state: AppUpdaterState): string {
  const auto = state.autoInstall;
  if (!auto?.enabled || state.phase !== 'ready' || !state.targetVersion) return AUTO_INSTALL_DESCRIPTION;
  return auto.waiting
    ? `Hay trabajo en curso: ${state.targetVersion} se instala apenas termine.`
    : `${state.targetVersion} se instala en un momento.`;
}

// ---- other machines -------------------------------------------------------

export interface MachineUpdateView {
  current: string;
  /** A newer release that machine's updater offers, exactly as its apply expects it. */
  offered: string;
  phase: string;
  supported: boolean;
  /** Packaged machine with no Workass window open: its updater cannot run. */
  appClosed: boolean;
  /** An update is already being applied there. */
  updating: boolean;
  /** Jobs and subagents there that an update would stop; null when it did not say. */
  wouldDie: number | null;
  error: string;
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/** Reads the exact-machine update status (app:update-status) of a renderer,
 *  headless or window-less machine into what the row shows. */
export function machineUpdateView(status: unknown): MachineUpdateView {
  const top = record(status);
  const diagnostics = record(top.diagnostics);
  const state = record(diagnostics.state);
  const current = text(state.currentVersion) || text(top.currentVersion);
  const phase = text(state.phase);
  const appClosed = top.daemonOnly === true;
  const supported = !appClosed && state.supported === true;
  const candidate = text(state.targetVersion) || text(state.availableVersion);
  const headless = top.headless === true;
  const updating = phase === 'installing' || phase === 'activating' || (headless && phase === 'downloading');
  const jobs = record(top.runningJobs ?? diagnostics.runningJobs);
  const wouldDie = Array.isArray(jobs.wouldDie) ? jobs.wouldDie.length : null;
  return {
    current,
    offered: supported && isNewerVersion(candidate, current) ? candidate : '',
    phase,
    supported,
    appClosed,
    updating,
    wouldDie,
    error: text(state.error),
  };
}

export function machineUpdateDetail(view: MachineUpdateView, applyingTo = ''): string {
  const lead = view.current ? `${view.current} · ` : '';
  if (applyingTo || view.updating) return `${lead}actualizando a ${applyingTo || view.offered || 'una versión nueva'}…`;
  if (view.appClosed) return `${lead}Workass está cerrado ahí: abrilo para poder actualizarlo.`;
  if (!view.supported) return `${lead}no se actualiza desde la app.`;
  if (view.offered) {
    if (view.phase === 'ready') return `${lead}${view.offered} ya está bajada ahí.`;
    if (view.phase === 'downloading' || view.phase === 'staging') return `${lead}hay ${view.offered}, bajándose.`;
    return `${lead}hay ${view.offered}.`;
  }
  if (view.phase === 'check_failed') return `${lead}no pudo buscar una versión nueva.`;
  if (view.phase === 'failed' || view.phase === 'rollback_healthy') return `${lead}el último intento de actualizar falló.`;
  return `${lead}la última.`;
}

/** What updating that machine would stop, said before the click goes through.
 *  Background work and jobs that die overlap (both count subagents), so the
 *  larger of the two is said once. Empty when nothing would stop. */
export function machineUpdateWarning(name: string, work: AppUpdateWork | null, wouldDie: number | null): string {
  const turns = work?.foregroundTurns ?? 0;
  const background = Math.max(work?.backgroundWork ?? 0, wouldDie ?? 0);
  const parts: string[] = [];
  if (turns) parts.push(`${turns} ${turns === 1 ? 'turno activo' : 'turnos activos'}`);
  if (background) parts.push(`${background} ${background === 1 ? 'tarea en segundo plano' : 'tareas en segundo plano'}`);
  if (!parts.length) return '';
  return `${turns + background === 1 ? 'Se detendrá' : 'Se detendrán'} ${parts.join(' y ')} en ${name}.`;
}

// ---- agents ---------------------------------------------------------------

export const AUTO_UPDATE_PROVIDERS_DESCRIPTION = 'Cuando sale una versión de un agente, la instala apenas ese agente no tenga un turno en curso. Si no, espera.';

/** One agent's row. With "Actualizar agentes solos" on (auto), a pending
 *  version says it installs by itself instead of only that it exists; one that
 *  already failed is left to the button. */
export function agentUpdateDetail(update: ProviderUpdate, progress?: ProviderUpdateProgress, auto = false): string {
  const installed = update.installed || '—';
  if (progress?.status === 'running') return `${installed} · actualizando a ${update.latest}…`;
  if (update.updateAvailable && (progress?.status === 'failed' || update.lastError)) {
    return `${installed} · no se pudo actualizar a ${update.latest}.${update.hint ? ` A mano: ${update.hint}` : ''}`;
  }
  if (update.updateAvailable && auto) return `${installed} · ${update.latest} se instala solo apenas este agente no tenga un turno en curso.`;
  if (update.updateAvailable) return `${installed} · hay ${update.latest}.`;
  return `${installed} · al día.`;
}
