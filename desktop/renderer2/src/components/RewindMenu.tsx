// R4 rewind menu — a checkpoint list overlay. Opened by double-Esc (all turns)
// or by a turn's "Deshacer" affordance (focused on that turn). Confirming a row
// invokes chat:rewind, which restores the worktree to the pre-turn state; the
// daemon's chat:checkpoint-restored event closes the menu and drops a quiet
// "Estado restaurado…" step row into the transcript. Refusals (e.g. the repo was
// modified outside this chat) are rendered honestly from the structured error.

import type { ChatCheckpoint } from '../wire/types';
import { store, useApp } from '../store/store';

function fmtTime(ts: string): string {
  const t = new Date(ts).getTime();
  if (Number.isNaN(t)) return '';
  const secs = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (secs < 60) return 'hace unos segundos';
  const mins = Math.round(secs / 60);
  if (mins < 60) return `hace ${mins} min`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `hace ${hrs} h`;
  return new Date(ts).toLocaleDateString('es', { day: 'numeric', month: 'short' });
}
function changedTotal(cp: ChatCheckpoint): number {
  return cp.repos.reduce((s, r) => s + (r.skipped ? 0 : r.changedFiles), 0);
}

export function RewindMenu() {
  const app = useApp();
  const rw = app.rewind;
  if (!rw.open) return null;
  const items = [...rw.items].sort((a, b) => b.turnSeq - a.turnSeq);
  const busy = rw.busyTurn != null;

  return (
    <div className="ovl" onMouseDown={(e) => { if (e.target === e.currentTarget) store.closeRewind(); }}>
      <div className="rewind" role="dialog" aria-label="Volver a un punto anterior">
        <div className="rwhead">
          <b>Volver a un punto anterior</b>
          <span className="sp" />
          <button className="tico" title="Cerrar · Esc" onClick={() => store.closeRewind()}>✕</button>
        </div>
        <div className="rwsub">Restaura los archivos de trabajo al estado previo a un turno. No toca el historial de git ni tu índice.</div>

        {rw.error && <div className="rwerr">{rw.error}</div>}
        {rw.loading && <div className="rwempty">Cargando puntos de control…</div>}
        {!rw.loading && !rw.error && items.length === 0 && (
          <div className="rwempty">Este chat todavía no registró puntos de control con cambios.</div>
        )}

        {items.length > 0 && (
          <div className="rwlist">
            {items.map((cp) => {
              const n = changedTotal(cp);
              const rowBusy = rw.busyTurn === cp.turnSeq;
              const focus = rw.focusTurn === cp.turnSeq;
              return (
                <div key={cp.turnSeq} className={`rwrow ${focus ? 'focus' : ''}`}>
                  <div className="rwinfo">
                    <div className="rt">Volver a antes del turno {cp.turnSeq}</div>
                    <div className="rm">
                      {n} {n === 1 ? 'archivo' : 'archivos'} · {fmtTime(cp.ts)}
                      {cp.repos.length > 1 ? ` · ${cp.repos.length} repos` : ''}
                    </div>
                  </div>
                  <button className="btn" disabled={busy} onClick={() => void store.rewindTo(cp.turnSeq)}>
                    {rowBusy ? 'Revirtiendo…' : 'Volver ↺'}
                  </button>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
