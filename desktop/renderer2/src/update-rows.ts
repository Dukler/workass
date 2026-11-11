// The update popover (left pane foot, owner 2026-11-11, mock
// desktop/docs/mocks/update-popover/mock-v2.html, variant A "Línea"):
// one fixed-height row per thing that needs updating, and nothing for what is
// up to date. Each row is two lines (name over state), a right slot that holds
// either the version or one quiet text action, and a hairline on its foot that
// carries the progress. Rows never change size between steps.

import { appUpdaterBlockerText, appUpdaterPhaseText, type AppUpdaterState } from './app-updater.ts';
import type { ProviderUpdate, ProviderUpdateProgress } from './wire/types.ts';

export type UpdateRowTone = '' | 'warn' | 'err' | 'mono';
export type UpdateRowAction = 'self' | 'provider-update' | 'provider-retry';

export interface UpdateRow {
  key: string;
  name: string;
  /** Provider id whose brand mark the row shows; '' for Workass. */
  providerId: string;
  status: string;
  tone: UpdateRowTone;
  /** Full text when the status line is cut. */
  title: string;
  /** The right slot: an action label, or else this version text. */
  action: { label: string; tone: '' | 'warn' | 'plain'; kind: UpdateRowAction; disabled?: boolean } | null;
  version: string;
  /** null: no line. A number 0..1 fills it; 'ind' sweeps. */
  progress: number | 'ind' | null;
}

export interface SelfRowInput {
  state: AppUpdaterState;
  /** The confirm step: what the next click would stop (appUpdateWorkWarning). */
  armed: string;
  actionBusy: boolean;
  /** Seconds the preflight has run, when known. */
  preflightSeconds: number | null;
}

const SELF_PHASES = new Set<AppUpdaterState['phase']>([
  'available', 'downloading', 'staging', 'ready', 'busy', 'installing', 'failed', 'rollback_healthy', 'check_failed',
]);

export function clockText(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

function failureSummary(state: AppUpdaterState): string {
  const failed = state.receipt?.targetVersion || state.targetVersion || '';
  if (state.phase === 'rollback_healthy') return `Falló; volvió a la ${state.currentVersion}`;
  if (state.phase === 'check_failed') return 'No se pudo buscar una versión nueva';
  if (state.error && /no pasó su prueba/.test(state.error)) return `${failed ? `La ${failed}` : 'La versión nueva'} no pasó su prueba`;
  return failed ? `No se pudo instalar la ${failed}` : 'No se pudo actualizar';
}

export function selfUpdateRow({ state, armed, actionBusy, preflightSeconds }: SelfRowInput): UpdateRow | null {
  if (!state.supported || !SELF_PHASES.has(state.phase)) return null;
  const current = state.currentVersion && state.currentVersion !== '—' ? state.currentVersion : '';
  const target = state.targetVersion?.trim() || '';
  const offered = state.availableVersion?.trim() || '';
  const row: UpdateRow = {
    key: 'workass', name: 'Workass', providerId: '', status: '', tone: '', title: appUpdaterPhaseText(state),
    action: null, version: current, progress: null,
  };
  const act = (label: string, tone: '' | 'warn' | 'plain' = '') => ({ label, tone, kind: 'self' as const, disabled: actionBusy });
  switch (state.phase) {
    case 'available':
      row.status = `Hay ${target || 'una versión nueva'}`;
      row.action = act('Actualizar');
      break;
    case 'downloading':
      row.status = `Bajando ${target} · ${Math.round((state.progress || 0) * 100)}%`;
      row.progress = Math.max(0, Math.min(1, state.progress || 0));
      break;
    case 'staging':
      row.status = state.stagingStep === 'preflight'
        ? `Probando ${target} con tu estado${preflightSeconds != null ? ` · ${clockText(preflightSeconds)}` : ''}`
        : `Verificando ${target}`;
      row.progress = 'ind';
      break;
    case 'ready':
      row.status = `${target} lista · solo reinicia`;
      row.action = act('Actualizar');
      break;
    case 'busy':
      row.status = appUpdaterBlockerText(state.blockers);
      row.tone = 'warn';
      row.action = act('Reintentar', 'plain');
      break;
    case 'installing':
      row.status = `Instalando ${target}…`;
      row.progress = 'ind';
      break;
    default:
      // failed, rollback_healthy, check_failed
      row.status = offered ? `Hay ${offered} · el intento anterior falló` : failureSummary(state);
      row.tone = 'err';
      row.action = act(offered ? 'Actualizar' : 'Reintentar', offered ? '' : 'plain');
  }
  if (armed && row.action) {
    row.status = armed;
    row.title = armed;
    row.tone = 'warn';
    row.action = act('Actualizar igual', 'warn');
  }
  if (actionBusy && row.action) row.action = { ...row.action, label: 'Comprobando…' };
  return row;
}

function lastLine(tail: string | undefined): string {
  if (!tail) return '';
  const lines = tail.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  return lines.length ? lines[lines.length - 1] : '';
}

/** The agents that need something: an update to install, one running, or one
 *  that failed and can be retried. An agent that is up to date has no row. */
export function providerUpdateRows(
  updates: readonly ProviderUpdate[],
  progress: Readonly<Record<string, ProviderUpdateProgress | undefined>>,
  failedId: string | null,
  nameOf: (providerId: string, cli: string) => string,
): UpdateRow[] {
  const rows: UpdateRow[] = [];
  for (const update of updates) {
    const run = progress[update.providerId];
    const running = run?.status === 'running';
    const failed = failedId === update.providerId || (run?.status === 'failed' && update.updateAvailable);
    if (!update.updateAvailable && !running) continue;
    const name = nameOf(update.providerId, update.cli);
    const row: UpdateRow = {
      key: update.providerId, name, providerId: update.providerId, status: '', tone: '', title: '',
      action: null, version: update.latest ? `→ ${update.latest}` : '', progress: null,
    };
    if (running) {
      row.status = lastLine(run?.tail) || 'iniciando…';
      row.tone = 'mono';
      row.title = row.status;
      row.progress = 'ind';
    } else if (failed) {
      const reason = run?.error || update.lastError || '';
      const code = run?.exitCode;
      row.status = reason || `No se pudo actualizar${typeof code === 'number' && code !== 0 ? ` (exit ${code})` : ''}`;
      row.tone = 'err';
      row.title = update.hint ? `${row.status}\nA mano: ${update.hint}` : row.status;
      row.action = { label: 'Reintentar', tone: 'plain', kind: 'provider-retry' };
    } else {
      row.status = `${update.installed} → ${update.latest}`;
      row.title = row.status;
      row.action = { label: 'Actualizar', tone: '', kind: 'provider-update' };
    }
    rows.push(row);
  }
  return rows;
}
